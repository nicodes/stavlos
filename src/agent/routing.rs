//! Atomic private/shared delivery and explicit request identities.
use super::{
    journal::Core,
    names,
    state::{AgentState, ChannelState},
};
use crate::{event, protocol, tools};
use anyhow::{Result, ensure};
use std::{collections::BTreeSet, sync::Arc};
use tokio_util::sync::CancellationToken;
fn quote(value: &str) -> String {
    serde_json::to_string(value).expect("serializable string")
}
pub fn resolve<'a>(state: &'a ChannelState, reference: &str) -> Option<&'a AgentState> {
    let reference = reference
        .trim()
        .strip_prefix('@')
        .unwrap_or(reference.trim());
    if let Some(agent) = state.agents.get(reference) {
        return Some(agent);
    }
    if !reference.is_empty()
        && let Some(id) = state.names.get(&names::normalize(reference))
    {
        return state.agents.get(id);
    }
    if reference.len() < 4 {
        return None;
    }
    let mut matches = state
        .agents
        .iter()
        .filter(|(id, _)| id.starts_with(reference));
    let (_, first) = matches.next()?;
    if matches.next().is_some() {
        None
    } else {
        Some(first)
    }
}
pub fn preorder(state: &ChannelState) -> Vec<String> {
    let roots = state.order.iter().filter(|id| {
        state
            .agents
            .get(*id)
            .is_some_and(|agent| !state.agents.contains_key(&agent.parent))
    });
    let mut stack: Vec<_> = roots.cloned().collect();
    stack.reverse();
    let mut out = Vec::new();
    let mut seen = BTreeSet::new();
    while let Some(id) = stack.pop() {
        if !seen.insert(id.clone()) {
            continue;
        }
        if let Some(agent) = state.agents.get(&id) {
            out.push(id);
            stack.extend(agent.children.iter().rev().cloned());
        }
    }
    out
}
pub fn descendant(state: &ChannelState, id: &str, owner: &str) -> bool {
    let mut id = id;
    let mut seen = BTreeSet::new();
    while let Some(agent) = state.agents.get(id) {
        if id == owner {
            return true;
        }
        if !seen.insert(id) {
            break;
        }
        id = &agent.parent;
    }
    false
}
pub fn message_targets(
    state: &ChannelState,
    caller: &str,
    recipients: &[String],
) -> Result<(Vec<String>, Vec<String>)> {
    let mut ids = Vec::new();
    let mut labels = Vec::new();
    let mut seen = BTreeSet::new();
    for reference in recipients {
        let reference = protocol::recipient(reference);
        let (id, name) = if reference == "user" {
            ("user".into(), "user".into())
        } else {
            let agent = resolve(state, &reference)
                .ok_or_else(|| anyhow::anyhow!("unknown agent {}", quote(&reference)))?;
            ensure!(agent.id != caller, "agent {} is you", quote(&reference));
            ensure!(!agent.killed, "agent {} is killed", quote(&reference));
            (agent.id.clone(), agent.name.clone())
        };
        if seen.insert(id.clone()) {
            ids.push(id);
            labels.push(name);
        }
    }
    ensure!(!ids.is_empty(), "at least one recipient is required");
    Ok((ids, labels))
}
pub fn validate_reply(
    state: &ChannelState,
    caller: &str,
    targets: &[String],
    kind: &str,
    ids: &[String],
    channel: &str,
    system: bool,
) -> Result<()> {
    if kind != "response" {
        ensure!(ids.is_empty(), "reply_to is only valid for a response");
        return Ok(());
    }
    ensure!(
        !ids.is_empty(),
        "responses require reply_to request IDs; use info for updates"
    );
    let mut seen = BTreeSet::new();
    let mut covered = BTreeSet::new();
    for id in ids {
        ensure!(
            seen.insert(id),
            "request {} appears more than once in reply_to",
            quote(id)
        );
        let request = state
            .owed_request(caller, id)
            .or_else(|| {
                state
                    .requests
                    .get(id)
                    .filter(|request| system && request.open.contains(caller))
                    .map(|request| request.request.clone())
            })
            .ok_or_else(|| {
                anyhow::anyhow!(
                    "request {} is not pending for this agent; check agent with action status",
                    quote(id)
                )
            })?;
        ensure!(
            request.channel == channel,
            "request {} must be answered in its original channel {}",
            quote(id),
            quote(&request.channel)
        );
        ensure!(
            targets.contains(&request.from),
            "request {} belongs to {}, who is not a recipient",
            quote(id),
            request.from_name
        );
        covered.insert(request.from);
    }
    for target in targets {
        ensure!(
            covered.contains(target),
            "recipient {} has no matching request in reply_to",
            quote(target)
        );
    }
    Ok(())
}
pub fn message_result(names: &[String], kind: &str) -> String {
    if names == ["user"] {
        return "message delivered to the user".into();
    }
    let to = names.join(", ");
    let plural = names.len() > 1;
    match kind {
        "response" | "response_request" => format!("response delivered to {to}"),
        "steer" => format!("steer delivered to {to}; no response required"),
        "info" if plural => {
            format!("info delivered to {to}; no replies are needed and idle agents are not woken")
        }
        "info" => format!("info delivered to {to}; it needs no reply and does not wake it"),
        _ if plural => format!("request delivered to {to}; agent responses wake you between turns"),
        _ => format!("request delivered to {to}; its response wakes you between turns"),
    }
}
fn human_message(
    state: &ChannelState,
    caller: &str,
    names: &[String],
    text: &str,
    reply_kind: &str,
    reply_to: &[String],
) -> event::ChatPayload {
    let mut payload = event::ChatPayload {
        from: state.agents[caller].name.clone(),
        text: text.into(),
        to: names.to_vec().into(),
        kind: "info".into(),
        ..Default::default()
    };
    if reply_kind == "response" {
        payload.kind = "response".into();
        payload.reply_to = reply_to.to_vec().into();
        let mut posts = Vec::new();
        for id in reply_to {
            if let Some(request) = state.owed_request(caller, id)
                && request.from == "user"
                && !request.post.is_empty()
            {
                posts.push(request.post);
            }
        }
        if posts.len() == 1 {
            payload.post = posts[0].clone();
        }
        payload.posts = posts.into();
    }
    payload
}
pub struct Routing {
    pub core: Arc<Core>,
}
impl Routing {
    pub fn board_members(&self, caller: &str, references: &[String]) -> Result<Vec<String>> {
        self.core.inspect(|state, config| {
            let agent = state
                .agents
                .get(caller)
                .filter(|agent| !agent.killed)
                .ok_or_else(|| anyhow::anyhow!("unknown or killed agent {}", quote(caller)))?;
            let role = super::prompt::role_view(state, &agent.id, config).unwrap();
            ensure!(
                super::prompt::can_orchestrate(&role),
                "this role cannot create shared channels"
            );
            let mut ids = vec![caller.into()];
            for reference in references {
                if reference == "self" {
                    continue;
                }
                let member = resolve(state, reference)
                    .filter(|member| !member.killed)
                    .ok_or_else(|| {
                        anyhow::anyhow!("unknown or killed member {}", quote(reference))
                    })?;
                ensure!(
                    descendant(state, &member.id, caller),
                    "member {} is outside your subtree",
                    quote(reference)
                );
                if !ids.contains(&member.id) {
                    ids.push(member.id.clone());
                }
            }
            Ok(ids)
        })
    }
    pub fn deliver_board(
        &self,
        board: &Core,
        caller: &str,
        message: tools::Delivery,
        human: &str,
        system: bool,
        cancel: &CancellationToken,
    ) -> Result<String> {
        ensure!(
            board.id != self.core.id,
            "channel is not a board of this execution channel"
        );
        let mut source = self.core.state.lock().unwrap();
        let mut target = board.state.lock().unwrap();
        let info = target
            .channel
            .board
            .as_ref()
            .filter(|info| info.source == self.core.id)
            .ok_or_else(|| anyhow::anyhow!("channel is not a board of this execution channel"))?;
        ensure!(
            !source.channel.archived
                && !target.channel.archived
                && !source.stopped
                && !target.stopped,
            "channel is archived or stopped"
        );
        let from = source.channel.agents.get(caller);
        ensure!(
            !human.is_empty()
                || from
                    .is_some_and(|from| !from.killed && info.members.iter().any(|id| id == caller)),
            "you are not a live member of this channel"
        );
        let (targets, names) = board_targets(&source.channel, info, caller, &message.to)?;
        let reply_kind = if message.kind == "response_request" {
            "response"
        } else {
            &message.kind
        };
        if human.is_empty() {
            validate_reply(
                &source.channel,
                caller,
                &targets,
                reply_kind,
                &message.reply_to,
                &board.id,
                system,
            )?;
        }
        let expected = matches!(message.kind.as_str(), "request" | "response_request");
        ensure!(
            !expected || !targets.is_empty(),
            "requests require explicit recipients"
        );
        let request_id = if expected {
            names::new_id("r")?
        } else {
            String::new()
        };
        let payload = event::ChatPayload {
            id: names::new_id("post")?,
            request_id: request_id.clone(),
            from: from.map_or_else(|| human.into(), |agent| agent.name.clone()),
            text: message.text,
            to: names.into(),
            kind: reply_kind.into(),
            reply_to: message.reply_to.into(),
            ..Default::default()
        };
        let mut events = vec![board.event(
            caller,
            if human.is_empty() {
                event::CHAT_MESSAGE
            } else {
                event::CHAT_POSTED
            },
            payload.clone(),
        )?];
        events.extend(board_deliveries(
            &self.core,
            BoardDelivery {
                state: &source.channel,
                board: info,
                channel: &board.id,
                name: &target.channel.name,
                caller,
                targets: &targets,
                payload: &payload,
                kind: &message.kind,
                human,
            },
        )?);
        if targets.iter().any(|target| target == "user") && reply_kind == "response" {
            events.push(self.core.event(caller, event::CHAT_MESSAGE, payload)?);
        }
        self.core
            .commit_pair_locked(&mut source, board, &mut target, events, cancel)?;
        Ok(format!(
            "posted in #{}{}",
            target.channel.name,
            if request_id.is_empty() {
                String::new()
            } else {
                format!("; request_id: {request_id}")
            }
        ))
    }
    pub fn accept_prepared_board(
        &self,
        board: &Core,
        post: PreparedPost,
        cancel: &CancellationToken,
    ) -> Result<()> {
        post.validate()?;
        ensure!(
            board.id != self.core.id,
            "shared channel cannot accept this input"
        );
        let mut source = self.core.state.lock().unwrap();
        let mut target = board.state.lock().unwrap();
        let info = target
            .channel
            .board
            .as_ref()
            .filter(|info| info.source == self.core.id)
            .ok_or_else(|| anyhow::anyhow!("shared channel cannot accept this input"))?;
        ensure!(
            !target.channel.archived
                && !source.channel.archived
                && !target.stopped
                && !source.stopped,
            "shared channel cannot accept this input"
        );
        post.check_targets(&source.channel, Some(&info.members))?;
        ensure!(
            post.kind != "request" || !post.targets.is_empty(),
            "requests require explicit recipients"
        );
        let payload = event::ChatPayload {
            id: post.id,
            request_id: post.request_id,
            kind: post.kind.clone(),
            from: "human:web".into(),
            text: post.text,
            to: post.names.into(),
            ..Default::default()
        };
        let mut events = vec![board.event("", event::CHAT_POSTED, payload.clone())?];
        events.extend(board_deliveries(
            &self.core,
            BoardDelivery {
                state: &source.channel,
                board: info,
                channel: &board.id,
                name: &target.channel.name,
                caller: "",
                targets: &post.targets,
                payload: &payload,
                kind: &post.kind,
                human: "human:web",
            },
        )?);
        self.core
            .commit_pair_locked(&mut source, board, &mut target, events, cancel)
    }
    pub fn message(&self, caller: &str, message: tools::Delivery) -> Result<String> {
        let mut state = self.core.state.lock().unwrap();
        let channel = &state.channel;
        let from = channel
            .agents
            .get(caller)
            .ok_or_else(|| anyhow::anyhow!("unknown agent {}", quote(caller)))?;
        let (targets, names) = message_targets(channel, caller, &message.to)?;
        let reply_kind = if message.kind == "response_request" {
            "response"
        } else {
            &message.kind
        };
        validate_reply(
            channel,
            caller,
            &targets,
            reply_kind,
            &message.reply_to,
            "",
            false,
        )?;
        let request_id = if matches!(message.kind.as_str(), "request" | "response_request" | "")
            && targets.iter().any(|target| target != "user")
        {
            names::new_id("r")?
        } else {
            String::new()
        };
        let kind = match message.kind.as_str() {
            "response" | "response_request" => event::INPUT_RESPONSE,
            "steer" => event::INPUT_AGENT_STEER,
            "info" => event::INPUT_INFO,
            _ => event::INPUT_REQUEST,
        };
        let mut events = Vec::new();
        for target in &targets {
            if target == "user" {
                events.push(self.core.event(
                    caller,
                    event::CHAT_MESSAGE,
                    human_message(
                        channel,
                        caller,
                        &names,
                        &message.text,
                        reply_kind,
                        &message.reply_to,
                    ),
                )?);
            } else {
                events.push(self.core.event(
                    target,
                    event::INPUT_QUEUED,
                    event::Input {
                        id: names::new_id("i")?,
                        expect_response: message.kind == "response_request",
                        request_id: request_id.clone(),
                        reply_to: message.reply_to.clone().into(),
                        kind: kind.into(),
                        text: message.text.clone(),
                        from: caller.into(),
                        from_name: from.name.clone(),
                        to: names.clone().into(),
                        ..Default::default()
                    },
                )?);
            }
        }
        self.core
            .commit_locked(&mut state, events, &CancellationToken::new())?;
        let mut result = message_result(&names, &message.kind);
        if !request_id.is_empty() {
            result.push_str("; request_id: ");
            result.push_str(&request_id);
        }
        Ok(result)
    }
    pub fn status(&self, caller: &str, id: &str) -> Result<Vec<tools::ChildStatus>> {
        self.core.inspect(|state, _| {
            ensure!(
                state.agents.contains_key(caller),
                "unknown agent {}",
                quote(caller)
            );
            let ids = if id.is_empty() {
                preorder(state)
            } else {
                vec![
                    resolve(state, id)
                        .ok_or_else(|| anyhow::anyhow!("unknown agent {}", quote(id)))?
                        .id
                        .clone(),
                ]
            };
            Ok(ids
                .into_iter()
                .map(|id| {
                    let agent = &state.agents[&id];
                    tools::ChildStatus {
                        pending_replies: state.pending_replies(&id).into(),
                        awaiting_replies: state.awaiting_replies(&id).into(),
                        id: id.clone(),
                        parent: agent.parent.clone(),
                        you: id == caller,
                        name: agent.name.clone(),
                        role: agent.role.clone(),
                        state: state.status(&id).into(),
                        turn: agent.turn,
                        cost_usd: agent.cost,
                    }
                })
                .collect())
        })
    }
    pub fn accept_prepared(&self, post: PreparedPost, cancel: &CancellationToken) -> Result<()> {
        post.validate()?;
        let mut state = self.core.state.lock().unwrap();
        ensure!(
            !state.channel.archived
                && !state.stopped
                && state.channel.board.is_none()
                && !post.targets.is_empty()
                && post.kind == "request",
            "execution channel cannot accept this input"
        );
        post.check_targets(&state.channel, None)?;
        let mut events = vec![self.core.event(
            "",
            event::CHAT_POSTED,
            event::ChatPayload {
                id: post.id.clone(),
                request_id: post.request_id.clone(),
                kind: post.kind.clone(),
                from: "human:web".into(),
                text: post.text.clone(),
                to: post.names.clone().into(),
                ..Default::default()
            },
        )?];
        for id in &post.targets {
            events.push(self.core.event(
                id,
                event::INPUT_QUEUED,
                event::Input {
                    id: names::new_id("i")?,
                    request_id: post.request_id.clone(),
                    kind: event::INPUT_STEER.into(),
                    text: post.text.clone(),
                    post: post.id.clone(),
                    to: post.names.clone().into(),
                    ..Default::default()
                },
            )?);
        }
        self.core.commit_locked(&mut state, events, cancel)
    }
    pub fn end_replies(&self, agent: &str, reason: &str) -> Result<()> {
        if !matches!(reason, event::REASON_END_TURN | event::REASON_MAX_TOKENS) {
            return Ok(());
        }
        let mut state = self.core.state.lock().unwrap();
        let target = state
            .channel
            .agents
            .get(agent)
            .ok_or_else(|| anyhow::anyhow!("agent not found"))?;
        let requests = state.channel.pending_replies(agent);
        if !state.config.reminders
            || requests.is_empty()
            || target.nudges >= 3
            || !target.jobs.is_empty()
        {
            return Ok(());
        }
        let parties: Vec<_> = requests
            .iter()
            .map(|request| request.from.clone())
            .collect();
        let labels = parties
            .iter()
            .map(|party| {
                state
                    .channel
                    .agents
                    .get(party)
                    .map_or(party.clone(), |agent| agent.name.clone())
            })
            .collect::<Vec<_>>();
        let event = self.core.event(
            agent,
            event::INPUT_QUEUED,
            event::Input {
                id: names::new_id("i")?,
                kind: event::INPUT_REMINDER.into(),
                parties: parties.into(),
                names: labels.into(),
                requests: requests.into(),
                ..Default::default()
            },
        )?;
        self.core
            .commit_locked(&mut state, vec![event], &CancellationToken::new())
    }
    pub fn set_recap(&self, minutes: i64, cancel: &CancellationToken) -> Result<()> {
        ensure!(
            minutes >= 0,
            "recap {minutes}: minutes, or 0 to turn it off"
        );
        ensure!(minutes <= 1440, "recap {minutes}: at most 24h0m0s");
        let mut state = self.core.state.lock().unwrap();
        if state.channel.recap == minutes {
            return Ok(());
        }
        self.core.commit_locked(
            &mut state,
            vec![self.core.event(
                "",
                event::CHANNEL_UPDATED,
                event::ChannelUpdatedPayload {
                    recap: Some(minutes),
                    ..Default::default()
                },
            )?],
            cancel,
        )
    }
    pub fn maybe_recap(&self, now: crate::wire::Time, cancel: &CancellationToken) -> Result<()> {
        const TEXT: &str = "Send the human a short status report: what you are working on now, what is done, what is blocked, and what comes next. Two or three sentences, no preamble. This request came from the channel's recap timer, not from the human typing.";
        let mut state = self.core.state.lock().unwrap();
        let channel = &state.channel;
        let Some(main) = channel.order.first().and_then(|id| channel.agents.get(id)) else {
            return Ok(());
        };
        let since = channel
            .last_heard
            .0
            .max(channel.last_recap.0)
            .max(self.core.created.0);
        if channel.recap == 0
            || main.killed
            || channel.recap_open
            || now.0 - since
                < chrono::Duration::try_minutes(channel.recap).unwrap_or(chrono::Duration::MAX)
            || channel.last_work.0 <= channel.last_recap.0
        {
            return Ok(());
        }
        let event = self.core.event(
            &main.id,
            event::INPUT_QUEUED,
            event::Input {
                id: names::new_id("i")?,
                request_id: names::new_id("recap")?,
                kind: event::INPUT_PROMPT.into(),
                text: TEXT.into(),
                to: vec![main.name.clone()].into(),
                from_name: "user".into(),
                recap: true,
                ..Default::default()
            },
        )?;
        self.core.commit_locked(&mut state, vec![event], cancel)
    }
}
fn board_targets(
    state: &ChannelState,
    board: &event::BoardInfo,
    caller: &str,
    references: &[String],
) -> Result<(Vec<String>, Vec<String>)> {
    let mut ids = Vec::new();
    let mut names = Vec::new();
    for reference in references {
        let (id, name) = if protocol::recipient(reference) == "user" {
            ("user".into(), "user".into())
        } else {
            let agent = resolve(state, reference)
                .filter(|agent| !agent.killed && board.members.contains(&agent.id))
                .ok_or_else(|| {
                    anyhow::anyhow!("{} is not a live member of this channel", quote(reference))
                })?;
            ensure!(agent.id != caller, "agent {} is you", quote(reference));
            (agent.id.clone(), agent.name.clone())
        };
        if !ids.contains(&id) {
            ids.push(id);
            names.push(name);
        }
    }
    Ok((ids, names))
}
struct BoardDelivery<'a> {
    state: &'a ChannelState,
    board: &'a event::BoardInfo,
    channel: &'a str,
    name: &'a str,
    caller: &'a str,
    targets: &'a [String],
    payload: &'a event::ChatPayload,
    kind: &'a str,
    human: &'a str,
}
fn board_deliveries(core: &Core, delivery: BoardDelivery<'_>) -> Result<Vec<event::Event>> {
    let BoardDelivery {
        state,
        board,
        channel,
        name,
        caller,
        targets,
        payload,
        kind,
        human,
    } = delivery;
    let mut events = Vec::new();
    let expected = matches!(kind, "request" | "response_request");
    for id in board.members.iter() {
        if state.agents.get(id).is_none_or(|agent| agent.killed) || id == caller {
            continue;
        }
        let mut input = event::Input {
            id: names::new_id("i")?,
            kind: event::INPUT_INFO.into(),
            text: payload.text.clone(),
            from: caller.into(),
            from_name: if human.is_empty() {
                payload.from.clone()
            } else {
                "user".into()
            },
            to: payload.to.clone(),
            channel: channel.into(),
            channel_name: name.into(),
            ..Default::default()
        };
        if targets.contains(id) {
            input.request_id = payload.request_id.clone();
            match kind {
                "request" => input.kind = event::INPUT_REQUEST.into(),
                "response" | "response_request" => {
                    input.kind = event::INPUT_RESPONSE.into();
                    input.reply_to = payload.reply_to.clone();
                    input.expect_response = kind == "response_request";
                }
                "steer" => input.kind = event::INPUT_AGENT_STEER.into(),
                _ => {}
            }
            if !human.is_empty() {
                input.from.clear();
                input.from_name = "user".into();
                input.kind = event::INPUT_STEER.into();
                if expected {
                    input.post = payload.id.clone();
                } else {
                    input.request_id.clear();
                }
            }
        }
        events.push(core.event(id, event::INPUT_QUEUED, input)?);
    }
    Ok(events)
}
#[derive(Clone)]
pub struct PreparedPost {
    pub id: String,
    pub request_id: String,
    pub text: String,
    pub kind: String,
    pub targets: Vec<String>,
    pub names: Vec<String>,
}
impl PreparedPost {
    pub fn validate(&self) -> Result<()> {
        ensure!(
            self.id.starts_with("post")
                && self.id.len() <= 64
                && !self.text.trim().is_empty()
                && self.text.len() <= 4 << 20,
            "invalid prepared input"
        );
        ensure!(
            matches!(self.kind.as_str(), "request" | "info"),
            "invalid prepared input kind"
        );
        ensure!(
            self.targets.len() == self.names.len() && self.targets.len() <= 1024,
            "invalid prepared input recipients"
        );
        Ok(())
    }
    pub fn check_targets(&self, state: &ChannelState, members: Option<&[String]>) -> Result<()> {
        let mut seen = BTreeSet::new();
        for (id, name) in self.targets.iter().zip(&self.names) {
            ensure!(seen.insert(id), "duplicate input recipient");
            if members.is_some() && id == "user" && name == "user" {
                continue;
            }
            let agent = state
                .agents
                .get(id)
                .filter(|agent| !agent.killed && agent.name == *name)
                .ok_or_else(|| anyhow::anyhow!("agent identity changed before input admission"))?;
            ensure!(
                members.is_none_or(|members| members.contains(&agent.id)),
                "recipient is no longer a member of this channel"
            );
        }
        Ok(())
    }
}
