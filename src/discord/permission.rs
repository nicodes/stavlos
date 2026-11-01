use super::text;
use crate::{protocol, toolname};
use anyhow::{Result, ensure};
use serde_json::{Value, json, value::RawValue};
use std::collections::BTreeMap;
pub fn code(language: &str, body: &str) -> String {
    let body = crate::textsafe::clean(body).replace("```", "`\u{200b}`\u{200b}`");
    format!("```{language}\n{}\n```", body.trim_end_matches('\n'))
}
// JSON indentation retains exact strings, number lexemes and object order.
fn pretty(raw: &[u8]) -> Option<String> {
    let _: Box<RawValue> = serde_json::from_slice(raw).ok()?;
    let source = std::str::from_utf8(raw).ok()?;
    let mut out = String::new();
    let mut depth = 0usize;
    let bytes = source.as_bytes();
    let mut index = 0;
    while index < bytes.len() {
        let byte = bytes[index];
        if byte.is_ascii_whitespace() {
            index += 1;
            continue;
        }
        if byte == b'"' {
            let start = index;
            index += 1;
            while index < bytes.len() {
                match bytes[index] {
                    b'\\' => index += 2,
                    b'"' => {
                        index += 1;
                        break;
                    }
                    _ => index += 1,
                }
            }
            out.push_str(&source[start..index]);
            continue;
        }
        match byte {
            b'{' | b'[' => {
                out.push(byte as char);
                depth += 1;
                let next = bytes[index + 1..].iter().find(|b| !b.is_ascii_whitespace());
                if !matches!(next, Some(b'}' | b']')) {
                    out.push('\n');
                    out.push_str(&"  ".repeat(depth));
                }
            }
            b'}' | b']' => {
                depth = depth.saturating_sub(1);
                if !matches!(out.as_bytes().last(), Some(b'{' | b'[')) {
                    out.push('\n');
                    out.push_str(&"  ".repeat(depth));
                }
                out.push(byte as char);
            }
            b',' => {
                out.push_str(",\n");
                out.push_str(&"  ".repeat(depth));
            }
            b':' => out.push_str(": "),
            _ => out.push(byte as char),
        }
        index += 1;
    }
    let end = bytes
        .iter()
        .rposition(|byte| !byte.is_ascii_whitespace())
        .map_or(0, |end| end + 1);
    out.push_str(&source[end..]);
    Some(out)
}
pub fn json_text(raw: &[u8]) -> String {
    pretty(raw)
        .map(|text| code("json", &text))
        .unwrap_or_else(|| code("text", &String::from_utf8_lossy(raw)))
}
pub fn label(name: &str) -> String {
    let name = name
        .replace('_', " ")
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ");
    let mut chars = name.chars();
    let Some(first) = chars.next() else {
        return "Tool".into();
    };
    let mut uppercase = first.to_uppercase();
    let upper = uppercase.next().unwrap();
    let upper = if uppercase.next().is_none() {
        upper
    } else {
        match first as u32 {
            0x1f80..=0x1f87 | 0x1f90..=0x1f97 | 0x1fa0..=0x1fa7 => {
                char::from_u32(first as u32 + 8).unwrap()
            }
            0x1fb3 | 0x1fc3 | 0x1ff3 => char::from_u32(first as u32 + 9).unwrap(),
            _ => first,
        }
    };
    format!("{upper}{}", chars.as_str())
}
pub fn heading(prompt: &protocol::PromptInfo) -> String {
    let operation = toolname::operation_raw(&prompt.tool, prompt.input.as_deref());
    let title = if let Some(name) = prompt.tool.strip_prefix(toolname::MCP_PREFIX)
        && let Some((server, tool)) = name.split_once("__")
    {
        format!("{} · {}", label(server), label(tool))
    } else {
        label(operation)
    };
    let mut head = format!("❗ **{}**", text::escape(&title));
    if !prompt.dir.is_empty() {
        head.push(' ');
        head.push_str(&text::escape(&prompt.dir));
    }
    head
}
fn layout(operation: &str) -> Option<(&'static str, Vec<(&'static str, &'static str)>)> {
    Some(match operation {
        toolname::PATCH => ("diff", vec![("patch", "")]),
        toolname::FETCH => ("text", vec![("url", ""), ("start", "Start")]),
        toolname::SEARCH => ("text", vec![("query", ""), ("n", "Results")]),
        toolname::READ => (
            "text",
            vec![
                ("path", ""),
                ("offset", "First line"),
                ("limit", "Line limit"),
            ],
        ),
        toolname::GREP => (
            "text",
            vec![
                ("pattern", "Pattern"),
                ("path", "Path"),
                ("glob", "File glob"),
                ("ignore_case", "Ignore case"),
                ("limit", "Match limit"),
            ],
        ),
        toolname::GLOB => (
            "text",
            vec![
                ("pattern", "Pattern"),
                ("path", "Path"),
                ("limit", "Path limit"),
            ],
        ),
        toolname::SKILL => ("text", vec![("name", "Skill")]),
        toolname::AGENT_CANCEL | toolname::SHELL_KILL => ("text", vec![("id", "Target")]),
        _ => return None,
    })
}
pub fn subject(prompt: &protocol::PromptInfo) -> String {
    let raw = prompt
        .input
        .as_ref()
        .map_or(&b""[..], |value| value.get().as_bytes());
    let operation = toolname::operation_raw(&prompt.tool, prompt.input.as_deref());
    if operation == toolname::SHELL {
        let command = (|| -> Option<String> {
            let mut command = None;
            for (key, value) in crate::config::jsonc::object_fields(raw).ok()? {
                if key.eq_ignore_ascii_case("command") {
                    command = serde_json::from_slice::<Option<String>>(&value).ok()?;
                }
            }
            command
        })();
        if let Some(command) = command {
            return code("sh", &command);
        }
    }
    let Some((language, fields)) = layout(operation) else {
        return json_text(raw);
    };
    let Ok(mut input) = serde_json::from_slice::<BTreeMap<String, Box<RawValue>>>(raw) else {
        return json_text(raw);
    };
    if input
        .get(fields[0].0)
        .is_none_or(|value| serde_json::from_str::<String>(value.get()).is_err())
    {
        return json_text(raw);
    }
    let mut lines = Vec::new();
    for (key, label) in fields {
        let Some(value) = input.remove(key) else {
            continue;
        };
        let value =
            serde_json::from_str::<String>(value.get()).unwrap_or_else(|_| value.get().into());
        lines.push(if label.is_empty() {
            value
        } else {
            format!("{label}: {value}")
        });
    }
    let mut text = code(language, &lines.join("\n"));
    if !input.is_empty() {
        let raw = serde_json::to_string(&input)
            .expect("validated raw JSON")
            .replace('<', "\\u003c")
            .replace('>', "\\u003e")
            .replace('&', "\\u0026")
            .replace('\u{2028}', "\\u2028")
            .replace('\u{2029}', "\\u2029");
        text.push_str("\nAdditional arguments:\n");
        text.push_str(&json_text(raw.as_bytes()));
    }
    text
}
pub fn trust_text(prompt: &protocol::PromptInfo) -> String {
    let raw = prompt
        .input
        .as_ref()
        .map_or(&b""[..], |value| value.get().as_bytes());
    let Ok(value) = serde_json::from_slice::<Value>(raw) else {
        return format!("❗ **Project configuration trust**\n{}", json_text(raw));
    };
    let mut text = "❗ **Project configuration trust**".to_owned();
    if let Some(dir) = value["dir"].as_str().filter(|dir| !dir.is_empty()) {
        text.push_str(" · ");
        text.push_str(&text::escape(dir));
    }
    text.push_str(
        "\nProject configuration can define MCP servers, policy, roles, skills and AGENTS.md.",
    );
    if let Some(files) = value["files"].as_array().filter(|files| !files.is_empty()) {
        let files = files
            .iter()
            .filter_map(Value::as_str)
            .collect::<Vec<_>>()
            .join("\n");
        text.push_str("\nConfiguration files:\n");
        text.push_str(&code("text", &files));
    }
    text
}
pub fn component_id(id: &str, action: &str, arg: &str) -> String {
    format!("sv|{id}|{action}|{arg}")
}
pub fn parse_id(id: &str) -> Option<(&str, &str, &str)> {
    let parts = id.split('|').collect::<Vec<_>>();
    if parts.len() != 4 || parts[0] != "sv" {
        return None;
    }
    Some((parts[1], parts[2], parts[3]))
}
pub fn button(id: &str, action: &str, label: &str, disabled: bool) -> Value {
    let mut button = json!({"type":2,"style":2,"label":text::clip(label,80),"custom_id":component_id(id,action,"")});
    if disabled {
        button["disabled"] = json!(true);
    }
    button
}
pub fn row(components: Vec<Value>) -> Value {
    json!({"type":1,"components":components})
}
pub fn answer(
    prompt: &protocol::PromptInfo,
    action: &str,
    value: &str,
) -> Result<protocol::PromptReplyParams> {
    let mut answer = protocol::PromptReplyParams {
        id: prompt.id.clone(),
        ..Default::default()
    };
    if prompt.kind == protocol::PROMPT_TRUST {
        answer.answer = match action {
            "allow" => "allow",
            "skip" => "deny",
            _ => anyhow::bail!("invalid trust answer"),
        }
        .into();
        return Ok(answer);
    }
    ensure!(
        prompt.kind == protocol::PROMPT_PERMISSION,
        "invalid prompt kind"
    );
    match action {
        "allow" => answer.answer = "allow".into(),
        "always" => {
            answer.answer = "allow_always".into();
            answer.dir = prompt.dir.clone();
        }
        "prefix" => {
            ensure!(
                !prompt.prefix.is_empty() && prompt.dir.is_empty(),
                "this prompt has no prefix choice"
            );
            answer.answer = "allow_prefix".into();
        }
        "dir-submit" => {
            ensure!(
                !prompt.dir.is_empty() && !value.trim().is_empty(),
                "directory choice is unavailable or empty"
            );
            answer.answer = "allow_always".into();
            answer.dir = value.trim().into();
        }
        "deny-now" => answer.answer = "deny".into(),
        "deny-submit" => {
            ensure!(!value.trim().is_empty(), "provide a reason, or use Deny");
            answer.answer = "deny".into();
            answer.reason = value.trim().into();
        }
        _ => anyhow::bail!("invalid permission action"),
    }
    Ok(answer)
}
