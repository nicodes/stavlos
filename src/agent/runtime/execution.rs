use super::super::{
    services::{AskService, JobApi, TodoService},
    sheets::SheetService,
};
use super::*;
use crate::{model, policy, toolfs, toolname};
struct ToolContext<'a> {
    channel: &'a Channel,
    scope: &'a permission::Scope,
    cancel: CancellationToken,
    turn: i64,
    call: &'a model::Block,
    subject: &'a policy::Subject,
    view: &'a prompt::RoleView,
    config: &'a config::Effective,
    before: Option<control::ControlStamp>,
}
impl Actor {
    fn finish_tool(
        &self,
        turn: i64,
        call: &model::Block,
        output: tools::Output,
        cancelled: bool,
        denied: bool,
        carried: Vec<String>,
    ) {
        if let Ok(event) = self.core.event(
            &self.id,
            event::TOOL_FINISHED,
            event::ToolFinishedPayload {
                turn,
                call_id: call.id.clone(),
                name: call.name.clone(),
                output: output.output,
                is_error: output.is_error,
                cancelled,
                denied,
                instructions: carried.into(),
            },
        ) {
            let _ = self.core.record(&self.id, vec![event]);
        }
    }
    fn asker(&self, channel: &Channel) -> AskService {
        AskService {
            core: self.core.clone(),
            agent: self.id.clone(),
            manager: channel.environment.prompts.clone(),
        }
    }
    pub(super) async fn execute(
        &self,
        cancel: CancellationToken,
        turn: i64,
        call: &model::Block,
        definitions: &[model::ToolDef],
        view: &prompt::RoleView,
        config: &Arc<config::Effective>,
    ) {
        let started = self
            .core
            .event(
                &self.id,
                event::TOOL_STARTED,
                event::ToolStartedPayload {
                    turn,
                    call_id: call.id.clone(),
                    name: call.name.clone(),
                },
            )
            .and_then(|event| self.core.record(&self.id, vec![event]));
        if started.is_err() {
            return;
        }
        let channel = match self.channel() {
            Ok(channel) => channel,
            Err(error) => {
                self.finish_tool(
                    turn,
                    call,
                    tools::Output::error(error.to_string()),
                    true,
                    false,
                    Vec::new(),
                );
                return;
            }
        };
        let builtin = tools::definitions()
            .iter()
            .any(|definition| definition.name == call.name);
        let advertised = definitions
            .iter()
            .any(|definition| definition.name == call.name);
        if !advertised
            || !builtin
                && !self
                    .mcp
                    .definitions()
                    .iter()
                    .any(|definition| definition.name == call.name)
        {
            self.finish_tool(
                turn,
                call,
                tools::Output::error(format!(
                    "unknown tool {}",
                    serde_json::to_string(&call.name).unwrap()
                )),
                false,
                false,
                Vec::new(),
            );
            return;
        }
        if call.name == toolname::AGENT
            && !prompt::can_orchestrate(view)
            && toolname::operation_raw(&call.name, call.input.as_deref()) != toolname::AGENT_STATUS
        {
            self.finish_tool(
                turn,
                call,
                tools::Output::error("this role may only inspect agent status"),
                false,
                true,
                Vec::new(),
            );
            return;
        }
        let admitted = self
            .admit(&channel, cancel.clone(), call, view, config)
            .await;
        let decision = match admitted {
            Ok(Some(decision)) => decision,
            Ok(None) => return,
            Err(error) => {
                self.finish_tool(
                    turn,
                    call,
                    tools::Output::error(error.to_string()),
                    false,
                    true,
                    Vec::new(),
                );
                return;
            }
        };
        let sheet = SheetService {
            core: self.core.clone(),
            agent: self.id.clone(),
            directory: super::super::sheets::directory(&channel.environment.paths, &self.core.id),
        };
        let scope =
            match permission::Scope::capture(&self.core, &self.id, &channel.environment.paths) {
                Ok(scope) => scope,
                Err(error) => {
                    self.finish_tool(
                        turn,
                        call,
                        tools::Output::error(error.to_string()),
                        false,
                        true,
                        Vec::new(),
                    );
                    return;
                }
            };
        let sheet_guard = if call.name == toolname::PATCH {
            match sheet.guard(&decision.subject, &scope.directory) {
                Ok(guard) => guard,
                Err(error) => {
                    self.finish_tool(
                        turn,
                        call,
                        tools::Output::error(error.to_string()),
                        false,
                        true,
                        Vec::new(),
                    );
                    return;
                }
            }
        } else {
            None
        };
        let before = if call.name == toolname::SHELL {
            Some(control::channel_control_stamp(
                &self.core,
                &channel.environment.paths,
            ))
        } else {
            None
        };
        let environment = self.tool_environment(ToolContext {
            channel: &channel,
            scope: &scope,
            cancel: cancel.clone(),
            turn,
            call,
            subject: &decision.subject,
            view,
            config,
            before: before.clone(),
        });
        let mut output = if builtin {
            tools::run(
                &call.name,
                call.input.as_deref(),
                &environment,
                cancel.clone(),
            )
            .await
        } else {
            self.mcp
                .call(
                    &call.name,
                    call.input.as_deref(),
                    &environment,
                    cancel.clone(),
                )
                .await
                .unwrap_or_else(|| {
                    tools::Output::error(format!(
                        "unknown tool {}",
                        serde_json::to_string(&call.name).unwrap()
                    ))
                })
        };
        if let Some(before) = before {
            let note = control::control_note(&before);
            if !note.is_empty() {
                output.output.push_str("\n\n");
                output.output.push_str(&note);
            }
        }
        if !output.is_error
            && let Some(guard) = sheet_guard
        {
            match guard.finish() {
                Ok(note) if !note.is_empty() => output = tools::Output::error(note),
                Err(error) => output = tools::Output::error(error.to_string()),
                _ => {}
            }
        }
        if cancel.is_cancelled() {
            output.is_error = true;
            self.finish_tool(turn, call, output, true, false, Vec::new());
            return;
        }
        if !output.is_error && call.name == toolname::PATCH {
            sheet.patched(&decision.subject, &scope.directory);
        }
        if !output.is_error && call.name == toolname::READ {
            output.output = self.conversation.deduplicate_read(call, &output.output);
        }
        let mut carried = Vec::new();
        if !output.is_error {
            let attachment = self.conversation.instructions.for_subject(
                &self.core,
                &self.id,
                &decision.subject,
                config,
                &scope.directory,
                &channel.environment.home,
            );
            if attachment.project_changed {
                let directory = self.core.inspect(|state, _| PathBuf::from(&state.dir));
                (channel.environment.project_changed)(&directory);
            }
            if !attachment.note.is_empty() {
                output.output.push_str("\n\n");
                output.output.push_str(&attachment.note);
                carried = attachment.carried;
            }
        }
        self.finish_tool(turn, call, output, false, false, carried);
    }
    async fn admit(
        &self,
        channel: &Channel,
        cancel: CancellationToken,
        call: &model::Block,
        view: &prompt::RoleView,
        config: &config::Effective,
    ) -> Result<Option<permission::Decision>> {
        let scope = permission::Scope::capture(&self.core, &self.id, &channel.environment.paths)?;
        let decision = self.boundary(channel).decide(&scope, call, view, config)?;
        let turn = self.core.inspect(|state, _| state.agents[&self.id].turn);
        let repeat = decision.verb == policy::Verb::Allow
            && self.repeat.lock().unwrap().repeated(
                &call.name,
                call.input.as_ref().map_or("", |input| input.get()),
                &scope.mode,
            );
        if repeat {
            let answer = self
                .asker(channel)
                .request(
                    permission::repeat_prompt(&self.core, &self.id, call, view)?,
                    call.id.clone(),
                    cancel.clone(),
                    None,
                )
                .await;
            if answer.withdrawn {
                self.finish_tool(
                    turn,
                    call,
                    tools::Output::error(""),
                    true,
                    false,
                    Vec::new(),
                );
                return Ok(None);
            }
            if !matches!(
                answer.value.as_str(),
                protocol::ANSWER_ALLOW
                    | protocol::ANSWER_ALLOW_ALWAYS
                    | protocol::ANSWER_ALLOW_PREFIX
            ) {
                self.finish_tool(
                    turn,
                    call,
                    tools::Output::error(permission::repeat_denial(&call.name, &answer)),
                    false,
                    true,
                    Vec::new(),
                );
                return Ok(None);
            }
        }
        match decision.verb {
            policy::Verb::Allow => {}
            policy::Verb::Deny => {
                self.finish_tool(
                    turn,
                    call,
                    tools::Output::error(decision.refusal(&call.name)),
                    false,
                    true,
                    Vec::new(),
                );
                return Ok(None);
            }
            policy::Verb::Ask => {
                let answer = self
                    .asker(channel)
                    .request(
                        decision.prompt(&self.core, &self.id, call, view)?,
                        call.id.clone(),
                        cancel,
                        None,
                    )
                    .await;
                match decision.answer(call, &scope, &answer)? {
                    permission::Admission::Withdrawn => {
                        self.finish_tool(
                            turn,
                            call,
                            tools::Output::error(""),
                            true,
                            false,
                            Vec::new(),
                        );
                        return Ok(None);
                    }
                    permission::Admission::Denied(text) => {
                        self.finish_tool(
                            turn,
                            call,
                            tools::Output::error(text),
                            false,
                            true,
                            Vec::new(),
                        );
                        return Ok(None);
                    }
                    permission::Admission::Allowed { grants, directory } => {
                        let events = grants
                            .into_iter()
                            .map(|permit| self.core.event(&self.id, event::PERMIT_GRANTED, permit))
                            .collect::<Result<Vec<_>>>()?;
                        if !events.is_empty() {
                            let _ = self.core.record(&self.id, events);
                        }
                        if let Some(directory) = directory {
                            let _ = channel.add_directory(
                                &self.id,
                                &directory,
                                "human",
                                &CancellationToken::new(),
                            );
                        }
                    }
                }
            }
        }
        Ok(Some(decision))
    }
    fn tool_environment(&self, context: ToolContext<'_>) -> tools::Env {
        let ToolContext {
            channel,
            scope,
            cancel,
            turn,
            call,
            subject,
            view,
            config,
            before,
        } = context;
        let judged = if subject.kind == policy::Kind::Path {
            subject
                .values
                .iter()
                .map(|value| {
                    (
                        value.clone(),
                        toolfs::resolve_path(&scope.directory, Path::new(value)),
                    )
                })
                .collect()
        } else {
            BTreeMap::new()
        };
        let core = self.core.clone();
        let agent = self.id.clone();
        let tool = call.name.clone();
        tools::Env {
            filesystem: toolfs::Filesystem {
                directory: scope.directory.clone(),
                roots: scope.file_roots(),
                judged,
            },
            skills: prompt::skills(config, view),
            agent: self.id.clone(),
            boards: channel
                .environment
                .boards
                .as_ref()
                .map(|host| host.tools(&self.core.id)),
            orchestrator: Some(Arc::new(Orchestrator {
                channel: self.channel.clone(),
            })),
            jobs: Some(Arc::new(JobApi {
                registry: self.jobs.clone(),
                turn: cancel,
                after: before.map(|stamp| {
                    Arc::new(move || control::control_note(&stamp))
                        as super::super::services::AfterCommand
                }),
            })),
            todo: view
                .definition
                .tools
                .iter()
                .any(|tool| tool == toolname::TODO)
                .then(|| {
                    Arc::new(TodoService {
                        core: self.core.clone(),
                        agent: self.id.clone(),
                    }) as Arc<dyn tools::Todos>
                }),
            ask: Some(Arc::new(self.asker(channel))),
            sheets: Some(Arc::new(SheetService {
                core: self.core.clone(),
                agent: self.id.clone(),
                directory: super::super::sheets::directory(
                    &channel.environment.paths,
                    &self.core.id,
                ),
            })),
            max_output: config.compaction.max_tool_output as i64,
            overflow: scope::scratch_directory(&channel.environment.paths, &self.core.id)
                .join("output"),
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
            search: tools::web::SearchConfig {
                provider: config.search.provider.clone(),
                api_key: config.search.api_key.clone(),
            },
            partial: Some(Arc::new(move |bytes| {
                core.stream(protocol::StreamNotification {
                    channel: core.id.clone(),
                    agent: agent.clone(),
                    turn,
                    tool_name: tool.clone(),
                    text: String::from_utf8_lossy(bytes).into_owned(),
                    ..Default::default()
                })
            })),
            ..Default::default()
        }
    }
}
struct Orchestrator {
    channel: Weak<Channel>,
}
impl tools::Orchestrator for Orchestrator {
    fn spawn<'a>(
        &'a self,
        request: tools::Spawn,
        cancel: CancellationToken,
    ) -> tools::ServiceFuture<'a, (String, String)> {
        Box::pin(async move {
            let channel = self
                .channel
                .upgrade()
                .ok_or_else(|| anyhow::anyhow!("channel is stopped"))?;
            let actor = channel.spawn(
                Spawn {
                    parent: request.parent,
                    role: request.archetype,
                    label: request.label,
                    task: request.task,
                    model: String::new(),
                    directories: request.directories,
                },
                &cancel,
            )?;
            Ok((actor.id.clone(), actor.name()))
        })
    }
    fn message(&self, caller: &str, message: tools::Delivery) -> Result<String> {
        let channel = self
            .channel
            .upgrade()
            .ok_or_else(|| anyhow::anyhow!("channel is stopped"))?;
        (routing::Routing {
            core: channel.core.clone(),
        })
        .message(caller, message)
    }
    fn cancel(&self, parent: &str, id: &str) -> Result<()> {
        let channel = self
            .channel
            .upgrade()
            .ok_or_else(|| anyhow::anyhow!("channel is stopped"))?;
        let target = channel
            .core
            .inspect(|state, _| {
                routing::resolve(state, id).map(|agent| (agent.id.clone(), agent.parent.clone()))
            })
            .ok_or_else(|| {
                anyhow::anyhow!("unknown agent {}", serde_json::to_string(id).unwrap())
            })?;
        ensure!(
            target.1 == parent,
            "agent {} is not your child",
            serde_json::to_string(id)?
        );
        channel.cancel(&target.0)
    }
    fn status(&self, parent: &str, id: &str) -> Result<Vec<tools::ChildStatus>> {
        let channel = self
            .channel
            .upgrade()
            .ok_or_else(|| anyhow::anyhow!("channel is stopped"))?;
        (routing::Routing {
            core: channel.core.clone(),
        })
        .status(parent, id)
    }
    fn can_spawn(&self, agent: &str) -> (bool, String) {
        self.channel
            .upgrade()
            .map(|channel| {
                channel.core.inspect(|state, config| {
                    state
                        .agents
                        .get(agent)
                        .map_or((false, "unknown agent".into()), |agent| {
                            prompt::can_spawn(state, agent, config)
                        })
                })
            })
            .unwrap_or((false, "channel is stopped".into()))
    }
    fn archetypes(&self, agent: &str) -> Vec<String> {
        self.channel
            .upgrade()
            .and_then(|channel| {
                channel
                    .core
                    .inspect(|state, config| prompt::role_view(state, agent, config))
            })
            .map(|view| view.definition.spawn)
            .unwrap_or_default()
    }
}
