use super::{hub::Hub, lock::DataLock, stream::Streams};
use crate::{
    agent::{
        journal::{Core, LogJournal},
        names, routing,
        runtime::{Actor, Channel, Environment},
    },
    config, escalation, event, eventlog,
    model::registry::Registry,
    paths::Paths,
    protocol, sandbox,
    wire::Time,
};
use anyhow::{Result, ensure};
use std::{
    collections::BTreeMap,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    sync::{
        Arc, Mutex, RwLock, Weak,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};
use tokio_util::{sync::CancellationToken, task::TaskTracker};
#[derive(Clone, serde::Serialize)]
pub struct ModelEpoch {
    pub seq: i64,
    pub active: bool,
}
pub struct Daemon {
    pub paths: Paths,
    pub home: PathBuf,
    pub registry: Arc<Registry>,
    pub web: RwLock<Option<Arc<dyn super::frontdoors::Web>>>,
    pub discord: RwLock<Option<Arc<dyn super::frontdoors::Discord>>>,
    pub log: Arc<eventlog::Log>,
    pub hub: Arc<Hub>,
    pub prompts: Arc<escalation::Manager>,
    pub cancel: CancellationToken,
    pub model_epochs: Arc<Mutex<BTreeMap<String, ModelEpoch>>>,
    pub(crate) channels: RwLock<BTreeMap<String, Arc<Channel>>>,
    pub(crate) names: Mutex<()>,
    pub(crate) editor: Mutex<()>,
    environment: Arc<Environment>,
    pub(super) trust: RwLock<BTreeMap<String, String>>,
    pub(super) logins: Mutex<BTreeMap<String, Arc<super::logins::Login>>>,
    trust_prompts: Mutex<BTreeMap<PathBuf, TrustPrompt>>,
    streams: Arc<Streams>,
    pub(super) owner: Weak<Self>,
    pub(super) plan_history: super::planusage::History,
    lock: Mutex<Option<DataLock>>,
    close: tokio::sync::Mutex<bool>,
    tasks: TaskTracker,
    loops_started: AtomicBool,
}
struct TrustPrompt {
    id: String,
    hash: String,
    channel: String,
    cancel: CancellationToken,
}
pub(super) fn lookup(name: &str) -> Option<String> {
    std::env::var(name).ok()
}
pub(super) fn escalation_config(config: &config::Effective) -> escalation::Config {
    escalation::Config {
        claim_timeout: Duration::from_nanos(config.escalation.claim_timeout_ns.max(0) as u64),
        answer_timeout: Duration::from_nanos(config.escalation.answer_timeout_ns.max(0) as u64),
        default: match config.escalation.default {
            crate::policy::Verb::Allow => "allow",
            crate::policy::Verb::Ask => "ask",
            crate::policy::Verb::Deny => "deny",
        }
        .into(),
        ..Default::default()
    }
}
impl Daemon {
    pub async fn open(paths: Paths, home: PathBuf, registry: Arc<Registry>) -> Result<Arc<Self>> {
        let lock = DataLock::acquire(&paths.data)?;
        Self::open_locked(paths, home, registry, lock).await
    }
    pub(crate) async fn open_locked(
        paths: Paths,
        home: PathBuf,
        registry: Arc<Registry>,
        lock: DataLock,
    ) -> Result<Arc<Self>> {
        std::fs::set_permissions(&paths.data, std::fs::Permissions::from_mode(0o700))?;
        let hub = Arc::new(Hub::default());
        let stream_hub = hub.clone();
        let streams = Streams::new(Arc::new(move |notification| {
            stream_hub.notify(
                protocol::NSTREAM,
                &notification,
                Some(&notification.channel),
                &[],
                true,
            )
        }));
        let commit_hub = hub.clone();
        let commit_streams = streams.clone();
        let model_epochs = Arc::new(Mutex::new(BTreeMap::new()));
        let epochs = model_epochs.clone();
        let log = eventlog::Log::open(
            paths.data.join("events.db"),
            false,
            Some(Arc::new(move |events| {
                for event in events {
                    commit_streams.flush(&event.channel);
                }
                // No channel/Core locks here: callbacks run before append acknowledgement.
                for event in events {
                    if event.kind == event::MODEL_CALL_STARTED {
                        epochs.lock().unwrap().insert(
                            event.agent.clone(),
                            ModelEpoch {
                                seq: event.global,
                                active: true,
                            },
                        );
                    } else if event.kind == event::MODEL_CALL_COMPLETED
                        && let Some(epoch) = epochs.lock().unwrap().get_mut(&event.agent)
                    {
                        epoch.active = false;
                    }
                }
                commit_hub.committed(events);
            })),
        )?;
        for name in [
            "events.db",
            "events.db-wal",
            "events.db-shm",
            "auth.json",
            "stavlosd.log",
        ] {
            let path = paths.data.join(name);
            if let Ok(metadata) = std::fs::symlink_metadata(&path) {
                ensure!(
                    metadata.is_file() && !metadata.file_type().is_symlink(),
                    "owner state is not a regular file: {}",
                    path.display()
                );
                std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;
            }
        }
        let cfg = config::Loader {
            paths: &paths,
            home: &home,
            lookup: &lookup,
        }
        .global()?;
        let trust = log.trust()?;
        let prompt_hub = hub.clone();
        let prompts = escalation::Manager::new(
            escalation_config(&cfg),
            Arc::new(move |notification, tiers| {
                prompt_hub.notify(protocol::NPROMPT, &notification, None, tiers, false)
            }),
        );
        let level = sandbox::probe().level;
        let daemon = Arc::new_cyclic(|owner: &Weak<Self>| {
            let changed = owner.clone();
            let boards = Arc::new(super::boards::OwnerBoards::new(owner.clone()));
            let environment = Arc::new(Environment {
                models: registry.clone(),
                prompts: prompts.clone(),
                project_changed: Arc::new(move |directory| {
                    if let Some(owner) = changed.upgrade() {
                        owner.project_changed(directory);
                    }
                }),
                paths: paths.clone(),
                home: home.clone(),
                runtime: std::env::var_os("XDG_RUNTIME_DIR").map(PathBuf::from),
                sandbox_level: level,
                boards: Some(boards),
            });
            Self {
                paths,
                home,
                registry,
                web: RwLock::default(),
                discord: RwLock::default(),
                log,
                hub,
                prompts,
                environment,
                cancel: CancellationToken::new(),
                model_epochs,
                channels: RwLock::default(),
                names: Mutex::new(()),
                editor: Mutex::new(()),
                trust: RwLock::new(trust),
                logins: Mutex::default(),
                trust_prompts: Mutex::default(),
                streams,
                owner: owner.clone(),
                plan_history: Mutex::default(),
                lock: Mutex::new(Some(lock)),
                close: tokio::sync::Mutex::new(false),
                tasks: TaskTracker::new(),
                loops_started: AtomicBool::new(false),
            }
        });
        daemon.load_plan_usage();
        if let Err(error) = daemon.recover() {
            daemon.close().await?;
            return Err(error);
        }
        Ok(daemon)
    }
    pub fn global(&self) -> Result<config::Effective> {
        config::Loader {
            paths: &self.paths,
            home: &self.home,
            lookup: &lookup,
        }
        .global()
    }
    pub fn configuration(&self, directory: &Path) -> Result<Arc<config::Effective>> {
        let trusted = |dir: &Path, hash: &str| {
            self.trust
                .read()
                .unwrap()
                .get(&dir.to_string_lossy().into_owned())
                .is_some_and(|value| value == hash)
        };
        Ok(Arc::new(
            config::Loader {
                paths: &self.paths,
                home: &self.home,
                lookup: &lookup,
            }
            .load(directory, Some(&trusted))?,
        ))
    }
    pub(crate) fn new_channel(
        &self,
        id: String,
        directory: String,
        created: Time,
        cfg: Arc<config::Effective>,
        model: String,
        role: String,
    ) -> Arc<Channel> {
        let streams = self.streams.clone();
        let journal = Arc::new(LogJournal {
            log: self.log.clone(),
            stream: Arc::new(move |delta| streams.add(delta)),
        });
        Channel::new(
            Arc::new(Core::new(id, directory, created, cfg, model, role, journal)),
            self.environment.clone(),
        )
    }
    fn recover(&self) -> Result<()> {
        for row in self.log.channels()? {
            if row.archived {
                continue;
            }
            let cfg = self.configuration(Path::new(&row.dir)).or_else(|_| {
                let mut cfg = self.global()?;
                cfg.dir = PathBuf::from(&row.dir);
                Ok::<_, anyhow::Error>(Arc::new(cfg))
            })?;
            let channel = self.new_channel(
                row.id.clone(),
                row.dir,
                Time(row.created.fixed_offset()),
                cfg,
                String::new(),
                String::new(),
            );
            let mut from = 1;
            loop {
                let page = self.log.read(&row.id, from, 2048)?;
                if page.is_empty() {
                    break;
                }
                from = page.last().unwrap().seq + 1;
                channel.core.restore_page(&page)?;
            }
            // Unlike an explicit user request, restore never starts another provider call
            // unless the durable inbox still contains unanswered work.
            if let Err(error) = channel.restore() {
                eprintln!("channel {}: recover: {error:#}", row.id);
                continue;
            }
            self.channels.write().unwrap().insert(row.id, channel);
        }
        Ok(())
    }
    pub fn channel(&self, id: &str) -> Result<Arc<Channel>> {
        self.channels
            .read()
            .unwrap()
            .get(id)
            .cloned()
            .ok_or_else(|| {
                protocol::Error::new(protocol::ERR_NOT_FOUND, format!("channel {id:?} not found"))
                    .into()
            })
    }
    pub fn channels(&self) -> Vec<Arc<Channel>> {
        self.channels.read().unwrap().values().cloned().collect()
    }
    pub fn agent(&self, id: &str) -> Result<(Arc<Channel>, Arc<Actor>)> {
        for channel in self.channels() {
            if let Some(agent) = channel.agent(id) {
                return Ok((channel, agent));
            }
        }
        Err(protocol::Error::new(protocol::ERR_NOT_FOUND, format!("agent {id:?} not found")).into())
    }
    pub(crate) fn choose_name(&self, want: &str, fallback: &str, except: &str) -> Result<String> {
        let taken = self
            .log
            .channels()?
            .into_iter()
            .filter(|row| row.id != except)
            .map(|row| (row.name, ()))
            .collect::<BTreeMap<_, _>>();
        if want.trim().is_empty() {
            return Ok(names::unique(fallback, "channel", |name| {
                taken.contains_key(name)
            }));
        }
        let name = names::normalize(want.trim().trim_start_matches('#'));
        ensure!(
            !name.is_empty(),
            "{want:?} has no letters or digits to name a channel with"
        );
        ensure!(
            !taken.contains_key(&name),
            "#{name} is taken by another channel"
        );
        Ok(name)
    }
    pub fn create(
        &self,
        directory: &str,
        model: &str,
        role: &str,
        want: &str,
        cancel: &CancellationToken,
    ) -> Result<Arc<Channel>> {
        ensure!(!self.cancel.is_cancelled(), "daemon is closing");
        let directory =
            config::effective::working_directory(Path::new(""), directory, &self.home, &lookup)?;
        let cfg = self.configuration(&directory)?;
        let channel = self.new_channel(
            names::new_id("c")?,
            directory.to_string_lossy().into_owned(),
            Time::now(),
            cfg,
            model.into(),
            role.into(),
        );
        {
            let _names = self.names.lock().unwrap();
            let name = self.choose_name(
                want,
                &directory.file_name().unwrap_or_default().to_string_lossy(),
                "",
            )?;
            channel.start(&name, cancel)?;
            self.channels
                .write()
                .unwrap()
                .insert(channel.core.id.clone(), channel.clone());
        }
        self.maybe_trust_prompt(&channel)?;
        Ok(channel)
    }
    pub fn rename(&self, id: &str, want: &str, cancel: &CancellationToken) -> Result<()> {
        let channel = self.channel(id)?;
        let _names = self.names.lock().unwrap();
        ensure!(!want.trim().is_empty(), "a channel name is required");
        channel.rename(&self.choose_name(want, "", id)?, cancel)
    }
    pub async fn archive(&self, id: &str, cancel: &CancellationToken) -> Result<()> {
        self.channel(id)?.archive(cancel).await?;
        for channel in self.channels() {
            if channel
                .core
                .inspect(|state, _| state.board.as_ref().is_some_and(|board| board.source == id))
            {
                channel.archive(cancel).await?;
            }
        }
        self.withdraw_trust(id, "archived");
        Ok(())
    }
    pub async fn set_directory(
        &self,
        id: &str,
        directory: &str,
        cancel: &CancellationToken,
    ) -> Result<protocol::ChannelInfo> {
        let channel = self.channel(id)?;
        let old = channel.info().dir;
        channel
            .set_directory(directory, |directory| self.configuration(directory), cancel)
            .await?;
        if channel.info().dir != old {
            self.withdraw_trust(id, "directory-change");
        }
        self.maybe_trust_prompt(&channel)?;
        Ok(channel.info())
    }
    pub fn channel_list(
        &self,
        directory: &str,
        archived: bool,
    ) -> Result<Vec<protocol::ChannelInfo>> {
        let mut out = Vec::new();
        for row in self.log.channels()? {
            if (!directory.is_empty() && row.dir != directory) || (row.archived && !archived) {
                continue;
            }
            let channel = self.channels.read().unwrap().get(&row.id).cloned();
            let mut info =
                channel
                    .map(|channel| channel.info())
                    .unwrap_or_else(|| protocol::ChannelInfo {
                        id: row.id.clone(),
                        name: row.name,
                        dir: row.dir,
                        created: row
                            .created
                            .to_rfc3339_opts(chrono::SecondsFormat::Secs, true),
                        archived: row.archived,
                        ..Default::default()
                    });
            info.seq = row.last_seq;
            info.title = row.title;
            if let Err(error) = std::fs::metadata(&info.dir).and_then(|metadata| {
                if metadata.is_dir() {
                    Ok(())
                } else {
                    Err(std::io::Error::other("not a directory"))
                }
            }) {
                info.dir_error = format!("default directory unavailable: {error}");
            }
            for prompt in self.prompts.pending(&info.id) {
                if prompt.kind == protocol::PROMPT_QUESTION {
                    info.questions += 1;
                } else {
                    info.permissions += 1;
                }
            }
            out.push(info);
        }
        Ok(out)
    }
    pub fn status(&self) -> protocol::DaemonStatusResult {
        let channels = self.channels();
        let mut agents = 0;
        let mut working = 0;
        for channel in &channels {
            for agent in channel.tree() {
                agents += 1;
                if matches!(agent.state.as_str(), "running" | "waiting") || !agent.jobs.is_empty() {
                    working += 1;
                }
            }
        }
        let mut providers = self.registry.providers();
        providers.sort();
        protocol::DaemonStatusResult {
            version: protocol::VERSION,
            build: crate::buildid::id().into(),
            pid: std::process::id() as i64,
            data_dir: self.paths.data.to_string_lossy().into_owned(),
            channels: channels.len() as i64,
            agents,
            working,
            providers: providers.into(),
        }
    }
    pub fn trust_status(&self, directory: &str) -> Result<protocol::TrustStatusResult> {
        let directory = crate::pathx::absolute(Path::new(directory))?;
        let snapshot = config::snapshot::Snapshot::take(&directory, &self.home)?;
        let dir = directory.to_string_lossy().into_owned();
        Ok(protocol::TrustStatusResult {
            pending: !snapshot.files.is_empty()
                && self.trust.read().unwrap().get(&dir) != Some(&snapshot.hash),
            dir,
            hash: snapshot.hash,
            files: snapshot.files.into(),
        })
    }
    pub fn trust(&self, directory: &str, hash: &str, allow: bool) -> Result<()> {
        let status = self.trust_status(directory)?;
        if allow {
            ensure!(
                !status.hash.is_empty(),
                "{} has no project configuration to trust",
                status.dir
            );
            if hash != status.hash {
                return Err(protocol::Error::new(protocol::ERR_CONFLICT,format!("trust: the project configuration in {} changed since it was shown; look again before trusting it",status.dir)).into());
            }
            self.log.set_trust(status.dir.clone(), hash.into())?;
            self.trust
                .write()
                .unwrap()
                .insert(status.dir.clone(), hash.into());
        }
        let prompt = self
            .trust_prompts
            .lock()
            .unwrap()
            .get(Path::new(&status.dir))
            .map(|p| p.id.clone());
        if let Some(prompt) = prompt {
            let _ = self.prompts.resolve(
                &prompt,
                "trust.reply",
                escalation::Answer {
                    value: if allow { "allow" } else { "deny" }.into(),
                    ..Default::default()
                },
            );
        }
        if allow {
            self.project_changed(Path::new(&status.dir));
        }
        Ok(())
    }
    pub fn maybe_trust_prompt(&self, channel: &Arc<Channel>) -> Result<()> {
        let cfg = channel.core.configuration();
        if !cfg.trust_pending
            || channel
                .core
                .inspect(|state, _| state.board.is_some() || state.archived)
        {
            return Ok(());
        }
        let channel_name = channel.info().name;
        let mut prompts = self.trust_prompts.lock().unwrap();
        if prompts
            .get(&cfg.dir)
            .is_some_and(|prompt| prompt.hash == cfg.trust_hash)
            || self.cancel.is_cancelled()
        {
            return Ok(());
        }
        if let Some(prior) = prompts.remove(&cfg.dir) {
            let _ = self.prompts.resolve(
                &prior.id,
                "trust-changed",
                escalation::Answer {
                    value: "deny".into(),
                    ..Default::default()
                },
            );
            prior.cancel.cancel();
        }
        let id = names::new_id("t")?;
        let cancel = self.cancel.child_token();
        let info = protocol::PromptInfo {
            id: id.clone(),
            channel: channel.core.id.clone(),
            channel_name,
            kind: protocol::PROMPT_TRUST.into(),
            input: Some(serde_json::value::to_raw_value(
                &serde_json::json!({"dir":cfg.dir,"hash":cfg.trust_hash,"files":cfg.trust_files}),
            )?),
            question: format!(
                "Trust the project configuration in {}? It can define MCP servers, policy, roles, skills, AGENTS.md, sandbox settings, fetched hosts and command environment variables.",
                cfg.dir.display()
            ),
            options: vec!["trust".into(), "skip".into()].into(),
            ..Default::default()
        };
        let waiter = self.prompts.begin(info, cancel.clone(), None);
        prompts.insert(
            cfg.dir.clone(),
            TrustPrompt {
                id: id.clone(),
                hash: cfg.trust_hash.clone(),
                channel: channel.core.id.clone(),
                cancel,
            },
        );
        let owner = self.owner.clone();
        let dir = cfg.dir.clone();
        let hash = cfg.trust_hash.clone();
        self.tasks.spawn(async move {
            let answer = waiter.await;
            if let Some(owner) = owner.upgrade() {
                let removed = {
                    let mut prompts = owner.trust_prompts.lock().unwrap();
                    if prompts.get(&dir).is_some_and(|p| p.id == id) {
                        prompts.remove(&dir);
                        true
                    } else {
                        false
                    }
                };
                if removed
                    && !owner.cancel.is_cancelled()
                    && matches!(answer.value.as_str(), "allow" | "allow_always")
                {
                    let _ = owner.trust(&dir.to_string_lossy(), &hash, true);
                }
            }
        });
        Ok(())
    }
    pub fn withdraw_trust(&self, channel: &str, reason: &str) {
        let removed = {
            let mut prompts = self.trust_prompts.lock().unwrap();
            let dirs = prompts
                .iter()
                .filter(|(_, prompt)| channel.is_empty() || prompt.channel == channel)
                .map(|(dir, _)| dir.clone())
                .collect::<Vec<_>>();
            dirs.into_iter()
                .filter_map(|dir| prompts.remove(&dir))
                .collect::<Vec<_>>()
        };
        for prompt in removed {
            let _ = self.prompts.resolve(
                &prompt.id,
                reason,
                escalation::Answer {
                    value: "deny".into(),
                    ..Default::default()
                },
            );
            prompt.cancel.cancel();
        }
    }
    pub fn project_changed(&self, directory: &Path) {
        for channel in self.channels() {
            if Path::new(&channel.info().dir) != directory {
                continue;
            }
            match self.configuration(directory) {
                Ok(config) => {
                    channel.set_configuration(config);
                    let _ = self.maybe_trust_prompt(&channel);
                }
                Err(error) => {
                    eprintln!("project config {}: {error:#}", directory.display());
                    if let Ok(mut config) = self.global() {
                        config.dir = directory.to_owned();
                        channel.set_configuration(Arc::new(config));
                    }
                }
            }
        }
    }
    pub fn changed(&self, what: &str) {
        self.hub.notify(
            protocol::NCHANGED,
            &protocol::ChangedNotification { what: what.into() },
            None,
            &[],
            true,
        );
    }
    pub fn start_loops(self: &Arc<Self>) {
        if self.loops_started.swap(true, Ordering::AcqRel) {
            return;
        }
        self.registry.enable_plan_polling();
        let registry = self.registry.clone();
        let cancel = self.cancel.clone();
        self.tasks.spawn(async move {
            registry.poll_all_plan_usage(Duration::ZERO, &cancel).await;
        });
        let root = self.paths.cache.join("tmp");
        let cancel = self.cancel.clone();
        self.tasks.spawn(async move {
            let mut tick=tokio::time::interval(Duration::from_secs(6*60*60));tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {tokio::select! {biased;_=cancel.cancelled()=>break,_=tick.tick()=>{let root=root.clone();let _=tokio::task::spawn_blocking(move||super::sweep::older(&root,std::time::SystemTime::now()-Duration::from_secs(7*24*60*60))).await;}}}
        });
        let weak = self.owner.clone();
        let cancel = self.cancel.clone();
        self.tasks.spawn(async move {
            let mut tick=tokio::time::interval(Duration::from_secs(15));
            loop {tokio::select! {biased;_=cancel.cancelled()=>break,_=tick.tick()=>{
                let Some(owner)=weak.upgrade() else {break};
                for channel in owner.channels() {let _=channel.maybe_resume(Time::now(),&cancel);let _=routing::Routing {core:channel.core.clone()}.maybe_recap(Time::now(),&cancel);}
            }}}
        });
    }
    pub async fn close(&self) -> Result<()> {
        let mut closed = self.close.lock().await;
        if *closed {
            return Ok(());
        }
        self.cancel.cancel();
        self.hub.close();
        let web = self.web.read().unwrap().clone();
        let discord = self.discord.read().unwrap().clone();
        if let Some(web) = web {
            let _ = web.close().await;
        }
        if let Some(discord) = discord {
            let _ = discord.close().await;
        }
        self.close_logins().await;
        self.withdraw_trust("", "shutdown");
        for channel in self.channels() {
            channel.stop().await;
        }
        self.tasks.close();
        self.tasks.wait().await;
        self.streams.flush_all();
        self.registry.usage.on_usage(None);
        self.log.close()?;
        self.lock.lock().unwrap().take();
        *closed = true;
        Ok(())
    }
}
impl Drop for Daemon {
    fn drop(&mut self) {
        self.cancel.cancel();
        self.hub.close();
    }
}
