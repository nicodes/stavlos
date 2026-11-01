use super::{permission, text};
use crate::protocol;
use serde_json::{Value, json};
pub const PAGE_SIZE: usize = 8;
pub fn page(id: &str) -> Option<(bool, usize)> {
    let parts = id.split('|').collect::<Vec<_>>();
    if parts.len() != 3 || parts[0] != "sv-status" {
        return None;
    }
    let all = match parts[1] {
        "true" => true,
        "false" => false,
        _ => return None,
    };
    Some((all, parts[2].parse().ok()?))
}
pub fn count(n: usize, noun: &str) -> String {
    format!("{n} {noun}{}", if n == 1 { "" } else { "s" })
}
pub fn text(body: &str, limit: usize) -> String {
    text::clip(
        &text::escape(&body.split_whitespace().collect::<Vec<_>>().join(" ")),
        limit,
    )
    .trim_end_matches('\\')
    .into()
}
pub fn state(state: &str) -> (&'static str, &'static str) {
    match state {
        "running" => ("⚙️", "Working"),
        "blocked" => ("🙋", "Needs input"),
        "waiting" => ("⏳", "Waiting"),
        "idle" => ("💤", "Idle"),
        "killed" => ("⛔", "Stopped"),
        _ => ("❔", "Unknown"),
    }
}
pub fn detail(agent: &protocol::AgentInfo) -> String {
    if !agent.last_error.is_empty() {
        return format!("Error: {}", agent.last_error);
    }
    if !agent.pending_replies.is_empty() {
        return format!(
            "{} awaiting an explicit response",
            count(agent.pending_replies.len(), "request")
        );
    }
    if let Some(todo) = agent.todos.iter().find(|todo| todo.status == "in_progress") {
        return todo.text.clone();
    }
    if !agent.awaiting.is_empty() {
        return format!("Waiting for {}", count(agent.awaiting.len(), "agent"));
    }
    String::new()
}
pub fn view(
    name: &str,
    agents: &[protocol::AgentInfo],
    prompts: &[protocol::PromptInfo],
    channel: &str,
    all: bool,
    page: i64,
) -> (String, Vec<Value>) {
    let shown = agents
        .iter()
        .filter(|agent| all || agent.state != "idle")
        .collect::<Vec<_>>();
    let cost = agents.iter().fold(0.0, |sum, agent| sum + agent.cost_usd);
    let summary = if all {
        format!("**{} agents** · **${cost:.2}** spent", agents.len())
    } else {
        format!(
            "**{} non-idle** · {} total · **${cost:.2}** spent",
            shown.len(),
            agents.len()
        )
    };
    let mut lines = vec![format!("**#{} · Status**", text(name, 100)), summary];
    let mut waiting = Vec::new();
    for (kind, noun) in [
        ("permission", "permission"),
        ("question", "question"),
        ("trust", "trust prompt"),
    ] {
        let n = prompts
            .iter()
            .filter(|p| p.channel == channel && p.kind == kind)
            .count();
        if n > 0 {
            waiting.push(count(n, noun));
        }
    }
    if !waiting.is_empty() {
        lines.push(format!("🙋 **Needs you:** {}", waiting.join(" · ")));
    }
    if shown.is_empty() {
        lines.push(String::new());
        lines.push(
            if agents.is_empty() {
                "No agents in this channel yet."
            } else {
                "💤 All agents are idle."
            }
            .into(),
        );
        if !all && !agents.is_empty() {
            lines.push("Use `/status all` to see them.".into());
        }
        return (lines.join("\n"), Vec::new());
    }
    let pages = shown.len().div_ceil(PAGE_SIZE);
    let page = (page.max(0) as usize).min(pages - 1);
    let start = page * PAGE_SIZE;
    let end = ((page + 1) * PAGE_SIZE).min(shown.len());
    for agent in &shown[start..end] {
        let (mark, state) = state(&agent.state);
        let name = if agent.name.is_empty() {
            &agent.id
        } else {
            &agent.name
        };
        let mut line = format!("{mark} **@{}** · {state}", text(name, 40));
        if !agent.role.is_empty() {
            line.push_str(&format!(" · {}", text(&agent.role, 32)));
        }
        lines.push(String::new());
        lines.push(line);
        let detail = detail(agent);
        if !detail.is_empty() {
            lines.push(format!("  ↳ {}", text(&detail, 100)));
        }
    }
    let mut controls = Vec::new();
    if pages > 1 {
        lines.push(String::new());
        lines.push(format!(
            "Page {}/{pages} · agents {}–{end} of {}",
            page + 1,
            start + 1,
            shown.len()
        ));
        let button = |label, target, disabled| {
            let mut value = json!({"type":2,"style":2,"custom_id":format!("sv-status|{all}|{target}"),"label":label,"disabled":disabled});
            if disabled {
                value["disabled"] = json!(true);
            }
            value
        };
        controls.push(permission::row(vec![
            button("Previous", page.saturating_sub(1), page == 0),
            button("Next", page + 1, page + 1 == pages),
        ]));
    }
    if !all && shown.len() < agents.len() {
        lines.push(String::new());
        lines.push("`/status all` includes idle agents.".into());
    }
    (lines.join("\n"), controls)
}
