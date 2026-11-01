use super::*;
use crate::{event, protocol::methods as rpc, textsafe};
impl Controller {
    pub fn notification(&mut self, message: crate::client::Notification) -> Vec<Effect> {
        match message {
            crate::client::Notification::Event(notification) => {
                self.apply_event(notification.event)
            }
            crate::client::Notification::Stream(stream) => {
                self.apply_stream(stream);
                vec![]
            }
            crate::client::Notification::Prompt(prompt) => {
                self.prompt_notification(*prompt);
                vec![]
            }
            crate::client::Notification::Changed(changed) => match changed.what.as_str() {
                protocol::CHANGED_PLAN => {
                    vec![self.global(rpc::PLAN_USAGE, protocol::None {}, Tag::Plans)]
                }
                protocol::CHANGED_DISCORD => {
                    self.discord_epoch = self.discord_epoch.wrapping_add(1);
                    vec![
                        self.global(
                            rpc::DISCORD_STATUS_METHOD,
                            protocol::None {},
                            Tag::Discord(
                                self.popup
                                    .as_ref()
                                    .is_some_and(|popup| popup.kind == Dialog::Discord),
                                self.discord_epoch,
                            ),
                        ),
                    ]
                }
                protocol::CHANGED_WEB => {
                    self.web_epoch = self.web_epoch.wrapping_add(1);
                    vec![
                        self.global(
                            rpc::WEB_STATUS_METHOD,
                            protocol::None {},
                            Tag::Web(
                                self.popup
                                    .as_ref()
                                    .is_some_and(|popup| popup.kind == Dialog::Web),
                                self.web_epoch,
                            ),
                        ),
                    ]
                }
                _ => vec![],
            },
        }
    }
    pub fn initial(&self) -> Vec<Effect> {
        vec![
            self.reconcile(),
            self.catalog_request(),
            self.global(
                rpc::PROMPT_LIST,
                protocol::PromptListParams::default(),
                Tag::Prompts,
            ),
            self.global(rpc::PLAN_USAGE, protocol::None {}, Tag::Plans),
            self.global(
                rpc::CACHE_USAGE,
                protocol::CacheUsageParams::default(),
                Tag::Cache,
            ),
            self.global(
                rpc::DISCORD_STATUS_METHOD,
                protocol::None {},
                Tag::Discord(false, 0),
            ),
            self.global(
                rpc::WEB_STATUS_METHOD,
                protocol::None {},
                Tag::Web(false, 0),
            ),
        ]
    }
    pub fn reconcile(&self) -> Effect {
        self.scoped(
            rpc::RECONCILE,
            protocol::ChannelRef {
                channel: self.scope.channel.clone(),
            },
            Tag::Reconcile,
        )
    }
    pub fn catalog_request(&self) -> Effect {
        self.global(
            rpc::CHANNEL_LIST,
            protocol::ChannelListParams::default(),
            Tag::Catalog,
        )
    }
    pub fn tick(&mut self, now: Time, catalog: bool, plans: bool) -> Vec<Effect> {
        self.now = now;
        let mut effects = vec![];
        if !self.status.is_empty() && now.0 >= self.status_until.0 {
            self.status.clear();
            self.dirty = true;
        }
        if self
            .channel
            .view()
            .is_some_and(|view| view.in_turn() || view.running() || view.compacting())
        {
            self.dirty = true;
        }
        if self.tree_dirty {
            self.tree_dirty = false;
            effects.push(self.scoped(
                rpc::AGENT_TREE,
                protocol::AgentTreeParams {
                    channel: self.scope.channel.clone(),
                },
                Tag::Tree(self.scope.channel.clone()),
            ));
        }
        if catalog {
            self.dirty = true;
            effects.push(self.catalog_request());
            effects.push(self.global(
                rpc::CACHE_USAGE,
                protocol::CacheUsageParams::default(),
                Tag::Cache,
            ));
            for info in &self.catalog {
                if info.id != self.scope.channel && !self.tree_closed.contains(&info.id) {
                    effects.push(self.global(
                        rpc::AGENT_TREE,
                        protocol::AgentTreeParams {
                            channel: info.id.clone(),
                        },
                        Tag::Tree(info.id.clone()),
                    ));
                }
            }
        }
        if plans {
            effects.push(self.global(rpc::PLAN_USAGE, protocol::None {}, Tag::Plans));
        }
        effects
    }
    pub fn set_agents(&mut self, agents: Vec<protocol::AgentInfo>) {
        self.channel.agents = agents;
        if !self
            .channel
            .agents
            .iter()
            .any(|agent| agent.id == self.channel.selected)
        {
            self.channel.selected = self
                .channel
                .agents
                .first()
                .map(|agent| agent.id.clone())
                .unwrap_or_default();
        }
        self.dirty = true;
    }
    pub fn on_reconcile(&mut self, snapshot: protocol::ReconcileResult) -> Vec<Effect> {
        if !snapshot.channel.id.is_empty() && snapshot.channel.id != self.scope.channel {
            return vec![];
        }
        if self.channel.reconciled && snapshot.seq < self.channel.seq {
            return vec![self.reconcile()];
        }
        self.channel.info = snapshot.channel;
        self.set_agents(snapshot.agents.into_vec());
        self.custom_commands.clear();
        self.channel.reconciled = true;
        for agent in &self.channel.agents {
            self.channel.stream_epochs.insert(
                agent.id.clone(),
                (agent.turn, protocol::agent_busy(&agent.state)),
            );
        }
        for prompt in snapshot.prompts.into_vec() {
            self.upsert_prompt(prompt);
        }
        self.channel.replay_to = snapshot.seq;
        self.channel.loading = snapshot.seq > self.channel.seq;
        if !self.channel.loading {
            self.materialize_prompts();
        }
        let tail = if self.channel.whole_log || self.channel.seq > 0 {
            0
        } else {
            3000
        };
        let mut subscribe = self.scoped(
            rpc::SUBSCRIBE,
            protocol::SubscribeParams {
                channel: self.scope.channel.clone(),
                from: self.channel.seq.saturating_add(1),
                tail,
            },
            Tag::Subscribed,
        );
        if let Effect::Request(request) = &mut subscribe {
            request.timeout = None;
        }
        vec![
            subscribe,
            self.catalog_request(),
            self.scoped(
                rpc::ROLES,
                protocol::RolesParams {
                    channel: self.scope.channel.clone(),
                },
                Tag::Roles(true),
            ),
            self.scoped(
                rpc::COMMAND_LIST,
                protocol::ChannelRef {
                    channel: self.scope.channel.clone(),
                },
                Tag::Commands,
            ),
            Effect::Remember {
                channel: self.scope.channel.clone(),
                at: self.now.0.timestamp_nanos_opt().unwrap_or(i64::MAX),
            },
        ]
    }
    pub fn bind_channel(&mut self, info: protocol::ChannelInfo, agent: String) -> Vec<Effect> {
        self.config = None;
        self.config_epoch = self.config_epoch.wrapping_add(1);
        let old = self.scope.channel.clone();
        let next = info.id.clone();
        if old != next {
            let mut channel = self
                .visited
                .remove(&next)
                .unwrap_or_else(|| Channel::new(next.clone()));
            channel.info = info;
            for transcript in channel.transcripts.values_mut() {
                transcript.apply_stream(&protocol::StreamNotification {
                    reset: true,
                    ..Default::default()
                });
            }
            let previous = std::mem::replace(&mut self.channel, channel);
            if previous.reconciled {
                self.trees.insert(old.clone(), previous.agents.clone());
                if self.visited.len() >= 32
                    && let Some(key) = self.visited.keys().next().cloned()
                {
                    self.visited.remove(&key);
                }
                self.visited.insert(old.clone(), previous);
            }
        } else {
            self.channel.info = info;
        }
        self.scope = Scope {
            channel: next,
            generation: self.scope.generation.wrapping_add(1),
        };
        self.channel.loading = true;
        self.channel.reconciled = false;
        self.channel.chat = agent.is_empty();
        if !agent.is_empty() {
            self.channel.selected = agent;
        }
        self.focus = Focus::Input;
        let mut effects = self.close_popup();
        if old != self.scope.channel {
            effects.push(self.global(
                rpc::UNSUBSCRIBE,
                protocol::SubscribeParams {
                    channel: old,
                    ..Default::default()
                },
                Tag::Changed(String::new()),
            ));
        }
        effects.push(self.reconcile());
        effects
    }
    pub fn load_history(&mut self) -> Vec<Effect> {
        if self.channel.history_from <= 1 {
            self.status("all of this channel's history is loaded", false);
            return vec![];
        }
        self.scope.generation = self.scope.generation.wrapping_add(1);
        self.channel.whole_log = true;
        self.channel.history_from = 0;
        self.channel.await_first = true;
        self.channel.seq = 0;
        self.channel.loading = true;
        self.channel.reconciled = false;
        self.channel.parents.clear();
        self.channel.transcripts.clear();
        self.channel.stream_epochs.clear();
        self.channel.scroll = 0;
        self.status("loading history…", false);
        vec![self.reconcile()]
    }
    pub fn apply_stream(&mut self, stream: protocol::StreamNotification) {
        if !stream.channel.is_empty() && stream.channel != self.scope.channel
            || self.channel.await_first
            || self.channel.loading
        {
            return;
        }
        if !self
            .channel
            .stream_epochs
            .get(&stream.agent)
            .is_some_and(|(turn, active)| *active && *turn == stream.turn)
        {
            return;
        }
        self.channel.transcript(&stream.agent).apply_stream(&stream);
        self.dirty = true;
    }
    pub fn apply_event(&mut self, ev: event::Event) -> Vec<Effect> {
        if !ev.channel.is_empty() && ev.channel != self.scope.channel {
            return vec![];
        }
        if self.channel.await_first {
            if ev.seq != 1 {
                return vec![];
            }
            self.channel.await_first = false;
        }
        self.channel.seq = self.channel.seq.max(ev.seq);
        let mut effects = vec![];
        let mut target = ev.agent.clone();
        match ev.kind.as_str() {
            event::AGENT_SPAWNED => {
                if let Ok(payload) = ev.decode::<event::AgentSpawnedPayload>()
                    && !payload.id.is_empty()
                {
                    target = payload.id.clone();
                    if !payload.parent.is_empty() {
                        self.channel
                            .parents
                            .insert(payload.id.clone(), payload.parent.clone());
                        self.channel
                            .transcript(&payload.parent)
                            .child_spawned(&payload.id);
                    }
                    if !self.channel.loading
                        && !self
                            .channel
                            .agents
                            .iter()
                            .any(|agent| agent.id == payload.id)
                    {
                        self.channel.agents.push(protocol::AgentInfo {
                            id: payload.id,
                            channel: ev.channel.clone(),
                            parent: payload.parent,
                            role: payload.role,
                            name: payload.name,
                            model: payload.model,
                            variant: payload.variant,
                            depth: payload.depth,
                            state: protocol::AGENT_IDLE.into(),
                            ..Default::default()
                        });
                    }
                }
            }
            event::AGENT_KILLED => {
                if let Some(parent) = self.channel.parents.get(&ev.agent).cloned() {
                    self.channel
                        .transcript(&parent)
                        .child_state(&ev.agent, protocol::AGENT_KILLED);
                }
                let label = self
                    .channel
                    .agents
                    .iter()
                    .find(|agent| agent.id == ev.agent)
                    .map(|agent| agent.name.clone())
                    .unwrap_or_else(|| ev.agent.clone());
                for transcript in self.channel.transcripts.values_mut() {
                    transcript.asker_gone(&label);
                }
                self.channel
                    .stream_epochs
                    .entry(ev.agent.clone())
                    .and_modify(|(_, active)| *active = false);
            }
            event::INPUT_QUEUED | event::CHAT_POSTED => self.remember_input(&ev),
            event::CHANNEL_UPDATED => {
                if let Ok(payload) = ev.decode::<event::ChannelUpdatedPayload>() {
                    if let Some(name) = payload.name {
                        self.channel.info.name = textsafe::clean(&name);
                    }
                    if let Some(model) = payload.model {
                        self.channel.info.model = model;
                    }
                    if let Some(dir) = payload.dir.as_ref().filter(|_| !self.channel.loading) {
                        self.channel.info.dir = textsafe::clean(dir);
                        self.channel.info.dir_error.clear();
                        self.channel.info.mode = protocol::MODE_ASK.into();
                        self.scope.generation = self.scope.generation.wrapping_add(1);
                        effects.push(self.reconcile());
                    }
                    if let Some(mode) = payload.mode.clone() {
                        self.channel.info.mode = mode;
                    }
                    if let Some(recap) = payload.recap {
                        self.channel.info.recap = recap;
                    }
                    if payload.dir.is_some() || payload.mode.is_some() {
                        let ids = self
                            .channel
                            .agents
                            .iter()
                            .map(|agent| agent.id.clone())
                            .collect::<Vec<_>>();
                        for id in ids {
                            self.channel.transcript(&id).apply(&ev);
                        }
                    }
                }
            }
            event::CHANNEL_DIR_ADDED => {
                if let Ok(payload) = ev.decode::<event::DirPayload>()
                    && payload.dir != self.channel.info.dir
                    && !self
                        .channel
                        .info
                        .dirs
                        .iter()
                        .any(|dir| dir.path == payload.dir)
                {
                    self.channel.info.dirs.push(protocol::DirInfo {
                        path: payload.dir,
                        source: payload.source,
                    });
                }
            }
            event::CHANNEL_DIR_REMOVED => {
                if let Ok(payload) = ev.decode::<event::DirPayload>() {
                    self.channel.info.dirs = self
                        .channel
                        .info
                        .dirs
                        .take_vec()
                        .into_iter()
                        .filter(|dir| dir.path != payload.dir)
                        .collect();
                }
            }
            _ => {}
        }
        let child_state = match ev.kind.as_str() {
            event::TURN_STARTED => {
                if let Ok(payload) = ev.decode::<event::TurnPayload>() {
                    self.channel
                        .stream_epochs
                        .insert(ev.agent.clone(), (payload.turn, true));
                }
                Some(protocol::AGENT_RUNNING)
            }
            event::TURN_ENDED | event::TURN_ABORTED => {
                let turn = ev
                    .decode::<event::TurnPayload>()
                    .map(|payload| payload.turn)
                    .unwrap_or_default();
                let entry = self
                    .channel
                    .stream_epochs
                    .entry(ev.agent.clone())
                    .or_default();
                if turn >= entry.0 {
                    *entry = (turn, false);
                }
                Some(protocol::AGENT_IDLE)
            }
            _ => None,
        };
        if let Some(state) = child_state
            && let Some(parent) = self.channel.parents.get(&ev.agent).cloned()
        {
            self.channel
                .transcript(&parent)
                .child_state(&ev.agent, state);
        }
        if !target.is_empty() {
            self.channel.transcript(&target).apply(&ev);
        }
        if super::super::transcript::chat_event(&ev.kind) {
            self.channel.transcript(CHAT).apply(&ev);
        }
        if matches!(
            ev.kind.as_str(),
            event::AGENT_SPAWNED
                | event::AGENT_KILLED
                | event::AGENT_UPDATED
                | event::CHANNEL_UPDATED
                | event::TURN_STARTED
                | event::TURN_ENDED
                | event::TURN_ABORTED
                | event::ASSISTANT_MESSAGE
                | event::JOB_STARTED
                | event::JOB_FINISHED
                | event::JOB_STOPPED
                | event::TODO_CHANGED
                | event::MCPSTARTED
                | event::MCPFAILED
                | event::MCPSTOPPED
                | event::INPUT_QUEUED
                | event::INPUT_TAKEN
                | event::CHAT_MESSAGE
                | event::ASK_REQUESTED
                | event::ASK_RESOLVED
        ) {
            self.tree_dirty = true;
        }
        if self.channel.loading && ev.seq >= self.channel.replay_to {
            self.channel.loading = false;
            self.materialize_prompts();
            self.tree_dirty = true;
        }
        self.dirty = true;
        effects
    }
    fn remember_input(&mut self, ev: &event::Event) {
        if ev.seq > 0 && ev.seq <= self.channel.history_seq {
            return;
        }
        self.channel.history_seq = self.channel.history_seq.max(ev.seq);
        if !self.channel.loading {
            return;
        }
        let text = if ev.kind == event::CHAT_POSTED {
            let Ok(payload) = ev.decode::<event::ChatPayload>() else {
                return;
            };
            if payload.text.is_empty() {
                return;
            }
            if !payload.to.is_empty() && (payload.to.len() != 1 || payload.to[0] != "main") {
                format!("@{} {}", payload.to.join(" @"), payload.text)
            } else {
                payload.text
            }
        } else {
            let Ok(input) = ev.decode::<event::Input>() else {
                return;
            };
            if !input.from.is_empty()
                || !input.post.is_empty()
                || input.text.is_empty()
                || !matches!(
                    input.kind.as_str(),
                    event::INPUT_PROMPT | event::INPUT_STEER
                )
            {
                return;
            }
            input.text
        };
        self.channel.push_history(text);
    }
}
