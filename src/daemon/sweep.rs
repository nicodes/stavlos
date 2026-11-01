//! Expire only tool overflow output under held directory capabilities.
use anyhow::Result;
use cap_std::{ambient_authority, fs::Dir};
use std::{path::Path, time::SystemTime};
pub fn older(root: &Path, cutoff: SystemTime) -> Result<usize> {
    let root = match Dir::open_ambient_dir(root, ambient_authority()) {
        Ok(root) => root,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(0),
        Err(error) => return Err(error.into()),
    };
    let mut removed = 0;
    for entry in root.entries()? {
        let entry = entry?;
        if !entry.file_type()?.is_dir() {
            continue;
        }
        let Ok(channel) = root.open_dir(entry.file_name()) else {
            continue;
        };
        let Ok(output) = channel.open_dir("output") else {
            continue;
        };
        let Ok(entries) = output.entries() else {
            continue;
        };
        for entry in entries.flatten() {
            let Ok(kind) = entry.file_type() else {
                continue;
            };
            if !kind.is_file() {
                continue;
            }
            let Ok(metadata) = entry.metadata() else {
                continue;
            };
            let Ok(modified) = metadata.modified() else {
                continue;
            };
            if modified.into_std() < cutoff && output.remove_file(entry.file_name()).is_ok() {
                removed += 1;
            }
        }
        if output.entries()?.next().is_none() {
            let _ = channel.remove_dir("output");
        }
    }
    Ok(removed)
}
