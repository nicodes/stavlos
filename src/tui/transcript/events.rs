use super::*;
use crate::{event, model, protocol, toolname, tui::format};
use serde_json::value::RawValue;
fn notice(kind: Kind, glyph: &str, tone: Tone, text: String) -> Line {
    Line {
        kind,
        glyph: glyph.into(),
        tone,
        text,
        ..Default::default()
    }
}
pub fn party_list(names: &[String]) -> String {
    names
        .iter()
        .map(|name| if name == toolname::USER { "you" } else { name })
        .collect::<Vec<_>>()
        .join(", ")
}
pub fn event_lines(event: &event::Event) -> Vec<Line> {
    match render(event) {
        Ok(lines) => lines,
        Err(error) => vec![Line::new(
            Kind::Error,
            format!("(bad {} payload: {error})", event.kind),
        )],
    }
}
fn render(ev: &event::Event) -> serde_json::Result<Vec<Line>> {
    Ok(match ev.kind.as_str() {
        event::AGENT_SPAWNED => {
            let p: event::AgentSpawnedPayload = ev.decode()?;
            if p.parent.is_empty() {
                vec![]
            } else {
                vec![notice(
                    Kind::Dim,
                    glyph::SPAWN,
                    Tone::None,
                    titled("Spawned", &format!("{} ({}) · {}", p.name, p.role, p.model)),
                )]
            }
        }
        event::INPUT_QUEUED => {
            let p: event::Input = ev.decode()?;
            if p.kind == event::INPUT_RESUME {
                vec![notice(
                    Kind::Dim,
                    glyph::NUDGE,
                    Tone::None,
                    titled("Resumed", "a model is available again"),
                )]
            } else if p.kind != event::INPUT_REMINDER {
                vec![]
            } else if !p.requests.is_empty() {
                let mut lines = vec![notice(
                    Kind::Dim,
                    glyph::NUDGE,
                    Tone::None,
                    titled(
                        "Nudge",
                        &format!("{} requests need responses", p.requests.len()),
                    ),
                )];
                for request in p.requests.iter() {
                    lines.push(Line {
                        kind: Kind::Dim,
                        text: format!("{} @{}: {}", request.id, request.from_name, request.text),
                        indent: 1,
                        ..Default::default()
                    });
                }
                lines
            } else {
                vec![notice(
                    Kind::Dim,
                    glyph::NUDGE,
                    Tone::None,
                    titled(
                        "Nudge",
                        &format!("owes a reply to {}", party_list(&p.names)),
                    ),
                )]
            }
        }
        event::ASSISTANT_MESSAGE => {
            let p: event::AssistantMessagePayload = ev.decode()?;
            let mut lines: Vec<Line> = vec![];
            let mut has_text = false;
            for block in p
                .blocks
                .iter()
                .filter(|block| block.kind == model::BLOCK_TEXT)
            {
                let text = block.text.trim_end_matches('\n');
                if text.is_empty() {
                    continue;
                }
                has_text = true;
                let mut body = markdown_lines(text);
                if body.is_empty() {
                    continue;
                }
                let mut has_title = lines.iter().any(|line| line.glyph == glyph::ASIDE);
                if !has_title && body[0].kind != Kind::Text {
                    body.insert(0, Line::new(Kind::Text, ASIDE_TITLE));
                } else if !has_title {
                    body[0].text = format!("{ASIDE_TITLE} {}", body[0].text);
                }
                for mut line in body {
                    line.note = true;
                    if has_title {
                        line.indent = 1;
                    } else {
                        line.glyph = glyph::ASIDE.into();
                        has_title = true;
                    }
                    lines.push(line);
                }
            }
            if has_text {
                lines.push(Line::new(Kind::Blank, ""));
            }
            lines
        }
        event::TOOL_STARTED => {
            let p: event::ToolStartedPayload = ev.decode()?;
            tool_started_lines(&p.name, &p.call_id, "")
        }
        event::TOOL_FINISHED => {
            let p: event::ToolFinishedPayload = ev.decode()?;
            if p.denied
                || matches!(
                    p.name.as_str(),
                    toolname::MESSAGE | toolname::AGENT_CREATE | toolname::PATCH
                ) && !p.is_error
            {
                vec![]
            } else {
                output_lines(p.output.trim_end_matches('\n'))
            }
        }
        event::TURN_ENDED => {
            let p: event::TurnEndedPayload = ev.decode()?;
            match p.reason.as_str() {
                event::REASON_CANCELLED => vec![
                    notice(
                        Kind::Dim,
                        glyph::TURN,
                        Tone::Error,
                        titled("Turn cancelled", ""),
                    ),
                    Line::new(Kind::Blank, ""),
                ],
                event::REASON_ERROR => error_block(
                    if p.error.is_empty() {
                        "turn error"
                    } else {
                        &p.error
                    },
                    glyph::ERROR,
                ),
                event::REASON_MAX_TOKENS => vec![
                    notice(
                        Kind::Dim,
                        glyph::TURN,
                        Tone::Error,
                        titled("Turn stopped", "max_tokens"),
                    ),
                    Line::new(Kind::Blank, ""),
                ],
                _ => vec![],
            }
        }
        event::TURN_ABORTED => vec![
            notice(
                Kind::Dim,
                glyph::TURN,
                Tone::Error,
                titled("Turn aborted", "daemon restart"),
            ),
            Line::new(Kind::Blank, ""),
        ],
        event::AGENT_KILLED => error_block("killed", glyph::KILLED),
        event::AGENT_UPDATED => {
            let p: event::AgentUpdatedPayload = ev.decode()?;
            let mut lines = vec![];
            for (what, value, why) in [
                ("Role", p.role, String::new()),
                ("Model", p.model, p.reason),
                (
                    "Variant",
                    p.variant.map(|variant| {
                        if variant.is_empty() {
                            "default".into()
                        } else {
                            variant
                        }
                    }),
                    String::new(),
                ),
            ] {
                if let Some(value) = value {
                    let mut text = format!("→ {value}");
                    if !why.is_empty() {
                        text.push_str(&format!(" · {why}"));
                    }
                    lines.push(notice(
                        Kind::Dim,
                        glyph::MODEL,
                        Tone::None,
                        titled(what, &text),
                    ));
                }
            }
            lines
        }
        event::CHANNEL_UPDATED => {
            let p: event::ChannelUpdatedPayload = ev.decode()?;
            if let Some(dir) = p.dir {
                vec![notice(
                    Kind::Dim,
                    glyph::MODEL,
                    Tone::None,
                    titled(
                        "Directory",
                        &format!("→ {dir} · mode ask · remembered approvals cleared"),
                    ),
                )]
            } else if let Some(mode) = p.mode {
                vec![notice(
                    Kind::Dim,
                    glyph::MODEL,
                    Tone::None,
                    titled(
                        "Mode",
                        &format!("→ {mode} · {}", protocol::mode_summary(&mode)),
                    ),
                )]
            } else {
                vec![]
            }
        }
        event::JOB_STARTED => {
            let p: event::JobStartedPayload = ev.decode()?;
            vec![notice(
                Kind::Dim,
                glyph::JOB,
                Tone::Working,
                titled("Job", &p.command),
            )]
        }
        event::JOB_FINISHED => job_finished_lines(&ev.decode()?),
        event::JOB_STOPPED => {
            let p: event::JobStoppedPayload = ev.decode()?;
            job_stopped_lines(&p.reason)
        }
        event::MCPSTARTED => {
            let p: event::McpStartedPayload = ev.decode()?;
            vec![notice(
                Kind::Dim,
                glyph::MCP,
                Tone::None,
                titled(
                    "MCP",
                    &format!("{} connected · {} tools", p.server, p.tools.len()),
                ),
            )]
        }
        event::MCPFAILED => {
            let p: event::McpFailedPayload = ev.decode()?;
            vec![notice(
                Kind::Error,
                glyph::MCP,
                Tone::Error,
                titled("MCP", &format!("{} failed: {}", p.server, p.error)),
            )]
        }
        event::MCPSTOPPED => {
            let p: event::McpRefPayload = ev.decode()?;
            vec![notice(
                Kind::Dim,
                glyph::MCP,
                Tone::None,
                titled("MCP", &format!("{} stopped", p.server)),
            )]
        }
        event::CHANNEL_DIR_ADDED => dir_added_lines(&ev.decode()?),
        event::COMPACTION_STARTED => vec![
            Line::new(Kind::Blank, ""),
            Line::new(Kind::Rule, glyph::COMPACTING),
            Line::new(Kind::Blank, ""),
        ],
        event::COMPACTION_FAILED => {
            let p: event::CompactionPayload = ev.decode()?;
            vec![
                Line::new(Kind::Blank, ""),
                Line::new(Kind::Dim, titled("Compaction failed", &p.error)),
                Line::new(Kind::Blank, ""),
            ]
        }
        event::COMPACTION_DONE => {
            let p: event::CompactionPayload = ev.decode()?;
            let rule = if p.before > 0 && p.after > 0 {
                format!(
                    "┄┄ compacted {} → {} tokens ┄┄",
                    format::tokens(p.before),
                    format::tokens(p.after)
                )
            } else {
                glyph::COMPACTED.into()
            };
            let mut lines = vec![Line::new(Kind::Blank, ""), Line::new(Kind::Rule, rule)];
            if !p.summary.trim().is_empty() {
                lines.extend(trunc_lines(p.summary.trim(), 3, Kind::Dim));
            }
            lines.push(Line::new(Kind::Blank, ""));
            lines
        }
        event::ASK_REQUESTED => {
            let p: event::AskRequestedPayload = ev.decode()?;
            let (glyph, text) = match p.kind.as_str() {
                protocol::PROMPT_QUESTION => (
                    glyph::PROMPT,
                    titled("Question", &format::first_line(&p.question)),
                ),
                protocol::PROMPT_TRUST => (glyph::PERMISSION, titled("Trust requested", "")),
                _ => (glyph::PERMISSION, titled("Permission", &p.tool)),
            };
            vec![notice(Kind::Notice, glyph, Tone::Working, text)]
        }
        event::ASK_RESOLVED => {
            let p: event::AskResolvedPayload = ev.decode()?;
            let (tone, text) = match p.outcome.as_str() {
                event::ASK_DEFAULTED => (
                    Tone::Error,
                    titled("Defaulted", &format::first_line(&p.answer)),
                ),
                event::ASK_WITHDRAWN => (Tone::Error, titled("Prompt withdrawn", "")),
                _ => (
                    if p.answer.to_lowercase().starts_with("deny") {
                        Tone::Error
                    } else {
                        Tone::None
                    },
                    titled("Answered", &format::first_line(&p.answer)),
                ),
            };
            vec![notice(Kind::Notice, glyph::ANSWER, tone, text)]
        }
        _ => vec![],
    })
}
pub fn tool_started_lines(name: &str, call_id: &str, raw: &str) -> Vec<Line> {
    let input = serde_json::from_str::<Box<RawValue>>(raw).ok();
    let name = toolname::operation_raw(name, input.as_deref());
    if matches!(name, toolname::MESSAGE | toolname::AGENT_CREATE) {
        let (who, text) = prompt_of(name, raw);
        let mut line = Line {
            kind: Kind::Tool,
            text: tool_line(name, raw),
            running: true,
            tool: name.into(),
            who,
            ..Default::default()
        };
        let value = serde_json::from_str::<serde_json::Value>(raw).unwrap_or_default();
        if name == toolname::MESSAGE
            && value.as_object().is_some_and(|object| {
                object
                    .iter()
                    .any(|(key, value)| key.eq_ignore_ascii_case("kind") && value == "info")
            })
        {
            line.glyph = glyph::INFO_SENT.into();
        }
        let mut lines = vec![line];
        if let Some((_, rest)) = text.split_once('\n') {
            lines.extend(output_lines(rest));
        }
        return lines;
    }
    let call = Line {
        kind: Kind::Tool,
        text: tool_line(name, raw),
        running: true,
        call_id: call_id.into(),
        tool: name.into(),
        ..Default::default()
    };
    let mut lines = vec![call];
    if name == toolname::PATCH {
        let value = serde_json::from_str::<serde_json::Value>(raw).unwrap_or_default();
        let patch = value
            .as_object()
            .and_then(|object| {
                object
                    .iter()
                    .find(|(key, _)| key.eq_ignore_ascii_case("patch"))
                    .and_then(|(_, value)| value.as_str())
            })
            .unwrap_or("");
        lines.extend(diff_lines(patch));
    }
    lines
}
pub fn denial_mark(output: &str) -> String {
    let output = output.trim();
    if output.starts_with("Denied by policy:") {
        "(by policy)".into()
    } else if output.starts_with("Permission denied: nobody answered") {
        "(no answer)".into()
    } else if output.starts_with("Denied in auto mode:") {
        "(outside dirs, auto mode)".into()
    } else if let Some(reason) = output
        .strip_prefix("Permission denied by the user:")
        .map(str::trim)
        .map(|s| s.strip_suffix('.').unwrap_or(s))
        .filter(|s| !s.is_empty())
    {
        format!("({reason})")
    } else {
        String::new()
    }
}
pub fn job_finished_lines(payload: &event::JobFinishedPayload) -> Vec<Line> {
    let summary = payload.summary.trim();
    let summary = summary.strip_prefix("Job ").unwrap_or(summary);
    let summary = if summary.is_empty() {
        "finished"
    } else {
        summary
    };
    let mut lines = vec![notice(
        Kind::Dim,
        glyph::JOB,
        if payload.is_error {
            Tone::Error
        } else {
            Tone::None
        },
        titled("Job", summary),
    )];
    lines.extend(output_lines(payload.output.trim_end_matches('\n')));
    lines
}
pub fn job_stopped_lines(reason: &str) -> Vec<Line> {
    let reason = reason.trim();
    vec![notice(
        Kind::Dim,
        glyph::JOB,
        Tone::Error,
        titled(
            "Job stopped",
            &if reason.is_empty() {
                String::new()
            } else {
                format!("({reason})")
            },
        ),
    )]
}
pub fn dir_added_lines(payload: &event::DirPayload) -> Vec<Line> {
    vec![notice(
        Kind::Dim,
        glyph::FILES,
        Tone::None,
        titled(
            "Dirs",
            &format!(
                "+ {} ({})",
                format::short_home(&payload.dir, &std::env::var("HOME").unwrap_or_default()),
                payload.source
            ),
        ),
    )]
}
