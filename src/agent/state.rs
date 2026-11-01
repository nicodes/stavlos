//! Deterministic channel/agent fold shared by live execution and journal recovery.
use crate::{
    event::{self, Event, Kind},
    project::Builder,
    toolname,
    wire::Time,
};
use std::collections::{BTreeMap, BTreeSet};
#[derive(Default, serde::Deserialize)]
struct OnlyId {
    #[serde(default, deserialize_with = "crate::wire::null_default")]
    id: String,
}
#[derive(Clone, Debug, Default)]
pub struct Permits {
    pub calls: BTreeSet<String>,
    pub prefixes: BTreeMap<String, Vec<String>>,
}
impl Permits {
    pub fn apply(&mut self, permit: &event::PermitPayload) {
        let tool = toolname::canonical(&permit.tool);
        if !permit.prefix.is_empty() {
            let prefixes = self.prefixes.entry(tool.into()).or_default();
            if !prefixes.contains(&permit.prefix) {
                prefixes.push(permit.prefix.clone());
            }
        } else if !permit.call.is_empty() {
            self.calls.insert(format!("{tool}\0{}", permit.call));
        }
    }
}
#[derive(Clone, Debug)]
pub struct Directory {
    pub path: String,
    pub source: String,
}
#[derive(Clone, Debug, Default)]
pub struct Sheet {
    pub id: String,
    pub title: String,
    pub author: String,
    pub hash: String,
    pub size: i64,
    pub updated: Time,
}
#[derive(Clone, Debug)]
pub struct Reply {
    pub request: event::ReplyRequest,
    pub seq: i64,
    pub legacy: bool,
    pub open: BTreeSet<String>,
    pub held: BTreeSet<String>,
}
#[derive(Default)]
pub struct AgentState {
    pub id: String,
    pub parent: String,
    pub role: String,
    pub name: String,
    pub model: String,
    pub variant: String,
    pub depth: i64,
    pub children: Vec<String>,
    // None inherits the channel working set; Some([]) grants no task directories.
    pub directories: crate::wire::Slice<String>,
    pub killed: bool,
    pub instructed: BTreeSet<String>,
    pub turn: i64,
    pub in_turn: bool,
    pub last_error: String,
    pub inbox: Vec<event::Input>,
    pub nudges: i64,
    pub last_post: String,
    pub jobs: BTreeMap<String, event::JobStartedPayload>,
    pub asks: BTreeSet<String>,
    pub resume_at: Time,
    pub resume_why: String,
    pub resumes: i64,
    pub turn_reminder_only: bool,
    pub turn_had_tools: bool,
    pub turn_settled: bool,
    pub todos: Vec<event::TodoItem>,
    pub todo_seq: i64,
    pub compacting: bool,
    pub tokens: i64,
    pub last_context: i64,
    pub cost: f64,
    pub history: Builder,
}
impl AgentState {
    pub fn starts_turn(&self) -> bool {
        self.inbox
            .iter()
            .any(|input| event::input_rule(&input.kind).wakes)
    }
    pub fn busy(&self) -> bool {
        !self.killed && (self.in_turn || self.starts_turn())
    }
    fn apply_turn(&mut self, event: &Event) {
        match Kind::parse(&event.kind) {
            Kind::TurnStarted => {
                if let Ok(turn) = event.decode::<event::TurnPayload>() {
                    self.turn = turn.turn;
                    self.in_turn = true;
                    self.last_error.clear();
                    self.resume_at = Time::default();
                    self.turn_reminder_only = true;
                    self.turn_had_tools = false;
                    self.turn_settled = false;
                }
            }
            Kind::AssistantMessage => {
                if let Ok(assistant) = event.decode::<event::AssistantMessagePayload>() {
                    self.tokens += assistant.usage.input_tokens + assistant.usage.output_tokens;
                    self.last_context = assistant.usage.input_tokens
                        + assistant.usage.cache_read_tokens
                        + assistant.usage.cache_write_tokens
                        + assistant.usage.output_tokens;
                    self.cost += assistant.cost_usd;
                    self.resumes = 0;
                }
            }
            Kind::TurnEnded => {
                if let Ok(ended) = event.decode::<event::TurnEndedPayload>() {
                    if ended.reason == event::REASON_ERROR {
                        self.last_error = ended.error;
                    }
                    self.resume_at = ended.resume_at;
                    self.resume_why = ended.resume;
                    if matches!(
                        ended.reason.as_str(),
                        event::REASON_END_TURN | event::REASON_MAX_TOKENS
                    ) && self.turn_reminder_only
                        && !self.turn_had_tools
                        && !self.turn_settled
                    {
                        self.nudges += 1;
                    }
                }
                self.in_turn = false;
            }
            Kind::TurnAborted => self.in_turn = false,
            Kind::ToolStarted => {
                self.turn_had_tools = true;
                self.nudges = 0;
            }
            Kind::AskRequested => {
                if let Ok(prompt) = event.decode::<OnlyId>() {
                    self.asks.insert(prompt.id);
                }
            }
            Kind::AskResolved => {
                if let Ok(prompt) = event.decode::<OnlyId>() {
                    self.asks.remove(&prompt.id);
                }
            }
            Kind::JobStarted => {
                if let Ok(job) = event.decode::<event::JobStartedPayload>() {
                    self.jobs.insert(job.id.clone(), job);
                }
            }
            Kind::JobFinished => {
                if let Ok(job) = event.decode::<OnlyId>() {
                    self.jobs.remove(&job.id);
                }
            }
            Kind::JobStopped => {
                if let Ok(job) = event.decode::<OnlyId>() {
                    self.jobs.remove(&job.id);
                }
            }
            Kind::TodoChanged => {
                if let Ok(todos) = event.decode::<event::TodoPayload>() {
                    self.todos = todos.items.into_vec();
                    for item in &self.todos {
                        if let Ok(number) =
                            item.id.strip_prefix('t').unwrap_or(&item.id).parse::<i64>()
                        {
                            self.todo_seq = self.todo_seq.max(number);
                        }
                    }
                }
            }
            Kind::CompactionStarted => self.compacting = true,
            Kind::CompactionDone | Kind::CompactionFailed => self.compacting = false,
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
            | Kind::InputQueued
            | Kind::InputTaken
            | Kind::ModelCallStarted
            | Kind::ModelCallCompleted
            | Kind::ToolFinished
            | Kind::PermitGranted
            | Kind::SheetWritten
            | Kind::SheetDeleted
            | Kind::McpStarted
            | Kind::McpFailed
            | Kind::McpStopped
            | Kind::Unknown => {}
        }
        match Kind::parse(&event.kind) {
            Kind::ToolFinished => {
                if let Ok(finished) = event.decode::<event::ToolFinishedPayload>() {
                    self.instructed.extend(finished.instructions.into_vec());
                }
            }
            Kind::CompactionDone => self.instructed.clear(),
            _ => {}
        }
    }
}
pub struct ChannelState {
    pub model_calls: i64,
    pub budget_tokens: i64,
    pub budget_cost: f64,
    pub board: Option<event::BoardInfo>,
    pub name: String,
    pub model: String,
    pub role: String,
    pub mode: String,
    pub dir: String,
    pub archived: bool,
    pub recap: i64,
    pub last_heard: Time,
    pub last_work: Time,
    pub last_recap: Time,
    pub recap_open: bool,
    pub dirs: Vec<Directory>,
    pub permits: Permits,
    pub agents: BTreeMap<String, AgentState>,
    pub order: Vec<String>,
    pub names: BTreeMap<String, String>,
    pub requests: BTreeMap<String, Reply>,
    pub request_seq: i64,
    pub sheets: BTreeMap<String, Sheet>,
    pub sheet_seq: i64,
}
impl ChannelState {
    pub fn new(model: impl Into<String>, role: impl Into<String>) -> Self {
        Self {
            model_calls: 0,
            budget_tokens: 0,
            budget_cost: 0.,
            board: None,
            name: String::new(),
            model: model.into(),
            role: role.into(),
            mode: "ask".into(),
            dir: String::new(),
            archived: false,
            recap: 0,
            last_heard: Time::default(),
            last_work: Time::default(),
            last_recap: Time::default(),
            recap_open: false,
            dirs: Vec::new(),
            permits: Permits::default(),
            agents: BTreeMap::new(),
            order: Vec::new(),
            names: BTreeMap::new(),
            requests: BTreeMap::new(),
            request_seq: 0,
            sheets: BTreeMap::new(),
            sheet_seq: 0,
        }
    }
    /// Return wakeups; the runtime controls durable commit and observed-fact handling.
    pub fn apply(&mut self, event: &Event) -> Vec<String> {
        let mut wake = Vec::new();
        let kind = Kind::parse(&event.kind);
        if matches!(kind, Kind::AssistantMessage | Kind::ToolFinished) {
            self.last_work = Time(event.time.fixed_offset());
        }
        match kind {
            Kind::ModelCallStarted => self.model_calls += 1,
            Kind::ModelCallCompleted => {
                if let Ok(call) = event.decode::<event::ModelCallPayload>() {
                    self.budget_tokens += call.usage.input_tokens
                        + call.usage.output_tokens
                        + call.usage.cache_read_tokens
                        + call.usage.cache_write_tokens;
                    self.budget_cost += call.cost_usd;
                }
            }
            Kind::ChannelCreated => {
                if let Ok(created) = event.decode::<event::ChannelCreatedPayload>() {
                    self.name = created.name;
                    self.model = created.model;
                    self.role = created.role;
                    self.board = created.board;
                    self.dir = created.dir;
                    if !created.mode.is_empty() {
                        self.mode = created.mode;
                    }
                }
            }
            Kind::ChannelUpdated => {
                if let Ok(updated) = event.decode::<event::ChannelUpdatedPayload>() {
                    if let Some(name) = updated.name {
                        self.name = name;
                    }
                    if let Some(model) = updated.model {
                        self.model = model;
                    }
                    if let Some(mode) = updated.mode {
                        self.mode = mode;
                    }
                    if let Some(dir) = updated.dir {
                        self.dir = dir;
                        self.mode = "ask".into();
                        self.permits = Permits::default();
                        for agent in self.agents.values_mut() {
                            agent.instructed.clear();
                        }
                    }
                    if let Some(recap) = updated.recap {
                        self.recap = recap;
                    }
                }
            }
            Kind::ChannelArchived => self.archived = true,
            Kind::ChannelDirAdded => {
                if let Ok(directory) = event.decode::<event::DirPayload>()
                    && !self.dirs.iter().any(|dir| dir.path == directory.dir)
                {
                    self.dirs.push(Directory {
                        path: directory.dir,
                        source: directory.source,
                    });
                }
            }
            Kind::ChannelDirRemoved => {
                if let Ok(directory) = event.decode::<event::DirPayload>() {
                    self.dirs.retain(|dir| dir.path != directory.dir);
                }
            }
            Kind::PermitGranted => {
                if let Ok(permit) = event.decode::<event::PermitPayload>() {
                    self.permits.apply(&permit);
                }
            }
            Kind::SheetWritten | Kind::SheetDeleted => {
                if let Ok(sheet) = event.decode::<event::SheetPayload>()
                    && !sheet.id.is_empty()
                {
                    if kind == Kind::SheetDeleted {
                        self.sheets.remove(&sheet.id);
                    } else {
                        if !self.sheets.contains_key(&sheet.id)
                            && let Ok(number) = sheet
                                .id
                                .strip_prefix('s')
                                .unwrap_or(&sheet.id)
                                .parse::<i64>()
                        {
                            self.sheet_seq = self.sheet_seq.max(number);
                        }
                        let target = self
                            .sheets
                            .entry(sheet.id.clone())
                            .or_insert_with(|| Sheet {
                                id: sheet.id,
                                ..Sheet::default()
                            });
                        if !sheet.title.is_empty() {
                            target.title = sheet.title;
                        }
                        target.author = sheet.author;
                        target.hash = sheet.hash;
                        target.size = sheet.size;
                        target.updated = Time(event.time.fixed_offset());
                    }
                }
            }
            Kind::AgentSpawned => {
                if let Ok(spawned) = event.decode::<event::AgentSpawnedPayload>()
                    && !spawned.id.is_empty()
                {
                    let id = spawned.id;
                    let parent = spawned.parent;
                    self.names.insert(spawned.name.clone(), id.clone());
                    self.agents.insert(
                        id.clone(),
                        AgentState {
                            id: id.clone(),
                            parent: parent.clone(),
                            role: spawned.role,
                            name: spawned.name,
                            model: spawned.model,
                            variant: spawned.variant,
                            depth: spawned.depth,
                            directories: spawned.directories,
                            ..AgentState::default()
                        },
                    );
                    self.order.push(id.clone());
                    if let Some(parent) = self.agents.get_mut(&parent) {
                        parent.children.push(id);
                    }
                }
            }
            Kind::AgentUpdated => {
                if let Some(agent) = self.agents.get_mut(&event.agent)
                    && let Ok(updated) = event.decode::<event::AgentUpdatedPayload>()
                {
                    if let Some(role) = updated.role {
                        agent.role = role;
                    }
                    if let Some(name) = updated.name {
                        agent.name = name;
                        self.names.insert(agent.name.clone(), agent.id.clone());
                    }
                    if let Some(model) = updated.model {
                        agent.model = model;
                    }
                    if let Some(variant) = updated.variant {
                        agent.variant = variant;
                    }
                }
            }
            Kind::AgentKilled => {
                if let Some(agent) = self.agents.get_mut(&event.agent) {
                    agent.killed = true;
                    agent.in_turn = false;
                    agent.jobs.clear();
                    agent.asks.clear();
                    self.forget_party(&event.agent);
                }
            }
            Kind::AgentCancelled => {
                self.forget_party(&event.agent);
                if let Some(agent) = self.agents.get_mut(&event.agent) {
                    agent.resume_at = Time::default();
                }
            }
            Kind::InputQueued => {
                if self.agents.contains_key(&event.agent)
                    && let Ok(input) = event.decode::<event::Input>()
                {
                    self.asked(&input, &event.agent);
                    if input.recap {
                        self.last_recap = Time(event.time.fixed_offset());
                        self.recap_open = true;
                    }
                    if input.kind == event::INPUT_RESPONSE && self.agents.contains_key(&input.from)
                    {
                        if input.reply_to.is_empty() {
                            self.settle_legacy(&input.from, &event.agent);
                        } else {
                            self.settle_replies(&input.from, &event.agent, &input.reply_to);
                        }
                    }
                    let target = self.agents.get_mut(&event.agent).unwrap();
                    if input.kind == event::INPUT_RESUME {
                        target.resumes += 1;
                    }
                    if event::input_rule(&input.kind).wakes && !target.killed {
                        wake.push(target.id.clone());
                    }
                    target.inbox.push(input);
                }
            }
            Kind::ChatMessage => {
                self.last_heard = Time(event.time.fixed_offset());
                self.recap_open = false;
                if self.agents.contains_key(&event.agent)
                    && let Ok(chat) = event.decode::<event::ChatPayload>()
                {
                    if chat.kind.is_empty() {
                        self.settle_legacy(&event.agent, toolname::USER);
                    } else if chat.kind == "response" {
                        self.settle_replies(&event.agent, toolname::USER, &chat.reply_to);
                    }
                }
            }
            Kind::InputTaken => {
                if let Ok(taken) = event.decode::<event::InputTakenPayload>() {
                    self.taken(&event.agent, &taken.ids);
                }
            }
            Kind::ChatPosted
            | Kind::TurnStarted
            | Kind::AssistantMessage
            | Kind::ToolStarted
            | Kind::ToolFinished
            | Kind::TurnEnded
            | Kind::TurnAborted
            | Kind::AskRequested
            | Kind::AskResolved
            | Kind::JobStarted
            | Kind::JobFinished
            | Kind::JobStopped
            | Kind::TodoChanged
            | Kind::McpStarted
            | Kind::McpFailed
            | Kind::McpStopped
            | Kind::CompactionStarted
            | Kind::CompactionDone
            | Kind::CompactionFailed
            | Kind::Unknown => {
                if let Some(agent) = self.agents.get_mut(&event.agent) {
                    agent.apply_turn(event);
                }
            }
        }
        if let Some(agent) = self.agents.get_mut(&event.agent) {
            agent.history.apply(event);
        }
        wake
    }
    pub fn open_requests(&self) -> Vec<&Reply> {
        let mut requests: Vec<_> = self.requests.values().collect();
        requests.sort_by_key(|r| r.seq);
        requests
    }
    pub fn asked(&mut self, input: &event::Input, target: &str) -> Option<String> {
        let party = owed_by(input);
        if party.is_empty() {
            return None;
        }
        let id = request_id(input).to_owned();
        if !self.requests.contains_key(&id) {
            self.request_seq += 1;
            self.requests.insert(
                id.clone(),
                Reply {
                    request: reply_request(input, party),
                    seq: self.request_seq,
                    legacy: input.request_id.is_empty(),
                    open: BTreeSet::new(),
                    held: BTreeSet::new(),
                },
            );
        }
        self.requests
            .get_mut(&id)
            .unwrap()
            .open
            .insert(target.into());
        Some(id)
    }
    fn settle(&mut self, id: &str, responder: &str) {
        if let Some(request) = self.requests.get_mut(id) {
            request.open.remove(responder);
            request.held.remove(responder);
            if request.open.is_empty() {
                self.requests.remove(id);
            }
        }
    }
    pub fn settle_replies(&mut self, responder: &str, recipient: &str, ids: &[String]) {
        for id in ids {
            if self
                .requests
                .get(id)
                .is_some_and(|r| r.request.from == recipient)
            {
                self.settle(id, responder);
                if let Some(agent) = self.agents.get_mut(responder) {
                    agent.nudges = 0;
                    agent.turn_settled = true;
                }
            }
        }
    }
    pub fn settle_legacy(&mut self, responder: &str, recipient: &str) {
        let ids: Vec<_> = self
            .open_requests()
            .into_iter()
            .filter(|r| r.legacy && r.request.from == recipient && r.open.contains(responder))
            .map(|r| r.request.id.clone())
            .collect();
        for id in ids {
            self.settle(&id, responder);
            if let Some(agent) = self.agents.get_mut(responder) {
                agent.nudges = 0;
                agent.turn_settled = true;
            }
        }
    }
    fn taken(&mut self, target: &str, ids: &[String]) {
        for id in ids {
            let Some(agent) = self.agents.get_mut(target) else {
                return;
            };
            let Some(index) = agent.inbox.iter().position(|input| &input.id == id) else {
                continue;
            };
            let input = agent.inbox.remove(index);
            if input.kind != event::INPUT_REMINDER {
                agent.turn_reminder_only = false;
            }
            if input.from.is_empty()
                && matches!(
                    input.kind.as_str(),
                    event::INPUT_PROMPT | event::INPUT_STEER
                )
            {
                agent.last_post = input.post.clone();
            }
            if let Some(request) = self.asked(&input, target) {
                self.requests
                    .get_mut(&request)
                    .unwrap()
                    .held
                    .insert(target.into());
                self.agents.get_mut(target).unwrap().nudges = 0;
            }
        }
    }
    pub fn pending_replies(&self, agent: &str) -> Vec<event::ReplyRequest> {
        self.open_requests()
            .into_iter()
            .filter(|r| r.held.contains(agent))
            .map(|r| r.request.clone())
            .collect()
    }
    pub fn owed_request(&self, agent: &str, id: &str) -> Option<event::ReplyRequest> {
        self.requests
            .get(id)
            .filter(|r| r.held.contains(agent))
            .map(|r| r.request.clone())
    }
    pub fn due(&self, agent: &str) -> Vec<String> {
        self.pending_replies(agent)
            .into_iter()
            .map(|r| r.from)
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect()
    }
    pub fn awaiting_replies(&self, agent: &str) -> Vec<event::ReplyRequest> {
        let mut out = Vec::new();
        for request in self.open_requests() {
            if request.request.from != agent {
                continue;
            }
            for target in &request.open {
                out.push(event::ReplyRequest {
                    id: request.request.id.clone(),
                    from: target.clone(),
                    from_name: self.agents.get(target).map_or(target, |a| &a.name).clone(),
                    text: request.request.text.clone(),
                    ..Default::default()
                });
            }
        }
        out.sort_by(|a, b| (&a.id, &a.from).cmp(&(&b.id, &b.from)));
        out
    }
    pub fn awaiting_on(&self, agent: &str, target: &str) -> Vec<String> {
        let mut ids: Vec<_> = self
            .requests
            .values()
            .filter(|r| r.request.from == agent && r.open.contains(target))
            .map(|r| r.request.id.clone())
            .collect();
        ids.sort();
        ids
    }
    pub fn awaiting_ids(&self, agent: &str) -> Vec<String> {
        self.requests
            .values()
            .filter(|r| r.request.from == agent)
            .flat_map(|r| r.open.iter().cloned())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect()
    }
    pub fn awaiting_any(&self, agent: &str) -> bool {
        self.requests
            .values()
            .any(|r| r.request.from == agent && !r.open.is_empty())
    }
    pub fn waiting(&self, agent: &str) -> bool {
        self.awaiting_any(agent) || self.agents.get(agent).is_some_and(|a| !a.jobs.is_empty())
    }
    pub fn status(&self, id: &str) -> &'static str {
        let Some(agent) = self.agents.get(id) else {
            return "idle";
        };
        if agent.killed {
            "killed"
        } else if agent.in_turn && !agent.asks.is_empty() {
            "blocked"
        } else if agent.in_turn {
            "running"
        } else if self.waiting(id) {
            "waiting"
        } else {
            "idle"
        }
    }
    pub fn forget_party(&mut self, party: &str) {
        let ids: Vec<_> = self
            .open_requests()
            .into_iter()
            .map(|r| r.request.id.clone())
            .collect();
        for id in ids {
            if self
                .requests
                .get(&id)
                .is_some_and(|r| r.request.from == party)
            {
                self.requests.remove(&id);
            } else {
                self.settle(&id, party);
            }
        }
    }
    pub fn party_names(&self, parties: &[String]) -> Vec<String> {
        parties
            .iter()
            .map(|id| self.agents.get(id).map_or(id, |a| &a.name).clone())
            .collect()
    }
    pub fn sorted_sheets(&self) -> Vec<&Sheet> {
        let mut sheets: Vec<_> = self
            .sheets
            .values()
            .filter_map(|sheet| {
                sheet
                    .id
                    .strip_prefix('s')
                    .and_then(|s| s.parse::<i64>().ok())
                    .filter(|n| *n >= 1 && *n <= self.sheet_seq && format!("s{n}") == sheet.id)
                    .map(|n| (n, sheet))
            })
            .collect();
        sheets.sort_by_key(|(n, _)| *n);
        sheets.into_iter().map(|(_, sheet)| sheet).collect()
    }
}
pub const MAX_NUDGES: i64 = 3;
pub fn nudge_limit(reminders: bool) -> i64 {
    if reminders { MAX_NUDGES } else { 0 }
}
pub fn owed_by(input: &event::Input) -> &str {
    if input.expect_response {
        return &input.from;
    }
    match input.kind.as_str() {
        event::INPUT_REQUEST => &input.from,
        event::INPUT_STEER
            if input.from.is_empty()
                && (!input.post.is_empty() || !input.request_id.is_empty()) =>
        {
            toolname::USER
        }
        event::INPUT_PROMPT if input.from.is_empty() && !input.request_id.is_empty() => {
            toolname::USER
        }
        _ => "",
    }
}
pub fn request_id(input: &event::Input) -> &str {
    if input.request_id.is_empty() {
        &input.id
    } else {
        &input.request_id
    }
}
pub fn reply_request(input: &event::Input, party: &str) -> event::ReplyRequest {
    let excerpt = input.text.split_whitespace().collect::<Vec<_>>().join(" ");
    let chars: Vec<_> = excerpt.chars().collect();
    let text = if chars.len() > 200 {
        chars[..199]
            .iter()
            .copied()
            .chain(std::iter::once('…'))
            .collect()
    } else {
        excerpt
    };
    event::ReplyRequest {
        channel: input.channel.clone(),
        channel_name: input.channel_name.clone(),
        id: request_id(input).into(),
        from: party.into(),
        from_name: if party == toolname::USER {
            toolname::USER.into()
        } else {
            input.from_name.clone()
        },
        to: input.to.clone(),
        text,
        post: input.post.clone(),
    }
}
