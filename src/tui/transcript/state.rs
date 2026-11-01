use super::*;
use crate::{
    event, model, protocol, textsafe, toolname,
    tui::format,
    wire::{Slice, Time},
};
use serde_json::value::RawValue;
use std::collections::{BTreeMap, BTreeSet};
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord)]
struct Reference {
    item: usize,
    offset: usize,
}
struct Segment {
    kind: Kind,
    text: String,
    tool: String,
}
struct Card {
    item: usize,
    prompt: protocol::PromptInfo,
    resolved: bool,
}
struct Explicit {
    reference: Reference,
    left: usize,
    names: Vec<String>,
    answered: BTreeSet<String>,
}
struct Request {
    names: BTreeSet<String>,
    legacy: bool,
}
#[derive(Default)]
pub struct Transcript {
    items: Vec<Vec<Line>>,
    revisions: Vec<u64>,
    version: u64,
    calls: BTreeMap<String, Reference>,
    prompts: BTreeMap<String, usize>,
    prompt_lines: BTreeMap<String, Reference>,
    jobs: BTreeMap<String, Reference>,
    tool_lines: BTreeMap<String, Vec<Reference>>,
    children: BTreeMap<String, Reference>,
    asks: BTreeMap<String, Vec<Reference>>,
    prompt_kinds: BTreeMap<String, String>,
    inputs: BTreeMap<String, event::Input>,
    call_inputs: BTreeMap<String, Option<Box<RawValue>>>,
    questions: BTreeMap<String, Card>,
    permissions: BTreeMap<String, Card>,
    explicit: BTreeMap<String, Explicit>,
    requests: BTreeMap<String, Request>,
    stream_turn: i64,
    stream: Vec<Segment>,
    in_turn: bool,
    turn_start: Time,
    turn_tokens: i64,
    turn_verb: String,
    compact: Option<usize>,
    turn_gap: bool,
    nudged: bool,
    spawn_task: String,
    spawn_held: Option<event::Event>,
    spawn_parent: String,
    spawn_as: String,
    at: Time,
    self_name: String,
    chat: bool,
    names: BTreeMap<String, String>,
    open: BTreeSet<String>,
}
const TURN_VERBS: [&str; 6] = [
    "Galloping",
    "Trotting",
    "Prancing",
    "Hoofing it",
    "Grazing",
    "Horsing around",
];
impl Transcript {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn chat() -> Self {
        Self {
            chat: true,
            ..Self::default()
        }
    }
    pub fn in_turn(&self) -> bool {
        self.in_turn
    }
    pub fn turn_verb(&self) -> &str {
        if self.in_turn { &self.turn_verb } else { "" }
    }
    pub fn turn_stats(&self, now: Time) -> (i64, i64) {
        if self.in_turn {
            (
                (now.0 - self.turn_start.0)
                    .num_nanoseconds()
                    .unwrap_or(i64::MAX),
                self.turn_tokens,
            )
        } else {
            (0, 0)
        }
    }
    pub fn committed(&self) -> usize {
        self.items.len()
    }
    pub fn item(&self, index: usize) -> &[Line] {
        &self.items[index]
    }
    pub fn revision(&self, index: usize) -> u64 {
        self.revisions[index]
    }
    pub fn compacting(&self) -> bool {
        self.compact.is_some()
    }
    pub fn empty(&self) -> bool {
        self.items.is_empty() && self.stream.is_empty()
    }
    pub fn running(&self) -> bool {
        !self.calls.is_empty() || self.stream.iter().any(|segment| segment.kind == Kind::Tool)
    }
    pub fn streaming(&self) -> bool {
        !self.stream.is_empty()
    }
    pub fn question_item(&self, id: &str) -> Option<usize> {
        self.questions.get(id).map(|card| card.item)
    }
    pub fn permission_item(&self, id: &str) -> Option<usize> {
        self.permissions.get(id).map(|card| card.item)
    }
    pub fn all(&self) -> Vec<Line> {
        let mut lines = self.items.iter().flatten().cloned().collect::<Vec<_>>();
        lines.extend(self.tail());
        lines
    }
    pub fn item_count(&self) -> usize {
        self.tail()
            .last()
            .map(|line| self.items.len().max(line.item as usize + 1))
            .unwrap_or(self.items.len())
    }
    fn valid(&self, reference: Reference) -> bool {
        self.items
            .get(reference.item)
            .is_some_and(|item| reference.offset < item.len())
    }
    fn line(&self, reference: Reference) -> Option<&Line> {
        self.items.get(reference.item)?.get(reference.offset)
    }
    fn touch(&mut self, item: usize) {
        self.version += 1;
        self.revisions[item] = self.version;
    }
    fn set_line(&mut self, reference: Reference, line: Line) {
        if self.valid(reference) {
            self.items[reference.item][reference.offset] = line;
            self.touch(reference.item);
        }
    }
    fn index_tool(&mut self, reference: Reference, line: &Line) {
        if line.kind == Kind::Tool && !line.tool.is_empty() {
            let entries = self.tool_lines.entry(line.tool.clone()).or_default();
            let index = entries.partition_point(|entry| *entry < reference);
            entries.insert(index, reference);
        }
    }
    fn append(&mut self, lines: Vec<Line>) -> Vec<Reference> {
        if lines.is_empty() {
            return vec![];
        }
        let mut runs: Vec<Vec<Line>> = vec![];
        for line in lines {
            if runs
                .last()
                .is_none_or(|run| (run[0].kind == Kind::Think) != (line.kind == Kind::Think))
            {
                runs.push(vec![]);
            }
            runs.last_mut().unwrap().push(line);
        }
        let mut references = vec![];
        for mut run in runs {
            let item = self.items.len();
            if self.turn_gap {
                run[0].turn_start = true;
                self.turn_gap = false;
            }
            let at = if run[0].at.is_zero() {
                self.at
            } else {
                run[0].at
            };
            for (offset, line) in run.iter_mut().enumerate() {
                line.item = item as i64;
                line.at = at;
                let reference = Reference { item, offset };
                self.index_tool(reference, line);
                references.push(reference);
            }
            self.items.push(run);
            self.version += 1;
            self.revisions.push(self.version);
        }
        references
    }
    fn insert(&mut self, item: usize, mut lines: Vec<Line>) -> Vec<Reference> {
        if lines.is_empty() || item >= self.items.len() {
            return vec![];
        }
        let at = self.items[item][0].at;
        let start = self.items[item].len();
        let mut references = vec![];
        for (offset, line) in lines.iter_mut().enumerate() {
            line.item = item as i64;
            line.at = at;
            let reference = Reference {
                item,
                offset: start + offset,
            };
            self.index_tool(reference, line);
            references.push(reference);
        }
        self.items[item].extend(lines);
        self.touch(item);
        references
    }
    fn replace(&mut self, item: usize, mut lines: Vec<Line>) {
        if item >= self.items.len() || lines.is_empty() {
            return;
        }
        let at = self.items[item][0].at;
        for line in &mut lines {
            line.item = item as i64;
            line.at = at;
        }
        self.items[item] = lines;
        self.touch(item);
    }
    fn find(
        &self,
        references: &[Reference],
        predicate: impl Fn(&Line) -> bool,
    ) -> Option<Reference> {
        references
            .iter()
            .copied()
            .find(|reference| self.line(*reference).is_some_and(&predicate))
    }
    fn untied(&self, tool: &str, tied: &BTreeMap<String, Reference>) -> Option<Reference> {
        self.tool_lines
            .get(tool)?
            .iter()
            .rev()
            .copied()
            .find(|reference| {
                !tied.values().any(|other| other == reference)
                    && self
                        .line(*reference)
                        .is_some_and(|line| line.kind == Kind::Tool && line.tool == tool)
            })
    }
    fn call_item(&self, id: &str, tool: &str) -> Option<usize> {
        self.calls
            .get(id)
            .copied()
            .filter(|reference| self.valid(*reference))
            .or_else(|| {
                self.calls
                    .values()
                    .copied()
                    .filter(|reference| {
                        self.line(*reference)
                            .is_some_and(|line| line.tool.eq_ignore_ascii_case(tool))
                    })
                    .max()
            })
            .map(|reference| reference.item)
    }
    pub fn notice(&mut self, body: &[String], now: Time) {
        let lines = body
            .iter()
            .enumerate()
            .map(|(index, text)| Line {
                kind: Kind::Notice,
                text: text.clone(),
                at: now,
                glyph: if index == 0 {
                    glyph::NOTICE.into()
                } else {
                    String::new()
                },
                ..Default::default()
            })
            .collect();
        self.append(lines);
    }
    pub fn apply(&mut self, event: &event::Event) {
        if event.time != Time::default().0 {
            self.at = Time(event.time);
        }
        if self.apply_permission(event) || self.apply_question(event) {
            return;
        }
        if self.chat {
            self.apply_chat(event);
            return;
        }
        let reminder = event.kind == event::INPUT_QUEUED
            && event
                .decode::<event::Input>()
                .is_ok_and(|input| input.kind == event::INPUT_REMINDER);
        if reminder {
            self.turn_gap = true;
        }
        if self.apply_compaction(event) || self.hold_spawn(event) {
            return;
        }
        match event.kind.as_str() {
            event::ASSISTANT_MESSAGE => {
                if let Ok(payload) = event.decode::<event::AssistantMessagePayload>() {
                    for block in payload
                        .blocks
                        .iter()
                        .filter(|block| block.kind == model::BLOCK_TOOL_USE)
                    {
                        self.call_inputs
                            .insert(block.id.clone(), block.input.clone());
                    }
                }
            }
            event::AGENT_UPDATED => {
                if let Ok(payload) = event.decode::<event::AgentUpdatedPayload>()
                    && let Some(name) = payload.name
                {
                    self.self_name = name;
                }
            }
            event::INPUT_QUEUED => {
                if let Ok(input) = event.decode::<event::Input>()
                    && !input.id.is_empty()
                    && !event::input_rule(&input.kind).harness
                {
                    self.inputs.insert(input.id.clone(), input);
                }
            }
            event::INPUT_TAKEN => {
                self.apply_taken(event);
                return;
            }
            event::TOOL_STARTED | event::TOOL_FINISHED => {
                if self.apply_tool(event) {
                    return;
                }
            }
            event::ASK_REQUESTED | event::ASK_RESOLVED => {
                if self.apply_prompt(event) {
                    return;
                }
            }
            event::JOB_STARTED | event::JOB_FINISHED | event::JOB_STOPPED => {
                if self.apply_job(event) {
                    return;
                }
            }
            _ => {}
        }
        let mut lines = clean(event_lines(event));
        self.answer_glyph(event, &mut lines);
        self.append(lines);
        self.after_append(event);
        if reminder {
            self.nudged = true;
        } else if event.kind == event::TURN_STARTED {
            self.turn_gap |= !self.nudged;
            self.nudged = false;
        }
    }
    fn after_append(&mut self, event: &event::Event) {
        match event.kind.as_str() {
            event::TURN_STARTED => {
                self.in_turn = true;
                self.turn_start = Time(event.time);
                self.turn_tokens = 0;
                let turn = event
                    .decode::<event::TurnPayload>()
                    .unwrap_or_default()
                    .turn;
                self.turn_verb =
                    TURN_VERBS[(turn - 1).rem_euclid(TURN_VERBS.len() as i64) as usize].into();
            }
            event::ASSISTANT_MESSAGE => {
                self.stream.clear();
                if let Ok(payload) = event.decode::<event::AssistantMessagePayload>() {
                    self.turn_tokens += payload.usage.input_tokens + payload.usage.output_tokens;
                }
            }
            event::TURN_ENDED | event::TURN_ABORTED | event::AGENT_KILLED => {
                self.stream.clear();
                self.in_turn = false;
                for item in 0..self.items.len() {
                    for offset in 0..self.items[item].len() {
                        if self.items[item][offset].running {
                            self.items[item][offset].running = false;
                            self.touch(item);
                        }
                    }
                }
                self.calls.clear();
            }
            _ => {}
        }
    }
    fn apply_taken(&mut self, event: &event::Event) {
        let Ok(payload) = event.decode::<event::InputTakenPayload>() else {
            return;
        };
        for id in payload.ids.iter() {
            let Some(input) = self.inputs.remove(id) else {
                continue;
            };
            let lines = if *id == self.spawn_task {
                self.spawn_task.clear();
                self.spawn_lines(&input)
            } else {
                input_lines(&input, &self.self_name)
            };
            self.append(clean(lines));
            if input.kind == event::INPUT_RESPONSE {
                if input.reply_to.is_empty() {
                    self.answered(&input.from_name);
                } else {
                    self.answered_requests(&input.from, &input.reply_to);
                }
            }
        }
    }
    fn hold_spawn(&mut self, event: &event::Event) -> bool {
        if event.kind == event::AGENT_SPAWNED {
            let Ok(payload) = event.decode::<event::AgentSpawnedPayload>() else {
                return false;
            };
            self.self_name = payload.name.clone();
            if payload.parent.is_empty() {
                return false;
            }
            self.spawn_held = Some(event.clone());
            self.spawn_parent = payload.parent;
            self.spawn_as = format::who(&payload.name, &payload.role);
            if !payload.model.is_empty() {
                self.spawn_as.push_str(&format!(" · {}", payload.model));
            }
            return true;
        }
        if self.spawn_parent.is_empty() {
            return false;
        }
        let parent = std::mem::take(&mut self.spawn_parent);
        if event.kind == event::INPUT_QUEUED
            && let Ok(input) = event.decode::<event::Input>()
            && input.kind == event::INPUT_REQUEST
            && input.from == parent
        {
            self.spawn_task = input.id.clone();
            self.inputs.insert(input.id.clone(), input);
            return true;
        }
        if let Some(event) = self.spawn_held.clone() {
            self.append(clean(event_lines(&event)));
        }
        false
    }
    fn spawn_lines(&self, input: &event::Input) -> Vec<Line> {
        let mut head = Line {
            kind: Kind::Text,
            text: titled(
                &format!("@{}", input.from_name),
                &format!("as {}", self.spawn_as),
            ),
            block: Block::Child,
            glyph: glyph::SPAWN.into(),
            who: input.from_name.clone(),
            ..Default::default()
        };
        if !self.self_name.is_empty() {
            let recipients = if input.to.is_empty() {
                vec![self.self_name.clone()]
            } else {
                input.to.to_vec()
            };
            let (address, names) = message_address(&input.from_name, &recipients, &self.self_name);
            head.text = format!(
                "{address} {}",
                self.spawn_as
                    .strip_prefix(&format!("{} ", self.self_name))
                    .unwrap_or(&self.spawn_as)
            );
            head.names = names.into();
        }
        let mut lines = vec![Line::new(Kind::Blank, ""), head];
        lines.extend(
            input
                .text
                .trim_end_matches('\n')
                .split('\n')
                .map(|text| Line {
                    kind: Kind::Text,
                    text: text.into(),
                    block: Block::Child,
                    indent: 1,
                    ..Default::default()
                }),
        );
        lines.push(Line::new(Kind::Blank, ""));
        lines
    }
    fn answer_glyph(&self, event: &event::Event, lines: &mut [Line]) {
        if event.kind == event::ASK_RESOLVED
            && let Ok(payload) = event.decode::<event::AskResolvedPayload>()
            && self
                .prompt_kinds
                .get(&payload.id)
                .is_none_or(|kind| kind != protocol::PROMPT_QUESTION)
        {
            for line in lines {
                if line.glyph == glyph::ANSWER {
                    line.glyph = glyph::PERMISSION.into();
                }
            }
        }
    }
    fn apply_compaction(&mut self, event: &event::Event) -> bool {
        match event.kind.as_str() {
            event::COMPACTION_STARTED => {
                let references = self.append(clean(event_lines(event)));
                self.compact = references.first().map(|reference| reference.item);
                return true;
            }
            event::COMPACTION_DONE | event::COMPACTION_FAILED => {
                if let Some(item) = self.compact.take() {
                    self.replace(item, clean(event_lines(event)));
                    return true;
                }
            }
            event::TURN_ENDED | event::TURN_ABORTED => {
                if let Some(item) = self.compact.take() {
                    self.replace(
                        item,
                        vec![
                            Line::new(Kind::Blank, ""),
                            Line::new(Kind::Dim, titled("Compaction interrupted", "")),
                            Line::new(Kind::Blank, ""),
                        ],
                    );
                }
            }
            _ => {}
        }
        false
    }
    fn apply_tool(&mut self, event: &event::Event) -> bool {
        if event.kind == event::TOOL_STARTED {
            let Ok(payload) = event.decode::<event::ToolStartedPayload>() else {
                return false;
            };
            if payload.call_id.is_empty() {
                return false;
            }
            let input = self.call_inputs.remove(&payload.call_id).flatten();
            let raw = input.as_ref().map(|raw| raw.get()).unwrap_or("");
            let mut lines = tool_started_lines(&payload.name, &payload.call_id, raw);
            if !lines[0].who.is_empty() {
                lines[0].names = if payload.name == toolname::MESSAGE {
                    message_recipients(raw).into()
                } else {
                    vec![lines[0].who.clone()].into()
                };
            }
            let references = self.append(clean(lines));
            if let Some(reference) = self.find(&references, |line| line.kind == Kind::Tool) {
                self.calls.insert(payload.call_id, reference);
            }
            return true;
        }
        let Ok(mut payload) = event.decode::<event::ToolFinishedPayload>() else {
            self.stream.clear();
            return false;
        };
        let reference = self
            .calls
            .get(&payload.call_id)
            .copied()
            .filter(|reference| self.valid(*reference));
        payload.name = reference
            .and_then(|reference| self.line(reference))
            .map(|line| line.tool.clone())
            .unwrap_or_else(|| toolname::canonical(&payload.name).into());
        self.finish_call(&payload);
        self.stream.clear();
        if let Some(reference) = reference {
            let mut event = event.clone();
            event.payload = serde_json::value::to_raw_value(&payload).ok();
            self.insert(reference.item, clean(event_lines(&event)));
            return true;
        }
        false
    }
    fn apply_prompt(&mut self, event: &event::Event) -> bool {
        if event.kind == event::ASK_REQUESTED {
            let Ok(payload) = event.decode::<event::AskRequestedPayload>() else {
                return false;
            };
            self.prompt_kinds
                .insert(payload.id.clone(), payload.kind.clone());
            let lines = clean(event_lines(event));
            let references = if let Some(item) = self
                .call_item(&payload.call_id, &payload.tool)
                .filter(|_| payload.kind == protocol::PROMPT_PERMISSION)
            {
                self.prompts.insert(payload.id.clone(), item);
                self.insert(item, nested(lines))
            } else {
                self.append(lines)
            };
            if let Some(reference) = self.find(&references, |line| {
                matches!(line.glyph.as_str(), glyph::PROMPT | glyph::PERMISSION)
            }) {
                self.prompt_lines.insert(payload.id, reference);
            }
            return true;
        }
        let Ok(payload) = event.decode::<event::AskResolvedPayload>() else {
            return false;
        };
        if let Some(reference) = self.prompt_lines.remove(&payload.id)
            && let Some(mut line) = self.line(reference).cloned()
        {
            line.tone = if payload.outcome == event::ASK_ANSWERED {
                Tone::None
            } else {
                Tone::Error
            };
            self.set_line(reference, line);
        }
        let Some(item) = self.prompts.remove(&payload.id) else {
            return false;
        };
        let mut lines = clean(event_lines(event));
        self.answer_glyph(event, &mut lines);
        self.insert(item, nested(lines));
        true
    }
    fn apply_job(&mut self, event: &event::Event) -> bool {
        match event.kind.as_str() {
            event::JOB_STARTED => {
                let Ok(payload) = event.decode::<event::JobStartedPayload>() else {
                    return false;
                };
                if payload.id.is_empty() {
                    return false;
                }
                if let Some(reference) = self.untied(toolname::SHELL, &self.jobs) {
                    self.jobs.insert(payload.id, reference);
                    if let Some(mut line) = self.line(reference).cloned() {
                        line.tone = Tone::Working;
                        self.set_line(reference, line);
                    }
                    self.stream.clear();
                    return true;
                }
                let references = self.append(clean(event_lines(event)));
                if let Some(reference) = self.find(&references, |line| line.kind != Kind::Blank) {
                    self.jobs.insert(payload.id, reference);
                }
                true
            }
            event::JOB_FINISHED => {
                let Ok(payload) = event.decode::<event::JobFinishedPayload>() else {
                    return false;
                };
                self.settle_job(
                    &payload.id,
                    if payload.is_error {
                        Tone::Error
                    } else {
                        Tone::None
                    },
                    clean(job_finished_lines(&payload)),
                );
                true
            }
            event::JOB_STOPPED => {
                let Ok(payload) = event.decode::<event::JobStoppedPayload>() else {
                    return false;
                };
                self.settle_job(
                    &payload.id,
                    Tone::Error,
                    clean(job_stopped_lines(&payload.reason)),
                );
                true
            }
            _ => false,
        }
    }
    fn settle_job(&mut self, id: &str, tone: Tone, mut lines: Vec<Line>) {
        if let Some(reference) = self.jobs.remove(id)
            && let Some(mut line) = self.line(reference).cloned()
        {
            line.tone = tone;
            if line.kind == Kind::Tool {
                lines = nested(lines);
            }
            self.set_line(reference, line);
            self.insert(reference.item, lines);
        } else {
            self.append(lines);
        }
    }
}
impl Transcript {
    fn card_from(&self, agent: &str, from: &str) -> String {
        if !from.is_empty() {
            from.into()
        } else if self.chat || self.self_name.is_empty() {
            self.agent_name(agent)
        } else {
            self.self_name.clone()
        }
    }
    fn question_lines(
        &self,
        prompt: &protocol::PromptInfo,
        result: Option<&event::AskResolvedPayload>,
    ) -> Vec<Line> {
        let lines = question_lines(prompt, result, self.chat);
        if self.chat { lines } else { linked(lines, "") }
    }
    pub fn ensure_question(&mut self, mut prompt: protocol::PromptInfo) {
        if let Some(old) = self.questions.get(&prompt.id) {
            if old.resolved {
                return;
            }
            if prompt.questions.is_empty() {
                prompt.questions = old.prompt.questions.clone();
            }
            if prompt.from.is_empty() {
                prompt.from = old.prompt.from.clone();
            }
            if prompt.role.is_empty() {
                prompt.role = old.prompt.role.clone();
            }
            if same_prompt(&old.prompt, &prompt) {
                return;
            }
            let item = old.item;
            let lines = self.question_lines(&prompt, None);
            self.questions.insert(
                prompt.id.clone(),
                Card {
                    item,
                    prompt,
                    resolved: false,
                },
            );
            self.replace(item, lines);
            return;
        }
        let lines = stamped(self.question_lines(&prompt, None), &prompt.created);
        self.questions.insert(
            prompt.id.clone(),
            Card {
                item: self.items.len(),
                prompt,
                resolved: false,
            },
        );
        self.append(lines);
    }
    pub fn ensure_permission(&mut self, mut prompt: protocol::PromptInfo) {
        if let Some(old) = self.permissions.get(&prompt.id) {
            if prompt.input.is_none() {
                prompt.input = old.prompt.input.clone();
            }
            if prompt.dir.is_empty() {
                prompt.dir = old.prompt.dir.clone();
            }
            if prompt.prefix.is_empty() {
                prompt.prefix = old.prompt.prefix.clone();
            }
            if prompt.from.is_empty() {
                prompt.from = old.prompt.from.clone();
            }
            if old.resolved || same_prompt(&old.prompt, &prompt) {
                return;
            }
            let item = old.item;
            let lines = permission_lines(&prompt, None, self.chat);
            self.permissions.insert(
                prompt.id.clone(),
                Card {
                    item,
                    prompt,
                    resolved: false,
                },
            );
            self.replace(item, lines);
            return;
        }
        let lines = stamped(permission_lines(&prompt, None, self.chat), &prompt.created);
        self.permissions.insert(
            prompt.id.clone(),
            Card {
                item: self.items.len(),
                prompt,
                resolved: false,
            },
        );
        self.append(lines);
    }
    fn apply_question(&mut self, event: &event::Event) -> bool {
        match event.kind.as_str() {
            event::ASK_REQUESTED => {
                let Ok(payload) = event.decode::<event::AskRequestedPayload>() else {
                    return false;
                };
                if payload.kind != protocol::PROMPT_QUESTION {
                    return false;
                }
                let from = self.card_from(&event.agent, &payload.from);
                self.ensure_question(protocol::PromptInfo {
                    id: payload.id,
                    agent: event.agent.clone(),
                    from,
                    role: payload.role,
                    kind: protocol::PROMPT_QUESTION.into(),
                    question: payload.question,
                    questions: payload.questions,
                    question_number: payload.question_number,
                    question_total: payload.question_total,
                    ..Default::default()
                });
                true
            }
            event::ASK_RESOLVED => {
                let Ok(payload) = event.decode::<event::AskResolvedPayload>() else {
                    return false;
                };
                let Some(card) = self.questions.get_mut(&payload.id) else {
                    return false;
                };
                card.resolved = true;
                let item = card.item;
                let prompt = card.prompt.clone();
                let lines = self.question_lines(&prompt, Some(&payload));
                self.replace(item, lines);
                true
            }
            _ => false,
        }
    }
    fn apply_permission(&mut self, event: &event::Event) -> bool {
        match event.kind.as_str() {
            event::ASK_REQUESTED => {
                let Ok(payload) = event.decode::<event::AskRequestedPayload>() else {
                    return false;
                };
                if payload.kind != protocol::PROMPT_PERMISSION {
                    return false;
                }
                let from = self.card_from(&event.agent, &payload.from);
                self.ensure_permission(protocol::PromptInfo {
                    id: payload.id,
                    channel: event.channel.clone(),
                    agent: event.agent.clone(),
                    from,
                    role: payload.role,
                    kind: protocol::PROMPT_PERMISSION.into(),
                    tool: payload.tool,
                    input: payload.input,
                    dir: payload.dir,
                    prefix: payload.prefix,
                    question: payload.question,
                    ..Default::default()
                });
                true
            }
            event::ASK_RESOLVED => {
                let Ok(payload) = event.decode::<event::AskResolvedPayload>() else {
                    return false;
                };
                let Some(card) = self.permissions.get_mut(&payload.id) else {
                    return false;
                };
                card.resolved = true;
                let item = card.item;
                let prompt = card.prompt.clone();
                self.replace(item, permission_lines(&prompt, Some(&payload), self.chat));
                true
            }
            _ => false,
        }
    }
    pub fn child_spawned(&mut self, id: &str) {
        if let Some(reference) = self.untied(toolname::AGENT_CREATE, &self.children) {
            if let Some(mut line) = self.line(reference).cloned() {
                line.tone = Tone::Working;
                self.set_line(reference, line);
            }
            self.children.insert(id.into(), reference);
        }
    }
    pub fn child_state(&mut self, id: &str, state: &str) {
        if let Some(reference) = self.children.get(id).copied()
            && let Some(mut line) = self.line(reference).cloned()
        {
            line.tone = if protocol::agent_busy(state) {
                Tone::Working
            } else if state == protocol::AGENT_KILLED {
                self.children.remove(id);
                Tone::Error
            } else {
                Tone::None
            };
            self.set_line(reference, line);
        }
    }
    fn finish_call(&mut self, payload: &event::ToolFinishedPayload) {
        let Some(reference) = self.calls.remove(&payload.call_id) else {
            return;
        };
        let Some(mut line) = self.line(reference).cloned() else {
            return;
        };
        line.running = false;
        line.error = payload.is_error;
        if payload.cancelled {
            line.suffix = "(cancelled)".into();
        } else if payload.denied {
            line.glyph = glyph::FAILED.into();
            let mark = denial_mark(&payload.output);
            if !mark.is_empty() {
                if is_prompt_call(&line) {
                    line.suffix = mark;
                } else if let Some((title, arg)) = line.text.split_once("  ") {
                    line.text = format!("{title} {mark}  {arg}");
                } else {
                    line.text.push(' ');
                    line.text.push_str(&mark);
                }
            }
        }
        if payload.name == toolname::AGENT_CREATE
            && !payload.is_error
            && let Some(name) = payload
                .output
                .strip_prefix("created ")
                .and_then(|rest| rest.split_once(" ("))
                .map(|(name, _)| name)
            && name != line.who
            && line.text.starts_with(&format!("@{}", line.who))
        {
            line.text = format!("@{name}{}", &line.text[line.who.len() + 1..]);
            for value in line.names.iter_mut() {
                if *value == line.who {
                    *value = name.into();
                }
            }
            line.who = name.into();
        }
        let recipients = delivered_recipients(payload);
        if !recipients.is_empty() {
            let prefix = addressed(&line.names, "");
            if !prefix.is_empty() && line.text.starts_with(&prefix) {
                line.text = addressed(&recipients, "") + line.text.strip_prefix(&prefix).unwrap();
            }
            line.names = recipients.clone().into();
            line.who = recipients[0].clone();
            if payload.output.starts_with(MESSAGE_DELIVERED) {
                let left = recipients
                    .iter()
                    .filter(|name| name.as_str() != toolname::USER)
                    .count();
                if let Some(id) = message_request_id(&payload.output) {
                    if left > 0 {
                        line.tone = Tone::Working;
                        self.explicit.insert(
                            id.into(),
                            Explicit {
                                reference,
                                left,
                                names: recipients,
                                answered: BTreeSet::new(),
                            },
                        );
                    }
                } else {
                    for name in recipients {
                        if name != toolname::USER {
                            line.tone = Tone::Working;
                            self.asks.entry(name).or_default().push(reference);
                        }
                    }
                }
            }
        }
        self.set_line(reference, line);
    }
    fn answered(&mut self, from: &str) {
        if let Some(references) = self.asks.remove(from) {
            for reference in references {
                if !self.asks.values().any(|others| others.contains(&reference))
                    && let Some(mut line) = self.line(reference).cloned()
                    && line.tone != Tone::Error
                {
                    line.tone = Tone::None;
                    self.set_line(reference, line);
                }
            }
        }
    }
    fn answered_requests(&mut self, from: &str, ids: &[String]) {
        for id in ids {
            let Some(wait) = self.explicit.get_mut(id) else {
                continue;
            };
            if !wait.answered.insert(from.into()) {
                continue;
            }
            wait.left = wait.left.saturating_sub(1);
            if wait.left == 0 {
                let reference = wait.reference;
                if let Some(mut line) = self.line(reference).cloned()
                    && line.tone != Tone::Error
                {
                    line.tone = Tone::None;
                    self.set_line(reference, line);
                }
                self.explicit.remove(id);
            }
        }
    }
    pub fn asker_gone(&mut self, name: &str) {
        let references = self
            .asks
            .remove(name)
            .unwrap_or_default()
            .into_iter()
            .chain(
                self.explicit
                    .values()
                    .filter(|wait| wait.names.iter().any(|other| other == name))
                    .map(|wait| wait.reference),
            )
            .collect::<Vec<_>>();
        for reference in references {
            if let Some(mut line) = self.line(reference).cloned() {
                line.tone = Tone::Error;
                self.set_line(reference, line);
            }
        }
    }
    pub fn apply_stream(&mut self, notification: &protocol::StreamNotification) {
        let text = textsafe::clean(&notification.text);
        let thinking = textsafe::clean(&notification.thinking);
        if notification.turn != self.stream_turn || notification.reset {
            self.stream_turn = notification.turn;
            self.stream.clear();
        }
        if notification.reset {
            return;
        }
        let (kind, value, tool) = if !notification.tool_name.is_empty() && !text.is_empty() {
            (Kind::ToolOut, text, String::new())
        } else if !text.is_empty() {
            (Kind::Stream, text, String::new())
        } else if !thinking.is_empty() {
            return;
        } else if !notification.tool_name.is_empty() {
            (
                Kind::Tool,
                tool_title(&notification.tool_name),
                notification.tool_name.clone(),
            )
        } else {
            return;
        };
        if matches!(kind, Kind::ToolOut | Kind::Stream)
            && let Some(last) = self
                .stream
                .last_mut()
                .filter(|segment| segment.kind == kind)
        {
            last.text.push_str(&value);
        } else {
            self.stream.push(Segment {
                kind,
                text: value,
                tool,
            });
        }
    }
    pub fn tail(&self) -> Vec<Line> {
        if self.stream.is_empty() {
            return vec![];
        }
        let next = self.items.len();
        let item = if next > 0
            && self.items[next - 1]
                .last()
                .is_some_and(|line| line.kind == Kind::Tool && line.running)
        {
            next - 1
        } else {
            next
        };
        let mut lines = vec![];
        for segment in &self.stream {
            match segment.kind {
                Kind::Stream => {
                    for (index, text) in segment.text.trim_end_matches('\n').split('\n').enumerate()
                    {
                        lines.push(Line {
                            kind: Kind::Stream,
                            text: format!("{ASIDE_TITLE} {text}"),
                            glyph: if index == 0 {
                                glyph::ASIDE.into()
                            } else {
                                String::new()
                            },
                            indent: if index == 0 { 0 } else { 1 },
                            ..Default::default()
                        });
                    }
                }
                Kind::ToolOut => lines.extend(output_lines(segment.text.trim_end_matches('\n'))),
                Kind::Tool => lines.push(Line {
                    kind: Kind::Tool,
                    text: segment.text.clone(),
                    running: true,
                    tool: segment.tool.clone(),
                    ..Default::default()
                }),
                kind => lines.push(Line::new(kind, segment.text.clone())),
            }
        }
        let mut current = item;
        for index in 0..lines.len() {
            if index > 0
                && (lines[index].kind == Kind::Think) != (lines[index - 1].kind == Kind::Think)
                && !(current == item && item != next)
            {
                current += 1;
            }
            lines[index].item = current as i64;
        }
        lines
    }
    fn agent_name(&self, id: &str) -> String {
        self.names
            .get(id)
            .filter(|name| !name.is_empty())
            .cloned()
            .unwrap_or_else(|| format::short_id(id))
    }
    fn apply_chat(&mut self, event: &event::Event) {
        match event.kind.as_str() {
            event::CHANNEL_UPDATED => {
                if let Ok(payload) = event.decode::<event::ChannelUpdatedPayload>()
                    && let Some(dir) = payload.dir
                {
                    self.append(clean(vec![Line::new(
                        Kind::Dim,
                        format!(
                            "Default directory → {dir} · mode ask · remembered approvals cleared"
                        ),
                    )]));
                }
            }
            event::AGENT_SPAWNED => {
                if let Ok(payload) = event.decode::<event::AgentSpawnedPayload>()
                    && !payload.id.is_empty()
                {
                    self.names.insert(payload.id, payload.name);
                }
            }
            event::AGENT_UPDATED => {
                if let Ok(payload) = event.decode::<event::AgentUpdatedPayload>()
                    && let Some(name) = payload.name
                {
                    let old = self.names.get(&event.agent).cloned().unwrap_or_default();
                    for request in self.requests.values_mut() {
                        if request.names.remove(&old) {
                            request.names.insert(name.clone());
                        }
                    }
                    self.names.insert(event.agent.clone(), name);
                }
            }
            event::AGENT_KILLED => {
                let name = self.names.get(&event.agent).cloned().unwrap_or_default();
                self.open.remove(&name);
                for id in self.requests.keys().cloned().collect::<Vec<_>>() {
                    self.settle_post(&id, &name);
                }
            }
            event::CHAT_POSTED => {
                if let Ok(payload) = event.decode::<event::ChatPayload>() {
                    let recipients = if payload.to.len() == 1 && payload.to[0] == "main" {
                        vec![]
                    } else {
                        payload.to.to_vec()
                    };
                    let mut lines = clean(block(
                        Block::User,
                        "",
                        &addressed(&recipients, &payload.text),
                        "",
                    ));
                    if let Some(line) = lines.iter_mut().find(|line| line.lead) {
                        line.who = toolname::USER.into();
                        line.names = nonempty(recipients);
                    }
                    self.append(lines);
                    self.requests.insert(
                        payload.id,
                        Request {
                            names: payload.to.into_vec().into_iter().collect(),
                            legacy: payload.kind.is_empty(),
                        },
                    );
                }
            }
            event::CHAT_MESSAGE => {
                if let Ok(payload) = event.decode::<event::ChatPayload>() {
                    self.reply(&event.agent, payload);
                }
            }
            _ => {}
        }
    }
    fn reply(&mut self, agent: &str, payload: event::ChatPayload) {
        let from = if payload.from.is_empty() {
            self.agent_name(agent)
        } else {
            payload.from.clone()
        };
        let mut lines = markdown_lines(payload.text.trim_end_matches('\n'));
        let (address, names) = message_address(&from, &payload.to, toolname::USER);
        if lines
            .first()
            .is_some_and(|line| matches!(line.kind, Kind::Text | Kind::Heading))
        {
            lines[0].text = format!("{address} {}", lines[0].text);
        } else {
            lines.insert(0, Line::new(Kind::Text, address));
        }
        lines[0].glyph = if payload.kind == "info" {
            glyph::INFO
        } else {
            glyph::REPLY
        }
        .into();
        lines[0].who = from.clone();
        lines[0].names = nonempty(names);
        for (index, line) in lines.iter_mut().enumerate() {
            line.note = true;
            if index > 0 {
                line.indent = 1;
            }
        }
        let mut visible = vec![Line::new(Kind::Blank, "")];
        visible.extend(collapsed(lines));
        visible.push(Line::new(Kind::Blank, ""));
        self.append(linked(clean(visible), agent));
        if payload.kind.is_empty() {
            self.open.remove(&from);
            for id in self
                .requests
                .iter()
                .filter(|(_, request)| request.legacy)
                .map(|(id, _)| id.clone())
                .collect::<Vec<_>>()
            {
                self.settle_post(&id, &from);
            }
        } else if payload.kind == "response" {
            for id in payload.reply_to.iter().chain(payload.posts.iter()) {
                self.settle_post(id, &from);
            }
            if !payload.post.is_empty() {
                self.settle_post(&payload.post, &from);
            }
        }
    }
    fn settle_post(&mut self, id: &str, from: &str) {
        if let Some(request) = self.requests.get_mut(id) {
            request.names.remove(from);
            if request.names.is_empty() {
                self.requests.remove(id);
            }
        }
    }
    pub fn waiting(&self) -> Vec<String> {
        self.open
            .iter()
            .chain(
                self.requests
                    .values()
                    .flat_map(|request| request.names.iter()),
            )
            .cloned()
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect()
    }
}
const MESSAGE_DELIVERED: &str = "request delivered to ";
pub fn message_request_id(output: &str) -> Option<&str> {
    output
        .split_once("; request_id: ")?
        .1
        .split_whitespace()
        .next()
}
pub fn delivered_recipients(payload: &event::ToolFinishedPayload) -> Vec<String> {
    if payload.name != toolname::MESSAGE || payload.is_error || payload.cancelled || payload.denied
    {
        return vec![];
    }
    if payload.output == "message delivered to the user" {
        return vec![toolname::USER.into()];
    }
    for prefix in [
        MESSAGE_DELIVERED,
        "response delivered to ",
        "info delivered to ",
    ] {
        if let Some(rest) = payload.output.strip_prefix(prefix) {
            let list = rest.split(';').next().unwrap();
            return if list.is_empty() {
                vec![]
            } else {
                list.split(", ").map(str::to_owned).collect()
            };
        }
    }
    vec![]
}
fn same_prompt(left: &protocol::PromptInfo, right: &protocol::PromptInfo) -> bool {
    serde_json::to_string(left).ok() == serde_json::to_string(right).ok()
}
fn nonempty(names: Vec<String>) -> Slice<String> {
    if names.is_empty() {
        Slice::default()
    } else {
        names.into()
    }
}
pub fn chat_event(kind: &str) -> bool {
    matches!(
        kind,
        event::AGENT_SPAWNED
            | event::AGENT_UPDATED
            | event::CHAT_POSTED
            | event::CHAT_MESSAGE
            | event::AGENT_KILLED
            | event::CHANNEL_UPDATED
            | event::ASK_REQUESTED
            | event::ASK_RESOLVED
    )
}
pub fn item_agent(lines: &[Line], item: i64) -> &str {
    lines
        .iter()
        .find(|line| line.item == item && !line.agent.is_empty())
        .map(|line| line.agent.as_str())
        .unwrap_or("")
}
pub fn item_is_input(lines: &[Line], item: i64) -> bool {
    lines
        .iter()
        .any(|line| line.item == item && matches!(line.block, Block::User | Block::Steer))
}
pub fn item_folds(lines: &[Line], item: i64) -> bool {
    lines
        .iter()
        .any(|line| line.item == item && line.vis != Visibility::Always)
}
pub fn item_range(lines: &[Line], item: i64) -> Option<(usize, usize)> {
    Some((
        lines.iter().position(|line| line.item == item)?,
        lines.iter().rposition(|line| line.item == item)?,
    ))
}
pub fn collapsed(mut lines: Vec<Line>) -> Vec<Line> {
    if lines.len() > OUTPUT_COLLAPSED {
        for line in lines.iter_mut().skip(OUTPUT_COLLAPSED) {
            line.vis = Visibility::Expanded;
        }
        lines.push(Line {
            kind: Kind::Dim,
            text: format!("… +{} lines", lines.len() - OUTPUT_COLLAPSED),
            vis: Visibility::Collapsed,
            indent: 1,
            ..Default::default()
        });
    }
    lines
}
pub fn input_lines(input: &event::Input, to: &str) -> Vec<Line> {
    let recipients = if input.to.is_empty() && !to.is_empty() {
        vec![to.into()]
    } else {
        input.to.to_vec()
    };
    match input.kind.as_str() {
        event::INPUT_PROMPT | event::INPUT_STEER => {
            let (address, names) = message_address(toolname::USER, &recipients, to);
            let mut lines = block(
                Block::User,
                "",
                format!("{address} {}", input.text).trim(),
                "",
            );
            if let Some(line) = lines.iter_mut().find(|line| line.lead) {
                line.who = toolname::USER.into();
                line.names = names.into();
            }
            lines
        }
        event::INPUT_REQUEST | event::INPUT_RESPONSE => {
            received(&input.from_name, &recipients, to, &input.text, glyph::ASK)
        }
        event::INPUT_INFO | event::INPUT_AGENT_STEER => {
            received(&input.from_name, &recipients, to, &input.text, glyph::INFO)
        }
        _ => vec![],
    }
}
fn nested(mut lines: Vec<Line>) -> Vec<Line> {
    for line in &mut lines {
        if matches!(line.kind, Kind::Dim | Kind::Notice) {
            line.kind = Kind::ToolNote;
        }
    }
    lines
}
