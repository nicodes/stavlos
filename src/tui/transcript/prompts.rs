use super::{Kind, Line, clean, glyph, linked, message_address, tool_glyph, tool_title};
use crate::{event, protocol, textsafe, toolname, tui::format};
pub fn question_header(prompt: &protocol::PromptInfo, index: usize, chat: bool) -> Line {
    let mut from = prompt.from.clone();
    if from.is_empty() {
        from = format::short_id(&prompt.agent);
    }
    if from.is_empty() {
        from = "agent".into();
    }
    let question = prompt
        .questions
        .get(index)
        .map(|question| question.question.as_str())
        .unwrap_or(&prompt.question);
    let viewer = if chat { toolname::USER } else { &from };
    let (address, names) = message_address(&from, &[toolname::USER.into()], viewer);
    Line {
        kind: Kind::Text,
        glyph: glyph::PROMPT.into(),
        text: format!("{address} {question}"),
        who: from,
        names: names.into(),
        ..Default::default()
    }
}
pub fn question_lines(
    prompt: &protocol::PromptInfo,
    result: Option<&event::AskResolvedPayload>,
    chat: bool,
) -> Vec<Line> {
    let fallback = [protocol::Question {
        question: prompt.question.clone(),
        ..Default::default()
    }];
    let questions = if prompt.questions.is_empty() {
        &fallback[..]
    } else {
        &prompt.questions
    };
    let mut lines = vec![];
    for (index, question) in questions.iter().enumerate() {
        lines.push(question_header(prompt, index, chat));
        for (number, option) in question.options.iter().enumerate() {
            let checked = result
                .and_then(|result| result.details.get(index))
                .is_some_and(|details| details.selected.contains(&(number as i64)));
            let mut text = format!("{} {}", if checked { "■" } else { "□" }, option.label);
            if !option.description.is_empty() {
                text.push_str(&format!(" — {}", option.description));
            }
            lines.push(Line {
                kind: Kind::Text,
                text,
                indent: 2,
                ..Default::default()
            });
        }
        if let Some(details) = result.and_then(|result| result.details.get(index))
            && !details.custom.trim().is_empty()
        {
            lines.push(Line {
                kind: Kind::Text,
                text: format!("■ {}", details.custom),
                indent: 2,
                ..Default::default()
            });
        }
    }
    let note = if let Some(result) = result {
        match result.outcome.as_str() {
            event::ASK_WITHDRAWN => "Question withdrawn".into(),
            event::ASK_DEFAULTED => format!("Defaulted: {}", result.answer),
            event::ASK_ANSWERED if result.details.is_empty() => {
                format!("Answer: {}", result.answer)
            }
            _ => "Submitted".into(),
        }
    } else {
        "Waiting for answer".into()
    };
    lines.push(Line {
        kind: Kind::Dim,
        text: note,
        indent: 1,
        ..Default::default()
    });
    if result.is_some() {
        for line in &mut lines {
            line.note = true;
        }
    }
    linked(clean(lines), &prompt.agent)
}
pub fn permission_header(prompt: &protocol::PromptInfo, chat: bool) -> Line {
    let mut prompt = prompt.clone();
    prompt.question = format!("Permission: {}", tool_title(&prompt.tool));
    prompt.questions = Default::default();
    let mut head = question_header(&prompt, 0, chat);
    head.glyph = glyph::PERMISSION.into();
    head
}
pub fn permission_lines(
    prompt: &protocol::PromptInfo,
    result: Option<&event::AskResolvedPayload>,
    chat: bool,
) -> Vec<Line> {
    let mut lines = vec![permission_header(prompt, chat)];
    let raw = prompt.input.as_ref().map(|raw| raw.get()).unwrap_or("");
    let mut subject = raw.to_owned();
    if prompt.tool == toolname::SHELL
        && let Ok(fields) = crate::config::jsonc::object_fields(raw.as_bytes())
    {
        let mut command = String::new();
        let mut valid = true;
        for (key, value) in fields {
            if key.eq_ignore_ascii_case("command") {
                if let Ok(value) = serde_json::from_slice::<Option<String>>(&value) {
                    if let Some(value) = value {
                        command = value;
                    }
                } else {
                    valid = false;
                }
            }
        }
        if valid {
            subject = command.trim_end_matches('\n').into();
        }
    } else if prompt.tool == toolname::SHELL && raw.trim() == "null" {
        subject.clear();
    }
    for (index, text) in textsafe::visible(&subject).split('\n').enumerate() {
        if !text.is_empty() {
            lines.push(Line {
                kind: Kind::Text,
                text: text.into(),
                glyph: if index == 0 {
                    tool_glyph(&prompt.tool).0.into()
                } else {
                    String::new()
                },
                indent: if index == 0 { 1 } else { 2 },
                ..Default::default()
            });
        }
    }
    if !prompt.dir.is_empty() {
        lines.push(Line {
            kind: Kind::Text,
            text: format!("Outside the channel's directories: {}", prompt.dir),
            indent: 1,
            ..Default::default()
        });
    }
    lines.push(Line {
        kind: Kind::Dim,
        text: result
            .map(|result| protocol::permission_result(prompt, result))
            .unwrap_or_else(|| "Waiting for permission".into()),
        indent: 1,
        ..Default::default()
    });
    if result.is_some() {
        for line in &mut lines {
            line.note = true;
        }
    }
    let lines = clean(lines);
    if chat {
        linked(lines, &prompt.agent)
    } else {
        lines
    }
}
