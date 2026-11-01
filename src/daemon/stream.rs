use crate::protocol::StreamNotification;
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::Duration,
};
pub const WINDOW: Duration = Duration::from_millis(30);
pub const MAX_WINDOW_BYTES: usize = 1 << 20;
pub type Sink = Arc<dyn Fn(StreamNotification) + Send + Sync>;
struct State {
    pending: BTreeMap<(String, String), StreamNotification>,
    generation: u64,
    armed: bool,
}
pub struct Streams {
    state: Mutex<State>,
    sink: Sink,
    window: Duration,
    runtime: tokio::runtime::Handle,
}
pub fn joins(prior: &StreamNotification, next: &StreamNotification) -> bool {
    prior.turn == next.turn
        && prior.tool_name == next.tool_name
        && !prior.reset
        && !next.reset
        && prior.text.is_empty() == next.text.is_empty()
        && prior.thinking.is_empty() == next.thinking.is_empty()
        && (!next.text.is_empty() || !next.thinking.is_empty())
}
impl Streams {
    pub fn new(sink: Sink) -> Arc<Self> {
        Self::with_window(sink, WINDOW)
    }
    pub fn with_window(sink: Sink, window: Duration) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::new(State {
                pending: BTreeMap::new(),
                generation: 0,
                armed: false,
            }),
            sink,
            window,
            runtime: tokio::runtime::Handle::current(),
        })
    }
    pub fn add(self: &Arc<Self>, notification: StreamNotification) {
        let mut state = self.state.lock().unwrap();
        let key = (notification.channel.clone(), notification.agent.clone());
        if let Some(prior) = state.pending.get_mut(&key)
            && joins(prior, &notification)
            && prior
                .text
                .len()
                .saturating_add(prior.thinking.len())
                .saturating_add(notification.text.len())
                .saturating_add(notification.thinking.len())
                <= MAX_WINDOW_BYTES
        {
            prior.text.push_str(&notification.text);
            prior.thinking.push_str(&notification.thinking);
            return;
        }
        if let Some(prior) = state.pending.insert(key, notification) {
            (self.sink)(prior);
        }
        if !state.armed {
            state.armed = true;
            state.generation = state.generation.wrapping_add(1);
            let generation = state.generation;
            let weak = Arc::downgrade(self);
            let window = self.window;
            self.runtime.spawn(async move {
                tokio::time::sleep(window).await;
                if let Some(streams) = weak.upgrade() {
                    streams.flush_generation(generation);
                }
            });
        }
    }
    fn flush_generation(&self, generation: u64) {
        let mut state = self.state.lock().unwrap();
        if !state.armed || state.generation != generation {
            return;
        }
        let pending = std::mem::take(&mut state.pending);
        state.armed = false;
        for (_, notification) in pending {
            (self.sink)(notification);
        }
    }
    pub fn flush(&self, channel: &str) {
        let mut state = self.state.lock().unwrap();
        let keys = state
            .pending
            .keys()
            .filter(|(name, _)| name == channel)
            .cloned()
            .collect::<Vec<_>>();
        for key in keys {
            if let Some(notification) = state.pending.remove(&key) {
                (self.sink)(notification);
            }
        }
        if state.pending.is_empty() {
            state.armed = false;
            state.generation = state.generation.wrapping_add(1);
        }
    }
    pub fn flush_all(&self) {
        let mut state = self.state.lock().unwrap();
        let pending = std::mem::take(&mut state.pending);
        state.armed = false;
        state.generation = state.generation.wrapping_add(1);
        for (_, notification) in pending {
            (self.sink)(notification);
        }
    }
}
