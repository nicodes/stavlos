//! Persisted identifiers and the shared agent/channel label rules.
use anyhow::{Result, ensure};
pub const MAX_NAME: usize = 32;
pub fn new_id(prefix: &str) -> Result<String> {
    let mut bytes = [0; 5];
    getrandom::fill(&mut bytes).map_err(|e| anyhow::anyhow!("random identifier: {e}"))?;
    Ok(format!(
        "{prefix}{}",
        bytes.iter().map(|b| format!("{b:02x}")).collect::<String>()
    ))
}
pub fn normalize(label: &str) -> String {
    let mut out = String::new();
    let mut dash = false;
    for c in crate::protocol::simple_lower(label.trim()).chars() {
        if c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, '_' | '-') {
            out.push(c);
            dash = c == '-';
        } else if !dash && !out.is_empty() {
            out.push('-');
            dash = true;
        }
    }
    let name = out.trim_matches(['-', '_']);
    name[..name.len().min(MAX_NAME)]
        .trim_end_matches(['-', '_'])
        .into()
}
pub fn unique(want: &str, fallback: &str, mut taken: impl FnMut(&str) -> bool) -> String {
    let mut base = normalize(want);
    if base.is_empty() {
        base = normalize(fallback);
    }
    if base.is_empty() {
        base = "agent".into();
    }
    let mut name = base.clone();
    let mut number = 2;
    while taken(&name) {
        let suffix = format!("-{number}");
        name = format!(
            "{}{suffix}",
            base[..base.len().min(MAX_NAME - suffix.len())].trim_end_matches(['-', '_'])
        );
        number += 1;
    }
    name
}
pub fn reserved(name: &str) -> bool {
    matches!(name, "user" | "human" | "system")
}
pub fn claim(
    want: &str,
    fallback: &str,
    id: &str,
    names: &std::collections::BTreeMap<String, String>,
) -> Result<String> {
    let name = unique(want, fallback, |name| {
        names.get(name).is_some_and(|owner| owner != id)
    });
    let mut base = normalize(want);
    if base.is_empty() {
        base = normalize(fallback);
    }
    ensure!(
        !reserved(&base),
        "label {} is reserved: a name may not read as the human or the system",
        serde_json::to_string(&base)?
    );
    Ok(name)
}
