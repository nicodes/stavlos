pub fn units(text: &str) -> usize {
    text.encode_utf16().count()
}
pub fn clip(text: &str, limit: usize) -> String {
    let mut used = 0;
    text.chars()
        .take_while(|c| {
            used += c.len_utf16();
            used <= limit
        })
        .collect()
}
pub fn clip_markdown(text: &str, limit: usize) -> String {
    if units(text) <= limit {
        return text.into();
    }
    let mut part = clip(text, limit.saturating_sub(4));
    if part
        .split('\n')
        .filter(|line| line.starts_with("```"))
        .count()
        % 2
        == 1
    {
        part.push_str("\n```");
    }
    part
}
pub fn escape(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    for c in text.chars() {
        if matches!(c, '\\' | '*' | '_' | '`') {
            out.push('\\');
        }
        out.push(c);
    }
    out
}
pub fn question_heading(question: &str, limit: usize) -> String {
    let mut question = escape(question);
    let room = limit.saturating_sub(units("❓ ****"));
    if units(&question) > room {
        question = format!(
            "{}…",
            clip(&question, room.saturating_sub(1)).trim_end_matches('\\')
        );
    }
    format!("❓ **{question}**")
}
pub fn split(text: &str) -> Vec<String> {
    let mut text = text;
    let mut fence = String::new();
    let mut out = Vec::new();
    while !text.is_empty() {
        let prefix = if fence.is_empty() {
            String::new()
        } else {
            format!("{fence}\n")
        };
        let mut part = clip(text, 1950 - units(&prefix));
        if part.len() < text.len()
            && let Some(index) = part.rfind('\n')
            && index > part.len() / 2
        {
            part.truncate(index + 1);
        }
        text = &text[part.len()..];
        for line in part.split('\n') {
            if line.starts_with("```") {
                fence = if fence.is_empty() {
                    clip(line, 40)
                } else {
                    String::new()
                };
            }
        }
        let mut message = prefix + &part;
        if !fence.is_empty() {
            message.push_str("\n```");
        }
        out.push(message);
    }
    out
}
pub fn strip_bot(text: &str, bot: &str) -> String {
    let text = text.trim();
    for mention in [format!("<@{bot}>"), format!("<@!{bot}>")] {
        if text == mention {
            return String::new();
        }
        if let Some(rest) = text.strip_prefix(&mention)
            && rest.starts_with([' ', '\n', '\t'])
        {
            return rest.trim().into();
        }
    }
    text.into()
}
pub fn addressed(recipients: &[String], body: &str) -> String {
    let names = recipients
        .iter()
        .filter(|name| name.as_str() != "user")
        .map(|name| format!("@{name}"))
        .collect::<Vec<_>>();
    if names.is_empty() {
        body.into()
    } else if body.is_empty() {
        names.join(" ")
    } else {
        format!("{} {body}", names.join(" "))
    }
}
