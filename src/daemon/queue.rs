use anyhow::{Result, ensure};
use std::{
    collections::VecDeque,
    sync::{Arc, Mutex},
};
use tokio::sync::Notify;
use tokio_util::sync::CancellationToken;
pub const MAX_MESSAGES: usize = 1 << 14;
pub const MAX_BYTES: usize = 64 << 20;
struct State {
    messages: VecDeque<Arc<[u8]>>,
    bytes: usize,
    closed: bool,
}
pub struct Queue {
    state: Mutex<State>,
    messages: usize,
    bytes: usize,
    ready: Notify,
    room: Notify,
    closed: CancellationToken,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Enqueued {
    Queued,
    Dropped,
    Closed,
}
impl Default for Queue {
    fn default() -> Self {
        Self::new(MAX_MESSAGES, MAX_BYTES)
    }
}
impl Queue {
    pub fn new(messages: usize, bytes: usize) -> Self {
        Self {
            state: Mutex::new(State {
                messages: VecDeque::new(),
                bytes: 0,
                closed: false,
            }),
            messages: messages.max(1),
            bytes: bytes.max(1),
            ready: Notify::new(),
            room: Notify::new(),
            closed: CancellationToken::new(),
        }
    }
    pub fn cancellation(&self) -> CancellationToken {
        self.closed.clone()
    }
    pub fn usage(&self) -> (usize, usize) {
        let state = self.state.lock().unwrap();
        (state.messages.len(), state.bytes)
    }
    pub fn close(&self) {
        let mut state = self.state.lock().unwrap();
        state.closed = true;
        state.messages.clear();
        state.bytes = 0;
        self.closed.cancel();
        self.ready.notify_waiters();
        self.room.notify_waiters();
    }
    pub fn enqueue(&self, line: Arc<[u8]>, droppable: bool) -> Enqueued {
        let mut state = self.state.lock().unwrap();
        if state.closed {
            return Enqueued::Closed;
        }
        if droppable && (state.messages.len() > self.messages / 2 || state.bytes > self.bytes / 2) {
            return Enqueued::Dropped;
        }
        if state.messages.len() >= self.messages
            || line.len() > self.bytes.saturating_sub(state.bytes)
        {
            state.closed = true;
            state.messages.clear();
            state.bytes = 0;
            self.closed.cancel();
            self.ready.notify_waiters();
            self.room.notify_waiters();
            return Enqueued::Closed;
        }
        state.bytes += line.len();
        state.messages.push_back(line);
        drop(state);
        self.ready.notify_one();
        Enqueued::Queued
    }
    /// History may wait for room; the log barrier is never held by this future.
    pub async fn replay(&self, line: Arc<[u8]>, cancel: &CancellationToken) -> Result<()> {
        ensure!(
            line.len() <= self.bytes,
            "outbound frame exceeds queue bound"
        );
        loop {
            let room = self.room.notified();
            tokio::pin!(room);
            room.as_mut().enable();
            {
                let mut state = self.state.lock().unwrap();
                ensure!(!state.closed, "connection closed");
                ensure!(!cancel.is_cancelled(), "context canceled");
                if state.messages.len() < (self.messages / 4).max(1)
                    && state.bytes < (self.bytes / 4).max(1)
                    && line.len() <= self.bytes.saturating_sub(state.bytes)
                {
                    state.bytes += line.len();
                    state.messages.push_back(line);
                    drop(state);
                    self.ready.notify_one();
                    return Ok(());
                }
            }
            tokio::select! {_=cancel.cancelled()=>anyhow::bail!("context canceled"),_=self.closed.cancelled()=>anyhow::bail!("connection closed"),_=&mut room=>{}}
        }
    }
    /// One socket writer drains the queue, retaining the order in which producers admitted frames.
    pub async fn recv(&self, cancel: &CancellationToken) -> Option<Arc<[u8]>> {
        loop {
            let ready = self.ready.notified();
            {
                let mut state = self.state.lock().unwrap();
                if state.closed || cancel.is_cancelled() {
                    return None;
                }
                if let Some(line) = state.messages.pop_front() {
                    state.bytes -= line.len();
                    drop(state);
                    self.room.notify_waiters();
                    return Some(line);
                }
            }
            tokio::select! {_=cancel.cancelled()=>return None,_=self.closed.cancelled()=>return None,_=ready=>{}}
        }
    }
}
