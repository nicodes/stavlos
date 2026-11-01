//! The shared byte budget for tool output, job reports and summaries.
pub const DEFAULT_MAX: usize = 50 * 1024;
pub fn head(text: &str, mut count: usize) -> &str {
    if text.len() <= count {
        return text;
    }
    while count > 0 && !text.is_char_boundary(count) {
        count -= 1;
    }
    &text[..count]
}
pub fn tail(text: &str, count: usize) -> &str {
    if text.len() <= count {
        return text;
    }
    let mut start = text.len() - count;
    while start < text.len() && !text.is_char_boundary(start) {
        start += 1;
    }
    &text[start..]
}
pub fn middle(text: &str, maximum: i64) -> String {
    let maximum = if maximum <= 0 {
        DEFAULT_MAX
    } else {
        maximum as usize
    };
    if text.len() <= maximum {
        return text.into();
    }
    let first = maximum * 2 / 3;
    format!(
        "{}\n\n… [{} bytes truncated: narrow the command (grep, head, tail, a smaller range) or read with an offset for the part you need] …\n\n{}",
        head(text, first),
        text.len() - maximum,
        tail(text, maximum - first)
    )
}
