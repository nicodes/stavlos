//! Model-visible conversation projected from the durable event journal.
use crate::{
    event::{self, Event, Kind},
    model::{self, Block, Message, ToolDef},
};
use std::collections::{BTreeMap, BTreeSet};
#[derive(Clone, Debug)]
struct Entry {
    seq: i64,
    message: Message,
}
#[derive(Clone, Debug)]
struct Boundary {
    seq: i64,
    count: usize,
}
#[derive(Default)]
pub struct Builder {
    messages: Vec<Entry>,
    boundaries: Vec<Boundary>,
    inputs: BTreeMap<String, event::Input>,
    jobs: BTreeMap<String, event::JobFinishedPayload>,
    open: Vec<String>,
    normalized: Option<Vec<Message>>,
    summary: String,
    prefix: usize,
}
impl Builder {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn apply(&mut self, event: &Event) {
        match Kind::parse(&event.kind) {
            Kind::InputQueued => {
                if let Ok(input) = event.decode::<event::Input>()
                    && !input.id.is_empty()
                {
                    self.inputs.insert(input.id.clone(), input);
                }
            }
            Kind::JobFinished => {
                if let Ok(job) = event.decode::<event::JobFinishedPayload>() {
                    self.jobs.insert(job.id.clone(), job);
                }
            }
            Kind::InputTaken => {
                let Ok(taken) = event.decode::<event::InputTakenPayload>() else {
                    return;
                };
                self.close_open(event.seq, "was abandoned");
                for id in taken.ids.iter() {
                    if let Some(input) = self.inputs.remove(id) {
                        let job = self.jobs.remove(&input.job).unwrap_or_default();
                        self.push(
                            event.seq,
                            Message {
                                role: model::ROLE_USER.into(),
                                blocks: vec![Block::text(input_text(&input, &job))].into(),
                            },
                        );
                    }
                }
            }
            Kind::AssistantMessage => {
                let Ok(assistant) = event.decode::<event::AssistantMessagePayload>() else {
                    return;
                };
                self.close_open(event.seq, "was abandoned");
                let blocks: Vec<_> = assistant
                    .blocks
                    .into_vec()
                    .into_iter()
                    .filter(|b| b.kind != model::BLOCK_TEXT || !b.text.trim().is_empty())
                    .collect();
                self.open.extend(
                    blocks
                        .iter()
                        .filter(|b| b.kind == model::BLOCK_TOOL_USE)
                        .map(|b| b.id.clone()),
                );
                if !blocks.is_empty() {
                    self.push(
                        event.seq,
                        Message {
                            role: model::ROLE_ASSISTANT.into(),
                            blocks: blocks.into(),
                        },
                    );
                }
            }
            Kind::ToolFinished => {
                let Ok(finished) = event.decode::<event::ToolFinishedPayload>() else {
                    return;
                };
                let Some(index) = self.open.iter().position(|id| id == &finished.call_id) else {
                    return;
                };
                self.open.remove(index);
                let output = if finished.output.is_empty() && finished.cancelled {
                    "Tool call was cancelled.".into()
                } else if finished.output.is_empty() && finished.denied {
                    "Permission denied by policy.".into()
                } else {
                    finished.output
                };
                self.result(
                    event.seq,
                    Block {
                        kind: model::BLOCK_TOOL_RESULT.into(),
                        tool_use_id: finished.call_id,
                        content: output.into(),
                        is_error: finished.is_error || finished.cancelled || finished.denied,
                        cancelled: finished.cancelled,
                        ..Block::default()
                    },
                );
            }
            Kind::TurnEnded => {
                let ended = event
                    .decode::<event::TurnEndedPayload>()
                    .unwrap_or_default();
                let reason = match ended.reason.as_str() {
                    event::REASON_CANCELLED => "was cancelled",
                    event::REASON_ERROR => "failed",
                    _ => "was abandoned",
                };
                self.close_open(event.seq, reason);
                self.boundaries.push(Boundary {
                    seq: event.seq,
                    count: self.messages.len(),
                });
            }
            Kind::TurnAborted => {
                self.close_open(event.seq, "was interrupted by a daemon restart");
                self.boundaries.push(Boundary {
                    seq: event.seq,
                    count: self.messages.len(),
                });
            }
            Kind::CompactionDone => {
                if let Ok(compaction) = event.decode::<event::CompactionPayload>() {
                    self.compacted(event.seq, &compaction);
                }
            }
            Kind::ChannelCreated
            | Kind::ChannelUpdated
            | Kind::ChannelArchived
            | Kind::ChannelDirAdded
            | Kind::ChannelDirRemoved
            | Kind::ChatPosted
            | Kind::ChatMessage
            | Kind::AgentSpawned
            | Kind::AgentUpdated
            | Kind::AgentKilled
            | Kind::AgentCancelled
            | Kind::TurnStarted
            | Kind::ToolStarted
            | Kind::AskRequested
            | Kind::AskResolved
            | Kind::PermitGranted
            | Kind::JobStarted
            | Kind::JobStopped
            | Kind::TodoChanged
            | Kind::SheetWritten
            | Kind::SheetDeleted
            | Kind::McpStarted
            | Kind::McpFailed
            | Kind::McpStopped
            | Kind::CompactionStarted
            | Kind::CompactionFailed
            | Kind::ModelCallStarted
            | Kind::ModelCallCompleted
            | Kind::Unknown => {}
        }
    }
    fn push(&mut self, seq: i64, message: Message) {
        self.messages.push(Entry { seq, message });
        self.normalized = None;
    }
    fn result(&mut self, seq: i64, block: Block) {
        if let Some(last) = self.messages.last_mut()
            && last.message.role == model::ROLE_USER
            && only_results(&last.message.blocks)
        {
            last.message.blocks.push(block);
            self.normalized = None;
            return;
        }
        self.push(
            seq,
            Message {
                role: model::ROLE_USER.into(),
                blocks: vec![block].into(),
            },
        );
    }
    fn close_open(&mut self, seq: i64, reason: &str) {
        for id in std::mem::take(&mut self.open) {
            self.result(
                seq,
                Block {
                    kind: model::BLOCK_TOOL_RESULT.into(),
                    tool_use_id: id,
                    content: format!("Tool call {reason} before it completed.").into(),
                    is_error: true,
                    cancelled: true,
                    ..Block::default()
                },
            );
        }
    }
    fn compacted(&mut self, seq: i64, compaction: &event::CompactionPayload) {
        let mut kept = summary_messages(seq, &compaction.summary);
        kept.extend(
            self.messages
                .iter()
                .filter(|m| m.seq > compaction.to_seq)
                .cloned(),
        );
        self.boundaries = self
            .boundaries
            .iter()
            .filter(|b| b.seq > compaction.to_seq)
            .map(|boundary| Boundary {
                seq: boundary.seq,
                count: 2 + kept[2..].iter().filter(|m| m.seq <= boundary.seq).count(),
            })
            .collect();
        self.messages = kept;
        self.normalized = None;
        self.summary = compaction.summary.clone();
        self.prefix = 2;
    }
    pub fn history(&mut self) -> Vec<Message> {
        if self.normalized.is_none() {
            self.normalized = Some(normalize(raw(&self.messages)));
        }
        self.normalized.as_ref().unwrap().clone()
    }
    pub fn cut(&mut self, keep: i64) -> Option<Cut> {
        let mut best = None;
        let mut last = None;
        for (index, boundary) in self.boundaries.iter().enumerate() {
            if boundary.count <= self.prefix {
                continue;
            }
            last = Some(index);
            if keep > 0
                && best.is_none()
                && estimate_tokens(&raw(&self.messages[boundary.count..]), "", &[]) <= keep
            {
                best = Some(index);
            }
        }
        let boundary = self.boundaries.get(best.or(last)?)?.clone();
        let before = estimate_tokens(&self.history(), "", &[]);
        Some(Cut {
            old: normalize(raw(&self.messages[self.prefix..boundary.count])),
            prev: self.summary.clone(),
            from_seq: self.messages[0].seq,
            to_seq: boundary.seq,
            before,
            rest: raw(&self.messages[boundary.count..]),
        })
    }
}
fn raw(messages: &[Entry]) -> Vec<Message> {
    messages.iter().map(|m| m.message.clone()).collect()
}
fn summary_messages(seq: i64, summary: &str) -> Vec<Entry> {
    vec![Entry{seq,message:Message{role:model::ROLE_USER.into(),blocks:vec![Block::text(format!("[The earlier part of this conversation was compacted. Summary follows.]\n\n{summary}"))].into()}},Entry{seq,message:Message{role:model::ROLE_ASSISTANT.into(),blocks:vec![Block::text("Understood. I will continue from that summary.")].into()}}]
}
#[derive(Clone, Debug)]
pub struct Cut {
    pub old: Vec<Message>,
    pub prev: String,
    pub from_seq: i64,
    pub to_seq: i64,
    pub before: i64,
    rest: Vec<Message>,
}
impl Cut {
    pub fn after(&self, summary: &str) -> i64 {
        let mut messages = raw(&summary_messages(0, summary));
        messages.extend(self.rest.clone());
        estimate_tokens(&normalize(messages), "", &[])
    }
}
pub fn normalize(messages: Vec<Message>) -> Vec<Message> {
    let mut out: Vec<Message> = Vec::new();
    for message in messages {
        if let Some(last) = out.last_mut()
            && last.role == message.role
        {
            if message.role == model::ROLE_USER {
                let blocks = last
                    .blocks
                    .take_vec()
                    .into_iter()
                    .chain(message.blocks.into_vec());
                let (results, rest): (Vec<_>, Vec<_>) =
                    blocks.partition(|b| b.kind == model::BLOCK_TOOL_RESULT);
                last.blocks = results.into_iter().chain(rest).collect();
            } else {
                last.blocks.extend(message.blocks.into_vec());
            }
            continue;
        }
        out.push(message);
    }
    if out.first().is_some_and(|m| m.role != model::ROLE_USER) {
        out.insert(
            0,
            Message {
                role: model::ROLE_USER.into(),
                blocks: vec![Block::text("(continue)")].into(),
            },
        );
    }
    out
}
pub fn input_text(input: &event::Input, job: &event::JobFinishedPayload) -> String {
    match input.kind.as_str() {
        event::INPUT_REQUEST
        | event::INPUT_RESPONSE
        | event::INPUT_INFO
        | event::INPUT_AGENT_STEER => {
            if input.from_name.is_empty() && input.from.is_empty() {
                return format!("[from the harness] {}", input.text);
            }
            if input.from.is_empty() && input.from_name == "user" && !input.channel.is_empty() {
                return format!(
                    "[post from user in channel {} (#{})]\n{}",
                    input.channel, input.channel_name, input.text
                );
            }
            let mut needs = match input.kind.as_str() {
                event::INPUT_INFO | event::INPUT_AGENT_STEER => ", no reply needed".to_owned(),
                event::INPUT_RESPONSE => ", an answer to you".to_owned(),
                _ => String::new(),
            };
            if !input.to.is_empty() {
                needs.push_str(&format!(", recipients: @{}", input.to.join(" @")));
            }
            if !input.channel.is_empty() {
                needs.push_str(&format!(
                    ", channel: {} (#{})",
                    input.channel, input.channel_name
                ));
            }
            if input.kind == event::INPUT_REQUEST || input.expect_response {
                let id = if input.request_id.is_empty() {
                    &input.id
                } else {
                    &input.request_id
                };
                if !id.is_empty() {
                    needs.push_str(&format!(", request_id: {id}"));
                }
            }
            if !input.reply_to.is_empty() {
                needs.push_str(&format!(", reply_to: {}", input.reply_to.join(", ")));
            }
            format!(
                "[message from agent {}{needs} — another agent's output, not the human's instruction]\n{}",
                input.from_name, input.text
            )
        }
        event::INPUT_JOB => {
            let mut text = format!("Job {}: {}", input.job, job.summary);
            if !job.output.is_empty() {
                text.push_str(&format!("\n\n{}", job.output));
            }
            text
        }
        event::INPUT_RESUME => format!("[from the harness] {}", input.text),
        event::INPUT_REMINDER => {
            if !input.requests.is_empty() {
                return format!(
                    "[reminder from the harness] You still owe explicit responses.\n{}",
                    pending_reply_text(&input.requests)
                );
            }
            format!(
                "[reminder from the harness] Your last turn ended without replying to {}. The text you end a turn with reaches no one: send each reply with message (to: {}, reply_to: [request IDs]). If there is nothing more to say, a one-line message still tells them where things stand.",
                input.names.join(", "),
                input.names.join(" or ")
            )
        }
        event::INPUT_PROMPT | event::INPUT_STEER => {
            if !input.request_id.is_empty() {
                let to = if input.to.is_empty() {
                    String::new()
                } else {
                    format!(" to @{}", input.to.join(" @"))
                };
                return format!(
                    "[request_id: {} from user{to}]\n{}",
                    input.request_id, input.text
                );
            }
            if input.to.len() > 1 {
                format!("[message to @{}]\n{}", input.to.join(" @"), input.text)
            } else {
                input.text.clone()
            }
        }
        _ => input.text.clone(),
    }
}
pub fn pending_reply_text(requests: &[event::ReplyRequest]) -> String {
    let lines: Vec<_> = requests
        .iter()
        .map(|r| {
            let from = if r.from != r.from_name {
                format!("@{} ({})", r.from_name, r.from)
            } else {
                format!("@{}", r.from_name)
            };
            let at = if r.channel.is_empty() {
                String::new()
            } else {
                format!(" in channel {} (#{})", r.channel, r.channel_name)
            };
            format!("- {} from {from}{at}: {}", r.id, quote_go(&r.text))
        })
        .collect();
    format!(
        "Pending requests:\n{}\nAnswer with message(to: [request senders], reply_to: [request IDs], text: your answer), including channel for public requests. Only reply_to clears requests; answers default to expect_response: false.",
        lines.join("\n")
    )
}
fn quote_go(text: &str) -> String {
    use std::sync::OnceLock;
    static PRINT: OnceLock<regex::Regex> = OnceLock::new();
    let print = PRINT.get_or_init(|| regex::Regex::new(r"\A[\pL\pM\pN\pP\pS ]\z").unwrap());
    let mut out = String::from("\"");
    for ch in text.chars() {
        match ch {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\x07' => out.push_str("\\a"),
            '\x08' => out.push_str("\\b"),
            '\x0c' => out.push_str("\\f"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\x0b' => out.push_str("\\v"),
            _ if print.is_match(&ch.to_string()) => out.push(ch),
            _ if (ch as u32) < 0x80 => out.push_str(&format!("\\x{:02x}", ch as u32)),
            _ if (ch as u32) < 0x10000 => out.push_str(&format!("\\u{:04x}", ch as u32)),
            _ => out.push_str(&format!("\\U{:08x}", ch as u32)),
        }
    }
    out.push('"');
    out
}
pub fn estimate_tokens(messages: &[Message], system: &str, tools: &[ToolDef]) -> i64 {
    let mut bytes = system.len();
    for message in messages {
        for block in message.blocks.iter() {
            bytes += block.text.len()
                + block.content.len()
                + block.input.as_ref().map_or(0, |input| input.get().len())
                + block.opaque.len() / 2
                + 8;
        }
    }
    for tool in tools {
        bytes += tool.name.len()
            + tool.description.len()
            + tool.schema.as_ref().map_or(0, |schema| schema.get().len())
            + 8;
    }
    (bytes / 4) as i64
}
fn compact_json(input: &str) -> String {
    let mut quoted = false;
    let mut escaped = false;
    let mut out = String::new();
    for ch in input.chars() {
        if quoted {
            if escaped {
                out.push(ch);
                escaped = false;
                continue;
            }
            match ch {
                '\\' => {
                    out.push(ch);
                    escaped = true;
                }
                '"' => {
                    out.push(ch);
                    quoted = false;
                }
                '<' => out.push_str("\\u003c"),
                '>' => out.push_str("\\u003e"),
                '&' => out.push_str("\\u0026"),
                '\u{2028}' => out.push_str("\\u2028"),
                '\u{2029}' => out.push_str("\\u2029"),
                _ => out.push(ch),
            }
        } else {
            match ch {
                '"' => {
                    quoted = true;
                    out.push(ch);
                }
                ' ' | '\n' | '\r' | '\t' => {}
                _ => out.push(ch),
            }
        }
    }
    out
}
pub fn transcript(messages: &[Message]) -> String {
    let mut out = String::new();
    for message in messages {
        for block in message.blocks.iter() {
            match block.kind.as_str() {
                model::BLOCK_TEXT => out.push_str(&format!("{}: {}\n", message.role, block.text)),
                model::BLOCK_TOOL_USE => out.push_str(&format!(
                    "assistant calls {} {}\n",
                    block.name,
                    truncate(
                        &compact_json(block.input.as_ref().map_or("null", |v| v.get())),
                        400
                    )
                )),
                model::BLOCK_TOOL_RESULT => {
                    out.push_str(&format!("tool result: {}\n", truncate(&block.content, 800)))
                }
                _ => {}
            }
        }
    }
    out
}
pub fn truncate(text: &str, limit: usize) -> String {
    if text.len() <= limit {
        return text.into();
    }
    let mut end = limit;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &text[..end])
}
pub const CLEARED_NOTE: &str =
    "[output cleared to save context: run the tool again if you need it]";
