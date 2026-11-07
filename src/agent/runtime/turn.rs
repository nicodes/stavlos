use super::*;
use crate::{
    agent::{
        journal::{BudgetExhausted, Stopped},
        services,
    },
    model,
};
struct Plan {
    config: Arc<config::Effective>,
    view: prompt::RoleView,
    id: String,
    variant: String,
    model: Arc<dyn model::Model>,
    info: model::Info,
    prefix: prompt::Prefix,
    history: Vec<model::Message>,
}
struct Turn {
    actor: Arc<Actor>,
    cancel: CancellationToken,
    number: i64,
    calls: i64,
    moves: i64,
    resume_at: Time,
    resume: String,
}
struct TurnScope {
    actor: Arc<Actor>,
    cancel: CancellationToken,
}
impl Drop for TurnScope {
    fn drop(&mut self) {
        self.cancel.cancel();
        if let Some(channel) = self.actor.channel.upgrade() {
            let _state = channel.core.state.lock().unwrap();
            if let Some(entry) = channel.agents.lock().unwrap().get_mut(&self.actor.id) {
                entry.turn = None;
            }
        }
    }
}
impl Actor {
    pub(super) async fn run(self: Arc<Self>) {
        loop {
            tokio::select! {_=self.lifetime.cancelled()=>return,_=self.wake.notified()=>{}}
            while !self.lifetime.is_cancelled() {
                let Some((number, cancel)) = self.begin_turn() else {
                    break;
                };
                let _scope = TurnScope {
                    actor: self.clone(),
                    cancel: cancel.clone(),
                };
                let mut turn = Turn {
                    actor: self.clone(),
                    cancel,
                    number,
                    calls: 0,
                    moves: 0,
                    resume_at: Time::default(),
                    resume: String::new(),
                };
                turn.run().await;
            }
        }
    }
    fn begin_turn(&self) -> Option<(i64, CancellationToken)> {
        let channel = self.channel.upgrade()?;
        let mut state = self.core.state.lock().unwrap();
        let agent = state.channel.agents.get(&self.id)?;
        if state.stopped
            || state.reconfiguring
            || agent.killed
            || agent.in_turn
            || !agent.starts_turn()
        {
            return None;
        }
        let number = agent.turn.checked_add(1)?;
        let ids = agent
            .inbox
            .iter()
            .map(|input| input.id.clone())
            .collect::<Vec<_>>();
        let events = vec![
            self.core
                .event(
                    &self.id,
                    event::TURN_STARTED,
                    event::TurnPayload { turn: number },
                )
                .ok()?,
            self.core
                .event(
                    &self.id,
                    event::INPUT_TAKEN,
                    event::InputTakenPayload {
                        turn: number,
                        ids: ids.into(),
                    },
                )
                .ok()?,
        ];
        self.core
            .commit_locked(&mut state, events, &CancellationToken::new())
            .ok()?;
        let cancel = self.lifetime.child_token();
        let mut agents = channel.agents.lock().unwrap();
        agents.get_mut(&self.id)?.turn = Some(cancel.clone());
        state.log_errors.remove(&self.id);
        self.repeat.lock().unwrap().reset();
        Some((number, cancel))
    }
    pub(super) fn build_context(
        &self,
        channel: &Channel,
        config: &Arc<config::Effective>,
        view: &prompt::RoleView,
    ) -> Result<prompt::Prefix> {
        let scope = permission::Scope::capture(&self.core, &self.id, &channel.environment.paths)?;
        let definitions = self.mcp.definitions();
        Ok(self.prefix.lock().unwrap().build(
            config,
            view,
            &prompt::Situation {
                id: &self.id,
                parent: &self.parent,
                directory: &scope.directory,
                directories: &scope.directories,
                home: &channel.environment.home,
            },
            &definitions,
        ))
    }
    fn launch_context(
        &self,
        channel: &Channel,
        config: &config::Effective,
        view: &prompt::RoleView,
    ) -> Result<mcp::LaunchContext> {
        let scope = permission::Scope::capture(&self.core, &self.id, &channel.environment.paths)?;
        Ok(mcp::LaunchContext {
            directory: scope.directory,
            pass_env: config.pass_env.clone(),
            sandbox: security::role_sandbox_spec(
                config,
                &scope.directories,
                &channel.environment.paths,
                &channel.environment.home,
                channel.environment.runtime.as_deref(),
                &self.core.id,
                view.definition.read_only,
            ),
            level: channel.environment.sandbox_level,
            helper: std::env::current_exe()?,
            readonly: view.definition.read_only,
            directories_revoked: !self.parent.is_empty() && scope.directories.is_empty(),
        })
    }
    fn arm_idle(self: &Arc<Self>) {
        let weak = Arc::downgrade(self);
        self.mcp.arm_idle(
            Duration::from_secs(600),
            Arc::new(move || {
                weak.upgrade().is_none_or(|actor| {
                    actor.core.inspect(|state, _| {
                        state.agents.get(&actor.id).is_none_or(|agent| {
                            !agent.in_turn && !agent.compacting && !agent.starts_turn()
                        })
                    })
                })
            }),
        );
    }
    fn take_mid_turn(&self, number: i64) -> Result<()> {
        let mut state = self.core.state.lock().unwrap();
        if let Some(error) = state.log_errors.remove(&self.id) {
            return Err(anyhow::anyhow!(error));
        }
        let ids = state
            .channel
            .agents
            .get(&self.id)
            .ok_or_else(|| anyhow::anyhow!("agent not found"))?
            .inbox
            .iter()
            .filter(|input| event::input_rule(&input.kind).mid_turn)
            .map(|input| input.id.clone())
            .collect::<Vec<_>>();
        if ids.is_empty() {
            return Ok(());
        }
        self.core.commit_locked(
            &mut state,
            vec![self.core.event(
                &self.id,
                event::INPUT_TAKEN,
                event::InputTakenPayload {
                    turn: number,
                    ids: ids.into(),
                },
            )?],
            &CancellationToken::new(),
        )
    }
    fn leave_limited_model(&self, channel: &Channel) {
        let id = self.model_id();
        let Ok((provider, _)) = model::split(&id) else {
            return;
        };
        let usage = channel.environment.models.plan_usage();
        let plan = pick::read_plan(
            usage.get(provider).unwrap_or(&model::PlanUsage::default()),
            chrono::Local::now().fixed_offset(),
        );
        if plan.limited {
            let _ = self.move_off(channel, provider, plan.until);
        }
    }
    fn move_off(&self, channel: &Channel, provider: &str, until: Time) -> Result<(bool, Time)> {
        let market = pick::Market::read(
            channel.environment.models.as_ref(),
            &self.core.configuration(),
            &[],
        );
        let mut state = self.core.state.lock().unwrap();
        let agent = &state.channel.agents[&self.id];
        let role = prompt::role_view(&state.channel, &self.id, &state.config)
            .unwrap()
            .definition;
        let (choice, soonest) = market.choose(&role, &state.config, provider);
        let Some(choice) = choice else {
            return Ok((false, soonest));
        };
        if choice.model == agent.model {
            return Ok((false, soonest));
        }
        let mut reason = format!("{provider} is at its limit");
        if !until.is_zero() {
            reason.push_str(&format!(
                " until {}",
                until.0.with_timezone(&chrono::Local).format("%H:%M")
            ));
        }
        reason.push_str("; ");
        reason.push_str(&choice.why);
        let update = market.retarget(agent, &role, "", &choice.model, &reason);
        self.core.commit_locked(
            &mut state,
            vec![self.core.event(&self.id, event::AGENT_UPDATED, update)?],
            &CancellationToken::new(),
        )?;
        Ok((true, soonest))
    }
    async fn report_turn_limit(&self, channel: &Channel, limit: i64) -> Result<()> {
        let (text, public) = {
            let mut state = self.core.state.lock().unwrap();
            let agent = &state.channel.agents[&self.id];
            let text = format!(
                "{} reached its turn limit of {limit} without answering; message it again only if you raise the limit in its role, or delegate elsewhere.",
                agent.name
            );
            let name = agent.name.clone();
            let public = state
                .channel
                .open_requests()
                .into_iter()
                .filter(|request| {
                    !request.request.channel.is_empty() && request.open.contains(&self.id)
                })
                .map(|request| {
                    (
                        request.request.channel.clone(),
                        vec![request.request.from.clone()],
                        vec![request.request.id.clone()],
                    )
                })
                .collect::<Vec<_>>();
            let mut events = Vec::new();
            for id in &state.channel.order {
                if id == &self.id || state.channel.agents[id].killed {
                    continue;
                }
                let ids = state
                    .channel
                    .open_requests()
                    .into_iter()
                    .filter(|request| {
                        request.request.from == *id
                            && request.open.contains(&self.id)
                            && request.request.channel.is_empty()
                    })
                    .map(|request| request.request.id.clone())
                    .collect::<Vec<_>>();
                if !ids.is_empty() {
                    events.push(self.core.event(
                        id,
                        event::INPUT_QUEUED,
                        event::Input {
                            id: names::new_id("i")?,
                            kind: event::INPUT_RESPONSE.into(),
                            text: text.clone(),
                            from: self.id.clone(),
                            from_name: name.clone(),
                            reply_to: ids.into(),
                            ..Default::default()
                        },
                    )?);
                }
            }
            let requests = state
                .channel
                .pending_replies(&self.id)
                .into_iter()
                .filter(|request| request.from == "user" && request.channel.is_empty())
                .collect::<Vec<_>>();
            if !requests.is_empty() {
                events.push(
                    self.core.event(
                        &self.id,
                        event::CHAT_MESSAGE,
                        event::ChatPayload {
                            from: name,
                            text: text.clone(),
                            kind: "response".into(),
                            reply_to: requests.iter().map(|request| request.id.clone()).collect(),
                            posts: requests
                                .into_iter()
                                .map(|request| request.post)
                                .filter(|post| !post.is_empty())
                                .collect(),
                            ..Default::default()
                        },
                    )?,
                );
            }
            let _ = self
                .core
                .fact_locked(&mut state, events, &CancellationToken::new());
            (text, public)
        };
        if let Some(host) = &channel.environment.boards {
            for (board, targets, reply) in public {
                let _ = host
                    .report(
                        &self.core.id,
                        &self.id,
                        &board,
                        targets,
                        text.clone(),
                        reply,
                    )
                    .await;
            }
        }
        Ok(())
    }
}
impl Turn {
    async fn run(&mut self) {
        let Ok(channel) = self.actor.channel() else {
            return;
        };
        self.actor.mcp.disarm_idle();
        channel.check_project();
        if let Ok(view) = self.actor.role()
            && !self.actor.parent.is_empty()
            && view.definition.max_turns > 0
            && self.number > view.definition.max_turns
        {
            self.end(
                event::REASON_ERROR,
                format!(
                    "turn limit reached: {} may take at most {} turns",
                    view.name, view.definition.max_turns
                ),
            );
            let _ = self
                .actor
                .report_turn_limit(&channel, view.definition.max_turns)
                .await;
            return;
        }
        loop {
            let cap = self.actor.core.configuration().limits.max_calls_per_turn;
            if cap > 0 && self.calls >= cap {
                self.end(event::REASON_ERROR,format!("stopped after {cap} model calls in one turn (limits.maxCallsPerTurn). To wait for something, run the command with until_changed: true, or end the turn: a job or a child wakes you."));
                return;
            }
            if self.cancel.is_cancelled() {
                self.end(event::REASON_CANCELLED, String::new());
                return;
            }
            let plan = match self.prepare(&channel).await {
                Ok(plan) => plan,
                Err(error) => {
                    self.end(event::REASON_ERROR, error.to_string());
                    return;
                }
            };
            let (response, error) = self.call(&plan).await;
            self.calls += 1;
            let mut payload = event::AssistantMessagePayload {
                turn: self.number,
                blocks: response.blocks.clone().into(),
                stop_reason: response.stop_reason.clone(),
                model: plan.id.clone(),
                usage: response.usage.clone(),
                cost_usd: plan.info.cost(&response.usage),
            };
            if let Some(error) = error {
                if error.is::<BudgetExhausted>() {
                    self.end(event::REASON_ERROR, error.to_string());
                    return;
                }
                let cancelled = self.cancel.is_cancelled();
                if !cancelled && self.moved_on(&channel, &error, &plan.id) {
                    continue;
                }
                let typed = error.downcast_ref::<model::CallError>();
                if !cancelled
                    && self.resume_at.is_zero()
                    && let Some(typed) = typed
                {
                    let at = self.actor.core.inspect(|state, config| {
                        pick::park_after_fault(
                            typed,
                            config.resume_after_limit,
                            state.agents[&self.actor.id].resumes,
                            Time::now(),
                        )
                    });
                    if !at.is_zero() {
                        self.resume_at = at;
                        self.resume = event::RESUME_FAULT.into();
                    }
                }
                if !response.blocks.is_empty()
                    || [
                        response.usage.input_tokens,
                        response.usage.output_tokens,
                        response.usage.cache_read_tokens,
                        response.usage.cache_write_tokens,
                    ]
                    .iter()
                    .any(|count| *count != 0)
                {
                    if cancelled {
                        payload.stop_reason = "cancelled".into();
                    }
                    if let Ok(event) =
                        self.actor
                            .core
                            .event(&self.actor.id, event::ASSISTANT_MESSAGE, payload)
                    {
                        let _ = self.actor.core.record(&self.actor.id, vec![event]);
                    }
                }
                if cancelled {
                    self.end(event::REASON_CANCELLED, String::new());
                } else {
                    self.end(event::REASON_ERROR, resume_hint(&error, self.resume_at));
                }
                return;
            }
            let recorded = self
                .actor
                .core
                .event(&self.actor.id, event::ASSISTANT_MESSAGE, payload)
                .and_then(|event| self.actor.core.record(&self.actor.id, vec![event]));
            if let Err(error) = recorded {
                self.end(event::REASON_ERROR, error.to_string());
                return;
            }
            let mut any = false;
            for call in &response.blocks {
                if call.kind != model::BLOCK_TOOL_USE {
                    continue;
                }
                any = true;
                if !self.cancel.is_cancelled() {
                    self.actor
                        .execute(
                            self.cancel.clone(),
                            self.number,
                            call,
                            &plan.prefix.definitions,
                            &plan.view,
                            &plan.config,
                        )
                        .await;
                }
            }
            if !any {
                let reason = if response.stop_reason == model::STOP_MAX_TOKENS {
                    event::REASON_MAX_TOKENS
                } else {
                    event::REASON_END_TURN
                };
                self.end(reason, String::new());
                let _ = (routing::Routing {
                    core: self.actor.core.clone(),
                })
                .end_replies(&self.actor.id, reason);
                return;
            }
        }
    }
    async fn prepare(&self, channel: &Channel) -> Result<Plan> {
        let directory = self.actor.core.inspect(|state, _| state.dir.clone());
        let error = super::settings::directory_error(&directory);
        ensure!(error.is_empty(), "{error}");
        self.actor.take_mid_turn(self.number)?;
        self.actor.leave_limited_model(channel);
        let (config, view, id, variant) = {
            let state = self.actor.core.state.lock().unwrap();
            let agent = &state.channel.agents[&self.actor.id];
            (
                state.config.clone(),
                prompt::role_view(&state.channel, &self.actor.id, &state.config).unwrap(),
                agent.model.clone(),
                agent.variant.clone(),
            )
        };
        ensure!(!id.is_empty(), NO_MODEL);
        ensure!(
            view.definition.allows_model(&id) && view.definition.allows_variant(&id, &variant),
            "the current model or variant is not allowed by this project's role; use /models or /variants to select one"
        );
        let (model, info) = channel.environment.models.resolve(&id)?;
        if !view.definition.mcp.is_empty() || self.actor.mcp.has_servers() {
            let launch = self.actor.launch_context(channel, &config, &view)?;
            self.actor
                .mcp
                .ensure(
                    &config.mcp,
                    &view.definition.mcp,
                    &launch,
                    self.actor.lifetime.clone(),
                )
                .await;
        }
        let prefix = self.actor.build_context(channel, &config, &view)?;
        let history = self
            .actor
            .conversation
            .prepare(
                self.cancel.clone(),
                model.as_ref(),
                &info,
                &prefix.system,
                &prefix.definitions,
            )
            .await;
        let note = self
            .actor
            .core
            .inspect(|state, _| prompt::state_note(state, &self.actor.id, &view, &config));
        let history = self.actor.conversation.with_notes(history, &note);
        Ok(Plan {
            config,
            view,
            id,
            variant,
            model,
            info,
            prefix,
            history,
        })
    }
    async fn call(&self, plan: &Plan) -> (model::Response, Option<anyhow::Error>) {
        let scope = match self
            .actor
            .core
            .begin_call(&self.actor.id, "turn", &self.cancel)
            .await
        {
            Ok(scope) => scope,
            Err(error) => return (model::Response::default(), Some(error)),
        };
        let core = self.actor.core.clone();
        let agent = self.actor.id.clone();
        let number = self.number;
        let on_delta = Arc::new(move |delta: model::Delta| {
            core.stream(protocol::StreamNotification {
                channel: core.id.clone(),
                agent: agent.clone(),
                turn: number,
                text: delta.text,
                thinking: delta.thinking,
                tool_name: delta.tool_name,
                reset: delta.reset,
            });
        });
        let result = plan
            .model
            .complete(
                model::Request {
                    model: super::super::conversation::bare_id(&plan.id).into(),
                    system: plan.prefix.system.clone(),
                    messages: plan.history.clone(),
                    tools: plan.prefix.definitions.clone(),
                    variant: plan.variant.clone(),
                    cache_key: self.actor.id.clone(),
                    ..Default::default()
                },
                Some(on_delta),
                scope.cancel.clone(),
            )
            .await;
        let (response, mut failure) = match result {
            Ok(response) => (response, None),
            Err(failure) => (failure.response, Some(anyhow::anyhow!(failure.error))),
        };
        if let Err(error) = self.actor.core.finish_call(
            &self.actor.id,
            "turn",
            response.usage.clone(),
            plan.info.cost(&response.usage),
        ) {
            failure = Some(error);
        }
        (response, failure)
    }
    fn moved_on(&mut self, channel: &Channel, error: &anyhow::Error, id: &str) -> bool {
        let Some(model::CallError::Limit { retry_after, .. }) =
            error.downcast_ref::<model::CallError>()
        else {
            return false;
        };
        if self.moves >= pick::MAX_MOVES {
            return false;
        }
        let Ok((provider, _)) = model::split(id) else {
            return false;
        };
        let now = Time::now();
        let mut until = Time(now.0 + chrono::Duration::nanoseconds(pick::LIMITED_FOR_NS));
        if !retry_after.is_zero() {
            until = Time(
                now.0
                    + chrono::Duration::nanoseconds(
                        retry_after.as_nanos().min(i64::MAX as u128) as i64
                    ),
            );
        } else {
            let usage = channel.environment.models.plan_usage();
            let plan = pick::read_plan(
                usage.get(provider).unwrap_or(&model::PlanUsage::default()),
                now.0,
            );
            if plan.limited && plan.until.0 > now.0 {
                until = plan.until;
            }
        }
        channel.environment.models.mark_limited(provider, until);
        let (moved, soonest) = self
            .actor
            .move_off(channel, provider, until)
            .unwrap_or((false, Time::default()));
        if moved {
            self.moves += 1;
            if self.actor.conversation.gauge().0 > pick::COMPACT_BEFORE_MOVE {
                self.actor.conversation.request_compaction();
            }
            true
        } else {
            self.resume_at = self.actor.core.inspect(|state, config| {
                pick::park_until(
                    soonest,
                    until,
                    config.resume_after_limit,
                    state.agents[&self.actor.id].resumes,
                    now,
                )
            });
            self.resume = event::RESUME_LIMIT.into();
            false
        }
    }
    fn end(&self, reason: &str, error: String) {
        let event = self.actor.core.event(
            &self.actor.id,
            event::TURN_ENDED,
            event::TurnEndedPayload {
                turn: self.number,
                reason: reason.into(),
                error,
                resume_at: if reason == event::REASON_ERROR {
                    self.resume_at
                } else {
                    Time::default()
                },
                resume: if reason == event::REASON_ERROR {
                    self.resume.clone()
                } else {
                    String::new()
                },
            },
        );
        {
            let mut state = self.actor.core.state.lock().unwrap();
            if let Err(error) = event.and_then(|event| {
                self.actor
                    .core
                    .fact_locked(&mut state, vec![event], &CancellationToken::new())
            }) && !error.is::<Stopped>()
            {
                if let Some(agent) = state.channel.agents.get_mut(&self.actor.id) {
                    agent.last_error = format!(
                        "the turn ended but could not be logged: {}",
                        error.to_string().trim_start_matches("event log: ")
                    );
                }
                state.log_errors.remove(&self.actor.id);
            }
            // A reader that observes the ended turn can immediately perform an idle-only edit.
            if let Some(channel) = self.actor.channel.upgrade()
                && let Some(entry) = channel.agents.lock().unwrap().get_mut(&self.actor.id)
                && let Some(cancel) = entry.turn.take()
            {
                cancel.cancel();
            }
        }
        self.actor.arm_idle();
    }
}
fn resume_hint(error: &anyhow::Error, at: Time) -> String {
    match error.downcast_ref::<model::CallError>() {
        Some(model::CallError::Limit { .. }) => format!(
            "{error} (this model's plan is at its limit, and no other model this agent's role may use is available: list more under models in the role or in stavlos.toml, or pick one with /models{})",
            if at.is_zero() {
                String::new()
            } else {
                format!(
                    "; the agent carries on by itself once a model is back, about {}",
                    at.0.with_timezone(&chrono::Local).format("%a %H:%M")
                )
            }
        ),
        Some(model::CallError::Transient(_)) => format!(
            "{error} (a passing fault at the provider, already retried{})",
            if at.is_zero() {
                String::new()
            } else {
                format!(
                    "; the agent tries again by itself in about {}",
                    services::format_duration(
                        at.0.signed_duration_since(chrono::Local::now())
                            .to_std()
                            .unwrap_or(Duration::ZERO)
                    )
                )
            }
        ),
        _ => error.to_string(),
    }
}
