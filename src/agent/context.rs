//! Runtime conversation memory and budgeted context compaction.
use super::{
    control::InstructionCache,
    conversation::{self, Reads, SentNote},
    journal::Core,
};
use crate::{event, model, project};
use anyhow::{Result, ensure};
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, Ordering},
};
use tokio_util::sync::CancellationToken;
pub struct Conversation {
    core: Arc<Core>,
    agent: String,
    notes: Mutex<Vec<SentNote>>,
    reads: Mutex<Reads>,
    pub instructions: InstructionCache,
    requested: AtomicBool,
    gauge: Mutex<(i64, i64)>,
}
impl Conversation {
    pub fn new(core: Arc<Core>, agent: String) -> Self {
        Self {
            core,
            agent,
            notes: Mutex::new(Vec::new()),
            reads: Mutex::new(Reads::default()),
            instructions: InstructionCache::default(),
            requested: AtomicBool::new(false),
            gauge: Mutex::new((0, 0)),
        }
    }
    pub fn request_compaction(&self) {
        self.requested.store(true, Ordering::Release);
    }
    pub fn gauge(&self) -> (i64, i64) {
        *self.gauge.lock().unwrap()
    }
    pub fn with_notes(&self, history: Vec<model::Message>, note: &str) -> Vec<model::Message> {
        let mut prior = self.notes.lock().unwrap();
        let (history, notes) = conversation::with_notes(history, &prior, note);
        *prior = notes;
        history
    }
    pub fn deduplicate_read(&self, call: &model::Block, output: &str) -> String {
        self.reads.lock().unwrap().deduplicate(call, output)
    }
    pub fn clear_caches(&self) {
        self.notes.lock().unwrap().clear();
        self.reads.lock().unwrap().clear();
        self.instructions.clear();
    }
    fn history(&self) -> Vec<model::Message> {
        self.core
            .state
            .lock()
            .unwrap()
            .channel
            .agents
            .get_mut(&self.agent)
            .map(|agent| agent.history.history())
            .unwrap_or_default()
    }
    pub async fn prepare(
        &self,
        cancel: CancellationToken,
        model: &dyn model::Model,
        info: &model::Info,
        system: &str,
        definitions: &[model::ToolDef],
    ) -> Vec<model::Message> {
        let (mut history, running, compaction, last) = {
            let mut state = self.core.state.lock().unwrap();
            let config = state.config.clone();
            match state.channel.agents.get_mut(&self.agent) {
                Some(agent) => (
                    agent.history.history(),
                    agent.compacting,
                    config.compaction.clone(),
                    agent.last_context,
                ),
                None => return Vec::new(),
            }
        };
        let wanted = if running {
            self.requested.load(Ordering::Acquire)
        } else {
            self.requested.swap(false, Ordering::AcqRel)
        };
        let (_, mut cleared) = project::clear_old(
            &mut history,
            compaction.clear_tokens,
            project::CLEAR_MINIMUM,
            &conversation::conversation_tools(),
        );
        let mut estimated = project::estimate_tokens(&history, system, definitions);
        let size = estimated.max(last);
        let full = info.context_window > 0
            && size > ((conversation::usable_window(info) as f64) * compaction.threshold) as i64;
        let over = compaction.max_tokens > 0 && size > compaction.max_tokens;
        if !running
            && (wanted || full || over)
            && self.compact(cancel, model, info, wanted).await.is_ok()
        {
            history = self.history();
            let (_, gone) = project::clear_old(
                &mut history,
                compaction.clear_tokens,
                project::CLEAR_MINIMUM,
                &conversation::conversation_tools(),
            );
            cleared = gone;
            estimated = project::estimate_tokens(&history, system, definitions);
        }
        *self.gauge.lock().unwrap() = (estimated, info.context_window);
        self.reads.lock().unwrap().forget(&cleared);
        history
    }
    pub async fn compact(
        &self,
        cancel: CancellationToken,
        model: &dyn model::Model,
        info: &model::Info,
        all: bool,
    ) -> Result<()> {
        let (cut, id) = {
            let mut state = self.core.state.lock().unwrap();
            let keep = if all {
                0
            } else {
                state.config.compaction.keep_tokens
            };
            let agent = state
                .channel
                .agents
                .get_mut(&self.agent)
                .ok_or_else(|| anyhow::anyhow!("agent not found"))?;
            ensure!(!agent.compacting, "a compaction is already running");
            self.notes.lock().unwrap().clear();
            let cut = agent
                .history
                .cut(keep)
                .ok_or_else(|| anyhow::anyhow!("nothing to compact"))?;
            let id = agent.model.clone();
            self.core.commit_locked(
                &mut state,
                vec![self.core.event(
                    &self.agent,
                    event::COMPACTION_STARTED,
                    event::CompactionPayload {
                        before: cut.before,
                        ..Default::default()
                    },
                )?],
                &CancellationToken::new(),
            )?;
            (cut, id)
        };
        let mut guard = CompactionGuard {
            core: self.core.clone(),
            agent: self.agent.clone(),
            before: cut.before,
            armed: true,
        };
        let mut request = conversation::summary_request(
            conversation::bare_id(&id),
            &crate::clip::middle(
                &project::transcript(&cut.old),
                conversation::SUMMARY_INPUT_MAX,
            ),
            &cut.prev,
            info,
        );
        request.cache_key = format!("{}:compact", self.agent);
        let call = match self
            .core
            .begin_call(&self.agent, "compaction", &cancel)
            .await
        {
            Ok(call) => call,
            Err(error) => {
                guard.failed(&error.to_string());
                return Err(error);
            }
        };
        let (response, mut failure) = match model.complete(request, None, call.cancel.clone()).await
        {
            Ok(response) => (response, None),
            Err(error) => (error.response, Some(anyhow::anyhow!(error.error))),
        };
        if let Err(error) = self.core.finish_call(
            &self.agent,
            "compaction",
            response.usage.clone(),
            info.cost(&response.usage),
        ) {
            failure = Some(error);
        }
        let summary = response
            .blocks
            .iter()
            .filter(|block| block.kind == model::BLOCK_TEXT)
            .map(|block| block.text.as_ref())
            .collect::<String>();
        if failure.is_none() && summary.is_empty() {
            failure = Some(anyhow::anyhow!("empty summary"));
        }
        if let Some(error) = failure {
            guard.failed(&error.to_string());
            return Err(error);
        }
        let payload = event::CompactionPayload {
            from_seq: cut.from_seq,
            to_seq: cut.to_seq,
            before: cut.before,
            after: cut.after(&summary),
            summary,
            ..Default::default()
        };
        let result = self
            .core
            .event(&self.agent, event::COMPACTION_DONE, payload)
            .and_then(|event| self.core.record(&self.agent, vec![event]));
        if let Err(error) = result {
            guard.failed(&error.to_string());
            return Err(error);
        }
        guard.armed = false;
        self.reads.lock().unwrap().clear();
        self.instructions.clear();
        Ok(())
    }
}
struct CompactionGuard {
    core: Arc<Core>,
    agent: String,
    before: i64,
    armed: bool,
}
impl CompactionGuard {
    fn failed(&mut self, error: &str) {
        if !self.armed {
            return;
        }
        self.armed = false;
        if let Ok(event) = self.core.event(
            &self.agent,
            event::COMPACTION_FAILED,
            event::CompactionPayload {
                before: self.before,
                error: error.into(),
                ..Default::default()
            },
        ) {
            let _ = self.core.fact(vec![event], &CancellationToken::new());
        }
    }
}
impl Drop for CompactionGuard {
    fn drop(&mut self) {
        self.failed("compaction interrupted");
    }
}
