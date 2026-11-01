//! Concurrent channel actors. The Core lock orders journal decisions and runtime transitions.
mod execution;
mod settings;
mod turn;
use super::{
    context::Conversation, control, journal::Core, mcp, names, permission, pick, prompt, routing,
    scope, security, services::JobRegistry,
};
use crate::{config, escalation, event, paths::Paths, protocol, sandbox, tools, wire::Time};
use anyhow::{Result, ensure};
use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::{Arc, Mutex, Weak},
    time::Duration,
};
use tokio::sync::Notify;
use tokio_util::{sync::CancellationToken, task::TaskTracker};
pub const NO_MODEL: &str =
    "no model selected: run /models to pick one, or /providers first to connect a provider";
pub trait BoardHost: Send + Sync {
    fn tools(&self, source: &str) -> Arc<dyn tools::Boards>;
    fn report<'a>(
        &'a self,
        source: &'a str,
        caller: &'a str,
        board: &'a str,
        targets: Vec<String>,
        text: String,
        reply: Vec<String>,
    ) -> tools::ServiceFuture<'a, ()>;
}
pub struct Environment {
    pub models: Arc<dyn pick::Models>,
    pub prompts: Arc<escalation::Manager>,
    pub project_changed: Arc<dyn Fn(&Path) + Send + Sync>,
    pub paths: Paths,
    pub home: PathBuf,
    pub runtime: Option<PathBuf>,
    pub sandbox_level: sandbox::Level,
    pub boards: Option<Arc<dyn BoardHost>>,
}
pub struct Channel {
    pub core: Arc<Core>,
    pub environment: Arc<Environment>,
    lifetime: CancellationToken,
    agents: Mutex<BTreeMap<String, Running>>,
    tasks: TaskTracker,
    stamp: Mutex<String>,
}
struct Running {
    agent: Arc<Actor>,
    turn: Option<CancellationToken>,
    maintenance: usize,
}
pub struct Actor {
    pub id: String,
    pub parent: String,
    pub depth: i64,
    channel: Weak<Channel>,
    core: Arc<Core>,
    lifetime: CancellationToken,
    wake: Arc<Notify>,
    pub conversation: Conversation,
    pub jobs: Arc<JobRegistry>,
    mcp: Arc<mcp::Manager>,
    prefix: Mutex<prompt::PrefixCache>,
    repeat: Mutex<security::RepeatCall>,
}
pub struct Spawn {
    pub parent: String,
    pub role: String,
    pub label: String,
    pub task: String,
    pub model: String,
    pub directories: Vec<String>,
}
impl Channel {
    pub fn new(core: Arc<Core>, environment: Arc<Environment>) -> Arc<Self> {
        let stamp = control::trust_stamp(&core.configuration());
        Arc::new(Self {
            core,
            environment,
            lifetime: CancellationToken::new(),
            agents: Mutex::new(BTreeMap::new()),
            tasks: TaskTracker::new(),
            stamp: Mutex::new(stamp),
        })
    }
    pub fn agent(&self, id: &str) -> Option<Arc<Actor>> {
        self.agents
            .lock()
            .unwrap()
            .get(id)
            .map(|running| running.agent.clone())
    }
    pub fn root(&self) -> Option<Arc<Actor>> {
        let id = self.core.inspect(|state, _| state.order.first().cloned())?;
        self.agent(&id)
    }
    pub fn start(self: &Arc<Self>, name: &str, cancel: &CancellationToken) -> Result<()> {
        let role = {
            let mut state = self.core.state.lock().unwrap();
            let role = state.channel.role.clone();
            ensure!(
                state.config.roles.contains_key(&role),
                "root role {} not found",
                serde_json::to_string(&role)?
            );
            let event = self.core.event(
                "",
                event::CHANNEL_CREATED,
                event::ChannelCreatedPayload {
                    name: name.into(),
                    dir: state.channel.dir.clone(),
                    model: state.channel.model.clone(),
                    role: role.clone(),
                    mode: state.config.mode.clone(),
                    ..Default::default()
                },
            )?;
            self.core.commit_locked(&mut state, vec![event], cancel)?;
            role
        };
        self.spawn(
            Spawn {
                parent: String::new(),
                role,
                label: "main".into(),
                task: String::new(),
                model: String::new(),
                directories: Vec::new(),
            },
            cancel,
        )?;
        Ok(())
    }
    pub fn spawn(
        self: &Arc<Self>,
        request: Spawn,
        cancel: &CancellationToken,
    ) -> Result<Arc<Actor>> {
        let handle = tokio::runtime::Handle::try_current()?;
        let current = self.core.inspect(|state, _| state.model.clone());
        let config = self.core.configuration();
        let market = pick::Market::read(
            self.environment.models.as_ref(),
            &config,
            &[request.model.clone(), current],
        );
        let mut state = self.core.state.lock().unwrap();
        ensure!(!state.reconfiguring, "a directory change is in progress");
        ensure!(
            !state.stopped && !state.channel.archived && !self.lifetime.is_cancelled(),
            "channel is stopped"
        );
        let definition = state
            .config
            .roles
            .get(&request.role)
            .cloned()
            .ok_or_else(|| {
                anyhow::anyhow!(
                    "unknown archetype {}",
                    serde_json::to_string(&request.role).unwrap()
                )
            })?;
        let parent = if request.parent.is_empty() {
            None
        } else {
            ensure!(
                !names::reserved(&names::normalize(&request.label)),
                "label {} is reserved: a child's name appears on its messages, so it may not read as the human or the system",
                serde_json::to_string(&request.label)?
            );
            let parent = state
                .channel
                .agents
                .get(&request.parent)
                .filter(|parent| !parent.killed)
                .ok_or_else(|| {
                    anyhow::anyhow!(
                        "parent {} not found",
                        serde_json::to_string(&request.parent).unwrap()
                    )
                })?;
            let view = prompt::role_view(&state.channel, &request.parent, &state.config).unwrap();
            ensure!(
                view.definition.spawn.contains(&request.role),
                "{} may not spawn {} (allowed: {:?})",
                view.definition.name,
                serde_json::to_string(&request.role)?,
                view.definition.spawn
            );
            ensure!(
                !view.definition.read_only || definition.read_only,
                "a read-only agent cannot delegate to a writable role"
            );
            ensure!(
                definition.can_be_subagent(),
                "role {} is primary-only: it cannot be spawned",
                serde_json::to_string(&request.role)?
            );
            let (allowed, reason) = prompt::can_spawn(&state.channel, parent, &state.config);
            ensure!(allowed, "{reason}");
            Some(parent)
        };
        if parent.is_none() {
            ensure!(
                definition.can_be_primary(),
                "role {} is subagent-only: it cannot be the main agent",
                serde_json::to_string(&request.role)?
            );
        }
        let directories = scope::child_scope(
            &state.channel,
            &state.config,
            &request.parent,
            &request.directories,
        )?;
        let model = market.resolve_model(
            &definition,
            &state.config,
            &request.model,
            &state.channel.model,
            state.model_chosen,
            parent,
        )?;
        let mut variant = String::new();
        if let Some(parent) = parent {
            ensure!(!model.is_empty(), NO_MODEL);
            ensure!(
                market.usable.get(&model) == Some(&true),
                "model {model} cannot be used now: run /providers to sign in to its provider, or /models to pick another"
            );
            if model == parent.model {
                variant = parent.variant.clone();
            }
        }
        variant = pick::fit_variant(
            &definition,
            market.variants.get(&model).map_or(&[], Vec::as_slice),
            &model,
            &variant,
        );
        let parent_message = parent.map(|parent| (parent.id.clone(), parent.name.clone()));
        let depth = parent.map_or(0, |parent| parent.depth + 1);
        let id = names::new_id("a")?;
        let name = names::claim(&request.label, &request.role, &id, &state.channel.names)?;
        let mut running = self.agents.lock().unwrap();
        let lifetime = running.get(&request.parent).map_or_else(
            || self.lifetime.child_token(),
            |parent| parent.agent.lifetime.child_token(),
        );
        let actor = Actor::new(self, id.clone(), request.parent.clone(), depth, lifetime);
        running.insert(
            id.clone(),
            Running {
                agent: actor.clone(),
                turn: None,
                maintenance: 0,
            },
        );
        state.wakes.insert(id.clone(), actor.wake.clone());
        let mut events = vec![self.core.event(
            &id,
            event::AGENT_SPAWNED,
            event::AgentSpawnedPayload {
                id: id.clone(),
                parent: request.parent,
                role: request.role,
                name,
                model,
                variant,
                depth,
                directories,
            },
        )?];
        if !request.task.is_empty() {
            let mut input = event::Input {
                id: names::new_id("i")?,
                kind: event::INPUT_PROMPT.into(),
                text: request.task,
                ..Default::default()
            };
            input.request_id = input.id.clone();
            if let Some((id, name)) = parent_message {
                input.kind = event::INPUT_REQUEST.into();
                input.from = id;
                input.from_name = name;
            }
            events.push(self.core.event(&id, event::INPUT_QUEUED, input)?);
        }
        if let Err(error) = self.core.commit_locked(&mut state, events, cancel) {
            running.remove(&id);
            state.wakes.remove(&id);
            actor.lifetime.cancel();
            return Err(error);
        }
        self.tasks.spawn_on(actor.clone().run(), &handle);
        Ok(actor)
    }
    pub fn restore(self: &Arc<Self>) -> Result<()> {
        let handle = tokio::runtime::Handle::try_current()?;
        let mut state = self.core.state.lock().unwrap();
        let mut running = self.agents.lock().unwrap();
        ensure!(running.is_empty(), "channel runtime already started");
        let archived = state.channel.archived;
        for id in routing::preorder(&state.channel) {
            let saved = &state.channel.agents[&id];
            let lifetime = running.get(&saved.parent).map_or_else(
                || self.lifetime.child_token(),
                |parent| parent.agent.lifetime.child_token(),
            );
            let actor = Actor::new(
                self,
                id.clone(),
                saved.parent.clone(),
                saved.depth,
                lifetime,
            );
            if archived || saved.killed {
                actor.lifetime.cancel();
            }
            state.wakes.insert(id.clone(), actor.wake.clone());
            running.insert(
                id,
                Running {
                    agent: actor,
                    turn: None,
                    maintenance: 0,
                },
            );
        }
        let repairs = super::conversation::recovery_events(&self.core, &state.channel)?;
        self.core
            .commit_locked(&mut state, repairs, &CancellationToken::new())?;
        for (id, entry) in running.iter() {
            if !archived && !state.channel.agents[id].killed {
                self.tasks.spawn_on(entry.agent.clone().run(), &handle);
                if state.channel.agents[id].starts_turn() {
                    entry.agent.wake.notify_one();
                }
            }
        }
        Ok(())
    }
    pub async fn stop(&self) {
        let agents = {
            let mut state = self.core.state.lock().unwrap();
            state.stopped = true;
            self.lifetime.cancel();
            self.tasks.close();
            self.agents
                .lock()
                .unwrap()
                .values()
                .map(|entry| entry.agent.clone())
                .collect::<Vec<_>>()
        };
        let cleanup = async {
            let mut stops = tokio::task::JoinSet::new();
            for actor in agents {
                stops.spawn(async move {
                    actor.mcp.stop(None, false).await;
                    actor.jobs.shutdown().await;
                });
            }
            while stops.join_next().await.is_some() {}
            self.tasks.wait().await;
        };
        if tokio::time::timeout(Duration::from_secs(10), cleanup)
            .await
            .is_err()
        {
            eprintln!(
                "channel {}: runtime shutdown exceeded 10 seconds",
                self.core.id
            );
        }
    }
    pub fn cancel(&self, id: &str) -> Result<()> {
        let mut state = self.core.state.lock().unwrap();
        let agents = self.agents.lock().unwrap();
        let running = agents.get(id).ok_or_else(|| {
            anyhow::anyhow!("agent {} not found", serde_json::to_string(id).unwrap())
        })?;
        if let Some(turn) = &running.turn {
            turn.cancel();
        }
        let mut events = vec![self.core.bare_event(id, event::AGENT_CANCELLED)];
        let (stopped, tokens) = running.agent.jobs.drain_for_cancel("cancelled by owner")?;
        events.extend(stopped);
        let result = self
            .core
            .fact_locked(&mut state, events, &CancellationToken::new());
        drop(agents);
        drop(state);
        for token in tokens {
            token.cancel();
        }
        result
    }
    pub async fn kill(&self, id: &str) -> Result<()> {
        let (actors, result) = {
            let mut state = self.core.state.lock().unwrap();
            ensure!(
                state.channel.agents.contains_key(id),
                "agent {} not found",
                serde_json::to_string(id)?
            );
            let running = self.agents.lock().unwrap();
            let mut actors = Vec::new();
            let mut events = Vec::new();
            for target in routing::preorder(&state.channel)
                .into_iter()
                .rev()
                .filter(|target| routing::descendant(&state.channel, target, id))
            {
                if !state.channel.agents[&target].killed {
                    if let Some(entry) = running.get(&target) {
                        entry.agent.lifetime.cancel();
                        actors.push(entry.agent.clone());
                    }
                    events.push(self.core.bare_event(&target, event::AGENT_KILLED));
                }
            }
            let result = self
                .core
                .fact_locked(&mut state, events, &CancellationToken::new());
            (actors, result)
        };
        for actor in actors {
            actor.mcp.stop(None, false).await;
            actor.jobs.shutdown().await;
        }
        result
    }
    pub fn check_project(&self) {
        let config = self.core.configuration();
        if !config.trust_files.is_empty()
            && control::trust_stamp(&config) != *self.stamp.lock().unwrap()
        {
            let directory = self.core.inspect(|state, _| PathBuf::from(&state.dir));
            (self.environment.project_changed)(&directory);
        }
    }
}
impl Drop for Channel {
    fn drop(&mut self) {
        self.lifetime.cancel();
    }
}
impl Actor {
    fn new(
        channel: &Arc<Channel>,
        id: String,
        parent: String,
        depth: i64,
        lifetime: CancellationToken,
    ) -> Arc<Self> {
        let weak = Arc::downgrade(&channel.core);
        let record_id = id.clone();
        let mcp = mcp::Manager::new(Arc::new(move |kind, payload| {
            let core = weak
                .upgrade()
                .ok_or_else(|| anyhow::anyhow!("channel is stopped"))?;
            core.record(&record_id, vec![core.event(&record_id, kind, payload)?])
        }));
        Arc::new(Self {
            core: channel.core.clone(),
            conversation: Conversation::new(channel.core.clone(), id.clone()),
            jobs: JobRegistry::new(
                channel.core.clone(),
                id.clone(),
                lifetime.clone(),
                scope::scratch_directory(&channel.environment.paths, &channel.core.id)
                    .join("output"),
            ),
            id,
            parent,
            depth,
            channel: Arc::downgrade(channel),
            lifetime,
            wake: Arc::new(Notify::new()),
            mcp,
            prefix: Mutex::new(prompt::PrefixCache::default()),
            repeat: Mutex::new(security::RepeatCall::default()),
        })
    }
    fn channel(&self) -> Result<Arc<Channel>> {
        self.channel
            .upgrade()
            .ok_or_else(|| anyhow::anyhow!("channel is stopped"))
    }
    fn role(&self) -> Result<prompt::RoleView> {
        self.core
            .inspect(|state, config| prompt::role_view(state, &self.id, config))
            .ok_or_else(|| anyhow::anyhow!("agent not found"))
    }
    fn boundary(&self, channel: &Channel) -> permission::BoundaryEnvironment {
        permission::BoundaryEnvironment {
            paths: channel.environment.paths.clone(),
            home: channel.environment.home.clone(),
            runtime: channel.environment.runtime.clone(),
            uid: unsafe { libc::getuid() },
            level: channel.environment.sandbox_level,
        }
    }
}
