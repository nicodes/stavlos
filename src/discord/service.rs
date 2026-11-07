//! Saved connection intent and one cancellable bridge run at a time.
use crate::{config, daemon::frontdoors, paths::Paths, protocol, tools::ServiceFuture};
use anyhow::{Result, ensure};
use std::{
    path::PathBuf,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::sync::{Notify, watch};
use tokio_util::sync::CancellationToken;
#[derive(Clone, Default)]
pub struct Progress {
    pub bot: String,
    pub guild_name: String,
    pub channels: i64,
    pub gateway: bool,
    pub daemon: bool,
    pub error: String,
}
pub type Publish = Arc<dyn Fn(Progress) + Send + Sync>;
pub type Runner = Arc<
    dyn Fn(config::Discord, Publish, CancellationToken) -> ServiceFuture<'static, ()> + Send + Sync,
>;
pub type Changed = Arc<dyn Fn() + Send + Sync>;
struct State {
    status: protocol::DiscordStatus,
    started: bool,
    closed: bool,
    run: Option<CancellationToken>,
    generation: u64,
    progress: Option<Progress>,
}
struct Shared {
    paths: Paths,
    home: PathBuf,
    state: Mutex<State>,
    operation: Mutex<()>,
    stop: CancellationToken,
    wake: Notify,
    done: watch::Sender<bool>,
    runner: Runner,
    changed: Changed,
    retry: Duration,
}
pub struct Service {
    shared: Arc<Shared>,
    done: watch::Receiver<bool>,
}
fn lookup(name: &str) -> Option<String> {
    std::env::var(name).ok()
}
impl Shared {
    fn configuration(&self) -> Result<Option<config::Discord>> {
        Ok(config::effective::read_file(
            &config::snapshot::Disk,
            &self.paths.config.join("stavlos.toml"),
        )?
        .discord)
    }
    fn error(&self, error: &anyhow::Error) {
        let mut state = self.state.lock().unwrap();
        state.status.state = "error".into();
        state.status.error = error.to_string();
    }
    fn configured(state: &mut State, config: Option<&config::Discord>) {
        state.status.configured = config.is_some();
        if let Some(config) = config {
            state.status.enabled = config.enabled;
            state.status.guild = config.guild.clone();
        }
    }
}
impl Service {
    pub fn new(
        paths: Paths,
        home: PathBuf,
        parent: CancellationToken,
        runner: Runner,
        changed: Changed,
    ) -> Self {
        Self::with_retry(paths, home, parent, runner, changed, Duration::from_secs(5))
    }
    pub fn with_retry(
        paths: Paths,
        home: PathBuf,
        parent: CancellationToken,
        runner: Runner,
        changed: Changed,
        retry: Duration,
    ) -> Self {
        let (done, finished) = watch::channel(false);
        let status = protocol::DiscordStatus {
            state: "disconnected".into(),
            config_path: paths
                .config
                .join("stavlos.toml")
                .to_string_lossy()
                .into_owned(),
            ..Default::default()
        };
        Self {
            shared: Arc::new(Shared {
                paths,
                home,
                state: Mutex::new(State {
                    status,
                    started: false,
                    closed: false,
                    run: None,
                    generation: 0,
                    progress: None,
                }),
                operation: Mutex::new(()),
                stop: parent.child_token(),
                wake: Notify::new(),
                done,
                runner,
                changed,
                retry,
            }),
            done: finished,
        }
    }
    fn start_loop(&self) {
        let mut state = self.shared.state.lock().unwrap();
        if state.started || state.closed {
            return;
        }
        state.started = true;
        drop(state);
        let shared = self.shared.clone();
        tokio::spawn(async move {
            let _finished = Finished(shared.clone());
            run(shared).await;
        });
    }
    pub fn start(&self) {
        let _operation = self.shared.operation.lock().unwrap();
        {
            let state = self.shared.state.lock().unwrap();
            if state.started || state.closed {
                return;
            }
        }
        match self.shared.configuration() {
            Ok(config) => {
                Shared::configured(&mut self.shared.state.lock().unwrap(), config.as_ref())
            }
            Err(error) => self.shared.error(&error),
        }
        self.start_loop();
        (self.shared.changed)();
    }
    pub fn status(&self) -> protocol::DiscordStatus {
        let state = self.shared.state.lock().unwrap();
        let mut status = state.status.clone();
        if let Some(progress) = &state.progress {
            status.bot = progress.bot.clone();
            status.guild_name = progress.guild_name.clone();
            status.channels = progress.channels;
            if status.enabled && status.state != "stopping" {
                status.state = if progress.gateway && progress.daemon {
                    "connected"
                } else {
                    "reconnecting"
                }
                .into();
                if !(progress.error.is_empty() || progress.gateway && progress.daemon) {
                    status.error = progress.error.clone();
                }
            }
        }
        status
    }
    pub fn connect(&self) -> Result<protocol::DiscordStatus> {
        let _operation = self.shared.operation.lock().unwrap();
        ensure!(
            !self.shared.state.lock().unwrap().closed && !self.shared.stop.is_cancelled(),
            "daemon is stopping"
        );
        let config = (|| -> Result<config::Discord> {
            let config=self.shared.configuration()?.ok_or_else(||anyhow::anyhow!("add a discord block to the global stavlos.toml first (see docs/discord-setup.md)"))?;
            config.resolve(&self.shared.home, &lookup)?;
            config::editor::set_discord_enabled(&self.shared.paths.config, true)?;
            Ok(config)
        })();
        let config = match config {
            Ok(config) => config,
            Err(error) => {
                self.shared.error(&error);
                (self.shared.changed)();
                return Err(error);
            }
        };
        let wake = {
            let mut state = self.shared.state.lock().unwrap();
            Shared::configured(&mut state, Some(&config));
            state.status.enabled = true;
            state.status.error.clear();
            let wake = state.run.is_none() || state.status.state == "stopping";
            if state.run.is_none() {
                state.status.state = "connecting".into();
            }
            wake
        };
        if wake {
            self.shared.wake.notify_one();
        }
        self.start_loop();
        (self.shared.changed)();
        Ok(self.status())
    }
    pub fn disconnect(&self) -> Result<protocol::DiscordStatus> {
        let _operation = self.shared.operation.lock().unwrap();
        ensure!(
            !self.shared.state.lock().unwrap().closed && !self.shared.stop.is_cancelled(),
            "daemon is stopping"
        );
        if let Err(error) = config::editor::set_discord_enabled(&self.shared.paths.config, false) {
            self.shared.error(&error);
            (self.shared.changed)();
            return Err(error);
        }
        let running = {
            let mut state = self.shared.state.lock().unwrap();
            state.status.enabled = false;
            state.status.error.clear();
            state.status.state = if state.run.is_some() {
                "stopping"
            } else {
                "disconnected"
            }
            .into();
            state.run.clone()
        };
        if let Some(running) = running {
            running.cancel();
        }
        self.start_loop();
        self.shared.wake.notify_one();
        (self.shared.changed)();
        Ok(self.status())
    }
    pub async fn close(&self) -> Result<()> {
        {
            let _operation = self.shared.operation.lock().unwrap();
            let mut state = self.shared.state.lock().unwrap();
            state.closed = true;
            self.shared.stop.cancel();
            if !state.started {
                self.shared.done.send_replace(true);
            }
        }
        let mut done = self.done.clone();
        tokio::time::timeout(Duration::from_secs(15), async {
            while !*done.borrow_and_update() {
                if done.changed().await.is_err() {
                    break;
                }
            }
        })
        .await
        .map_err(|_| anyhow::anyhow!("Discord service shutdown timed out"))?;
        Ok(())
    }
}
impl frontdoors::Discord for Service {
    fn call<'a>(
        &'a self,
        method: &'a str,
        _cancel: CancellationToken,
    ) -> ServiceFuture<'a, protocol::DiscordStatus> {
        Box::pin(async move {
            match method {
                protocol::MDISCORD_STATUS => Ok(self.status()),
                protocol::MDISCORD_CONNECT => self.connect(),
                protocol::MDISCORD_DISCONNECT => self.disconnect(),
                _ => anyhow::bail!("unsupported Discord operation"),
            }
        })
    }
    fn close(&self) -> ServiceFuture<'_, ()> {
        Box::pin(Service::close(self))
    }
}
impl Drop for Service {
    fn drop(&mut self) {
        self.shared.stop.cancel();
    }
}
#[derive(Debug, thiserror::Error)]
#[error("context canceled")]
struct Cancelled;
struct Finished(Arc<Shared>);
impl Drop for Finished {
    fn drop(&mut self) {
        if let Ok(mut state) = self.0.state.lock() {
            state.run = None;
            state.progress = None;
            state.status.state = "disconnected".into();
        }
        self.0.done.send_replace(true);
    }
}
async fn run(shared: Arc<Shared>) {
    let mut backoff = shared.retry;
    while !shared.stop.is_cancelled() {
        {
            let wake = shared.wake.notified();
            tokio::pin!(wake);
            wake.as_mut().enable();
            if !shared.state.lock().unwrap().status.enabled {
                tokio::select! {biased;_=shared.stop.cancelled()=>break,_=&mut wake=>backoff=shared.retry}
                continue;
            }
            // Enabling above consumes the command which started this attempt.
        }
        let started = tokio::time::Instant::now();
        let config = shared
            .configuration()
            .and_then(|config| {
                config.ok_or_else(|| anyhow::anyhow!("discord configuration is missing"))
            })
            .and_then(|config| config.resolve(&shared.home, &lookup));
        let result = match config {
            Err(error) => Err(error),
            Ok(config) => {
                let cancel = shared.stop.child_token();
                let generation = {
                    let mut state = shared.state.lock().unwrap();
                    if !state.status.enabled {
                        continue;
                    }
                    state.generation = state.generation.wrapping_add(1);
                    state.run = Some(cancel.clone());
                    state.progress = None;
                    state.status.state = "connecting".into();
                    state.status.guild = config.guild.clone();
                    state.status.error.clear();
                    state.generation
                };
                (shared.changed)();
                let publishing = Arc::downgrade(&shared);
                let stopping = cancel.clone();
                let publish: Publish = Arc::new(move |progress| {
                    if stopping.is_cancelled() {
                        return;
                    }
                    let Some(shared) = publishing.upgrade() else {
                        return;
                    };
                    let accepted = {
                        let mut state = shared.state.lock().unwrap();
                        if state.generation != generation
                            || state.closed
                            || state
                                .run
                                .as_ref()
                                .is_none_or(CancellationToken::is_cancelled)
                        {
                            false
                        } else {
                            state.progress = Some(progress);
                            true
                        }
                    };
                    if accepted {
                        (shared.changed)();
                    }
                });
                // The one runner owns its network work. Never start a replacement concurrently.
                let result = (shared.runner)(config, publish, cancel.clone()).await;
                let result = if cancel.is_cancelled() {
                    Err(Cancelled.into())
                } else {
                    result
                };
                cancel.cancel();
                let mut state = shared.state.lock().unwrap();
                state.run = None;
                state.progress = None;
                result
            }
        };
        if shared.stop.is_cancelled() {
            break;
        }
        let enabled = {
            let mut state = shared.state.lock().unwrap();
            if state.status.enabled {
                if let Err(error) = result {
                    if error.is::<Cancelled>() {
                        state.status.state = "connecting".into();
                        state.status.error.clear();
                    } else {
                        state.status.state = "error".into();
                        state.status.error = error.to_string();
                    }
                }
            } else {
                state.status.state = "disconnected".into();
                state.status.error.clear();
            }
            state.status.enabled
        };
        (shared.changed)();
        if !enabled {
            continue;
        }
        if started.elapsed() > Duration::from_secs(30) {
            backoff = shared.retry;
        }
        tokio::select! {biased;_=shared.stop.cancelled()=>break,_=shared.wake.notified()=>backoff=shared.retry,_=tokio::time::sleep(backoff)=>backoff=(backoff*2).min(shared.retry.max(Duration::from_secs(300)))}
    }
}
