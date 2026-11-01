use super::api::Channel;
use anyhow::{Result, ensure};
use std::{collections::BTreeMap, path::Path};
pub fn allowed(directory: &str, grants: &[String]) -> bool {
    std::fs::canonicalize(directory)
        .is_ok_and(|path| grants.iter().any(|grant| path == Path::new(grant)))
}
pub fn category(channels: &[Channel], name: &str) -> Result<Option<String>> {
    let mut matches = channels
        .iter()
        .filter(|channel| channel.kind == 4 && channel.name == name);
    let first = matches.next().map(|channel| channel.id.clone());
    ensure!(
        matches.next().is_none(),
        "multiple Discord categories match configured category"
    );
    Ok(first)
}
pub fn markers(channels: &[Channel], category: &str) -> Result<BTreeMap<String, Channel>> {
    let mut out = BTreeMap::new();
    for channel in channels {
        if channel.kind != 0 || channel.parent_id.as_deref() != Some(category) {
            continue;
        }
        let mut marked = false;
        for line in channel.topic.as_deref().unwrap_or("").split('\n') {
            let Some(id) = line
                .strip_prefix("stavlos-channel:")
                .map(str::trim)
                .filter(|id| !id.is_empty())
            else {
                continue;
            };
            ensure!(
                !marked,
                "discord channel {} has multiple stavlos markers",
                channel.id
            );
            marked = true;
            ensure!(
                !out.contains_key(id),
                "duplicate Discord topic marker for {id}"
            );
            out.insert(id.into(), channel.clone());
        }
    }
    Ok(out)
}
