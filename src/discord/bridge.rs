//! Socket-only bridge authority, immutable channel routing and serial bounded workers.
#[cfg(test)]
#[path = "bridge_tests.rs"]
mod tests;
use super::{
    api::{Api, Channel, ChannelCreate, Message},
    gateway, planning,
    service::{Progress, Publish},
    store::{BoardCursors, PromptMessage, PromptStore},
};
use crate::{
    client::Client,
    config, event,
    protocol::{
        self,
        methods::{self, Method},
    },
    tools::ServiceFuture,
};
use anyhow::{Result, anyhow, bail, ensure};
use serde::{Serialize, de::DeserializeOwned};
use serde_json::Value;
use std::{
    collections::BTreeMap,
    path::Path,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::sync::{mpsc, oneshot};
use tokio_util::sync::CancellationToken;
pub trait Rpc: Send + Sync {
    fn call(&self, method: &str, params: Value) -> ServiceFuture<'_, Value>;
}
impl Rpc for Client {
    fn call(&self, method: &str, params: Value) -> ServiceFuture<'_, Value> {
        let method = method.to_owned();
        Box::pin(async move { Ok(Client::call(self, &method, &params).await?) })
    }
}
pub struct Link {
    pub(crate) rpc: Arc<dyn Rpc>,
    pub(crate) id: String,
    pub(crate) generation: u64,
    pub cancel: CancellationToken,
}
impl Link {
    pub async fn call<P: Serialize, R: DeserializeOwned>(
        &self,
        method: Method<P, R>,
        params: &P,
    ) -> Result<R> {
        let params = serde_json::to_value(params)?;
        let value = tokio::select! {biased;_=self.cancel.cancelled()=>bail!("connection closed"),result=tokio::time::timeout(Duration::from_secs(10),self.rpc.call(method.name,params))=>result.map_err(|_|anyhow!("daemon request timed out"))??};
        Ok(serde_json::from_value(value)?)
    }
}
pub(crate) enum Payload {
    Snapshot(Box<protocol::ReconcileResult>),
    Event(Box<event::Event>),
    Prompt(Box<protocol::PromptNotification>),
    Message(Box<Message>),
    Interaction(Value),
    Tick,
}
pub(crate) struct Work {
    pub link: Arc<Link>,
    pub payload: Payload,
    pub done: Option<oneshot::Sender<Result<()>>>,
}
struct Route {
    discord: String,
    queue: mpsc::Sender<Work>,
    stop: CancellationToken,
    ready: u64,
}
struct Routing {
    live: Option<Arc<Link>>,
    workers: BTreeMap<String, Route>,
    by_discord: BTreeMap<String, String>,
    generation: u64,
    progress: Progress,
}
pub struct Bridge {
    pub(crate) cfg: config::Discord,
    pub(crate) api: Arc<dyn Api>,
    pub(crate) bot: String,
    pub(crate) store: PromptStore,
    pub(crate) boards: BoardCursors,
    routing: Mutex<Routing>,
    tasks: Mutex<Vec<tokio::task::JoinHandle<()>>>,
    stop: CancellationToken,
    publish: Publish,
}
impl Bridge {
    pub fn new(
        cfg: config::Discord,
        api: Arc<dyn Api>,
        bot: String,
        prompts: std::path::PathBuf,
        boards: std::path::PathBuf,
        publish: Publish,
        parent: CancellationToken,
    ) -> Result<Arc<Self>> {
        Ok(Arc::new(Self {
            cfg,
            api,
            bot,
            store: PromptStore::open(prompts)?,
            boards: BoardCursors::open(boards)?,
            routing: Mutex::new(Routing {
                live: None,
                workers: BTreeMap::new(),
                by_discord: BTreeMap::new(),
                generation: 0,
                progress: Progress::default(),
            }),
            tasks: Mutex::new(vec![]),
            stop: parent.child_token(),
            publish,
        }))
    }
    pub fn identity(&self, bot: String, guild: String) {
        let mut routing = self.routing.lock().unwrap();
        routing.progress.bot = bot;
        routing.progress.guild_name = guild;
        drop(routing);
        self.changed();
    }
    pub fn gateway_state(&self, connected: bool) {
        self.routing.lock().unwrap().progress.gateway = connected;
        self.changed();
    }
    fn changed(&self) {
        let progress = {
            let routing = self.routing.lock().unwrap();
            let mut progress = routing.progress.clone();
            progress.daemon = routing
                .live
                .as_ref()
                .is_some_and(|link| !link.cancel.is_cancelled());
            progress.channels = if progress.daemon {
                routing
                    .workers
                    .values()
                    .filter(|route| {
                        Some(route.ready) == routing.live.as_ref().map(|link| link.generation)
                    })
                    .count() as i64
            } else {
                0
            };
            progress
        };
        (self.publish)(progress);
    }
    pub(crate) fn allowed(&self, directory: &str) -> bool {
        planning::allowed(directory, &self.cfg.dirs)
    }
    pub async fn run(self: &Arc<Self>, socket: &Path) -> Result<()> {
        let mut delay = Duration::from_secs(1);
        while !self.stop.is_cancelled() {
            let started = tokio::time::Instant::now();
            let connection = tokio::select! {biased;_=self.stop.cancelled()=>break,result=Client::connect(socket)=>result};
            let result = match connection {
                Ok(client) => {
                    let client = Arc::new(client);
                    let result = self.connected(client.clone()).await;
                    client.close();
                    result
                }
                Err(error) => Err(error.into()),
            };
            if self.stop.is_cancelled() {
                break;
            }
            self.routing.lock().unwrap().progress.error = result
                .err()
                .map(|e| e.to_string())
                .unwrap_or_else(|| "daemon connection closed".into());
            self.changed();
            if started.elapsed() > Duration::from_secs(30) {
                delay = Duration::from_secs(1);
            }
            tokio::select! {biased;_=self.stop.cancelled()=>break,_=tokio::time::sleep(delay)=>{}}
            delay = (delay * 2).min(Duration::from_secs(30));
        }
        self.close_workers().await;
        Ok(())
    }
    async fn connected(self: &Arc<Self>, client: Arc<Client>) -> Result<()> {
        let attached: protocol::AttachResult = tokio::time::timeout(
            Duration::from_secs(10),
            client.do_method(
                methods::ATTACH,
                &protocol::AttachParams {
                    client: "discord".into(),
                    tier: protocol::TIER_FALLBACK.into(),
                },
            ),
        )
        .await??;
        let link = self.attach(client.clone(), attached.client_id);
        let mut discover: std::pin::Pin<
            Box<dyn std::future::Future<Output = Result<()>> + Send + '_>,
        > = Box::pin(self.discover(link.clone()));
        let result = loop {
            tokio::select! {
                _=self.stop.cancelled()=>break Ok(()),
                _=link.cancel.cancelled()=>break Err(anyhow!("connection closed or worker overloaded")),
                error=client.closed()=>break Err(error.into()),
                response=client.notification()=>match response {Some(response)=>{let _=self.notification(&link,response);},None=>break Err(anyhow!("daemon notification stream closed"))},
                result=&mut discover=>{if let Err(error)=result {break Err(error);}let next=link.clone();discover=Box::pin(async move {tokio::time::sleep(Duration::from_secs(5)).await;self.discover(next).await});}
            }
        };
        link.cancel.cancel();
        client.close();
        drop(discover);
        self.detach(&link);
        let _ = tokio::time::timeout(Duration::from_secs(10), self.disable_prompts()).await;
        result
    }
    /// Also used by local protocol fixtures; authority still resides in the scoped RPC connection.
    pub fn attach(self: &Arc<Self>, rpc: Arc<dyn Rpc>, client: String) -> Arc<Link> {
        let mut routing = self.routing.lock().unwrap();
        if let Some(old) = routing.live.take() {
            old.cancel.cancel();
        }
        routing.generation = routing.generation.wrapping_add(1);
        let link = Arc::new(Link {
            rpc,
            id: client,
            generation: routing.generation,
            cancel: self.stop.child_token(),
        });
        routing.live = Some(link.clone());
        routing.progress.error.clear();
        drop(routing);
        self.changed();
        link
    }
    fn detach(&self, link: &Arc<Link>) {
        let mut routing = self.routing.lock().unwrap();
        if routing
            .live
            .as_ref()
            .is_some_and(|live| Arc::ptr_eq(live, link))
        {
            routing.live = None;
            for route in routing.workers.values_mut() {
                route.ready = 0;
            }
        }
        drop(routing);
        self.changed();
    }
    pub async fn discover(self: &Arc<Self>, link: Arc<Link>) -> Result<()> {
        let list = link
            .call(
                methods::CHANNEL_LIST,
                &protocol::ChannelListParams::default(),
            )
            .await?;
        let channels = self
            .api
            .channels(self.cfg.guild.clone(), link.cancel.clone())
            .await?;
        let category = match planning::category(&channels, &self.cfg.category)? {
            Some(id) => id,
            None => {
                self.api
                    .create(
                        self.cfg.guild.clone(),
                        ChannelCreate {
                            name: self.cfg.category.clone(),
                            kind: 4,
                            ..Default::default()
                        },
                        link.cancel.clone(),
                    )
                    .await?
                    .id
            }
        };
        let mapped = planning::markers(&channels, &category)?;
        let mut keep = BTreeMap::new();
        for info in list
            .channels
            .iter()
            .filter(|info| !info.archived && self.allowed(&info.dir))
        {
            keep.insert(info.id.clone(), ());
            self.bind(link.clone(), info.clone(), &category, mapped.get(&info.id))
                .await?;
        }
        let removed = {
            let mut routing = self.routing.lock().unwrap();
            let ids = routing
                .workers
                .keys()
                .filter(|id| !keep.contains_key(*id))
                .cloned()
                .collect::<Vec<_>>();
            let mut removed = vec![];
            for id in ids {
                if let Some(route) = routing.workers.remove(&id) {
                    routing.by_discord.remove(&route.discord);
                    route.stop.cancel();
                    removed.push(id);
                }
            }
            removed
        };
        for id in removed {
            let _ = link
                .call(
                    methods::UNSUBSCRIBE,
                    &protocol::SubscribeParams {
                        channel: id,
                        ..Default::default()
                    },
                )
                .await;
        }
        self.retire_unmapped(link.cancel.clone()).await?;
        self.changed();
        Ok(())
    }
    async fn bind(
        self: &Arc<Self>,
        link: Arc<Link>,
        info: protocol::ChannelInfo,
        category: &str,
        existing: Option<&Channel>,
    ) -> Result<()> {
        let discord = match existing {
            Some(channel) => {
                if channel.name != info.name {
                    self.api
                        .rename(
                            channel.id.clone(),
                            super::text::clip(&info.name, 100),
                            link.cancel.clone(),
                        )
                        .await?;
                }
                channel.id.clone()
            }
            None => {
                self.api
                    .create(
                        self.cfg.guild.clone(),
                        ChannelCreate {
                            name: super::text::clip(&info.name, 100),
                            kind: 0,
                            parent_id: category.into(),
                            topic: format!("stavlos-channel:{}", info.id),
                        },
                        link.cancel.clone(),
                    )
                    .await?
                    .id
            }
        };
        let ready = {
            let mut routing = self.routing.lock().unwrap();
            if routing
                .workers
                .get(&info.id)
                .is_some_and(|route| route.discord != discord)
            {
                let old = routing.workers.remove(&info.id).unwrap();
                old.stop.cancel();
                routing.by_discord.remove(&old.discord);
            }
            if !routing.workers.contains_key(&info.id) {
                let (queue, receiver) = mpsc::channel(256);
                let stop = self.stop.child_token();
                routing.by_discord.insert(discord.clone(), info.id.clone());
                routing.workers.insert(
                    info.id.clone(),
                    Route {
                        discord: discord.clone(),
                        queue,
                        stop: stop.clone(),
                        ready: 0,
                    },
                );
                let worker = super::worker::Worker::new(
                    self.clone(),
                    info.id.clone(),
                    discord.clone(),
                    stop,
                );
                self.tasks
                    .lock()
                    .unwrap()
                    .push(tokio::spawn(worker.run(receiver)));
            }
            routing.workers[&info.id].ready == link.generation
        };
        if ready {
            return Ok(());
        }
        link.call(
            methods::CHANNEL_RESUME,
            &protocol::ChannelRef {
                channel: info.id.clone(),
            },
        )
        .await?;
        let mut snapshot = link
            .call(
                methods::RECONCILE,
                &protocol::ChannelRef {
                    channel: info.id.clone(),
                },
            )
            .await?;
        if info.board.is_some() {
            snapshot.seq = self.boards.get(&discord);
        }
        let from = self.store.replay_from(&discord, snapshot.seq);
        let (done, received) = oneshot::channel();
        ensure!(
            self.enqueue(
                &info.id,
                Work {
                    link: link.clone(),
                    payload: Payload::Snapshot(Box::new(snapshot)),
                    done: Some(done)
                }
            ),
            "worker overloaded"
        );
        tokio::select! {biased;_=link.cancel.cancelled()=>bail!("connection closed"),result=received=>result.map_err(|_|anyhow!("worker stopped"))??}
        link.call(
            methods::SUBSCRIBE,
            &protocol::SubscribeParams {
                channel: info.id.clone(),
                from,
                ..Default::default()
            },
        )
        .await?;
        if let Some(route) = self.routing.lock().unwrap().workers.get_mut(&info.id) {
            route.ready = link.generation;
        }
        Ok(())
    }
    fn enqueue(&self, id: &str, work: Work) -> bool {
        let sender = self
            .routing
            .lock()
            .unwrap()
            .workers
            .get(id)
            .map(|route| route.queue.clone());
        if let Some(sender) = sender {
            match sender.try_send(work) {
                Ok(()) => true,
                Err(error) => {
                    error.into_inner().link.cancel.cancel();
                    false
                }
            }
        } else {
            false
        }
    }
    pub fn notification(&self, link: &Arc<Link>, response: protocol::Response) -> Result<()> {
        let raw = response
            .params
            .as_ref()
            .map(|raw| raw.get())
            .unwrap_or("null");
        let (id, payload) = match response.method.as_str() {
            protocol::NEVENT => {
                let notification: protocol::EventNotification = serde_json::from_str(raw)?;
                (
                    notification.event.channel.clone(),
                    Payload::Event(Box::new(notification.event)),
                )
            }
            protocol::NPROMPT => {
                let notification: protocol::PromptNotification = serde_json::from_str(raw)?;
                (
                    notification.prompt.channel.clone(),
                    Payload::Prompt(Box::new(notification)),
                )
            }
            _ => return Ok(()),
        };
        self.enqueue(
            &id,
            Work {
                link: link.clone(),
                payload,
                done: None,
            },
        );
        Ok(())
    }
    fn route(&self, guild: &str, discord: &str, user: &str) -> Option<(String, Option<Arc<Link>>)> {
        if guild != self.cfg.guild || !self.cfg.approvers.iter().any(|allowed| allowed == user) {
            return None;
        }
        let routing = self.routing.lock().unwrap();
        let id = routing.by_discord.get(discord)?;
        let route = routing.workers.get(id)?;
        let live = routing
            .live
            .as_ref()
            .filter(|live| !live.cancel.is_cancelled() && route.ready == live.generation)
            .cloned();
        Some((id.clone(), live))
    }
    pub async fn event(&self, event: gateway::Event) {
        if event.kind == "MESSAGE_CREATE" {
            if let Ok(message) = serde_json::from_value(event.data) {
                self.message(message).await;
            }
        } else if event.kind == "INTERACTION_CREATE" {
            self.interaction(event.data).await;
        }
    }
    pub async fn message(&self, message: Message) {
        let Some(author) = &message.author else {
            return;
        };
        if author.bot
            || message
                .webhook_id
                .as_deref()
                .is_some_and(|id| !id.is_empty())
        {
            return;
        }
        let Some((id, live)) = self.route(&message.guild_id, &message.channel_id, &author.id)
        else {
            return;
        };
        let discord = message.channel_id.clone();
        if let Some(link) = live
            && self.enqueue(
                &id,
                Work {
                    link,
                    payload: Payload::Message(Box::new(message)),
                    done: None,
                },
            )
        {
            return;
        }
        let _=tokio::time::timeout(Duration::from_secs(3),self.api.send(discord,String::new(),"stavlos is offline or busy; this task was not queued. Please try again when connected.".into(),vec![],self.stop.child_token())).await;
    }
    pub async fn interaction(&self, interaction: Value) {
        let response = super::interaction::acknowledge(&interaction);
        let route = self.route(
            interaction["guild_id"].as_str().unwrap_or(""),
            interaction["channel_id"].as_str().unwrap_or(""),
            super::interaction::user(&interaction),
        );
        let Some((id, Some(link))) = route else {
            let _ = tokio::time::timeout(
                Duration::from_millis(2500),
                self.api.respond(
                    interaction,
                    super::interaction::ephemeral("Not authorized here, or stavlos is offline."),
                    self.stop.child_token(),
                ),
            )
            .await;
            return;
        };
        if let Some(modal) = super::interaction::modal(&interaction) {
            let _ = tokio::time::timeout(
                Duration::from_millis(2500),
                self.api
                    .respond(interaction, modal, self.stop.child_token()),
            )
            .await;
            return;
        }
        if !matches!(
            tokio::time::timeout(
                Duration::from_millis(2500),
                self.api
                    .respond(interaction.clone(), response, self.stop.child_token())
            )
            .await,
            Ok(Ok(()))
        ) {
            return;
        }
        if !self.enqueue(
            &id,
            Work {
                link,
                payload: Payload::Interaction(interaction.clone()),
                done: None,
            },
        ) {
            let _ = tokio::time::timeout(
                Duration::from_secs(3),
                self.api.response_edit(
                    interaction,
                    "Bridge overloaded; no action was sent. Try again after reconnect.".into(),
                    vec![],
                    self.stop.child_token(),
                ),
            )
            .await;
        }
    }
    pub(crate) async fn edit_prompt(
        &self,
        message: &PromptMessage,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> Result<()> {
        if message.webhook.is_empty() {
            self.api
                .edit(
                    message.channel.clone(),
                    message.message.clone(),
                    body,
                    controls,
                    cancel,
                )
                .await
        } else {
            self.api
                .edit_webhook(
                    message.webhook.clone(),
                    message.message.clone(),
                    body,
                    controls,
                    cancel,
                )
                .await
        }
    }
    async fn disable_prompts(&self) -> Result<()> {
        for message in self.store.snapshot().values() {
            self.edit_prompt(
                message,
                "stavlos disconnected — waiting to reconcile this prompt.".into(),
                vec![],
                CancellationToken::new(),
            )
            .await?;
        }
        Ok(())
    }
    async fn retire_unmapped(&self, cancel: CancellationToken) -> Result<()> {
        for (id, message) in self.store.snapshot() {
            let present = self
                .routing
                .lock()
                .unwrap()
                .by_discord
                .contains_key(&message.channel);
            if !present {
                let result = self
                    .edit_prompt(
                        &message,
                        "This channel is no longer bridged.".into(),
                        vec![],
                        cancel.clone(),
                    )
                    .await;
                if let Err(error) = result
                    && !error.is::<super::api::Missing>()
                {
                    return Err(error);
                }
                self.store.set(&id, PromptMessage::default())?;
            }
        }
        Ok(())
    }
    pub async fn close_workers(&self) {
        self.stop.cancel();
        let tasks = std::mem::take(&mut *self.tasks.lock().unwrap());
        for task in tasks {
            let _ = task.await;
        }
        let mut routing = self.routing.lock().unwrap();
        routing.live = None;
        routing.workers.clear();
        routing.by_discord.clear();
    }
}
