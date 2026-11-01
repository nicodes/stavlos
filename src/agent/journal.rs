//! One serialized commit boundary for channel state, wakeups and shared call budgets.
use super::state::ChannelState;
use crate::{
    config,
    event::{self, Event},
    model, protocol,
    wire::Time,
};
use anyhow::{Result, ensure};
use chrono::{DateTime, FixedOffset, Utc};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::sync::Notify;
use tokio_util::sync::CancellationToken;
pub trait Journal: Send + Sync {
    fn append(&self, events: Vec<Event>, cancel: &CancellationToken) -> Result<Vec<Event>>;
    fn stream(&self, notification: protocol::StreamNotification);
}
pub struct LogJournal {
    pub log: Arc<crate::eventlog::Log>,
    pub stream: Arc<dyn Fn(protocol::StreamNotification) + Send + Sync>,
}
impl Journal for LogJournal {
    fn append(&self, events: Vec<Event>, cancel: &CancellationToken) -> Result<Vec<Event>> {
        ensure!(!cancel.is_cancelled(), "context canceled");
        // Once submitted, wait for a definite acknowledgement rather than abandoning a commit.
        self.log.append(events)
    }
    fn stream(&self, notification: protocol::StreamNotification) {
        (self.stream)(notification);
    }
}
#[derive(Debug, thiserror::Error)]
#[error("channel is stopped")]
pub struct Stopped;
#[derive(Debug, thiserror::Error)]
#[error(
    "channel budget exhausted ({0}); an operator must increase the limit or start a new channel"
)]
pub struct BudgetExhausted(pub String);
pub(crate) struct State {
    pub channel: ChannelState,
    pub config: Arc<config::Effective>,
    pub stopped: bool,
    pub reconfiguring: bool,
    pub model_chosen: bool,
    pub wakes: BTreeMap<String, Arc<Notify>>,
    pub log_errors: BTreeMap<String, String>,
}
pub struct Core {
    pub id: String,
    pub created: Time,
    pub(crate) state: Mutex<State>,
    journal: Arc<dyn Journal>,
}
impl Core {
    pub fn new(
        id: String,
        directory: String,
        created: Time,
        config: Arc<config::Effective>,
        model: String,
        role: String,
        journal: Arc<dyn Journal>,
    ) -> Self {
        let chosen = !model.is_empty();
        let model = if chosen { model } else { config.model.clone() };
        let role = if role.is_empty() {
            config.root_agent.clone()
        } else {
            role
        };
        let mut channel = ChannelState::new(model, role);
        channel.dir = directory;
        Self {
            id,
            created,
            state: Mutex::new(State {
                channel,
                config,
                stopped: false,
                reconfiguring: false,
                model_chosen: chosen,
                wakes: BTreeMap::new(),
                log_errors: BTreeMap::new(),
            }),
            journal,
        }
    }
    pub fn event(&self, agent: &str, kind: &str, payload: impl serde::Serialize) -> Result<Event> {
        Ok(Event::new(&self.id, agent, kind, payload)?)
    }
    pub fn bare_event(&self, agent: &str, kind: &str) -> Event {
        Event {
            channel: self.id.clone(),
            agent: agent.into(),
            kind: kind.into(),
            ..Default::default()
        }
    }
    pub fn inspect<T>(&self, read: impl FnOnce(&ChannelState, &config::Effective) -> T) -> T {
        let state = self.state.lock().unwrap();
        read(&state.channel, &state.config)
    }
    pub fn register_wake(&self, id: String, wake: Arc<Notify>) {
        self.state.lock().unwrap().wakes.insert(id, wake);
    }
    pub fn remove_wake(&self, id: &str) {
        self.state.lock().unwrap().wakes.remove(id);
    }
    fn apply(state: &mut State, events: &[Event]) {
        for event in events {
            for id in state.channel.apply(event) {
                if let Some(wake) = state.wakes.get(&id) {
                    wake.notify_one();
                }
            }
        }
    }
    pub(crate) fn commit_locked(
        &self,
        state: &mut State,
        events: Vec<Event>,
        cancel: &CancellationToken,
    ) -> Result<()> {
        if state.stopped {
            return Err(Stopped.into());
        }
        if events.is_empty() {
            return Ok(());
        }
        ensure!(
            events.iter().all(|e| e.channel == self.id),
            "channel event belongs to another channel"
        );
        let out = self.journal.append(events, cancel)?;
        Self::apply(state, &out);
        Ok(())
    }
    pub fn commit(&self, events: Vec<Event>, cancel: &CancellationToken) -> Result<()> {
        let mut state = self.state.lock().unwrap();
        self.commit_locked(&mut state, events, cancel)
    }
    /// A failed decision write prevents this agent from taking its next runtime step.
    pub fn record(&self, agent: &str, events: Vec<Event>) -> Result<()> {
        let mut state = self.state.lock().unwrap();
        if let Err(error) = self.commit_locked(&mut state, events, &CancellationToken::new()) {
            let message = format!("event log: {error:#}");
            state
                .log_errors
                .entry(agent.into())
                .or_insert_with(|| message.clone());
            return Err(anyhow::anyhow!(message));
        }
        Ok(())
    }
    pub fn log_error(&self, agent: &str) -> Option<String> {
        self.state.lock().unwrap().log_errors.get(agent).cloned()
    }
    pub fn clear_log_error(&self, agent: &str) {
        self.state.lock().unwrap().log_errors.remove(agent);
    }
    pub(crate) fn fact_locked(
        &self,
        state: &mut State,
        mut events: Vec<Event>,
        cancel: &CancellationToken,
    ) -> Result<()> {
        ensure!(
            events.iter().all(|e| e.channel == self.id),
            "channel event belongs to another channel"
        );
        match self.commit_locked(state, events.clone(), cancel) {
            Ok(()) => Ok(()),
            Err(error) if error.is::<Stopped>() => Err(error),
            Err(error) => {
                let now = Utc::now().fixed_offset();
                for event in &mut events {
                    event.time = now;
                }
                Self::apply(state, &events);
                eprintln!(
                    "channel {}: {} happened but could not be logged: {error:#}",
                    self.id,
                    events
                        .iter()
                        .map(|e| e.kind.as_str())
                        .collect::<Vec<_>>()
                        .join(", ")
                );
                Err(error)
            }
        }
    }
    pub fn fact(&self, events: Vec<Event>, cancel: &CancellationToken) -> Result<()> {
        let mut state = self.state.lock().unwrap();
        self.fact_locked(&mut state, events, cancel)
    }
    pub fn restore_page(&self, events: &[Event]) -> Result<()> {
        ensure!(
            events.iter().all(|e| e.channel == self.id),
            "channel event belongs to another channel"
        );
        let mut state = self.state.lock().unwrap();
        for event in events {
            state.channel.apply(event);
        }
        Ok(())
    }
    pub fn stop(&self) {
        self.state.lock().unwrap().stopped = true;
    }
    pub fn configuration(&self) -> Arc<config::Effective> {
        self.state.lock().unwrap().config.clone()
    }
    pub fn set_configuration(&self, config: Arc<config::Effective>) {
        let mut state = self.state.lock().unwrap();
        if !state.reconfiguring
            && (config.dir.as_os_str().is_empty()
                || config.dir.to_string_lossy() == state.channel.dir)
        {
            state.config = config;
        }
    }
    pub fn model_chosen(&self) -> bool {
        self.state.lock().unwrap().model_chosen
    }
    pub(crate) fn budget_reason_locked(
        &self,
        state: &State,
        now: DateTime<FixedOffset>,
    ) -> &'static str {
        let limit = &state.config.limits;
        if limit.max_channel_calls > 0 && state.channel.model_calls >= limit.max_channel_calls {
            return "limits.maxChannelCalls";
        }
        if limit.max_channel_tokens > 0 && state.channel.budget_tokens >= limit.max_channel_tokens {
            return "limits.maxChannelTokens";
        }
        if limit.max_channel_cost_usd > 0.
            && state.channel.budget_cost >= limit.max_channel_cost_usd
        {
            return "limits.maxChannelCostUSD";
        }
        if let Ok(ns) = crate::duration::parse(&limit.max_channel_duration)
            && ns > 0
            && self
                .created
                .0
                .checked_add_signed(chrono::Duration::nanoseconds(ns))
                .is_some_and(|end| now >= end)
        {
            return "limits.maxChannelDuration";
        }
        ""
    }
    pub fn budget_reason(&self, now: DateTime<FixedOffset>) -> &'static str {
        self.budget_reason_locked(&self.state.lock().unwrap(), now)
    }
    pub fn reserve_call(
        &self,
        agent: &str,
        purpose: &str,
        cancel: &CancellationToken,
    ) -> Result<Option<DateTime<FixedOffset>>> {
        let mut state = self.state.lock().unwrap();
        let reason = self.budget_reason_locked(&state, Utc::now().fixed_offset());
        if !reason.is_empty() {
            return Err(BudgetExhausted(reason.into()).into());
        }
        self.commit_locked(
            &mut state,
            vec![self.event(
                agent,
                event::MODEL_CALL_STARTED,
                event::ModelCallPayload {
                    purpose: purpose.into(),
                    ..Default::default()
                },
            )?],
            cancel,
        )?;
        Ok(
            crate::duration::parse(&state.config.limits.max_channel_duration)
                .ok()
                .filter(|ns| *ns > 0)
                .and_then(|ns| {
                    self.created
                        .0
                        .checked_add_signed(chrono::Duration::nanoseconds(ns))
                }),
        )
    }
    pub async fn begin_call(
        &self,
        agent: &str,
        purpose: &str,
        parent: &CancellationToken,
    ) -> Result<CallScope> {
        let deadline = self.reserve_call(agent, purpose, parent)?;
        let cancel = parent.child_token();
        let timer = CancellationToken::new();
        if let Some(deadline) = deadline {
            let delay = deadline
                .signed_duration_since(Utc::now())
                .to_std()
                .unwrap_or(Duration::ZERO);
            let expired = cancel.clone();
            let stop = timer.clone();
            tokio::spawn(async move {
                tokio::select! {_=stop.cancelled()=>{},_=tokio::time::sleep(delay)=>expired.cancel()}
            });
        }
        Ok(CallScope { cancel, timer })
    }
    pub fn finish_call(
        &self,
        agent: &str,
        purpose: &str,
        usage: model::Usage,
        cost: f64,
    ) -> Result<()> {
        self.fact(
            vec![self.event(
                agent,
                event::MODEL_CALL_COMPLETED,
                event::ModelCallPayload {
                    purpose: purpose.into(),
                    usage,
                    cost_usd: cost,
                },
            )?],
            &CancellationToken::new(),
        )
    }
    pub fn stream(&self, notification: protocol::StreamNotification) {
        self.journal.stream(notification);
    }
}
pub struct CallScope {
    pub cancel: CancellationToken,
    timer: CancellationToken,
}
impl Drop for CallScope {
    fn drop(&mut self) {
        self.timer.cancel();
        self.cancel.cancel();
    }
}
