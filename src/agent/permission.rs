//! Tool admission from a single scope snapshot; only explicit human answers grant access.
use super::{journal::Core, prompt::RoleView, scope, security, state::Permits};
use crate::{
    config::Effective,
    escalation::Answer,
    event, model,
    paths::Paths,
    policy::{Kind, Subject, Verb},
    protocol,
    sandbox::Level,
    toolname, tools,
};
use anyhow::{Result, ensure};
use std::path::PathBuf;
pub struct Scope {
    pub directory: PathBuf,
    pub directories: Vec<PathBuf>,
    pub channel_directories: Vec<PathBuf>,
    pub sheets: PathBuf,
    pub scratch: PathBuf,
    pub parent: String,
    pub mode: String,
    pub permits: Permits,
}
impl Scope {
    pub fn capture(core: &Core, agent: &str, paths: &Paths) -> Result<Self> {
        core.inspect(|state, config| {
            let agent = state
                .agents
                .get(agent)
                .ok_or_else(|| anyhow::anyhow!("agent not found"))?;
            let directories = scope::agent_directories(state, config, &agent.id);
            Ok(Self {
                directory: directories
                    .first()
                    .cloned()
                    .unwrap_or_else(|| state.dir.clone().into()),
                directories,
                channel_directories: scope::channel_directories(state, config),
                sheets: super::sheets::directory(paths, &core.id),
                scratch: scope::scratch_directory(paths, &core.id),
                parent: agent.parent.clone(),
                mode: state.mode.clone(),
                permits: state.permits.clone(),
            })
        })
    }
    pub fn file_roots(&self) -> Vec<PathBuf> {
        self.directories
            .iter()
            .cloned()
            .chain([self.sheets.clone(), self.scratch.clone()])
            .collect()
    }
}
pub struct BoundaryEnvironment {
    pub paths: Paths,
    pub home: PathBuf,
    pub runtime: Option<PathBuf>,
    pub uid: u32,
    pub level: Level,
}
pub struct Decision {
    pub subject: Subject,
    pub argument: String,
    pub verb: Verb,
    pub boundary: String,
    pub control: String,
    pub egress: bool,
    pub bare: bool,
    pub bare_why: String,
    pub why: String,
}
impl BoundaryEnvironment {
    pub fn decide(
        &self,
        scope: &Scope,
        call: &model::Block,
        view: &RoleView,
        config: &Effective,
    ) -> Result<Decision> {
        let subject = if call.name.starts_with(toolname::MCP_PREFIX) {
            super::mcp::subject(call.input.as_deref())
        } else {
            tools::subject(&call.name, call.input.as_deref())
        };
        let (ruled, mut argument) = config.policy.with(view.definition.policy()?).decide(
            &call.name,
            &tools::policy_subject(&call.name, call.input.as_deref(), subject.clone()),
        );
        if matches!(
            call.name.as_str(),
            toolname::AGENT | toolname::WEB | toolname::SHELL
        ) {
            argument = subject.primary().into();
        }
        let mut decision = Decision {
            subject,
            argument,
            verb: Verb::Deny,
            boundary: String::new(),
            control: String::new(),
            egress: false,
            bare: false,
            bare_why: String::new(),
            why: String::new(),
        };
        if view.definition.read_only {
            if matches!(call.name.as_str(), toolname::PATCH | toolname::SHEET) {
                decision.why = "This role is read-only; file changes are refused.".into();
                return Ok(decision);
            }
            if call.name == toolname::SHELL
                && (!config.sandbox.enabled || self.level != Level::Full)
            {
                decision.why = "Read-only shell execution requires the full OS sandbox.".into();
                return Ok(decision);
            }
        }
        let mut directories = scope.directories.clone();
        directories.push(scope.sheets.clone());
        decision.control = security::control_file(
            &call.name,
            &decision.subject,
            &scope.directory,
            &directories,
        )
        .unwrap_or_default();
        decision.boundary =
            scope::outside_directory(&decision.subject, &scope.directory, &directories)
                .map(|path| path.to_string_lossy().into_owned())
                .unwrap_or_default();
        let hidden = security::hidden_paths(
            config,
            &scope.channel_directories,
            &self.paths,
            &self.home,
            self.runtime.as_deref(),
            self.uid,
        );
        let hidden = security::hidden_from(
            &decision.subject,
            &scope.directory,
            &hidden,
            &[scope.sheets.clone(), scope.scratch.clone()],
        );
        if !scope.parent.is_empty()
            && (scope.directories.is_empty() || !decision.boundary.is_empty())
        {
            decision.why="This call is outside the agent's task directories. Delegate a new scoped task from the parent instead of widening a child.".into();
            return Ok(decision);
        }
        let operation = toolname::operation_raw(&call.name, call.input.as_deref());
        decision.bare = decision.subject.kind == Kind::Command
            && config.sandbox.enabled
            && self.level != Level::Full;
        decision.egress = security::egress(operation, &decision.subject);
        decision.verb = security::judge(security::Facts {
            hidden: hidden.is_some(),
            bare: decision.bare,
            ruled,
            compound: decision.subject.kind == Kind::Command
                && !crate::shellcmd::simple(&decision.argument),
            control: !decision.control.is_empty(),
            permitted: scope.permits.covers(operation, &decision.subject)
                || decision.subject.kind == Kind::Url
                    && security::hosts_allow(&config.hosts, &decision.subject.values),
            mode: &scope.mode,
            egress: decision.egress,
            outside: !decision.boundary.is_empty(),
        });
        if !decision.control.is_empty() && ruled == Verb::Allow {
            decision.argument = decision.control.clone();
        }
        if let Some(hidden) = hidden {
            decision.boundary.clear();
            decision.why = format!(
                "{} is hidden from agents in every mode: it holds credentials or stavlos's own state. Ask the human for what you need from it.",
                hidden.display()
            );
        } else if ruled == Verb::Deny {
            decision.boundary.clear();
        } else if decision.verb == Verb::Deny {
            decision.why = auto_outside(&decision.boundary);
        }
        if decision.bare {
            decision.bare_why=if self.level==Level::Landlock{"the sandbox on this system hides nothing: the command can reach your session's services and stavlos's socket"}else{"no sandbox on this system: the command runs with your full access"}.into();
        }
        Ok(decision)
    }
}
pub fn auto_outside(directory: &str) -> String {
    format!(
        "Denied in auto mode: {directory} is outside the channel's working directories, and auto mode does not allow calls outside them."
    )
}
pub fn denial_text(answer: &Answer, decision: &Decision) -> String {
    if answer.defaulted {
        return "Permission denied: nobody answered the prompt and the headless default is deny."
            .into();
    }
    if answer.client == protocol::MODE_AUTO && !decision.boundary.is_empty() {
        return auto_outside(&decision.boundary);
    }
    if !answer.reason.trim().is_empty() {
        return format!("Permission denied by the user: {}", answer.reason.trim());
    }
    "Permission denied by the user.".into()
}
impl Decision {
    pub fn refusal(&self, tool: &str) -> String {
        if self.why.is_empty() {
            format!("Denied by policy: {tool} {}", self.argument)
        } else {
            self.why.clone()
        }
    }
    pub fn prompt(
        &self,
        core: &Core,
        agent: &str,
        call: &model::Block,
        view: &RoleView,
    ) -> Result<protocol::PromptInfo> {
        let mut question = if self.boundary.is_empty() {
            format!("{} wants to run {}", view.name, call.name)
        } else {
            format!(
                "{} wants to run {} outside the channel's directories ({})",
                view.name, call.name, self.boundary
            )
        };
        let mut prefix = security::prefix_for(self.subject.kind, &self.argument);
        if self.bare {
            question.push_str(" · ");
            question.push_str(&self.bare_why);
            prefix.clear();
        }
        Ok(protocol::PromptInfo {
            id: super::names::new_id("p")?,
            channel: core.id.clone(),
            channel_name: core.inspect(|state, _| state.name.clone()),
            agent: agent.into(),
            from: view.name.clone(),
            role: view.role.clone(),
            kind: protocol::PROMPT_PERMISSION.into(),
            tool: call.name.clone(),
            input: call.input.clone(),
            question,
            dir: self.boundary.clone(),
            prefix,
            sticky: !self.control.is_empty() || self.bare,
            egress: self.egress,
            ..Default::default()
        })
    }
    pub fn answer(&self, call: &model::Block, scope: &Scope, answer: &Answer) -> Result<Admission> {
        if answer.withdrawn {
            return Ok(Admission::Withdrawn);
        }
        if !matches!(
            answer.value.as_str(),
            protocol::ANSWER_ALLOW | protocol::ANSWER_ALLOW_ALWAYS | protocol::ANSWER_ALLOW_PREFIX
        ) {
            return Ok(Admission::Denied(denial_text(answer, self)));
        }
        let prefix = if self.bare {
            String::new()
        } else {
            security::prefix_for(self.subject.kind, &self.argument)
        };
        let mut grants = Vec::new();
        if matches!(
            answer.value.as_str(),
            protocol::ANSWER_ALLOW_ALWAYS | protocol::ANSWER_ALLOW_PREFIX
        ) {
            let tool = toolname::operation_raw(&call.name, call.input.as_deref());
            if answer.value == protocol::ANSWER_ALLOW_PREFIX && !prefix.is_empty() {
                grants.push(event::PermitPayload {
                    tool: tool.into(),
                    prefix,
                    ..Default::default()
                });
            } else {
                grants.extend(
                    self.subject
                        .values
                        .iter()
                        .map(|value| event::PermitPayload {
                            tool: tool.into(),
                            call: value.clone(),
                            ..Default::default()
                        }),
                );
            }
        }
        let directory = if !self.boundary.is_empty() && answer.value != protocol::ANSWER_ALLOW {
            Some(if answer.dir.trim().is_empty() {
                PathBuf::from(&self.boundary)
            } else {
                scope::resolve_directory(&scope.directory, &answer.dir)
            })
        } else {
            None
        };
        ensure!(
            directory
                .as_ref()
                .is_none_or(|directory| !directory.as_os_str().is_empty()),
            "a directory is required"
        );
        Ok(Admission::Allowed { grants, directory })
    }
}
pub enum Admission {
    Allowed {
        grants: Vec<event::PermitPayload>,
        directory: Option<PathBuf>,
    },
    Denied(String),
    Withdrawn,
}
pub fn repeat_prompt(
    core: &Core,
    agent: &str,
    call: &model::Block,
    view: &RoleView,
) -> Result<protocol::PromptInfo> {
    Ok(protocol::PromptInfo {
        id: super::names::new_id("p")?,
        channel: core.id.clone(),
        channel_name: core.inspect(|state, _| state.name.clone()),
        agent: agent.into(),
        from: view.name.clone(),
        role: view.role.clone(),
        kind: protocol::PROMPT_PERMISSION.into(),
        tool: call.name.clone(),
        input: call.input.clone(),
        question: format!(
            "{} has made the same {} call, with the same arguments, 3 times in a row",
            view.name, call.name
        ),
        sticky: true,
        ..Default::default()
    })
}
pub fn repeat_denial(tool: &str, answer: &Answer) -> String {
    let mut denial = format!(
        "Denied: this is the same {tool} call, with the same arguments, for the 3rd time in a row. Do something different, or end the turn and let a job or a message wake you."
    );
    if answer.defaulted {
        denial = format!(
            "Denied: nobody answered the prompt and the headless default is deny. {denial}"
        );
    } else if !answer.reason.trim().is_empty() {
        denial.push_str(" The user says: ");
        denial.push_str(answer.reason.trim());
    }
    denial
}
