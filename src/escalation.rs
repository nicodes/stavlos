//! Concurrent owner prompts, claims, fallback delivery and cancellation-safe resolution.
use crate::protocol::{self, PromptInfo, PromptNotification};
use anyhow::{Result, ensure};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{sync::oneshot, time::Instant};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Debug, Default)]
pub struct Answer {
    pub details: Vec<protocol::QuestionAnswer>,
    pub value: String,
    pub dir: String,
    pub reason: String,
    pub answers: Vec<String>,
    pub client: String,
    pub defaulted: bool,
    pub withdrawn: bool,
}
#[derive(Clone, Debug)]
pub struct Config {
    pub claim_timeout: Duration,
    pub answer_timeout: Duration,
    pub default: String,
    pub claim_expiry: Duration,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            claim_timeout: Duration::ZERO,
            answer_timeout: Duration::ZERO,
            default: "deny".into(),
            claim_expiry: Duration::from_secs(120),
        }
    }
}
impl Config {
    fn normalized(mut self) -> Self {
        if self.default.is_empty() {
            self.default = "deny".into();
        }
        if self.claim_expiry.is_zero() {
            self.claim_expiry = Duration::from_secs(120);
        }
        self
    }
}
pub type Sink = Arc<dyn Fn(PromptNotification, &[String]) + Send + Sync>;
struct Pending {
    info: PromptInfo,
    answer: oneshot::Sender<Answer>,
    claimed_at: Instant,
    generation: u64,
}
struct State {
    config: Config,
    pending: BTreeMap<String, Pending>,
    generation: u64,
}
pub struct Manager {
    state: Mutex<State>,
    sink: Sink,
}
impl Manager {
    pub fn new(config: Config, sink: Sink) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::new(State {
                config: config.normalized(),
                pending: BTreeMap::new(),
                generation: 0,
            }),
            sink,
        })
    }
    pub fn set_config(&self, config: Config) {
        self.state.lock().unwrap().config = config.normalized();
    }
    fn notify(&self, action: &str, info: PromptInfo) {
        let mut tiers = vec![protocol::TIER_INTERACTIVE.into()];
        if info.escalated {
            tiers.push(protocol::TIER_FALLBACK.into());
        }
        (self.sink)(
            PromptNotification {
                action: action.into(),
                prompt: info,
            },
            &tiers,
        );
    }
    pub async fn request(
        self: &Arc<Self>,
        mut info: PromptInfo,
        cancel: CancellationToken,
        opened: Option<Box<dyn FnOnce() + Send>>,
    ) -> Answer {
        info.created = chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Nanos, true);
        if matches!(
            info.kind.as_str(),
            protocol::PROMPT_QUESTION | protocol::PROMPT_PERMISSION | protocol::PROMPT_TRUST
        ) {
            info.escalated = true;
        }
        let (sender, mut receiver) = oneshot::channel();
        let (config, generation) = {
            let mut state = self.state.lock().unwrap();
            // A repeated ID cannot displace a live request and strand its waiter.
            if state.pending.contains_key(&info.id) {
                return Answer {
                    withdrawn: true,
                    ..Default::default()
                };
            }
            state.generation = state.generation.wrapping_add(1);
            let generation = state.generation;
            state.pending.insert(
                info.id.clone(),
                Pending {
                    info: info.clone(),
                    answer: sender,
                    claimed_at: Instant::now(),
                    generation,
                },
            );
            (state.config.clone(), generation)
        };
        let mut guard = RequestGuard {
            manager: self.clone(),
            id: info.id.clone(),
            generation,
            armed: true,
        };
        self.notify(protocol::ACTION_REQUESTED, info.clone());
        if let Some(opened) = opened {
            opened();
        }
        let claim = tokio::time::sleep(config.claim_timeout);
        tokio::pin!(claim);
        let answer = tokio::time::sleep(config.answer_timeout);
        tokio::pin!(answer);
        let mut claim_waiting = !info.escalated;
        let mut answer_waiting = !matches!(
            info.kind.as_str(),
            protocol::PROMPT_QUESTION | protocol::PROMPT_TRUST
        );
        let mut expiry = tokio::time::interval_at(
            Instant::now() + Duration::from_secs(10),
            Duration::from_secs(10),
        );
        let result = loop {
            tokio::select! {
                accepted = &mut receiver => break accepted.unwrap_or_else(|_| Answer {withdrawn:true, ..Default::default()}),
                _ = &mut claim, if claim_waiting => {
                    claim_waiting = false;
                    let announcement = {
                        let mut state = self.state.lock().unwrap();
                        state.pending.get_mut(&info.id).filter(|p| p.generation == generation && p.info.claimed_by.is_empty()).map(|p| {p.info.escalated = true; p.info.clone()})
                    };
                    if let Some(info) = announcement { self.notify(protocol::ACTION_ESCALATED, info); }
                }
                _ = expiry.tick() => self.expire_claims(Instant::now(), config.claim_expiry),
                _ = &mut answer, if answer_waiting => {
                    answer_waiting = false;
                    self.finish(&info.id, Some(generation), Answer {value:config.default.clone(), defaulted:true, ..Default::default()}, protocol::ACTION_DEFAULTED);
                }
                _ = cancel.cancelled() => {
                    self.finish(&info.id, Some(generation), Answer {withdrawn:true, ..Default::default()}, protocol::ACTION_WITHDRAWN);
                    // A reply already accepted wins simultaneous cancellation.
                    break receiver.await.unwrap_or_else(|_| Answer {withdrawn:true, ..Default::default()});
                }
            }
        };
        guard.armed = false;
        result
    }
    fn finish(&self, id: &str, generation: Option<u64>, answer: Answer, action: &str) -> bool {
        let pending = {
            let mut state = self.state.lock().unwrap();
            if state
                .pending
                .get(id)
                .is_none_or(|p| generation.is_some_and(|generation| p.generation != generation))
            {
                return false;
            }
            state.pending.remove(id).unwrap()
        };
        let _ = pending.answer.send(answer);
        self.notify(action, pending.info);
        true
    }
    pub fn release_client(&self, client: &str) {
        let announcements = {
            let mut state = self.state.lock().unwrap();
            state
                .pending
                .values_mut()
                .filter(|p| p.info.claimed_by == client)
                .map(|p| {
                    p.info.claimed_by.clear();
                    p.info.clone()
                })
                .collect::<Vec<_>>()
        };
        for info in announcements {
            self.notify(protocol::ACTION_REQUESTED, info);
        }
    }
    pub fn expire_claims(&self, now: Instant, expiry: Duration) {
        let announcements = {
            let mut state = self.state.lock().unwrap();
            state
                .pending
                .values_mut()
                .filter(|p| {
                    !p.info.claimed_by.is_empty()
                        && now.saturating_duration_since(p.claimed_at) > expiry
                })
                .map(|p| {
                    p.info.claimed_by.clear();
                    p.info.clone()
                })
                .collect::<Vec<_>>()
        };
        for info in announcements {
            self.notify(protocol::ACTION_REQUESTED, info);
        }
    }
    pub fn claim(&self, id: &str, client: &str) -> Result<()> {
        let info = {
            let mut state = self.state.lock().unwrap();
            let p = state.pending.get_mut(id).ok_or_else(|| {
                anyhow::anyhow!("prompt was already resolved; late answer rejected")
            })?;
            ensure!(
                p.info.claimed_by.is_empty() || p.info.claimed_by == client,
                "prompt is claimed by another client"
            );
            p.info.claimed_by = client.into();
            p.claimed_at = Instant::now();
            p.info.clone()
        };
        self.notify(protocol::ACTION_CLAIMED, info);
        Ok(())
    }
    pub fn reply(&self, id: &str, client: &str, answer: Answer) -> Result<()> {
        self.answer(id, client, answer, false)
    }
    pub fn resolve(&self, id: &str, client: &str, answer: Answer) -> Result<()> {
        self.answer(id, client, answer, true)
    }
    fn answer(&self, id: &str, client: &str, mut answer: Answer, force: bool) -> Result<()> {
        // Claim validation, removal and send share one lock, so another claimant cannot slip between them.
        let pending = {
            let mut state = self.state.lock().unwrap();
            let p = state.pending.get_mut(id).ok_or_else(|| {
                anyhow::anyhow!("prompt was already resolved; late answer rejected")
            })?;
            if !force {
                ensure!(
                    p.info.claimed_by.is_empty() || p.info.claimed_by == client,
                    "prompt is claimed by another client"
                );
                if p.info.kind == protocol::PROMPT_QUESTION && p.info.question_number > 0 {
                    let single = if answer.answers.len() == 1 {
                        !answer.answers[0].trim().is_empty()
                    } else {
                        answer.answers.is_empty()
                            && answer.value != protocol::ANSWER_ANSWERED
                            && !answer.value.trim().is_empty()
                    };
                    ensure!(
                        single,
                        "provide exactly one nonempty answer for this question"
                    );
                }
                p.info.claimed_by = client.into();
            }
            state.pending.remove(id).unwrap()
        };
        answer.client = client.into();
        let _ = pending.answer.send(answer);
        self.notify(protocol::ACTION_ANSWERED, pending.info);
        Ok(())
    }
    pub fn answer_where(
        &self,
        channel: &str,
        kind: &str,
        value: &str,
        client: &str,
        keep: impl Fn(&PromptInfo) -> bool,
    ) -> usize {
        let pending = {
            let mut state = self.state.lock().unwrap();
            let ids = state
                .pending
                .iter()
                .filter(|(_, p)| p.info.channel == channel && p.info.kind == kind && keep(&p.info))
                .map(|(id, _)| id.clone())
                .collect::<Vec<_>>();
            ids.into_iter()
                .filter_map(|id| state.pending.remove(&id))
                .collect::<Vec<_>>()
        };
        let count = pending.len();
        for mut p in pending {
            p.info.claimed_by = client.into();
            let _ = p.answer.send(Answer {
                value: value.into(),
                client: client.into(),
                ..Default::default()
            });
            self.notify(protocol::ACTION_ANSWERED, p.info);
        }
        count
    }
    pub fn pending(&self, channel: &str) -> Vec<PromptInfo> {
        let mut out: Vec<_> = self
            .state
            .lock()
            .unwrap()
            .pending
            .values()
            .filter(|p| channel.is_empty() || p.info.channel == channel)
            .map(|p| p.info.clone())
            .collect();
        out.sort_by(|a, b| a.created.cmp(&b.created).then_with(|| a.id.cmp(&b.id)));
        out
    }
}
struct RequestGuard {
    manager: Arc<Manager>,
    id: String,
    generation: u64,
    armed: bool,
}
impl Drop for RequestGuard {
    fn drop(&mut self) {
        if self.armed {
            self.manager.finish(
                &self.id,
                Some(self.generation),
                Answer {
                    withdrawn: true,
                    ..Default::default()
                },
                protocol::ACTION_WITHDRAWN,
            );
        }
    }
}