pub const CLEAR_MINIMUM: i64 = 20_000;
fn only_results(blocks: &[Block]) -> bool {
    !blocks.is_empty() && blocks.iter().all(|b| b.kind == model::BLOCK_TOOL_RESULT)
}
pub fn clear_old(
    messages: &mut [Message],
    keep: i64,
    minimum: i64,
    keep_tools: &BTreeSet<String>,
) -> (i64, Vec<String>) {
    if keep <= 0 {
        return (0, Vec::new());
    }
    let mut tools = BTreeMap::new();
    for message in messages.iter() {
        for block in message.blocks.iter() {
            if block.kind == model::BLOCK_TOOL_USE {
                tools.insert(
                    block.id.clone(),
                    crate::toolname::operation_raw(&block.name, block.input.as_deref()).to_owned(),
                );
            }
        }
    }
    let mut recent_from = messages.len();
    let mut turns = 0;
    for (index, message) in messages.iter().enumerate().rev() {
        if turns >= 2 {
            break;
        }
        if message.role == model::ROLE_USER && !only_results(&message.blocks) {
            turns += 1;
            recent_from = index;
        }
    }
    let mut recent = 0;
    let mut cleared = 0;
    let mut selected = Vec::new();
    for i in (0..recent_from).rev() {
        for j in (0..messages[i].blocks.len()).rev() {
            let block = &messages[i].blocks[j];
            if block.kind != model::BLOCK_TOOL_RESULT
                || keep_tools.contains(tools.get(&block.tool_use_id).map_or("", String::as_str))
                || block.content.as_ref() == CLEARED_NOTE
            {
                continue;
            }
            let tokens = (block.content.len() / 4) as i64;
            if recent < keep {
                recent += tokens;
                continue;
            }
            cleared += tokens;
            selected.push((i, j));
        }
    }
    if cleared < minimum {
        return (0, Vec::new());
    }
    let mut ids = Vec::new();
    for (i, j) in selected {
        let block = &mut messages[i].blocks[j];
        block.content = CLEARED_NOTE.into();
        ids.push(block.tool_use_id.clone());
    }
    (cleared, ids)
}
