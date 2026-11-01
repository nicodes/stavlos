use super::{
    Block, DIFF_EXPANDED, Kind, Line, OUTPUT_COLLAPSED, OUTPUT_EXPANDED, Visibility, glyph,
};
use crate::{toolname, tui::format};
use serde_json::{Value, value::RawValue};
pub const ASIDE_TITLE: &str = "**Aside**";
pub fn titled(title: &str, detail: &str) -> String {
    if detail.is_empty() {
        format!("**{title}**")
    } else {
        format!("**{title}** {detail}")
    }
}
pub fn block(kind: Block, label: &str, body: &str, glyph: &str) -> Vec<Line> {
    let mut lines = vec![Line::new(Kind::Blank, "")];
    if !label.is_empty() {
        lines.push(Line {
            kind: Kind::Label,
            text: label.into(),
            block: kind,
            ..Default::default()
        });
    }
    for (index, text) in body.trim_end_matches('\n').split('\n').enumerate() {
        lines.push(Line {
            kind: Kind::Text,
            text: text.into(),
            block: kind,
            lead: index == 0 && matches!(kind, Block::User | Block::Steer),
            glyph: if index == 0 {
                glyph.into()
            } else {
                String::new()
            },
            ..Default::default()
        });
    }
    lines.push(Line::new(Kind::Blank, ""));
    lines
}
pub fn message_address(from: &str, to: &[String], viewer: &str) -> (String, Vec<String>) {
    let mut parts = vec![];
    let mut names = vec![];
    if !from.is_empty() && from != viewer {
        parts.push(format!("@{from}:"));
        names.push(from.into());
    }
    for name in to {
        if name.is_empty() || viewer == toolname::USER && name == toolname::USER {
            continue;
        }
        parts.push(format!("@{name}"));
        names.push(name.clone());
    }
    (parts.join(" "), names)
}
pub fn addressed(to: &[String], body: &str) -> String {
    let address = to
        .iter()
        .map(|name| format!("@{name}"))
        .collect::<Vec<_>>()
        .join(" ");
    if address.is_empty() {
        body.into()
    } else if body.is_empty() {
        address
    } else {
        format!("{address} {body}")
    }
}
pub fn received(from: &str, to: &[String], viewer: &str, body: &str, glyph: &str) -> Vec<Line> {
    let mut parts = body.trim_end_matches('\n').split('\n');
    let (address, names) = message_address(from, to, viewer);
    let text = format!("{address} {}", parts.next().unwrap_or(""))
        .trim()
        .into();
    let mut lines = vec![
        Line::new(Kind::Blank, ""),
        Line {
            kind: Kind::Text,
            text,
            block: Block::Child,
            glyph: glyph.into(),
            who: from.into(),
            names: names.into(),
            ..Default::default()
        },
    ];
    for text in parts {
        lines.push(Line {
            kind: Kind::Text,
            text: text.into(),
            block: Block::Child,
            indent: 1,
            ..Default::default()
        });
    }
    lines.push(Line::new(Kind::Blank, ""));
    lines
}
pub fn error_block(body: &str, glyph: &str) -> Vec<Line> {
    let mut lines = vec![Line::new(Kind::Blank, "")];
    for (index, text) in body.trim_end_matches('\n').split('\n').enumerate() {
        lines.push(Line {
            kind: Kind::Error,
            text: text.into(),
            block: Block::Error,
            tone: super::Tone::Error,
            glyph: if index == 0 {
                glyph.into()
            } else {
                String::new()
            },
            ..Default::default()
        });
    }
    lines.push(Line::new(Kind::Blank, ""));
    lines
}
pub fn heading_level(input: &str) -> usize {
    let count = input.bytes().take_while(|byte| *byte == b'#').count();
    if count == 0 || count > 6 || input.as_bytes().get(count) != Some(&b' ') {
        0
    } else {
        count
    }
}
pub fn markdown_lines(body: &str) -> Vec<Line> {
    let mut lines = vec![];
    let mut code = false;
    for text in body.split('\n') {
        let trim = text.trim();
        if trim.starts_with("```") {
            code = !code;
            continue;
        }
        let level = heading_level(trim);
        lines.push(if code {
            Line::new(Kind::Code, text)
        } else if level > 0 {
            Line::new(Kind::Heading, trim[level..].trim())
        } else {
            Line::new(Kind::Text, text)
        });
    }
    lines
}
pub fn thinking_line(summary: &str) -> Line {
    let summary = summary.trim();
    if summary.is_empty() {
        Line::new(Kind::Think, "◌ thinking…")
    } else {
        Line::new(
            Kind::Think,
            format!(
                "◌ {}",
                format::trunc(summary.split('\n').next().unwrap(), 100)
            ),
        )
    }
}
pub fn trunc_lines(body: &str, count: usize, kind: Kind) -> Vec<Line> {
    let parts = body.trim_end_matches('\n').split('\n').collect::<Vec<_>>();
    parts
        .iter()
        .take(count)
        .enumerate()
        .map(|(index, text)| {
            Line::new(
                kind,
                format!(
                    "{text}{}",
                    if parts.len() > count && index + 1 == count {
                        "…"
                    } else {
                        ""
                    }
                ),
            )
        })
        .collect()
}
pub fn output_lines(body: &str) -> Vec<Line> {
    if body.is_empty() {
        return vec![];
    }
    let parts = body.split('\n').collect::<Vec<_>>();
    let total = parts.len();
    let mut lines = parts
        .into_iter()
        .take(OUTPUT_EXPANDED)
        .enumerate()
        .map(|(index, text)| Line {
            kind: Kind::ToolOut,
            text: text.into(),
            vis: if index < OUTPUT_COLLAPSED {
                Visibility::Always
            } else {
                Visibility::Expanded
            },
            ..Default::default()
        })
        .collect::<Vec<_>>();
    append_tail(&mut lines, total, OUTPUT_EXPANDED);
    lines
}
fn append_tail(lines: &mut Vec<Line>, total: usize, expanded: usize) {
    for (limit, vis) in [
        (OUTPUT_COLLAPSED, Visibility::Collapsed),
        (expanded, Visibility::Expanded),
    ] {
        if total > limit {
            lines.push(Line {
                kind: Kind::ToolOut,
                text: format!("… +{} lines", total - limit),
                vis,
                ..Default::default()
            });
        }
    }
}
pub fn diff_lines(patch: &str) -> Vec<Line> {
    let mut lines = vec![];
    for text in patch.trim_end_matches('\n').split('\n') {
        if matches!(text, "*** Begin Patch" | "*** End Patch") || text.trim() == "*** End of File" {
            continue;
        }
        let (text, diff) = if let Some(path) = text.strip_prefix("*** Update File: ") {
            (path.into(), b'f')
        } else if let Some(path) = text.strip_prefix("*** Add File: ") {
            (format!("{path} (new)"), b'f')
        } else if let Some(path) = text.strip_prefix("*** Delete File: ") {
            (format!("{path} (deleted)"), b'f')
        } else if let Some(path) = text.strip_prefix("*** Move to: ") {
            (format!("→ {path}"), b'f')
        } else if text.starts_with("@@") {
            (text.into(), b'@')
        } else if text.starts_with('+') {
            (text.into(), b'+')
        } else if text.starts_with('-') {
            (text.into(), b'-')
        } else {
            (text.into(), 0)
        };
        lines.push(Line {
            kind: Kind::ToolOut,
            text,
            diff,
            ..Default::default()
        });
    }
    let total = lines.len();
    lines.truncate(DIFF_EXPANDED);
    for line in lines.iter_mut().skip(OUTPUT_COLLAPSED) {
        line.vis = Visibility::Expanded;
    }
    append_tail(&mut lines, total, DIFF_EXPANDED);
    lines
}
pub fn patch_files(patch: &str) -> String {
    let files = patch
        .split('\n')
        .filter_map(|line| {
            ["*** Add File: ", "*** Update File: ", "*** Delete File: "]
                .iter()
                .find_map(|prefix| line.strip_prefix(prefix).map(str::trim))
        })
        .collect::<Vec<_>>();
    if files.len() <= 2 {
        files.join(", ")
    } else {
        format!("{} (+{} more)", files[..2].join(", "), files.len() - 2)
    }
}
fn compact(raw: &str) -> String {
    let valid = serde_json::from_str::<Box<RawValue>>(raw).is_ok();
    let compact = if valid {
        let mut text = String::new();
        let mut quote = false;
        let mut escaped = false;
        for ch in raw.chars() {
            if quote {
                text.push(ch);
                if escaped {
                    escaped = false;
                } else if ch == '\\' {
                    escaped = true;
                } else if ch == '"' {
                    quote = false;
                }
            } else if ch == '"' {
                quote = true;
                text.push(ch);
            } else if !ch.is_ascii_whitespace() {
                text.push(ch);
            }
        }
        text
    } else {
        raw.into()
    };
    format::trunc(&compact.replace('\n', " "), 100)
}
fn folded(value: &Value, key: &str) -> Value {
    value
        .as_object()
        .and_then(|object| {
            object
                .iter()
                .rev()
                .find(|(candidate, _)| candidate.eq_ignore_ascii_case(key))
                .map(|(_, value)| value.clone())
        })
        .unwrap_or(Value::Null)
}
pub fn message_recipients(raw: &str) -> Vec<String> {
    let value = serde_json::from_str::<Value>(raw).unwrap_or_default();
    serde_json::from_value::<crate::protocol::Recipients>(folded(&value, "to"))
        .unwrap_or_default()
        .normalized()
}
pub fn prompt_of(name: &str, raw: &str) -> (String, String) {
    let value = serde_json::from_str::<Value>(raw).unwrap_or_default();
    let string = |name: &str| folded(&value, name).as_str().unwrap_or("").to_string();
    if name == toolname::AGENT_CREATE {
        (string("label"), string("task").trim().into())
    } else {
        (
            message_recipients(raw).first().cloned().unwrap_or_default(),
            string("text").trim().into(),
        )
    }
}
pub fn tool_arg(name: &str, raw: &str) -> String {
    let parsed = serde_json::from_str::<Value>(raw);
    let input = parsed.as_ref().ok().and_then(Value::as_object);
    let raw_input = serde_json::from_str::<Box<RawValue>>(raw).ok();
    let name = toolname::operation_raw(name, raw_input.as_deref());
    if raw.is_empty() {
        return String::new();
    }
    if parsed.is_err() || input.is_none() && !parsed.as_ref().is_ok_and(Value::is_null) {
        return compact(raw);
    }
    let value = parsed.unwrap();
    let string = |key: &str| {
        value[key]
            .as_str()
            .unwrap_or("")
            .replace('\n', " ")
            .trim()
            .to_owned()
    };
    match name {
        toolname::GREP | toolname::GLOB => {
            let path = string("path");
            let pattern = string("pattern");
            if path.is_empty() {
                pattern
            } else {
                format!("{pattern}  in {path}")
            }
        }
        toolname::PATCH => patch_files(value["patch"].as_str().unwrap_or("")),
        toolname::AGENT_CREATE => {
            let label = string("label");
            let role = string("archetype");
            if label.is_empty() {
                role
            } else if role.is_empty() {
                label
            } else {
                format!("{label} ({role})")
            }
        }
        toolname::ASK => {
            let questions = folded(&value, "questions");
            match questions {
                Value::Null => String::new(),
                Value::Array(questions) => {
                    let mut out = vec![];
                    for question in questions {
                        if !question.is_null() && !question.is_object() {
                            return String::new();
                        }
                        match folded(&question, "question") {
                            Value::String(value) => out.push(value),
                            Value::Null => out.push(String::new()),
                            _ => return String::new(),
                        }
                    }
                    out.join(" · ")
                }
                _ => String::new(),
            }
        }
        toolname::TODO => todo_arg(&value),
        toolname::SHEET => format!("{} {} {}", string("action"), string("id"), string("title"))
            .trim()
            .into(),
        toolname::MESSAGE => addressed(&message_recipients(raw), ""),
        _ => {
            let key = match name {
                toolname::SHELL => "command",
                toolname::AGENT | toolname::CHANNEL | toolname::WEB => "action",
                toolname::SHELL_KILL | toolname::AGENT_CANCEL | toolname::AGENT_STATUS => "id",
                toolname::READ => "path",
                toolname::SKILL => "name",
                toolname::FETCH => "url",
                toolname::SEARCH => "query",
                _ => "",
            };
            if key.is_empty() {
                compact(raw)
            } else {
                string(key)
            }
        }
    }
}
fn todo_arg(value: &Value) -> String {
    let mut parts = vec![];
    for (field, update) in [("update", true), ("add", false)] {
        let values = folded(value, field);
        let values = match values {
            Value::Null => vec![],
            Value::Array(values) => values,
            _ => return String::new(),
        };
        for item in values {
            if !item.is_null() && !item.is_object() {
                return String::new();
            }
            let get = |key: &str| match folded(&item, key) {
                Value::Null => Some(String::new()),
                Value::String(value) => Some(value),
                _ => None,
            };
            let (Some(id), Some(status), Some(text)) = (get("id"), get("status"), get("text"))
            else {
                return String::new();
            };
            let mut part = if update { id } else { text.clone() };
            if !status.is_empty() && (update || status != "pending") {
                part.push_str(&format!(" → {status}"));
            }
            if update && !text.is_empty() {
                part.push_str(&format!("  {text}"));
            }
            parts.push(part);
        }
    }
    parts.join(" · ")
}
pub fn tool_title(name: &str) -> String {
    let name = toolname::canonical(name);
    if let Some(name) = name.strip_prefix(toolname::MCP_PREFIX)
        && let Some((server, tool)) = name.split_once("__")
    {
        return format!("{server} · {tool}");
    }
    match name {
        toolname::FETCH => "Fetch".into(),
        toolname::SEARCH => "Search".into(),
        toolname::PATCH => "Patch".into(),
        _ => {
            let name = name.replace('_', " ");
            let mut chars = name.chars();
            let Some(first) = chars.next() else {
                return name;
            };
            let mut upper = first.to_uppercase();
            let one = upper.next().unwrap();
            let one = if upper.next().is_none() {
                one
            } else {
                match first as u32 {
                    0x1f80..=0x1f87 | 0x1f90..=0x1f97 | 0x1fa0..=0x1fa7 => {
                        char::from_u32(first as u32 + 8).unwrap()
                    }
                    0x1fb3 | 0x1fc3 | 0x1ff3 => char::from_u32(first as u32 + 9).unwrap(),
                    _ => first,
                }
            };
            format!("{one}{}", chars.as_str())
        }
    }
}
pub fn tool_line(name: &str, raw: &str) -> String {
    let input = serde_json::from_str::<Box<RawValue>>(raw).ok();
    let name = toolname::operation_raw(name, input.as_deref());
    if matches!(name, toolname::MESSAGE | toolname::AGENT_CREATE) {
        let (to, body) = prompt_of(name, raw);
        let first = body.split('\n').next().unwrap_or("");
        if name == toolname::MESSAGE {
            addressed(&message_recipients(raw), first).trim().into()
        } else {
            format!("@{to} {first}").trim().into()
        }
    } else {
        let title = tool_title(name);
        let argument = tool_arg(name, raw);
        if argument.is_empty() {
            title
        } else {
            format!("{title}  {}", format::trunc(&argument, 100))
        }
    }
}
pub fn tool_glyph(name: &str) -> (&'static str, &'static str) {
    let name = toolname::canonical(name);
    let glyph = match name {
        toolname::READ => glyph::READ,
        toolname::PATCH => glyph::PATCH,
        toolname::AGENT_CREATE => glyph::CREATE,
        toolname::AGENT | toolname::MESSAGE => glyph::AGENTS,
        toolname::SHELL | toolname::SHELL_KILL => glyph::SHELL,
        toolname::TODO => glyph::TODO,
        toolname::SEARCH => glyph::SEARCH,
        toolname::WEB | toolname::FETCH => glyph::WEB,
        _ if name.starts_with("agent_") => glyph::AGENTS,
        _ if name.starts_with(toolname::MCP_PREFIX) => glyph::MCP,
        _ if name.starts_with("web_") => glyph::WEB,
        _ => glyph::FILES,
    };
    (glyph, " ")
}
pub fn split_model(id: &str) -> (&str, &str) {
    crate::model::split(id)
        .map(|(provider, model)| (model, provider))
        .unwrap_or((id, ""))
}
pub fn is_message(line: &Line) -> bool {
    line.kind == Kind::Tool && line.tool == toolname::MESSAGE
}
pub fn is_prompt_call(line: &Line) -> bool {
    is_message(line) || line.kind == Kind::Tool && line.tool == toolname::AGENT_CREATE
}
pub fn call_glyph(line: &Line) -> (&str, &str) {
    if !line.glyph.is_empty() {
        (&line.glyph, " ")
    } else if is_message(line) {
        (glyph::REPLY, " ")
    } else {
        tool_glyph(&line.tool)
    }
}
