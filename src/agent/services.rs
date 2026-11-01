//! Journal-backed todo lists and owned background command lifetimes.
use super::{journal::Core, names::new_id};
use crate::{event, proc, protocol, tools};
use anyhow::{Result, ensure};
use std::{
    collections::BTreeMap,
    path::PathBuf,
    sync::{Arc, Mutex, Weak},
    time::{Duration, SystemTime},
};
use tokio_util::{sync::CancellationToken, task::TaskTracker};

pub struct TodoService {
    pub core: Arc<Core>,
    pub agent: String,
}
#[derive(Clone)]
pub struct AskService {
    pub core: Arc<Core>,
    pub agent: String,
    pub manager: Arc<crate::escalation::Manager>,
}
impl AskService {
    pub async fn request(
        &self,
        info: protocol::PromptInfo,
        call_id: String,
        cancel: CancellationToken,
        opened: Option<Box<dyn FnOnce() + Send>>,
    ) -> crate::escalation::Answer {
        let id = info.id.clone();
        let core = self.core.clone();
        let agent = self.agent.clone();
        let recorded = info.clone();
        let answer = self
            .manager
            .request(
                info,
                cancel,
                Some(Box::new(move || {
                    let payload = event::AskRequestedPayload {
                        id: recorded.id,
                        kind: recorded.kind,
                        call_id,
                        tool: recorded.tool,
                        input: recorded.input,
                        dir: recorded.dir,
                        prefix: recorded.prefix,
                        question: recorded.question,
                        questions: recorded.questions,
                        from: recorded.from,
                        role: recorded.role,
                        question_number: recorded.question_number,
                        question_total: recorded.question_total,
                    };
                    if let Ok(event) = core.event(&agent, event::ASK_REQUESTED, payload) {
                        let _ = core.record(&agent, vec![event]);
                    }
                    if let Some(opened) = opened {
                        opened();
                    }
                })),
            )
            .await;
        let payload = event::AskResolvedPayload {
            id,
            outcome: if answer.withdrawn {
                event::ASK_WITHDRAWN.into()
            } else if answer.defaulted {
                event::ASK_DEFAULTED.into()
            } else {
                event::ASK_ANSWERED.into()
            },
            answer: if !answer.withdrawn && !answer.defaulted && !answer.answers.is_empty() {
                answer.answers.join(" · ")
            } else {
                answer.value.clone()
            },
            by: answer.client.clone(),
            answers: answer.answers.clone().into(),
            details: answer.details.clone().into(),
            reason: answer.reason.clone(),
            dir: answer.dir.clone(),
        };
        if let Ok(event) = self.core.event(&self.agent, event::ASK_RESOLVED, payload) {
            let _ = self.core.fact(vec![event], &CancellationToken::new());
        }
        answer
    }
}
impl tools::Asker for AskService {
    fn ask<'a>(
        &'a self,
        questions: Vec<event::Question>,
        cancel: CancellationToken,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = tools::Answers> + Send + 'a>> {
        Box::pin(async move {
            if cancel.is_cancelled() {
                return tools::Answers {
                    answers: Vec::new(),
                    error: Some("context canceled".into()),
                };
            }
            let cancel = cancel.child_token();
            let mut tasks = tokio::task::JoinSet::new();
            let mut answers = vec![String::new(); questions.len()];
            let mut error = None;
            let total = questions.len() as i64;
            for (index, question) in questions.into_iter().enumerate() {
                if cancel.is_cancelled() {
                    error = Some("context canceled".into());
                    break;
                }
                let info = self.core.inspect(|state, config| {
                    let view = super::prompt::role_view(state, &self.agent, config)
                        .ok_or_else(|| anyhow::anyhow!("agent not found"))?;
                    Ok::<_, anyhow::Error>(protocol::PromptInfo {
                        id: new_id("p")?,
                        channel: self.core.id.clone(),
                        channel_name: state.name.clone(),
                        agent: self.agent.clone(),
                        from: view.name.clone(),
                        role: view.role,
                        kind: protocol::PROMPT_QUESTION.into(),
                        tool: crate::toolname::ASK.into(),
                        question: format!("{} asks: {}", view.name, question.question),
                        questions: vec![question].into(),
                        question_number: index as i64 + 1,
                        question_total: total,
                        ..Default::default()
                    })
                });
                let info = match info {
                    Ok(info) => info,
                    Err(failure) => {
                        error = Some(failure.to_string());
                        cancel.cancel();
                        break;
                    }
                };
                let (published, mut publication) = tokio::sync::oneshot::channel();
                let service = self.clone();
                let token = cancel.clone();
                tasks.spawn(async move {
                    (
                        index,
                        service
                            .request(
                                info,
                                String::new(),
                                token,
                                Some(Box::new(move || {
                                    let _ = published.send(());
                                })),
                            )
                            .await,
                    )
                });
                tokio::select! {_ = &mut publication => {}, _ = cancel.cancelled() => {}}
            }
            while let Some(result) = tasks.join_next().await {
                let failure = match result {
                    Ok((index, answer)) => {
                        if answer.withdrawn {
                            Some(format!(
                                "question {} was withdrawn before it was answered",
                                index + 1
                            ))
                        } else if answer
                            .answers
                            .first()
                            .is_some_and(|answer| !answer.trim().is_empty())
                        {
                            answers[index] = answer.answers[0].clone();
                            None
                        } else if !answer.value.is_empty()
                            && answer.value != protocol::ANSWER_ANSWERED
                        {
                            answers[index] = answer.value;
                            None
                        } else {
                            Some(format!("question {} received no answer", index + 1))
                        }
                    }
                    Err(failure) => Some(format!("question task failed: {failure}")),
                };
                if error.is_none()
                    && let Some(failure) = failure
                {
                    error = Some(failure);
                    cancel.cancel();
                }
            }
            cancel.cancel();
            tools::Answers { answers, error }
        })
    }
}
impl tools::Todos for TodoService {
    fn edit(
        &self,
        add: Vec<tools::TodoAdd>,
        updates: Vec<tools::TodoUpdate>,
    ) -> Result<Vec<event::TodoItem>> {
        let mut state = self.core.state.lock().unwrap();
        let agent = state
            .channel
            .agents
            .get(&self.agent)
            .ok_or_else(|| anyhow::anyhow!("agent not found"))?;
        let mut items = agent.todos.clone();
        for update in updates {
            let item = items
                .iter_mut()
                .find(|item| item.id == update.id)
                .ok_or_else(|| {
                    anyhow::anyhow!(
                        "no todo item {}",
                        serde_json::to_string(&update.id).unwrap()
                    )
                })?;
            if !update.status.is_empty() {
                item.status = update.status;
            }
            if !update.text.is_empty() {
                item.text = update.text;
            }
        }
        let mut next = agent.todo_seq;
        for item in &items {
            if let Ok(number) = item.id.strip_prefix('t').unwrap_or(&item.id).parse::<i64>() {
                next = next.max(number);
            }
        }
        for item in add {
            next = next
                .checked_add(1)
                .ok_or_else(|| anyhow::anyhow!("todo identifier space exhausted"))?;
            items.push(event::TodoItem {
                id: format!("t{next}"),
                text: item.text,
                status: if item.status.is_empty() {
                    event::TODO_PENDING.into()
                } else {
                    item.status
                },
            });
        }
        self.core.commit_locked(
            &mut state,
            vec![self.core.event(
                &self.agent,
                event::TODO_CHANGED,
                event::TodoPayload {
                    items: items.clone().into(),
                },
            )?],
            &CancellationToken::new(),
        )?;
        Ok(items)
    }
    fn list(&self) -> Vec<event::TodoItem> {
        self.core
            .state
            .lock()
            .unwrap()
            .channel
            .agents
            .get(&self.agent)
            .map(|agent| agent.todos.clone())
            .unwrap_or_default()
    }
}
pub type AfterCommand = Arc<dyn Fn() -> String + Send + Sync>;
struct RunningJob {
    id: String,
    command: String,
    job: proc::Job,
    cancel: CancellationToken,
}
pub struct JobRegistry {
    core: Arc<Core>,
    agent: String,
    own: Weak<JobRegistry>,
    lifetime: CancellationToken,
    jobs: Mutex<BTreeMap<String, Arc<RunningJob>>>,
    tasks: TaskTracker,
    overflow: PathBuf,
}
pub struct JobApi {
    pub registry: Arc<JobRegistry>,
    pub turn: CancellationToken,
    pub after: Option<AfterCommand>,
}
impl JobRegistry {
    pub fn new(
        core: Arc<Core>,
        agent: String,
        lifetime: CancellationToken,
        overflow: PathBuf,
    ) -> Arc<Self> {
        Arc::new_cyclic(|own| Self {
            core,
            agent,
            own: own.clone(),
            lifetime: lifetime.child_token(),
            jobs: Mutex::new(BTreeMap::new()),
            tasks: TaskTracker::new(),
            overflow,
        })
    }
    fn adopt(
        &self,
        turn: &CancellationToken,
        command: &str,
        job: proc::Job,
        timeout: Duration,
        after: Option<AfterCommand>,
    ) -> Result<String> {
        let handle = tokio::runtime::Handle::try_current()?;
        let run = {
            let mut state = self.core.state.lock().unwrap();
            ensure!(
                !turn.is_cancelled() && !self.lifetime.is_cancelled(),
                "context canceled"
            );
            let agent = state
                .channel
                .agents
                .get(&self.agent)
                .ok_or_else(|| anyhow::anyhow!("agent not found"))?;
            ensure!(!agent.killed, "agent {} is killed", agent.name);
            let id = new_id("m")?;
            self.core.commit_locked(
                &mut state,
                vec![self.core.event(
                    &self.agent,
                    event::JOB_STARTED,
                    event::JobStartedPayload {
                        id: id.clone(),
                        command: command.into(),
                    },
                )?],
                &CancellationToken::new(),
            )?;
            let run = Arc::new(RunningJob {
                id: id.clone(),
                command: command.into(),
                job,
                cancel: self.lifetime.child_token(),
            });
            self.jobs.lock().unwrap().insert(id, run.clone());
            (run, self.tasks.token())
        };
        let (run, tracker_guard) = run;
        let id = run.id.clone();
        let own = self.own.clone();
        self.tasks.spawn_on(async move {
            let _tracked = tracker_guard;
            let timeout = if timeout.is_zero() {Duration::from_secs(3600)} else {timeout};
            let remaining = run.job.started().checked_add(timeout).and_then(|deadline| deadline.duration_since(SystemTime::now()).ok()).unwrap_or(Duration::ZERO);
            let deadline = tokio::time::sleep(remaining); tokio::pin!(deadline);
            let mut timed_out = false;
            let completion = loop {
                tokio::select! {
                    completion = run.job.wait_async() => break completion,
                    _ = run.cancel.cancelled() => { run.job.kill(); run.job.wait_async().await; return; },
                    _ = &mut deadline, if !timed_out => { timed_out = true; run.job.kill(); }
                }
            };
            if let Some(registry) = own.upgrade() { registry.finish(&run, completion, timed_out, timeout, after); }
        }, &handle);
        Ok(id)
    }
    fn finish(
        &self,
        run: &Arc<RunningJob>,
        completion: proc::Completion,
        timed_out: bool,
        timeout: Duration,
        after: Option<AfterCommand>,
    ) {
        // Hooks may inspect channel configuration; invoke them before acquiring the channel lock.
        let note = after.map(|after| after()).unwrap_or_default();
        let mut state = self.core.state.lock().unwrap();
        let mut jobs = self.jobs.lock().unwrap();
        if !jobs
            .get(&run.id)
            .is_some_and(|current| Arc::ptr_eq(current, run))
        {
            return;
        }
        jobs.remove(&run.id);
        drop(jobs);
        let env = tools::Env {
            max_output: state.config.compaction.max_tool_output as i64,
            overflow: self.overflow.clone(),
            ..Default::default()
        };
        let mut result = summarize_exit(&run.id, &completion, timed_out, timeout);
        result.output = env.clip(&run.job.output());
        if !note.is_empty() {
            result.output.push_str("\n\n");
            result.output.push_str(&note);
        }
        let events = (|| -> Result<_> {
            Ok(vec![
                self.core.event(&self.agent, event::JOB_FINISHED, result)?,
                self.core.event(
                    &self.agent,
                    event::INPUT_QUEUED,
                    event::Input {
                        id: new_id("i")?,
                        kind: event::INPUT_JOB.into(),
                        job: run.id.clone(),
                        ..Default::default()
                    },
                )?,
            ])
        })();
        if let Ok(events) = events {
            let _ = self
                .core
                .fact_locked(&mut state, events, &CancellationToken::new());
        }
    }
    pub fn stop(&self, id: &str, reason: &str) -> Result<()> {
        let mut state = self.core.state.lock().unwrap();
        let run = self.jobs.lock().unwrap().remove(id).ok_or_else(|| {
            anyhow::anyhow!("no running job {}", serde_json::to_string(id).unwrap())
        })?;
        let result = self.core.fact_locked(
            &mut state,
            vec![self.core.event(
                &self.agent,
                event::JOB_STOPPED,
                event::JobStoppedPayload {
                    id: id.into(),
                    reason: reason.into(),
                },
            )?],
            &CancellationToken::new(),
        );
        drop(state);
        run.cancel.cancel();
        result
    }
    pub fn stop_all(&self, reason: &str) {
        let ids: Vec<_> = self.jobs.lock().unwrap().keys().cloned().collect();
        for id in ids {
            let _ = self.stop(&id, reason);
        }
    }
    pub fn has(&self, id: &str) -> bool {
        self.jobs.lock().unwrap().contains_key(id)
    }
    pub fn infos(&self) -> Vec<protocol::JobInfo> {
        let jobs = self.jobs.lock().unwrap();
        let mut runs: Vec<_> = jobs.values().collect();
        runs.sort_by_key(|run| run.job.started());
        runs.into_iter()
            .map(|run| protocol::JobInfo {
                id: run.id.clone(),
                agent: self.agent.clone(),
                label: job_label(&run.command),
                spec: run.command.clone(),
                started: chrono::DateTime::<chrono::Utc>::from(run.job.started())
                    .to_rfc3339_opts(chrono::SecondsFormat::Secs, true),
                progress: format!("{} lines", run.job.lines()),
            })
            .collect()
    }
    pub async fn shutdown(&self) {
        {
            let _state = self.core.state.lock().unwrap();
            self.lifetime.cancel();
            self.tasks.close();
        }
        self.tasks.wait().await;
    }
}
impl Drop for JobRegistry {
    fn drop(&mut self) {
        self.lifetime.cancel();
        for run in self.jobs.get_mut().unwrap().values() {
            run.job.kill();
        }
    }
}
impl tools::Jobs for JobApi {
    fn adopt(&self, command: &str, job: proc::Job, timeout: Duration) -> Result<String> {
        self.registry
            .adopt(&self.turn, command, job, timeout, self.after.clone())
    }
    fn stop(&self, id: &str) -> Result<()> {
        self.registry.stop(id, "stopped by agent")
    }
    fn has(&self, id: &str) -> bool {
        self.registry.has(id)
    }
}
pub fn format_duration(duration: Duration) -> String {
    let seconds = duration
        .as_secs()
        .saturating_add(u64::from(duration.subsec_nanos() >= 500_000_000));
    if seconds < 60 {
        format!("{seconds}s")
    } else if seconds < 3600 {
        format!("{}m{:02}s", seconds / 60, seconds % 60)
    } else {
        format!("{}h{:02}m", seconds / 3600, seconds / 60 % 60)
    }
}
pub fn job_label(command: &str) -> String {
    if command.chars().count() > 40 {
        format!("{}…", command.chars().take(40).collect::<String>())
    } else {
        command.into()
    }
}
pub fn summarize_exit(
    id: &str,
    completion: &proc::Completion,
    timed_out: bool,
    timeout: Duration,
) -> event::JobFinishedPayload {
    let mut result = event::JobFinishedPayload {
        id: id.into(),
        ..Default::default()
    };
    if timed_out {
        result.is_error = true;
        result.exit_code = -1;
        result.summary = format!(
            "background command killed after {} timeout",
            format_duration(timeout)
        );
    } else if !completion.success() {
        result.is_error = true;
        result.exit_code = -1;
        if let Some(status) = completion.status {
            result.exit_code = status.code().map(i64::from).unwrap_or(-1);
            result.summary = format!("background command exited {}", result.exit_code);
        } else {
            result.summary = format!(
                "background command failed: {}",
                completion.error.as_deref().unwrap_or("unknown error")
            );
        }
    } else {
        result.summary = "background command finished (exit 0)".into();
    }
    result
}
