//! The journal writer only encodes and enqueues; replay waits outside its barrier.
use super::queue::Queue;
use crate::{event::Event, eventlog::Log, protocol};
use anyhow::Result;
use serde::Serialize;
use serde_json::value::RawValue;
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex, RwLock},
};
use tokio_util::sync::CancellationToken;

pub fn notification(method: &str, params: &impl Serialize) -> Result<Arc<[u8]>> {
    frame(&protocol::Response {
        jsonrpc: "2.0".into(),
        method: method.into(),
        params: Some(serde_json::value::to_raw_value(params)?),
        ..Default::default()
    })
}
pub fn frame(value: &impl Serialize) -> Result<Arc<[u8]>> {
    let mut bytes = serde_json::to_vec(value)?;
    bytes.push(b'\n');
    Ok(bytes.into())
}
fn event_line(event: &Event) -> Result<Arc<[u8]>> {
    notification(
        protocol::NEVENT,
        &protocol::EventNotification {
            event: event.clone(),
        },
    )
}
struct State {
    name: String,
    tier: String,
    subscriptions: BTreeMap<String, i64>,
}
pub struct Client {
    pub id: String,
    pub queue: Queue,
    state: Mutex<State>,
    // Concurrent resubscriptions cannot interleave two histories with live delivery.
    replay: tokio::sync::Mutex<()>,
}
impl Client {
    pub fn new() -> Result<Arc<Self>> {
        Ok(Arc::new(Self {
            id: crate::agent::names::new_id("c")?,
            queue: Queue::default(),
            state: Mutex::new(State {
                name: "anonymous".into(),
                tier: protocol::TIER_INTERACTIVE.into(),
                subscriptions: BTreeMap::new(),
            }),
            replay: tokio::sync::Mutex::new(()),
        }))
    }
    pub fn identity(&self) -> (String, String) {
        let state = self.state.lock().unwrap();
        (state.name.clone(), state.tier.clone())
    }
    pub fn attach(&self, name: &str, tier: &str, web: bool) {
        let mut state = self.state.lock().unwrap();
        if tier == protocol::TIER_FALLBACK {
            state.tier = tier.into();
        }
        if !name.is_empty() {
            state.name = name.into();
        }
        if web {
            state.name = "web".into();
        }
    }
    pub fn unsubscribe(&self, channel: &str) {
        self.state.lock().unwrap().subscriptions.remove(channel);
    }
    pub fn subscribed(&self, channel: &str) -> bool {
        self.state
            .lock()
            .unwrap()
            .subscriptions
            .contains_key(channel)
    }
    fn deliver(&self, event: &Event, encoded: &mut Option<Arc<[u8]>>) {
        let mut state = self.state.lock().unwrap();
        let Some(last) = state.subscriptions.get_mut(&event.channel) else {
            return;
        };
        if event.seq <= *last {
            return;
        }
        *last = event.seq;
        if encoded.is_none() {
            *encoded = event_line(event).ok();
        }
        if let Some(line) = encoded {
            self.queue.enqueue(line.clone(), false);
        }
    }
    pub fn reply(
        &self,
        id: Option<Box<RawValue>>,
        result: std::result::Result<serde_json::Value, protocol::Error>,
    ) {
        let mut response = protocol::Response {
            jsonrpc: "2.0".into(),
            id,
            ..Default::default()
        };
        match result {
            Ok(value) => match serde_json::value::to_raw_value(&value) {
                Ok(value) => response.result = Some(value),
                Err(e) => {
                    response.error =
                        Some(protocol::Error::new(protocol::ERR_INTERNAL, e.to_string()))
                }
            },
            Err(error) => response.error = Some(error),
        }
        if let Ok(line) = frame(&response) {
            self.queue.enqueue(line, false);
        }
    }
    pub async fn subscribe(
        self: &Arc<Self>,
        log: &Arc<Log>,
        channel: &str,
        from: i64,
        cancel: &CancellationToken,
    ) -> Result<i64> {
        let _replay = tokio::select! {guard=self.replay.lock()=>guard,_=cancel.cancelled()=>anyhow::bail!("context canceled")};
        self.unsubscribe(channel);
        let mut last = from.max(1) - 1;
        loop {
            anyhow::ensure!(!cancel.is_cancelled(), "context canceled");
            let mut events = log.read(channel, last + 1, 512)?;
            if events.is_empty() {
                let returned = Arc::new(Mutex::new(None));
                let output = returned.clone();
                let reader = log.clone();
                let client = self.clone();
                let channel = channel.to_owned();
                log.barrier(move || {
                    let result = reader.read(&channel, last + 1, 512);
                    if result.as_ref().is_ok_and(Vec::is_empty) {
                        client
                            .state
                            .lock()
                            .unwrap()
                            .subscriptions
                            .insert(channel, last);
                    }
                    *output.lock().unwrap() = Some(result);
                })?;
                events = returned
                    .lock()
                    .unwrap()
                    .take()
                    .expect("barrier acknowledged")?;
                if events.is_empty() {
                    return Ok(last);
                }
            }
            for event in events {
                self.queue.replay(event_line(&event)?, cancel).await?;
                last = event.seq;
            }
        }
    }
}
#[derive(Default)]
pub struct Hub {
    clients: RwLock<BTreeMap<String, Arc<Client>>>,
    feeds: RwLock<BTreeMap<String, std::sync::Weak<dyn Feed>>>,
}
pub trait Feed: Send + Sync {
    fn notify(&self, line: &[u8]);
}
impl Hub {
    pub fn feed(&self, id: String, feed: std::sync::Weak<dyn Feed>) {
        self.feeds.write().unwrap().insert(id, feed);
    }
    pub fn remove_feed(&self, id: &str) {
        self.feeds.write().unwrap().remove(id);
    }
    pub fn add(&self, client: Arc<Client>) {
        self.clients
            .write()
            .unwrap()
            .insert(client.id.clone(), client);
    }
    pub fn remove(&self, id: &str) {
        self.clients.write().unwrap().remove(id);
    }
    pub fn clients(&self) -> Vec<Arc<Client>> {
        self.clients.read().unwrap().values().cloned().collect()
    }
    pub fn committed(&self, events: &[Event]) {
        let clients = self.clients();
        for event in events {
            let mut line = None;
            for client in &clients {
                client.deliver(event, &mut line);
            }
        }
    }
    pub fn notify(
        &self,
        method: &str,
        params: &impl Serialize,
        channel: Option<&str>,
        tiers: &[String],
        droppable: bool,
    ) {
        let Ok(line) = notification(method, params) else {
            return;
        };
        let feeds = self
            .feeds
            .read()
            .unwrap()
            .values()
            .filter_map(std::sync::Weak::upgrade)
            .collect::<Vec<_>>();
        for feed in feeds {
            feed.notify(&line);
        }
        for client in self.clients() {
            if channel.is_some_and(|c| !client.subscribed(c)) {
                continue;
            }
            if !tiers.is_empty() && !tiers.contains(&client.identity().1) {
                continue;
            }
            client.queue.enqueue(line.clone(), droppable);
        }
    }
    pub fn close(&self) {
        for client in self.clients() {
            client.queue.close();
        }
    }
}
