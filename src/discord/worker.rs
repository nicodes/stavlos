use super::{
    api::{Message, Missing},
    bridge::{Bridge, Link, Payload, Work},
    interaction, permission,
    questions::{self, Draft, Submission},
    status,
    store::PromptMessage,
    text,
};
use crate::{
    event,
    protocol::{self, methods},
};
use anyhow::{Result, anyhow, bail, ensure};
use serde_json::Value;
use std::{
    collections::{BTreeMap, VecDeque},
    sync::Arc,
    time::Duration,
};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;
pub(crate) struct Worker {
    bridge: Arc<Bridge>,
    id: String,
    discord: String,
    stop: CancellationToken,
    link: Option<Arc<Link>>,
    seq: i64,
    info: protocol::ChannelInfo,
    agents: Vec<protocol::AgentInfo>,
    prompts: BTreeMap<String, protocol::PromptInfo>,
    drafts: BTreeMap<String, Draft>,
    shown: BTreeMap<String, String>,
    seen: VecDeque<String>,
}
impl Worker {
    pub fn new(bridge: Arc<Bridge>, id: String, discord: String, stop: CancellationToken) -> Self {
        Self {
            bridge,
            id,
            discord,
            stop,
            link: None,
            seq: 0,
            info: Default::default(),
            agents: vec![],
            prompts: BTreeMap::new(),
            drafts: BTreeMap::new(),
            shown: BTreeMap::new(),
            seen: VecDeque::new(),
        }
    }
    fn link(&self) -> &Arc<Link> {
        self.link
            .as_ref()
            .expect("worker initialized by reconciliation")
    }
    fn context(&self) -> CancellationToken {
        self.link().cancel.child_token()
    }
    pub async fn run(mut self, mut queue: mpsc::Receiver<Work>) {
        let mut tick = tokio::time::interval(Duration::from_secs(5));
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        tick.tick().await;
        loop {
            let work = tokio::select! {biased;_=self.stop.cancelled()=>break,work=queue.recv()=>match work {Some(work)=>work,None=>break},_=tick.tick()=>{let Some(link)=self.link.clone().filter(|link|!link.cancel.is_cancelled()) else {continue;};Work{link,payload:Payload::Tick,done:None}}};
            let Work {
                link,
                payload,
                done,
            } = work;
            let result = if link.cancel.is_cancelled() {
                self.abandoned(&payload).await;
                Err(anyhow!("connection changed"))
            } else {
                let stop = self.stop.clone();
                let changed = link.cancel.clone();
                tokio::select! {biased;_=stop.cancelled()=>break,_=changed.cancelled()=>Err(anyhow!("connection changed")),result=tokio::time::timeout(Duration::from_secs(30),self.execute(link.clone(),payload))=>result.unwrap_or_else(|_|Err(anyhow!("Discord channel operation timed out")))}
            };
            if result.is_err() {
                link.cancel.cancel();
            }
            if let Some(done) = done {
                let _ = done.send(result);
            }
        }
    }
    async fn execute(&mut self, link: Arc<Link>, payload: Payload) -> Result<()> {
        if let Payload::Snapshot(snapshot) = payload {
            self.seq = snapshot.seq;
            self.info = snapshot.channel;
            self.agents = snapshot.agents.into_vec();
            self.link = Some(link);
            self.shown.clear();
            self.drafts.clear();
            return self.sync_prompts(None).await;
        }
        if self
            .link
            .as_ref()
            .is_none_or(|current| !Arc::ptr_eq(current, &link))
        {
            return Ok(());
        }
        match payload {
            Payload::Event(event) => self.mirror(*event).await,
            Payload::Prompt(notification) => self.sync_prompts(Some(&notification)).await,
            Payload::Message(message) => self.post(&message).await,
            Payload::Interaction(interaction) => {
                let result = self.interact(&interaction).await;
                if let Some(id) = interaction::question(&interaction) {
                    return self.update_card(id, result.err()).await;
                }
                let (body, controls) =
                    result.unwrap_or_else(|error| (plain_error(&error).into(), vec![]));
                tokio::time::timeout(
                    Duration::from_secs(5),
                    self.bridge.api.response_edit(
                        interaction,
                        text::clip(&body, 2000),
                        controls,
                        self.stop.child_token(),
                    ),
                )
                .await??;
                Ok(())
            }
            Payload::Tick => self.refresh().await,
            Payload::Snapshot(_) => unreachable!(),
        }
    }
    async fn abandoned(&self, payload: &Payload) {
        let feedback = async {
            match payload {
            Payload::Message(_)=>self.say("","Connection changed before this task was sent. Please resend when connected.",self.stop.child_token()).await,
            Payload::Interaction(interaction)=>self.bridge.api.response_edit(interaction.clone(),"Connection changed before this action was sent. Reopen the current prompt and try again.".into(),vec![],self.stop.child_token()).await,
            _=>Ok(()),
        }
        };
        let _ = tokio::time::timeout(Duration::from_secs(5), feedback).await;
    }
    async fn refresh(&mut self) -> Result<()> {
        self.agents = self
            .link()
            .call(
                methods::AGENT_TREE,
                &protocol::AgentTreeParams {
                    channel: self.id.clone(),
                },
            )
            .await?
            .agents
            .into_vec();
        self.sync_prompts(None).await?;
        if self
            .agents
            .iter()
            .any(|agent| agent.state == protocol::AGENT_RUNNING)
        {
            self.bridge
                .api
                .typing(self.discord.clone(), self.context())
                .await?;
        }
        Ok(())
    }
    async fn say(&self, agent: &str, body: &str, cancel: CancellationToken) -> Result<()> {
        for part in text::split(body) {
            self.bridge
                .api
                .send(
                    self.discord.clone(),
                    agent.into(),
                    part,
                    vec![],
                    cancel.clone(),
                )
                .await?;
        }
        Ok(())
    }
    async fn terminal(&self, body: &str) -> Result<()> {
        if self.bridge.cfg.approvers.len() != 1 {
            return self
                .say("", &format!("**You (terminal)**\n{body}"), self.context())
                .await;
        }
        for part in text::split(body) {
            self.bridge
                .api
                .send_user(
                    self.discord.clone(),
                    self.bridge.cfg.guild.clone(),
                    self.bridge.cfg.approvers[0].clone(),
                    part,
                    self.context(),
                )
                .await?;
        }
        Ok(())
    }
    fn agent_name(&self, id: &str) -> String {
        self.agents
            .iter()
            .find(|agent| agent.id == id)
            .map(|agent| agent.name.clone())
            .unwrap_or_else(|| id.into())
    }
    async fn mirror(&mut self, event: event::Event) -> Result<()> {
        let previous = self.seq;
        let result = self.mirror_event(event).await;
        if self.info.board.is_none() || self.seq <= previous {
            return result;
        }
        let result = result.and_then(|()| self.bridge.boards.set(&self.discord, self.seq));
        if result.is_err() {
            self.seq = previous;
        }
        result
    }
    async fn mirror_event(&mut self, event: event::Event) -> Result<()> {
        if !event.channel.is_empty() && event.channel != self.id {
            return Ok(());
        }
        if matches!(
            event.kind.as_str(),
            event::ASK_REQUESTED | event::ASK_RESOLVED
        ) {
            self.seq = self.seq.max(event.seq);
            return self.question_event(event).await;
        }
        if event.seq <= self.seq {
            return Ok(());
        }
        self.seq = event.seq;
        match event.kind.as_str() {
            event::CHANNEL_UPDATED => {
                let payload: event::ChannelUpdatedPayload = event.decode()?;
                if let Some(name) = payload.name {
                    self.info.name = name;
                }
                if let Some(dir) = payload.dir {
                    self.info.dir = dir.clone();
                    return self.say("",&format!("Default directory changed to {dir}. Mode is ask; remembered approvals cleared."),self.context()).await;
                }
                Ok(())
            }
            event::CHAT_POSTED | event::CHAT_MESSAGE => {
                let payload: event::ChatPayload = event.decode()?;
                if event.kind == event::CHAT_POSTED {
                    if payload.from == "human:discord" {
                        return Ok(());
                    }
                    let recipients = if payload.to.len() == 1 && payload.to[0] == "main" {
                        &[][..]
                    } else {
                        &payload.to
                    };
                    self.terminal(&addressed(recipients, &payload.text)).await
                } else {
                    self.say(
                        &payload.from,
                        &text::addressed(&payload.to, &payload.text),
                        self.context(),
                    )
                    .await
                }
            }
            event::AGENT_SPAWNED => {
                let payload: event::AgentSpawnedPayload = event.decode()?;
                let parent = self.agent_name(&payload.parent);
                if let Some(agent) = self.agents.iter_mut().find(|agent| agent.id == payload.id) {
                    agent.name = payload.name.clone();
                    agent.role = payload.role.clone();
                    agent.parent = payload.parent.clone();
                } else {
                    self.agents.push(protocol::AgentInfo {
                        id: payload.id,
                        channel: self.id.clone(),
                        name: payload.name.clone(),
                        role: payload.role.clone(),
                        parent: payload.parent,
                        ..Default::default()
                    });
                }
                self.say(
                    &parent,
                    &format!("{} joined ({}).", payload.name, payload.role),
                    self.context(),
                )
                .await
            }
            event::AGENT_UPDATED => {
                let payload: event::AgentUpdatedPayload = event.decode()?;
                let mut changes = vec![];
                if let Some(name) = &payload.name {
                    changes.push(format!("name → {name}"));
                }
                if let Some(role) = &payload.role {
                    changes.push(format!("role → {role}"));
                }
                if let Some(model) = &payload.model
                    && !payload.reason.is_empty()
                {
                    changes.push(format!("model → {model} · {}", payload.reason));
                }
                if changes.is_empty() {
                    return Ok(());
                }
                let body = format!(
                    "{} updated · {}",
                    self.agent_name(&event.agent),
                    changes.join(" · ")
                );
                if let Some(agent) = self.agents.iter_mut().find(|agent| agent.id == event.agent) {
                    if let Some(name) = payload.name {
                        agent.name = name;
                    }
                    if let Some(role) = payload.role {
                        agent.role = role;
                    }
                }
                self.say("", &body, self.context()).await
            }
            event::AGENT_KILLED => {
                self.say(
                    "",
                    &format!("{} stopped.", self.agent_name(&event.agent)),
                    self.context(),
                )
                .await
            }
            _ => Ok(()),
        }
    }
    async fn post(&mut self, message: &Message) -> Result<()> {
        if !message.id.is_empty() {
            if self.seen.contains(&message.id) {
                return Ok(());
            }
            self.seen.push_back(message.id.clone());
            if self.seen.len() > 1024 {
                self.seen.pop_front();
            }
        }
        if !self.bridge.allowed(&self.info.dir) {
            return self
                .say(
                    "",
                    "This channel's directory is no longer allowed by the Discord configuration.",
                    self.context(),
                )
                .await;
        }
        if let Some(reference) = &message.message_reference
            && (!reference.channel_id.is_empty() && reference.channel_id != self.discord
                || !reference.guild_id.is_empty() && reference.guild_id != self.bridge.cfg.guild)
        {
            return self
                .say("", "Reply targets must be in this channel.", self.context())
                .await;
        }
        let mut body = text::strip_bot(&message.content, &self.bridge.bot);
        if body.is_empty() {
            return self
                .say(
                    "",
                    "Send a text task; attachment-only messages are not supported.",
                    self.context(),
                )
                .await;
        }
        if protocol::addressees(&body).0.is_empty()
            && let Some(reference) = &message.message_reference
            && !reference.message_id.is_empty()
        {
            let target = match self
                .bridge
                .api
                .message(
                    self.discord.clone(),
                    reference.message_id.clone(),
                    self.context(),
                )
                .await
            {
                Ok(target) => target,
                Err(_) => return self
                    .say(
                        "",
                        "Could not read the reply target; address the agent with @name instead.",
                        self.context(),
                    )
                    .await,
            };
            if let (Some(webhook), Some(author)) = (target.webhook_id, target.author)
                && !webhook.is_empty()
                && self
                    .bridge
                    .api
                    .own_webhook(self.discord.clone(), webhook, self.context())
                    .await
                    .unwrap_or(false)
            {
                body = format!("@{} {body}", author.username);
            }
        }
        if let Err(error) = self
            .link()
            .call(
                methods::CHANNEL_POST,
                &protocol::ChannelPostParams {
                    channel: self.id.clone(),
                    text: body,
                },
            )
            .await
        {
            let feedback = if let Some(error) =
                error.downcast_ref::<crate::client::Error>().and_then(|e| {
                    if let crate::client::Error::Protocol(e) = e {
                        Some(e)
                    } else {
                        None
                    }
                }) {
                format!("Task rejected: {}", error.message)
            } else {
                "Task delivery is uncertain after a connection error. Check the TUI before resending.".into()
            };
            tokio::time::timeout(
                Duration::from_secs(5),
                self.say("", &feedback, self.stop.child_token()),
            )
            .await??;
        }
        Ok(())
    }
    fn prompt_view(&mut self, prompt: &protocol::PromptInfo) -> (String, Vec<Value>) {
        if prompt.kind == protocol::PROMPT_QUESTION {
            let client = self.link().id.clone();
            let draft = self
                .drafts
                .entry(prompt.id.clone())
                .or_insert_with(|| Draft::new(prompt));
            questions::prompt_view(prompt, draft, &client)
        } else {
            permission::view(prompt, &self.link().id)
        }
    }
    async fn sync_prompts(
        &mut self,
        notification: Option<&protocol::PromptNotification>,
    ) -> Result<()> {
        let list = self
            .link()
            .call(
                methods::PROMPT_LIST,
                &protocol::PromptListParams {
                    channel: self.id.clone(),
                },
            )
            .await?;
        self.prompts = list
            .prompts
            .iter()
            .filter(|prompt| {
                prompt.channel == self.id && prompt.escalated && self.bridge.allowed(&self.info.dir)
            })
            .map(|prompt| (prompt.id.clone(), prompt.clone()))
            .collect();
        self.drafts
            .retain(|_, draft| self.prompts.contains_key(&draft.prompt));
        self.settle_missing(notification).await?;
        for prompt in list
            .prompts
            .iter()
            .filter(|prompt| self.prompts.contains_key(&prompt.id))
            .cloned()
            .collect::<Vec<_>>()
        {
            let encoded = serde_json::to_string(&prompt)?;
            if self.shown.get(&prompt.id) == Some(&encoded) {
                continue;
            }
            let (body, controls) = self.prompt_view(&prompt);
            let parts = if prompt.kind == protocol::PROMPT_QUESTION {
                vec![body]
            } else {
                text::split(&body)
            };
            let Some(last) = parts.last() else {
                continue;
            };
            let mut message = self
                .bridge
                .store
                .snapshot()
                .get(&prompt.id)
                .cloned()
                .unwrap_or_default();
            if message.message.is_empty() {
                for part in &parts[..parts.len() - 1] {
                    self.say("", part, self.context()).await?;
                }
                let (id, webhook) = if matches!(
                    prompt.kind.as_str(),
                    protocol::PROMPT_QUESTION | protocol::PROMPT_PERMISSION
                ) {
                    let mut name = prompt.from.clone();
                    if name.is_empty() {
                        name = self.agent_name(&prompt.agent);
                    }
                    if name.is_empty() {
                        name = "agent".into();
                    }
                    self.bridge
                        .api
                        .send_question(
                            self.discord.clone(),
                            name,
                            last.clone(),
                            controls,
                            self.context(),
                        )
                        .await?
                } else {
                    (
                        self.bridge
                            .api
                            .send(
                                self.discord.clone(),
                                String::new(),
                                last.clone(),
                                controls,
                                self.context(),
                            )
                            .await?,
                        String::new(),
                    )
                };
                message = PromptMessage {
                    channel: self.discord.clone(),
                    message: id,
                    webhook,
                    ..Default::default()
                }
                .with_prompt(&prompt, self.seq);
                self.bridge.store.set(&prompt.id, message)?;
            } else {
                if message.missing_prompt(&prompt) {
                    message = message.clone().with_prompt(&prompt, message.seq);
                    self.bridge.store.set(&prompt.id, message.clone())?;
                }
                if let Err(error) = self
                    .bridge
                    .edit_prompt(&message, last.clone(), controls, self.context())
                    .await
                {
                    if !error.is::<Missing>() {
                        return Err(error);
                    }
                    self.bridge
                        .store
                        .set(&prompt.id, PromptMessage::default())?;
                    self.shown.remove(&prompt.id);
                    continue;
                }
            }
            self.shown.insert(prompt.id, encoded);
        }
        Ok(())
    }
    async fn finish_prompt(&mut self, id: &str, body: String) -> Result<()> {
        if let Some(message) = self.bridge.store.snapshot().get(id) {
            if let Err(error) = self
                .bridge
                .edit_prompt(message, text::clip(&body, 2000), vec![], self.context())
                .await
                && !error.is::<Missing>()
            {
                return Err(error);
            }
            self.bridge.store.set(id, PromptMessage::default())?;
        }
        self.shown.remove(id);
        self.prompts.remove(id);
        self.drafts.remove(id);
        Ok(())
    }
    async fn settle_missing(
        &mut self,
        notification: Option<&protocol::PromptNotification>,
    ) -> Result<()> {
        for (id, message) in self.bridge.store.snapshot() {
            if message.channel != self.discord || self.prompts.contains_key(&id) {
                continue;
            }
            let notification = notification.filter(|n| n.prompt.id == id);
            if let Some(permission) = message.permission.as_ref() {
                if notification.is_some_and(|n| n.action == protocol::ACTION_WITHDRAWN) {
                    self.finish_prompt(
                        &id,
                        permission::recorded_result(
                            permission,
                            &event::AskResolvedPayload {
                                outcome: event::ASK_WITHDRAWN.into(),
                                ..Default::default()
                            },
                        ),
                    )
                    .await?;
                    continue;
                }
                if self.shown.get(&id).is_some_and(|s| s == "awaiting-result") {
                    continue;
                }
                let (body, _) = permission::view(permission, "");
                self.await_result(
                    &id,
                    &message,
                    text::clip_markdown(&body, 1950) + "\n\nUpdating result…",
                )
                .await?;
            } else if message.is_question() {
                let mut prompt = protocol::PromptInfo {
                    questions: message.questions.clone(),
                    ..Default::default()
                };
                if let Some(notification) = notification {
                    if !notification.prompt.questions.is_empty()
                        || !notification.prompt.question.is_empty()
                    {
                        prompt.questions = prompt_questions(&notification.prompt);
                    }
                    if matches!(
                        notification.action.as_str(),
                        protocol::ACTION_WITHDRAWN | protocol::ACTION_DEFAULTED
                    ) {
                        let outcome = if notification.action == protocol::ACTION_DEFAULTED {
                            event::ASK_DEFAULTED
                        } else {
                            event::ASK_WITHDRAWN
                        };
                        self.finish_prompt(
                            &id,
                            questions::recorded_result(
                                &prompt,
                                &event::AskResolvedPayload {
                                    outcome: outcome.into(),
                                    ..Default::default()
                                },
                            ),
                        )
                        .await?;
                        continue;
                    }
                }
                if self.shown.get(&id).is_none_or(|s| s != "awaiting-result") {
                    self.await_result(
                        &id,
                        &message,
                        questions::result_note(&prompt, "Updating result…"),
                    )
                    .await?;
                }
            } else {
                let body = match notification.map(|n| n.action.as_str()) {
                    Some(protocol::ACTION_WITHDRAWN) => "Withdrawn.",
                    Some(protocol::ACTION_DEFAULTED) => "Defaulted by the daemon.",
                    _ => "Resolved or withdrawn at another client (or during restart).",
                };
                self.finish_prompt(&id, body.into()).await?;
            }
        }
        Ok(())
    }
    async fn await_result(
        &mut self,
        id: &str,
        message: &PromptMessage,
        body: String,
    ) -> Result<()> {
        if let Err(error) = self
            .bridge
            .edit_prompt(message, body, vec![], self.context())
            .await
        {
            if error.is::<Missing>() {
                self.shown.remove(id);
                return self.bridge.store.set(id, PromptMessage::default());
            }
            return Err(error);
        }
        self.shown.insert(id.into(), "awaiting-result".into());
        Ok(())
    }
    async fn question_event(&mut self, event: event::Event) -> Result<()> {
        if event.kind == event::ASK_REQUESTED {
            let payload: event::AskRequestedPayload = event.decode()?;
            let Some(mut message) = self.bridge.store.snapshot().get(&payload.id).cloned() else {
                return Ok(());
            };
            if message.channel != self.discord {
                return Ok(());
            }
            if payload.kind == protocol::PROMPT_PERMISSION {
                if message.permission.is_none() {
                    message.permission = Some(protocol::PromptInfo {
                        id: payload.id.clone(),
                        kind: protocol::PROMPT_PERMISSION.into(),
                        tool: payload.tool,
                        input: payload.input,
                        dir: payload.dir,
                        prefix: payload.prefix,
                        question: payload.question,
                        from: payload.from,
                        ..Default::default()
                    });
                }
            } else if payload.kind == protocol::PROMPT_QUESTION {
                if message.questions.is_empty() {
                    message.questions = prompt_questions(&protocol::PromptInfo {
                        questions: payload.questions,
                        question: payload.question,
                        ..Default::default()
                    });
                }
            } else {
                return Ok(());
            }
            message.seq = message.seq.max(event.seq);
            return self.bridge.store.set(&payload.id, message);
        }
        let payload: event::AskResolvedPayload = event.decode()?;
        let Some(message) = self.bridge.store.snapshot().get(&payload.id).cloned() else {
            return Ok(());
        };
        if message.channel != self.discord {
            return Ok(());
        }
        if let Some(permission) = message.permission {
            self.finish_prompt(
                &payload.id,
                permission::recorded_result(&permission, &payload),
            )
            .await
        } else if message.is_question() {
            self.finish_prompt(
                &payload.id,
                questions::recorded_result(
                    &protocol::PromptInfo {
                        questions: message.questions,
                        ..Default::default()
                    },
                    &payload,
                ),
            )
            .await
        } else {
            Ok(())
        }
    }
    async fn update_card(&mut self, id: &str, problem: Option<anyhow::Error>) -> Result<()> {
        let Some(prompt) = self.prompts.get(id).cloned() else {
            return Ok(());
        };
        let Some(message) = self.bridge.store.snapshot().get(id).cloned() else {
            return Ok(());
        };
        let (mut body, controls) = self.prompt_view(&prompt);
        if problem.is_none() && prompt.kind != protocol::PROMPT_QUESTION {
            body = text::split(&body).pop().unwrap_or_default();
        }
        if let Some(problem) = problem {
            let note = format!("\n\n{}", text::clip(&problem.to_string(), 400));
            body = text::clip_markdown(&body, 2000usize.saturating_sub(text::units(&note))) + &note;
        }
        if let Err(error) = self
            .bridge
            .edit_prompt(&message, body, controls, self.stop.child_token())
            .await
        {
            if !error.is::<Missing>() {
                return Err(error);
            }
            self.shown.remove(id);
            self.bridge.store.set(id, PromptMessage::default())?;
        }
        Ok(())
    }
    async fn interact(&mut self, interaction: &Value) -> Result<(String, Vec<Value>)> {
        if interaction["type"] == 2 {
            return self.command(interaction).await;
        }
        if let Some((all, page)) = interaction::custom_id(interaction).and_then(status::page) {
            return self.status(all, page as i64).await;
        }
        let (id, action, arg) = interaction::custom_id(interaction)
            .and_then(permission::parse_id)
            .ok_or_else(|| anyhow!("unsupported interaction"))?;
        self.sync_prompts(None).await?;
        let Some(prompt) = self.prompts.get(id).cloned() else {
            return Ok((
                "This prompt is already resolved or not available here.".into(),
                vec![],
            ));
        };
        if !prompt.claimed_by.is_empty() && prompt.claimed_by != self.link().id {
            return Ok(("This prompt is claimed by another client.".into(), vec![]));
        }
        ensure!(
            self.bridge.allowed(&self.info.dir),
            "This channel's directory is no longer allowed by the Discord configuration."
        );
        let value = interaction::modal_value(interaction);
        if prompt.kind == protocol::PROMPT_QUESTION {
            ensure!(!prompt.questions.is_empty(), "empty question batch");
            let draft = self
                .drafts
                .entry(id.into())
                .or_insert_with(|| Draft::new(&prompt));
            if action == "submit" {
                match draft.submit(&prompt)? {
                    Submission::Ready(reply) => {
                        return Ok((self.answer(interaction, &prompt, reply).await?, vec![]));
                    }
                    Submission::Continue => return Ok(questions::view(&prompt, draft)),
                }
            }
            let values = interaction["data"]["values"].as_array().map(|values| {
                values
                    .iter()
                    .filter_map(|v| v.as_str().map(str::to_owned))
                    .collect::<Vec<_>>()
            });
            let empty = vec![];
            let values = if interaction["type"] == 3 {
                Some(values.as_deref().unwrap_or(&empty))
            } else {
                None
            };
            draft.update(&prompt, action, arg, &value, values)?;
            return Ok(questions::view(&prompt, draft));
        }
        let reply = permission::answer(&prompt, action, &value)?;
        Ok((self.answer(interaction, &prompt, reply).await?, vec![]))
    }
    async fn answer(
        &mut self,
        interaction: &Value,
        prompt: &protocol::PromptInfo,
        reply: protocol::PromptReplyParams,
    ) -> Result<String> {
        self.link()
            .call(
                methods::PROMPT_CLAIM,
                &protocol::PromptClaimParams {
                    id: prompt.id.clone(),
                },
            )
            .await?;
        self.link().call(methods::PROMPT_REPLY, &reply).await?;
        let label = if prompt.kind == protocol::PROMPT_TRUST {
            if reply.answer == protocol::ANSWER_DENY {
                "Trust deferred"
            } else {
                "Trust decision submitted"
            }
        } else {
            match reply.answer.as_str() {
                protocol::ANSWER_ALLOW => "Allowed once",
                protocol::ANSWER_ALLOW_ALWAYS => "Allowed for this channel",
                protocol::ANSWER_ALLOW_PREFIX => "Prefix allowed for this channel",
                protocol::ANSWER_DENY => "Denied",
                protocol::ANSWER_ANSWERED => "Questions answered",
                _ => "",
            }
        };
        let mut body = format!("✓ {label} — <@{}>", interaction::user(interaction));
        if !reply.reason.is_empty() {
            body.push('\n');
            body.push_str(&reply.reason);
        }
        if prompt.kind == protocol::PROMPT_QUESTION {
            body = questions::answered(prompt, self.drafts.get(&prompt.id));
        } else if prompt.kind == protocol::PROMPT_PERMISSION {
            body = permission::recorded_result(
                prompt,
                &event::AskResolvedPayload {
                    outcome: event::ASK_ANSWERED.into(),
                    answer: reply.answer,
                    reason: reply.reason,
                    dir: reply.dir,
                    ..Default::default()
                },
            );
        } else if prompt.kind == protocol::PROMPT_TRUST {
            let note = format!("\n\n{body}");
            body = text::clip_markdown(
                &permission::trust_text(prompt),
                2000usize.saturating_sub(text::units(&note)),
            ) + &note;
        }
        self.finish_prompt(&prompt.id, body.clone()).await?;
        Ok(body)
    }
    async fn status(&mut self, all: bool, page: i64) -> Result<(String, Vec<Value>)> {
        self.agents = self
            .link()
            .call(
                methods::AGENT_TREE,
                &protocol::AgentTreeParams {
                    channel: self.id.clone(),
                },
            )
            .await?
            .agents
            .into_vec();
        let prompts = self
            .link()
            .call(
                methods::PROMPT_LIST,
                &protocol::PromptListParams {
                    channel: self.id.clone(),
                },
            )
            .await?;
        Ok(status::view(
            if self.info.name.is_empty() {
                &self.id
            } else {
                &self.info.name
            },
            &self.agents,
            &prompts.prompts,
            &self.id,
            all,
            page,
        ))
    }
    async fn command(&mut self, interaction: &Value) -> Result<(String, Vec<Value>)> {
        let options = interaction["data"]["options"]
            .as_array()
            .cloned()
            .unwrap_or_default();
        match interaction["data"]["name"].as_str() {
            Some("status") => {
                ensure!(
                    options.len() <= 1
                        && options.iter().all(|option| option["type"] == 1
                            && matches!(option["name"].as_str(), Some("active" | "all"))),
                    "use /status active or /status all"
                );
                self.status(options.iter().any(|option| option["name"] == "all"), 0)
                    .await
            }
            Some("cancel") => {
                let name = options
                    .iter()
                    .find(|option| option["name"] == "agent")
                    .and_then(|option| option["value"].as_str())
                    .unwrap_or("main")
                    .trim_start_matches('@');
                let tree = self
                    .link()
                    .call(
                        methods::AGENT_TREE,
                        &protocol::AgentTreeParams {
                            channel: self.id.clone(),
                        },
                    )
                    .await?;
                let agent = tree
                    .agents
                    .iter()
                    .find(|agent| {
                        protocol::simple_lower(&agent.name) == protocol::simple_lower(name)
                    })
                    .ok_or_else(|| anyhow!("no agent named @{name} in this channel"))?;
                self.link()
                    .call(
                        methods::AGENT_SEND,
                        &protocol::AgentSendParams {
                            agent: agent.id.clone(),
                            kind: protocol::KIND_CANCEL.into(),
                            ..Default::default()
                        },
                    )
                    .await?;
                Ok((format!("Cancellation sent to @{}", agent.name), vec![]))
            }
            _ => bail!("unknown command"),
        }
    }
}
fn prompt_questions(prompt: &protocol::PromptInfo) -> crate::wire::Slice<protocol::Question> {
    if prompt.questions.is_empty() {
        vec![protocol::Question {
            question: prompt.question.clone(),
            ..Default::default()
        }]
        .into()
    } else {
        prompt.questions.clone()
    }
}
fn addressed(recipients: &[String], body: &str) -> String {
    if recipients.is_empty() {
        return body.into();
    }
    let address = recipients
        .iter()
        .map(|name| format!("@{name}"))
        .collect::<Vec<_>>()
        .join(" ");
    if body.is_empty() {
        address
    } else {
        format!("{address} {body}")
    }
}
fn plain_error(error: &anyhow::Error) -> &'static str {
    if let Some(crate::client::Error::Protocol(error)) =
        error.downcast_ref::<crate::client::Error>()
        && error.code == protocol::ERR_NOT_FOUND
    {
        return "That channel or agent no longer exists.";
    }
    if error.to_string().contains("timed out") {
        "The daemon did not answer in time. Try again."
    } else if error.to_string().contains("connection closed") {
        "The daemon is not running."
    } else {
        "Could not complete the action."
    }
}
