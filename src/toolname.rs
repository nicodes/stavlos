//! One vocabulary for current tools and legacy transcript/config aliases.
pub const SHELL: &str = "shell";
pub const SHELL_KILL: &str = "shell_kill";
pub const READ: &str = "read";
pub const GREP: &str = "grep";
pub const GLOB: &str = "glob";
pub const PATCH: &str = "patch";
pub const SKILL: &str = "skill";
pub const FETCH: &str = "fetch";
pub const SEARCH: &str = "search";
pub const WEB: &str = "web";
pub const TODO: &str = "todo";
pub const ASK: &str = "ask";
pub const SHEET: &str = "sheet";
pub const OPERATOR: &str = "operator";
pub const MESSAGE: &str = "message";
pub const AGENT: &str = "agent";
pub const CHANNEL: &str = "channel";
pub const AGENT_CREATE: &str = "agent_create";
pub const AGENT_CANCEL: &str = "agent_cancel";
pub const AGENT_STATUS: &str = "agent_status";
pub const MCP_PREFIX: &str = "mcp__";
pub const USER: &str = "user";
pub const ROLE_TOOLS: &[&str] = &[
    SHELL, READ, GREP, GLOB, PATCH, SKILL, TODO, WEB, SHEET, OPERATOR,
];
pub const MESSAGING: &[&str] = &[MESSAGE, AGENT, CHANNEL];
pub fn canonical(name: &str) -> &str {
    match name {
        "apply_patch" => PATCH,
        "web_fetch" => FETCH,
        "web_search" => SEARCH,
        "ask_user" => ASK,
        _ => name,
    }
}
pub fn expand(names: impl IntoIterator<Item = String>) -> Vec<String> {
    let mut result = Vec::new();
    for name in names {
        if !result.contains(&name) {
            result.push(name);
        }
    }
    result
}
pub fn operation<'a>(name: &'a str, input: &serde_json::Value) -> &'a str {
    let encoded = serde_json::to_string(input).expect("serializable JSON");
    let raw: &serde_json::value::RawValue =
        serde_json::from_str(&encoded).expect("encoded JSON is valid");
    operation_raw(name, Some(raw))
}
pub fn operation_raw<'a>(name: &'a str, input: Option<&serde_json::value::RawValue>) -> &'a str {
    if !matches!(name, AGENT | WEB | SHELL) {
        return canonical(name);
    }
    let mut action = String::new();
    if let Some(input) = input
        && let Ok(fields) = crate::config::jsonc::object_fields(input.get().as_bytes())
    {
        for (key, value) in fields {
            if key.eq_ignore_ascii_case("action")
                && let Ok(value) = serde_json::from_slice::<String>(&value)
            {
                action = value;
            }
        }
    }
    operation_action(name, &action)
}
fn operation_action<'a>(name: &'a str, action: &str) -> &'a str {
    match (name, action) {
        (WEB, "fetch") => FETCH,
        (WEB, "search") => SEARCH,
        (SHELL, "kill") => SHELL_KILL,
        (AGENT, "create") => AGENT_CREATE,
        (AGENT, "cancel") => AGENT_CANCEL,
        (AGENT, "status") => AGENT_STATUS,
        _ => canonical(name),
    }
}
pub fn policy_rule(name: &str, pattern: &str) -> (String, String) {
    let name = canonical(name);
    let (tool, action) = match name {
        SHELL if pattern != "*" => (SHELL, "run"),
        AGENT_CREATE => (AGENT, "create"),
        AGENT_CANCEL => (AGENT, "cancel"),
        AGENT_STATUS => (AGENT, "status"),
        FETCH => (WEB, "fetch"),
        SEARCH => (WEB, "search"),
        SHELL_KILL => (SHELL, "kill"),
        _ => (name, ""),
    };
    (
        tool.into(),
        if action.is_empty() {
            pattern.into()
        } else {
            format!("{action} {pattern}")
        },
    )
}
