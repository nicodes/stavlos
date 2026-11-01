//! Conversation preparation that preserves cached prefixes and recovery boundaries.
use super::{journal::Core, names, state::ChannelState};
use crate::{event, model};
use anyhow::Result;
use sha2::{Digest as _, Sha256};
use std::collections::{BTreeMap, BTreeSet};
#[derive(Clone, Debug, Default, serde::Serialize, serde::Deserialize)]
pub struct SentNote {
    pub message: i64,
    pub block: i64,
    pub synthetic: bool,
    pub text: String,
}
fn apply_note(history: &mut Vec<model::Message>, note: &SentNote) -> bool {
    let Ok(message) = usize::try_from(note.message) else {
        return false;
    };
    if note.synthetic {
        if message > history.len() {
            return false;
        }
        history.insert(
            message,
            model::Message {
                role: model::ROLE_USER.into(),
                blocks: vec![model::Block::text(note.text.clone())].into(),
            },
        );
        return true;
    }
    let Some(message) = history
        .get_mut(message)
        .filter(|m| m.role == model::ROLE_USER)
    else {
        return false;
    };
    let Ok(block) = usize::try_from(note.block) else {
        return false;
    };
    if block > message.blocks.len() {
        return false;
    }
    let mut blocks = message.blocks.take_vec();
    blocks.insert(block, model::Block::text(note.text.clone()));
    message.blocks = blocks.into();
    true
}
pub fn with_notes(
    history: Vec<model::Message>,
    prior: &[SentNote],
    note: &str,
) -> (Vec<model::Message>, Vec<SentNote>) {
    let mut output = history.clone();
    let mut kept = Vec::with_capacity(prior.len() + 1);
    for note in prior {
        if !apply_note(&mut output, note) {
            output = history.clone();
            kept.clear();
            break;
        }
        kept.push(note.clone());
    }
    let mut next = SentNote {
        message: output.len() as i64 - 1,
        text: note.into(),
        ..Default::default()
    };
    if output.last().is_none_or(|m| m.role != model::ROLE_USER) {
        next.message = output.len() as i64;
        next.synthetic = true;
    } else {
        if note.is_empty() && output.last().is_some_and(|m| !m.blocks.is_empty()) {
            return (output, kept);
        }
        next.block = output.last().map_or(0, |m| m.blocks.len()) as i64;
    }
    if note.is_empty() {
        next.text = "(continue)".into();
    }
    if !apply_note(&mut output, &next) {
        return (history, vec![]);
    }
    kept.push(next);
    (output, kept)
}
#[derive(Default)]
pub struct Reads(BTreeMap<String, ([u8; 32], String)>);
impl Reads {
    pub fn deduplicate(&mut self, call: &model::Block, output: &str) -> String {
        let key = call.input.as_ref().map_or("", |v| v.get());
        let hash: [u8; 32] = Sha256::digest(output.as_bytes()).into();
        if let Some((old, id)) = self.0.get(key)
            && *old == hash
        {
            return format!(
                "[unchanged since your read {id}: its content is still in your context]"
            );
        }
        self.0.insert(key.into(), (hash, call.id.clone()));
        output.into()
    }
    pub fn forget(&mut self, cleared: &[String]) {
        let cleared = cleared.iter().collect::<BTreeSet<_>>();
        self.0.retain(|_, (_, id)| !cleared.contains(id));
    }
    pub fn clear(&mut self) {
        self.0.clear();
    }
}
pub const SUMMARY_INPUT_MAX: i64 = 600000;
pub const SUMMARY_MAX_TOKENS: i64 = 4000;
pub const SUMMARY_TEMPLATE: &str = r#"Write the summary as this Markdown, keeping every heading in this order, and "(none)" under a heading with nothing to say:

## Task
- what the human asked for, and the current status

## Decisions
- what was decided and why, and constraints the human gave

## Work so far
- what is done and verified
- what is in progress, and where it stands
- what is blocked, with the error or unknown

