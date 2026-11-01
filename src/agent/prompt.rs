//! Stable prompt prefixes and the per-call state note. Text here instructs product agents only.
use super::state::{AgentState, ChannelState};
use crate::{
    config::{Effective, frontmatter::Role},
    instructions, model, project, toolname, tools,
};
use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::{Arc, OnceLock},
};
#[derive(Clone)]
pub struct RoleView {
    pub definition: Role,
    pub missing: bool,
    pub role: String,
    pub name: String,
    pub parent_name: String,
    pub turn: i64,
}
pub fn missing_role(name: &str) -> Role {
    Role{name:name.into(),description:"(role no longer exists)".into(),kind:"all".into(),layer:"builtin".into(),tools:vec![toolname::READ.into()],body:"Your role's definition is gone from the configuration. You can only read files until the human picks a role with /role; say so if asked to do more.".into(),..Default::default()}
}
pub fn role_view(state: &ChannelState, id: &str, cfg: &Effective) -> Option<RoleView> {
    let agent = state.agents.get(id)?;
    let role = cfg.roles.get(&agent.role);
    Some(RoleView {
        definition: role.cloned().unwrap_or_else(|| missing_role(&agent.role)),
        missing: role.is_none(),
        role: agent.role.clone(),
        name: agent.name.clone(),
        parent_name: state
            .agents
            .get(&agent.parent)
            .map_or(String::new(), |p| p.name.clone()),
        turn: agent.turn,
    })
}
pub fn can_orchestrate(view: &RoleView) -> bool {
    !view.definition.spawn.is_empty()
}
pub fn can_spawn(state: &ChannelState, agent: &AgentState, cfg: &Effective) -> (bool, String) {
    if agent.depth + 1 >= cfg.limits.max_depth {
        return (false, format!("max depth {} reached", cfg.limits.max_depth));
    }
    if state.agents.values().filter(|a| a.busy()).count() as i64 >= cfg.limits.max_agents {
        return (
            false,
            format!(
                "max busy agents {} reached (idle children do not count)",
                cfg.limits.max_agents
            ),
        );
    }
    if cfg
        .roles
        .get(&agent.role)
        .is_none_or(|r| r.spawn.is_empty())
    {
        return (false, "this archetype cannot spawn".into());
    }
    (true, String::new())
}
pub struct Situation<'a> {
    pub id: &'a str,
    pub parent: &'a str,
    pub directory: &'a Path,
    pub directories: &'a [PathBuf],
    pub home: &'a Path,
}
#[derive(Clone, Default)]
pub struct Prefix {
    pub system: String,
    pub definitions: Vec<model::ToolDef>,
}
#[derive(Default)]
pub struct PrefixCache {
    key: String,
    prefix: Prefix,
}
fn section(name: &str) -> &'static str {
    static SECTIONS: OnceLock<BTreeMap<String, String>> = OnceLock::new();
    &SECTIONS.get_or_init(|| {
        serde_json::from_str(include_str!("prompt_sections.json")).expect("prompt sections")
    })[name]
}
pub fn skills(cfg: &Effective, view: &RoleView) -> BTreeMap<String, tools::Skill> {
    view.definition
        .skills
        .iter()
        .filter_map(|name| {
            cfg.skills.get(name).map(|s| {
                (
                    name.clone(),
                    tools::Skill {
                        name: s.name.clone(),
                        description: s.description.clone(),
                        body: s.body.clone(),
                        directory: s.dir.clone(),
                    },
                )
            })
        })
        .collect()
}
impl PrefixCache {
    pub fn clear(&mut self) {
        self.key.clear();
        self.prefix = Prefix::default();
    }
    pub fn build(
        &mut self,
        cfg: &Arc<Effective>,
        view: &RoleView,
        situation: &Situation<'_>,
        mcp: &[model::ToolDef],
    ) -> Prefix {
        let mut key = format!(
            "{:p}\0{}\0{}\0{}\0{}",
            Arc::as_ptr(cfg),
            view.role,
            view.name,
            view.parent_name,
            view.missing
        );
        for directory in situation.directories {
            key.push('\0');
            key.push_str(&directory.to_string_lossy());
        }
        for definition in mcp {
            key.push('\x01');
            key.push_str(&definition.name);
        }
        if self.key == key {
            return self.prefix.clone();
        }
        let prefix = build_prefix(cfg, view, situation, mcp);
        self.key = key;
        self.prefix = prefix.clone();
        prefix
    }
}
pub fn build_prefix(
    cfg: &Effective,
    view: &RoleView,
    situation: &Situation<'_>,
    mcp: &[model::ToolDef],
) -> Prefix {
    use std::fmt::Write as _;
    let mut system = view.definition.body.clone();
    system.push_str("\n\n");
    let _ = writeln!(
        system,
        "Working directory: {}",
        situation.directory.display()
    );
    if situation.directories.len() > 1 {
        let _ = writeln!(
            system,
            "Your task's granted working directories: {}. Child agents cannot expand their task directories.",
            situation
                .directories
                .iter()
                .map(|d| d.to_string_lossy())
                .collect::<Vec<_>>()
                .join(", ")
        );
    } else {
        system.push_str("Reading, editing or running commands outside the working directory asks the human first.\n");
    }
    let _ = writeln!(
        system,
        "Your name is {} (agent id {}). Every agent in this channel has a unique name; tools take a name wherever they take an id.",
        view.name, situation.id
    );
    if !situation.parent.is_empty() {
        let _ = writeln!(
            system,
            "You are a subagent (archetype {}) created by a parent agent (id {}) named {}. Your task arrives as the first message. When it is done, or cannot be done, answer with message, kind response, to the agent that asked (the message names it); it only sees what you put there. You stay alive afterwards: the parent or another agent may message you again, and you keep your context.",
            view.role, situation.parent, view.parent_name
        );
    }
    let instructions =
        instructions::section(&cfg.instructions, instructions::BUDGET, situation.home);
    if !instructions.is_empty() {
        system.push('\n');
        system.push_str(&instructions);
    }
    let skills = skills(cfg, view);
    if !skills.is_empty() {
        system.push_str("\n# Skills\nLoad a skill with the skill tool when its description matches your task.\n");
        for (name, skill) in skills {
            let _ = writeln!(system, "- {name}: {}", skill.description);
        }
    }
    let mut names = toolname::expand(view.definition.tools.clone());
    names.extend(toolname::MESSAGING.iter().map(|s| (*s).into()));
    names.push(toolname::ASK.into());
    system.push_str(section("asking_the_human"));
    system.push_str(section("messaging_and_shared_channels"));
    for (tool, name) in [
        (toolname::SHELL, "background_jobs"),
        (toolname::WEB, "web"),
        (toolname::TODO, "todo_list"),
        (toolname::SHEET, "sheets"),
    ] {
        if names.iter().any(|n| n == tool) {
            system.push_str(section(name));
        }
    }
    if can_orchestrate(view) {
        system.push_str(section("delegation"));
        for archetype in &view.definition.spawn {
            if let Some(role) = cfg.roles.get(archetype) {
                let _ = writeln!(system, "- {archetype}: {}", role.description);
            }
        }
        system.push_str(section("delegation_tail"));
    }
    let mut seen = std::collections::BTreeSet::new();
    let mut definitions = names
        .iter()
        .filter(|n| seen.insert((*n).clone()))
        .filter_map(|n| tools::definition(n, can_orchestrate(view)))
        .collect::<Vec<_>>();
    if !mcp.is_empty() {
        system.push_str("\n# MCP tools\nTools named mcp__<server>__<tool> come from MCP servers this role runs; their descriptions are the servers' own.\n");
        definitions.extend_from_slice(mcp);
    }
    Prefix {
        system,
        definitions,
    }
}
pub fn state_note(state: &ChannelState, id: &str, view: &RoleView, cfg: &Effective) -> String {
    use std::fmt::Write as _;
    let Some(agent) = state.agents.get(id) else {
        return String::new();
    };
    let mut lines = Vec::new();
    if !agent.parent.is_empty() && view.definition.max_turns > 0 {
        lines.push(format!("This is turn {} of at most {}: answer with message (kind response) before the limit; after it your turns end at once and the agents waiting on you are told you ran out.",agent.turn,view.definition.max_turns));
    }
    if can_orchestrate(view) {
        let (can, why) = can_spawn(state, agent, cfg);
        lines.push(if can {
            format!(
                "Agents busy in this channel: {} of at most {} (depth {} of {}).",
                state.agents.values().filter(|a| a.busy()).count(),
                cfg.limits.max_agents,
                agent.depth,
                cfg.limits.max_depth
            )
        } else {
            format!("You cannot create agents right now ({why}): do the work yourself.")
        });
    }
    if view.definition.tools.iter().any(|t| t == toolname::TODO) {
        let mut todo = "Your todo list: (empty)".to_owned();
        if !agent.todos.is_empty() {
            todo = "Your todo list:".into();
            for item in &agent.todos {
                let _ = write!(todo, "\n- {} [{}] {}", item.id, item.status, item.text);
            }
        }
        lines.push(todo);
    }
    let pending = state.pending_replies(id);
    lines.push(if pending.is_empty() {
        "Pending requests: none. Use info for updates that answer no request.".into()
    } else {
        project::pending_reply_text(&pending)
    });
    format!("[harness state for this request]\n{}", lines.join("\n"))
}
