use super::super::state::AgentState;
use super::*;
fn quoted(value: &str) -> String {
    serde_json::to_string(value).expect("serializable string")
}
pub(super) fn directory_error(directory: &str) -> String {
    match std::fs::metadata(directory) {
        Ok(metadata) if metadata.is_dir() => String::new(),
        Ok(_) => format!("default directory is not a directory: {directory}"),
        Err(error) => format!("default directory unavailable: {error}"),
    }
}
fn changed(update: &event::AgentUpdatedPayload) -> bool {
    update.role.is_some()
        || update.name.is_some()
        || update.model.is_some()
        || update.variant.is_some()
        || !update.reason.is_empty()
}
fn parked(agent: &AgentState, now: Time) -> bool {
    !agent.killed
        && !agent.in_turn
        && !agent.resume_at.is_zero()
        && agent.resume_at.0 <= now.0
        && !agent.starts_turn()
}
impl Channel {
    pub fn set_configuration(&self, config: Arc<config::Effective>) {
        let stamp = control::trust_stamp(&config);
        let mut state = self.core.state.lock().unwrap();
        if !state.reconfiguring
            && (config.dir.as_os_str().is_empty()
                || config.dir.to_string_lossy() == state.channel.dir)
        {
            state.config = config;
            *self.stamp.lock().unwrap() = stamp;
        }
    }
    pub fn set_mode(&self, mode: &str, cancel: &CancellationToken) -> Result<()> {
        ensure!(
            matches!(mode, "ask" | "auto" | "yolo"),
            "unknown mode {}: ask, auto or yolo",
            quoted(mode)
        );
        let mut state = self.core.state.lock().unwrap();
        if state.channel.mode == mode {
            return Ok(());
        }
        self.core.commit_locked(
            &mut state,
            vec![self.core.event(
                "",
                event::CHANNEL_UPDATED,
                event::ChannelUpdatedPayload {
                    mode: Some(mode.into()),
                    ..Default::default()
                },
            )?],
            cancel,
        )
    }
    pub fn rename(&self, name: &str, cancel: &CancellationToken) -> Result<()> {
        self.core.commit(
            vec![self.core.event(
                "",
                event::CHANNEL_UPDATED,
                event::ChannelUpdatedPayload {
                    name: Some(name.into()),
                    ..Default::default()
                },
            )?],
            cancel,
        )
    }
    pub fn set_model(&self, id: &str, cancel: &CancellationToken) -> Result<()> {
        self.environment.models.check(id)?;
        self.core.commit(
            vec![self.core.event(
                "",
                event::CHANNEL_UPDATED,
                event::ChannelUpdatedPayload {
                    model: Some(id.into()),
                    ..Default::default()
                },
            )?],
            cancel,
        )
    }
    pub async fn archive(&self, cancel: &CancellationToken) -> Result<()> {
        if let Some(root) = self.root() {
            let _ = self.kill(&root.id).await;
        }
        let result = self.core.commit(
            vec![self.core.bare_event("", event::CHANNEL_ARCHIVED)],
            cancel,
        );
        self.lifetime.cancel();
        result
    }
    pub fn queue(
        &self,
        id: &str,
        text: &str,
        source: &str,
        kind: &str,
        cancel: &CancellationToken,
    ) -> Result<()> {
        let mut state = self.core.state.lock().unwrap();
        let agent = state
            .channel
            .agents
            .get(id)
            .ok_or_else(|| anyhow::anyhow!("agent {} not found", quoted(id)))?;
        ensure!(!agent.killed, "agent {} is killed", agent.name);
        let mut input = event::Input {
            id: names::new_id("i")?,
            kind: kind.into(),
            text: text.into(),
            ..Default::default()
        };
        input.request_id = input.id.clone();
        if let Some(from) = source
            .strip_prefix("agent:")
            .and_then(|id| state.channel.agents.get(id))
        {
            input.kind = event::INPUT_REQUEST.into();
            input.from = from.id.clone();
            input.from_name = from.name.clone();
        }
        self.core.commit_locked(
            &mut state,
            vec![self.core.event(id, event::INPUT_QUEUED, input)?],
            cancel,
        )
    }
    pub fn post(
        &self,
        text: &str,
        source: &str,
        cancel: &CancellationToken,
    ) -> Result<Vec<String>> {
        let (references, text) = protocol::addressees(text);
        ensure!(!text.trim().is_empty(), "empty message");
        let mut state = self.core.state.lock().unwrap();
        let mut targets = Vec::new();
        for reference in references {
            let agent = routing::resolve(&state.channel, &reference)
                .ok_or_else(|| anyhow::anyhow!("no agent named @{reference} in this channel"))?;
            ensure!(!agent.killed, "@{reference} is killed");
            if !targets.contains(&agent.id) {
                targets.push(agent.id.clone());
            }
        }
        if targets.is_empty() {
            targets.push(
                state
                    .channel
                    .order
                    .first()
                    .cloned()
                    .ok_or_else(|| anyhow::anyhow!("the channel has no agents"))?,
            );
        }
        let post = names::new_id("post")?;
        let labels = targets
            .iter()
            .map(|id| state.channel.agents[id].name.clone())
            .collect::<Vec<_>>();
        let mut events = vec![self.core.event(
            "",
            event::CHAT_POSTED,
            event::ChatPayload {
                id: post.clone(),
                request_id: post.clone(),
                kind: "request".into(),
                from: source.into(),
                text: text.clone(),
                to: labels.clone().into(),
                ..Default::default()
            },
        )?];
        for id in targets {
            events.push(self.core.event(
                &id,
                event::INPUT_QUEUED,
                event::Input {
                    id: names::new_id("i")?,
                    request_id: post.clone(),
                    kind: event::INPUT_STEER.into(),
                    text: text.clone(),
                    post: post.clone(),
                    to: labels.clone().into(),
                    ..Default::default()
                },
            )?);
        }
        self.core.commit_locked(&mut state, events, cancel)?;
        Ok(labels)
    }
    pub fn run_command(
        &self,
        directory: &str,
        selected: &str,
        text: &str,
        source: &str,
        cancel: &CancellationToken,
    ) -> Result<()> {
        let mut state = self.core.state.lock().unwrap();
        ensure!(
            !state.reconfiguring && state.channel.dir == directory,
            "channel directory changed; retry the command"
        );
        let is_post = selected.is_empty();
        let id = if is_post {
            state.channel.order.first().map_or("", String::as_str)
        } else {
            selected
        };
        let agent = state
            .channel
            .agents
            .get(id)
            .filter(|agent| !agent.killed)
            .ok_or_else(|| anyhow::anyhow!("no live agent selected in this channel"))?;
        let id = agent.id.clone();
        let name = agent.name.clone();
        let mut input = event::Input {
            id: names::new_id("i")?,
            kind: event::INPUT_STEER.into(),
            text: text.into(),
            ..Default::default()
        };
        input.request_id = input.id.clone();
        let mut events = Vec::new();
        if is_post {
            input.post = names::new_id("post")?;
            input.to = vec![name].into();
            input.request_id = input.post.clone();
            events.push(self.core.event(
                "",
                event::CHAT_POSTED,
                event::ChatPayload {
                    id: input.post.clone(),
                    request_id: input.post.clone(),
                    kind: "request".into(),
                    from: source.into(),
                    text: text.into(),
                    to: input.to.clone(),
                    ..Default::default()
                },
            )?);
        }
        events.push(self.core.event(&id, event::INPUT_QUEUED, input)?);
        self.core.commit_locked(&mut state, events, cancel)
    }
    pub fn add_directory(
        &self,
        agent: &str,
        directory: &Path,
        source: &str,
        cancel: &CancellationToken,
    ) -> Result<()> {
        let mut state = self.core.state.lock().unwrap();
        let directory = crate::pathx::clean(directory);
        if scope::contains(
            &scope::channel_directories(&state.channel, &state.config),
            &directory,
        ) {
            return Ok(());
        }
        self.core.commit_locked(
            &mut state,
            vec![self.core.event(
                agent,
                event::CHANNEL_DIR_ADDED,
                event::DirPayload {
                    dir: directory.to_string_lossy().into_owned(),
                    source: source.into(),
                },
            )?],
            cancel,
        )
    }
    pub fn add_directory_from_owner(
        &self,
        directory: &str,
        cancel: &CancellationToken,
    ) -> Result<()> {
        ensure!(!directory.trim().is_empty(), "a directory is required");
        let base = self.core.inspect(|state, _| PathBuf::from(&state.dir));
        self.add_directory(
            "",
            &scope::resolve_directory(&base, directory),
            "human",
            cancel,
        )
    }
    pub fn remove_directory(&self, directory: &str, cancel: &CancellationToken) -> Result<()> {
        let mut state = self.core.state.lock().unwrap();
        let directory = scope::resolve_directory(Path::new(&state.channel.dir), directory);
        ensure!(
            directory != crate::pathx::clean(Path::new(&state.channel.dir)),
            "the channel directory cannot be removed"
        );
        ensure!(
            !state.config.dirs.iter().any(|dir| scope::resolve_directory(
                Path::new(&state.channel.dir),
                &dir.to_string_lossy()
            ) == directory),
            "{} comes from stavlos.json's dirs: change it there",
            directory.display()
        );
        ensure!(
            state
                .channel
                .dirs
                .iter()
                .any(|dir| Path::new(&dir.path) == directory),
            "{} is not one of the channel's directories",
            directory.display()
        );
        self.core.commit_locked(
            &mut state,
            vec![self.core.event(
                "",
                event::CHANNEL_DIR_REMOVED,
                event::DirPayload {
                    dir: directory.to_string_lossy().into_owned(),
                    source: String::new(),
                },
            )?],
            cancel,
        )
    }
    pub async fn set_directory(
        &self,
        directory: &str,
        load: impl FnOnce(&Path) -> Result<Arc<config::Effective>>,
        cancel: &CancellationToken,
    ) -> Result<()> {
        let old = {
            let mut state = self.core.state.lock().unwrap();
            ensure!(
                !state.stopped && !state.channel.archived,
                "cannot change the directory of a stopped or archived channel"
            );
            ensure!(
                !state.reconfiguring,
                "a directory change is already in progress"
            );
            let running = self.agents.lock().unwrap();
            for (id, agent) in &state.channel.agents {
                if !agent.killed {
                    let active = running
                        .get(id)
                        .is_some_and(|entry| entry.turn.is_some() || entry.maintenance > 0);
                    ensure!(
                        !agent.busy()
                            && !agent.compacting
                            && agent.jobs.is_empty()
                            && agent.asks.is_empty()
                            && !active,
                        "channel must be idle before changing its default directory: finish or cancel active turns, jobs and prompts"
                    );
                }
            }
            state.reconfiguring = true;
            PathBuf::from(&state.channel.dir)
        };
        let _transition = Transition(self.core.clone());
        let directory = config::effective::working_directory(
            &old,
            directory,
            &self.environment.home,
            &|key| std::env::var(key).ok(),
        )?;
        if directory == old {
            return Ok(());
        }
        let config = load(&directory)?;
        ensure!(!cancel.is_cancelled(), "context canceled");
        let actors = self
            .agents
            .lock()
            .unwrap()
            .values()
            .map(|entry| entry.agent.clone())
            .collect::<Vec<_>>();
        for actor in &actors {
            actor.mcp.disarm_idle();
            actor.mcp.stop(None, false).await;
        }
        let stamp = control::trust_stamp(&config);
        let mut state = self.core.state.lock().unwrap();
        let mut events = vec![self.core.event(
            "",
            event::CHANNEL_UPDATED,
            event::ChannelUpdatedPayload {
                dir: Some(directory.to_string_lossy().into_owned()),
                ..Default::default()
            },
        )?];
        for id in &state.channel.order {
            if !state.channel.agents[id].killed {
                events.push(self.core.event(id,event::INPUT_QUEUED,event::Input {id:names::new_id("i")?,kind:event::INPUT_INFO.into(),text:format!("The human changed this channel's default directory from {} to {}. Earlier relative paths refer to the old directory. Project configuration and instructions have been reloaded; remembered permissions were cleared and mode is now ask.",old.display(),directory.display()),..Default::default()})?);
            }
        }
        self.core.commit_locked(&mut state, events, cancel)?;
        state.config = config;
        *self.stamp.lock().unwrap() = stamp;
        for actor in actors {
            actor.prefix.lock().unwrap().clear();
            actor.conversation.instructions.clear();
        }
        Ok(())
    }
    pub fn maybe_resume(&self, now: Time, cancel: &CancellationToken) -> Result<()> {
        let config = {
            let state = self.core.state.lock().unwrap();
            if !state
                .channel
                .agents
                .values()
                .any(|agent| parked(agent, now))
            {
                return Ok(());
            }
            state.config.clone()
        };
        let market = pick::Market::read(self.environment.models.as_ref(), &config, &[]);
        let mut state = self.core.state.lock().unwrap();
        if state.reconfiguring {
            return Ok(());
        }
        let mut events = Vec::new();
        for id in &state.channel.order {
            let agent = &state.channel.agents[id];
            if !parked(agent, now) {
                continue;
            }
            let view = prompt::role_view(&state.channel, id, &state.config).unwrap();
            if !market.available(&view.definition, &state.config, &agent.model) {
                continue;
            }
            events.push(self.core.event(
                id,
                event::INPUT_QUEUED,
                event::Input {
                    id: names::new_id("i")?,
                    kind: event::INPUT_RESUME.into(),
                    text: pick::resume_text(&agent.resume_why).into(),
                    ..Default::default()
                },
            )?);
        }
        self.core.commit_locked(&mut state, events, cancel)
    }
    pub fn tree(&self) -> Vec<protocol::AgentInfo> {
        let state = self.core.state.lock().unwrap();
        let actors = self.agents.lock().unwrap();
        routing::preorder(&state.channel)
            .into_iter()
            .filter_map(|id| actors.get(&id).map(|entry| entry.agent.info_locked(&state)))
            .collect()
    }
    pub fn info(&self) -> protocol::ChannelInfo {
        let state = self.core.state.lock().unwrap();
        let channel = &state.channel;
        let mut live = 0;
        let mut tokens = 0;
        let mut cost = 0.;
        let mut statuses = Vec::new();
        for id in &channel.order {
            let agent = &channel.agents[id];
            statuses.push(channel.status(id));
            cost += agent.cost;
            tokens += agent.tokens;
            if !agent.killed {
                live += 1;
            }
        }
        protocol::ChannelInfo {
            board: channel.board.clone(),
            id: self.core.id.clone(),
            name: channel.name.clone(),
            dir: channel.dir.clone(),
            dir_error: directory_error(&channel.dir),
            model: channel.model.clone(),
            root_agent: channel.role.clone(),
            created: self
                .core
                .created
                .0
                .to_rfc3339_opts(chrono::SecondsFormat::Secs, true),
            archived: channel.archived,
            live_agents: live,
            cost_usd: cost,
            tokens,
            trust_pending: state.config.trust_pending,
            trust_files: state.config.trust_files.len() as i64,
            sandbox: if !state.config.sandbox.enabled {
                "off"
            } else {
                match self.environment.sandbox_level {
                    sandbox::Level::Full => "full",
                    sandbox::Level::Landlock => "limited",
                    sandbox::Level::None => "none",
                }
            }
            .into(),
            mode: channel.mode.clone(),
            state: protocol::roll_up(statuses).into(),
            dirs: scope::directory_infos(channel, &state.config).into(),
            recap: channel.recap,
            budget: Some(protocol::ChannelBudget {
                calls: channel.model_calls,
                tokens: channel.budget_tokens,
                cost_usd: channel.budget_cost,
                exhausted: self
                    .core
                    .budget_reason_locked(&state, chrono::Utc::now().fixed_offset())
                    .into(),
            }),
            ..Default::default()
        }
    }
    pub fn roles(&self) -> Vec<protocol::RoleInfo> {
        self.core
            .configuration()
            .roles
            .values()
            .map(|role| protocol::RoleInfo {
                name: role.name.clone(),
                description: role.description.clone(),
                r#type: role.kind.clone(),
                models: role
                    .models
                    .iter()
                    .map(|model| protocol::ModelSpec {
                        id: model.id.clone(),
                        variants: model.variants.clone().into(),
                    })
                    .collect(),
                spawn: role.spawn.clone().into(),
                color: role.color.clone(),
                max_turns: role.max_turns,
            })
            .collect()
    }
}
struct Transition(Arc<Core>);
impl Drop for Transition {
    fn drop(&mut self) {
        let mut state = self.0.state.lock().unwrap();
        state.reconfiguring = false;
        for wake in state.wakes.values() {
            wake.notify_one();
        }
    }
}
struct Maintenance {
    channel: Weak<Channel>,
    agent: String,
}
impl Drop for Maintenance {
    fn drop(&mut self) {
        if let Some(channel) = self.channel.upgrade() {
            let _state = channel.core.state.lock().unwrap();
            if let Some(entry) = channel.agents.lock().unwrap().get_mut(&self.agent) {
                entry.maintenance = entry.maintenance.saturating_sub(1);
            }
        }
    }
}
impl Actor {
    pub fn model_id(&self) -> String {
        self.core
            .inspect(|state, _| state.agents[&self.id].model.clone())
    }
    pub fn name(&self) -> String {
        self.core
            .inspect(|state, _| state.agents[&self.id].name.clone())
    }
    pub fn alive(&self) -> bool {
        self.core.inspect(|state, _| {
            state
                .agents
                .get(&self.id)
                .is_some_and(|agent| !agent.killed)
        })
    }
    pub fn info(&self) -> protocol::AgentInfo {
        let state = self.core.state.lock().unwrap();
        self.info_locked(&state)
    }
    fn info_locked(&self, state: &super::super::journal::State) -> protocol::AgentInfo {
        let agent = &state.channel.agents[&self.id];
        let view = prompt::role_view(&state.channel, &self.id, &state.config).unwrap();
        let (tokens, window) = self.conversation.gauge();
        protocol::AgentInfo {
            id: self.id.clone(),
            channel: self.core.id.clone(),
            parent: self.parent.clone(),
            role: agent.role.clone(),
            name: agent.name.clone(),
            model: agent.model.clone(),
            variant: agent.variant.clone(),
            depth: self.depth,
            state: state.channel.status(&self.id).into(),
            turn: agent.turn,
            directories: scope::agent_directories(&state.channel, &state.config, &self.id)
                .iter()
                .map(|path| path.to_string_lossy().into_owned())
                .collect(),
            queued: agent.inbox.len() as i64,
            cost_usd: agent.cost,
            tokens: agent.tokens,
            context: tokens,
            context_window: window,
            last_error: if view.missing && agent.last_error.is_empty() {
                format!(
                    "role {} no longer exists: running read-only until /role picks another",
                    quoted(&agent.role)
                )
            } else {
                agent.last_error.clone()
            },
            awaiting: state.channel.awaiting_ids(&self.id).into(),
            due: state.channel.due(&self.id).into(),
            todos: agent.todos.clone().into(),
            pending_replies: state.channel.pending_replies(&self.id).into(),
            awaiting_replies: state.channel.awaiting_replies(&self.id).into(),
            nudges: agent.nudges,
            nudge_limit: super::super::state::nudge_limit(state.config.reminders),
            mcp: self.mcp.info(&view.definition.mcp).into(),
            jobs: self.jobs.infos().into(),
        }
    }
    pub fn set_variant(&self, variant: &str, cancel: &CancellationToken) -> Result<()> {
        let channel = self.channel()?;
        let model = self.model_id();
        let current = self
            .core
            .inspect(|state, _| state.agents[&self.id].variant.clone());
        if current == variant {
            return Ok(());
        }
        ensure!(
            variant.is_empty()
                || channel
                    .environment
                    .models
                    .variants(&model)
                    .iter()
                    .any(|offered| offered == variant),
            "unknown variant {} for {model} (see /variants)",
            quoted(variant)
        );
        let role = self.role()?.definition;
        ensure!(
            role.allows_variant(&model, variant),
            "role {} does not allow variant {} for {model} (allowed: {})",
            role.name,
            quoted(variant),
            role.variants(&model).join(", ")
        );
        self.core.commit(
            vec![self.core.event(
                &self.id,
                event::AGENT_UPDATED,
                event::AgentUpdatedPayload {
                    variant: Some(variant.into()),
                    ..Default::default()
                },
            )?],
            cancel,
        )
    }
    pub fn set_model(&self, id: &str, cancel: &CancellationToken) -> Result<()> {
        let channel = self.channel()?;
        channel.environment.models.check(id)?;
        let market = pick::Market::read(
            channel.environment.models.as_ref(),
            &self.core.configuration(),
            &[id.into()],
        );
        let mut state = self.core.state.lock().unwrap();
        let agent = &state.channel.agents[&self.id];
        let role = prompt::role_view(&state.channel, &self.id, &state.config)
            .unwrap()
            .definition;
        ensure!(
            role.allows_model(id),
            "role {} does not allow model {id} (allowed: {})",
            role.name,
            role.models
                .iter()
                .map(|model| model.id.as_str())
                .collect::<Vec<_>>()
                .join(", ")
        );
        let update = market.retarget(agent, &role, "", id, "");
        if !changed(&update) {
            return Ok(());
        }
        self.core.commit_locked(
            &mut state,
            vec![self.core.event(&self.id, event::AGENT_UPDATED, update)?],
            cancel,
        )
    }
    pub fn set_role(&self, role: &str, cancel: &CancellationToken) -> Result<()> {
        let channel = self.channel()?;
        let market = pick::Market::read(
            channel.environment.models.as_ref(),
            &self.core.configuration(),
            &[self.model_id()],
        );
        let mut state = self.core.state.lock().unwrap();
        let definition = state
            .config
            .roles
            .get(role)
            .ok_or_else(|| anyhow::anyhow!("unknown role {} (see /roles)", quoted(role)))?;
        ensure!(
            !self.parent.is_empty() || definition.can_be_primary(),
            "role {} is subagent-only: the main agent cannot take it",
            quoted(role)
        );
        ensure!(
            self.parent.is_empty() || definition.can_be_subagent(),
            "role {} is primary-only: a subagent cannot take it",
            quoted(role)
        );
        let agent = &state.channel.agents[&self.id];
        let model =
            if !definition.allows_model(&agent.model) && !definition.default_model().is_empty() {
                definition.default_model()
            } else {
                &agent.model
            };
        let mut update = market.retarget(agent, definition, role, model, "");
        if agent.name == agent.role
            && role != agent.role
            && let Ok(name) = names::claim(role, role, &self.id, &state.channel.names)
        {
            update.name = Some(name);
        }
        if !changed(&update) {
            return Ok(());
        }
        self.core.commit_locked(
            &mut state,
            vec![self.core.event(&self.id, event::AGENT_UPDATED, update)?],
            cancel,
        )
    }
    pub async fn compact(&self, cancel: CancellationToken) -> Result<&'static str> {
        let channel = self.channel()?;
        let id = {
            let state = self.core.state.lock().unwrap();
            ensure!(!state.reconfiguring, "a directory change is in progress");
            let agent = &state.channel.agents[&self.id];
            ensure!(!agent.killed, "agent {} is killed", agent.name);
            if agent.in_turn {
                self.conversation.request_compaction();
                return Ok("queued");
            }
            ensure!(!agent.compacting, "a compaction is already running");
            let mut running = channel.agents.lock().unwrap();
            running
                .get_mut(&self.id)
                .ok_or_else(|| anyhow::anyhow!("agent not found"))?
                .maintenance += 1;
            agent.model.clone()
        };
        let _maintenance = Maintenance {
            channel: self.channel.clone(),
            agent: self.id.clone(),
        };
        ensure!(!id.is_empty(), NO_MODEL);
        let (model, info) = channel.environment.models.resolve(&id)?;
        self.conversation
            .compact(cancel, model.as_ref(), &info, true)
            .await?;
        let config = self.core.configuration();
        let view = self.role()?;
        let prefix = self.build_context(&channel, &config, &view)?;
        self.conversation
            .refresh_gauge(&prefix.system, &prefix.definitions, info.context_window);
        Ok("compacted")
    }
}