## Files and commands
- paths touched and why they matter; commands run and their outcomes

## Next
- the next concrete step, then the one after it

Rules: terse bullets, not prose. Keep exact paths, identifiers, commands and error text. Do not mention this summary or that the conversation was compacted."#;
pub fn summary_request(
    id: &str,
    transcript: &str,
    previous: &str,
    info: &model::Info,
) -> model::Request {
    let mut system = format!(
        "You summarise an AI coding agent's conversation so another agent can continue the work with less context. Be concrete and complete; omit pleasantries. {SUMMARY_TEMPLATE}"
    );
    let mut text = format!("Summarise this transcript:\n\n{transcript}");
    if !previous.is_empty() {
        system.push_str("\n\nThe transcript follows an earlier summary of everything before it. Merge both: carry forward every still-relevant task, decision and constraint from the earlier summary even when the transcript does not mention it, drop what is finished, and where they disagree the transcript is newer and wins.");
        text = format!(
            "Earlier summary of the conversation before this transcript:\n\n{previous}\n\n---\n\nSummarise this transcript:\n\n{transcript}"
        );
    }
    let max_tokens = if info.capabilities.ignores_max_tokens {
        system.push_str(" Keep the summary under 2,500 words.");
        0
    } else {
        SUMMARY_MAX_TOKENS
    };
    model::Request {
        model: id.into(),
        system,
        messages: vec![model::Message {
            role: model::ROLE_USER.into(),
            blocks: vec![model::Block::text(text)].into(),
        }],
        max_tokens,
        ..Default::default()
    }
}
pub fn usable_window(info: &model::Info) -> i64 {
    let mut reserve = 20000.min(info.context_window / 10);
    if info.max_output > 0 {
        reserve = reserve.min(info.max_output);
    }
    info.context_window - reserve
}
pub fn conversation_tools() -> BTreeSet<String> {
    [
        crate::toolname::AGENT_CREATE,
        crate::toolname::MESSAGE,
        crate::toolname::ASK,
    ]
    .into_iter()
    .map(String::from)
    .collect()
}
pub fn bare_id(full: &str) -> &str {
    model::split(full).map_or(full, |(_, id)| id)
}
pub const LOST_JOB: &str =
    "background job lost in a daemon restart; rerun it if you still need the result";
/// Repair a recovered journal before starting any actor; lost processes are never rerun here.
pub fn recovery_events(core: &Core, state: &ChannelState) -> Result<Vec<event::Event>> {
    let mut events = Vec::new();
    for id in &state.order {
        let agent = &state.agents[id];
        if agent.in_turn {
            events.push(core.event(
                id,
                event::TURN_ABORTED,
                event::TurnPayload { turn: agent.turn },
            )?);
        }
        for ask in &agent.asks {
            events.push(core.event(
                id,
                event::ASK_RESOLVED,
                event::AskResolvedPayload {
                    id: ask.clone(),
                    outcome: event::ASK_WITHDRAWN.into(),
                    ..Default::default()
                },
            )?);
        }
        if agent.compacting {
            events.push(core.event(
                id,
                event::COMPACTION_FAILED,
                event::CompactionPayload {
                    error: "interrupted by a daemon restart".into(),
                    ..Default::default()
                },
            )?);
        }
        if agent.killed || state.archived {
            continue;
        }
        for job in agent.jobs.keys() {
            events.push(core.event(
                id,
                event::JOB_FINISHED,
                event::JobFinishedPayload {
                    id: job.clone(),
                    summary: LOST_JOB.into(),
                    is_error: true,
                    exit_code: -1,
                    ..Default::default()
                },
            )?);
            events.push(core.event(
                id,
                event::INPUT_QUEUED,
                event::Input {
                    id: names::new_id("i")?,
                    kind: event::INPUT_JOB.into(),
                    job: job.clone(),
                    ..Default::default()
                },
            )?);
        }
    }
    Ok(events)
}
