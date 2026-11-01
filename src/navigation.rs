//! The last human selection is independent of daemon activity and the launch directory.
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use std::{
    io::Read,
    os::unix::fs::{DirBuilderExt, OpenOptionsExt},
    path::Path,
    sync::Mutex,
};
#[derive(Default, Serialize, Deserialize)]
#[serde(default)]
struct State {
    channel: String,
    viewed: i64,
}
static WRITE: Mutex<()> = Mutex::new(());
fn load(data: &Path) -> State {
    (|| -> Result<State> {
        let file = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
            .open(data.join("navigation.json"))?;
        ensure!(
            file.metadata()?.is_file(),
            "navigation state is not a regular file"
        );
        let mut raw = vec![];
        file.take(64 * 1024 + 1).read_to_end(&mut raw)?;
        ensure!(raw.len() <= 64 * 1024, "navigation state is too large");
        Ok(serde_json::from_slice(&raw)?)
    })()
    .unwrap_or_default()
}
pub fn last(data: &Path) -> String {
    load(data).channel
}
pub fn remember(data: &Path, channel: &str, selected: i64) -> Result<()> {
    let _write = WRITE.lock().unwrap();
    if channel.is_empty() || load(data).viewed > selected {
        return Ok(());
    }
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(data)?;
    crate::statefile::write_atomic(
        &data.join("navigation.json"),
        &serde_json::to_vec(&State {
            channel: channel.into(),
            viewed: selected,
        })?,
        0o600,
        false,
    )?;
    Ok(())
}
pub fn resume(channels: &[crate::protocol::ChannelInfo], last: &str) -> String {
    channels
        .iter()
        .find(|channel| channel.id == last && !channel.archived)
        .or_else(|| channels.iter().find(|channel| !channel.archived))
        .map(|channel| channel.id.clone())
        .unwrap_or_default()
}
