//! Bounded channel-owned HTML pages, recorded as facts after filesystem changes.
use super::journal::{Core, State};
use crate::{
    event, pathx,
    policy::{Kind, Subject},
    protocol,
    toolfs::{self, Filesystem},
    tools,
};
use anyhow::{Result, bail, ensure};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    fs,
    os::unix::fs::DirBuilderExt,
    path::{Path, PathBuf},
    sync::Arc,
};
use tokio_util::sync::CancellationToken;
pub const MAX_SHEETS: usize = 50;
pub const MAX_SIZE: usize = 2 << 20;
pub const MAX_TITLE: usize = 200;
#[derive(Clone)]
pub struct SheetService {
    pub core: Arc<Core>,
    pub agent: String,
    pub directory: PathBuf,
}
pub fn directory(paths: &crate::paths::Paths, channel: &str) -> PathBuf {
    paths.data.join("sheets").join(channel)
}
pub fn path(directory: &Path, id: &str) -> PathBuf {
    directory.join(format!("{id}.html"))
}
fn valid_id(id: &str) -> bool {
    id.strip_prefix('s').is_some_and(|number| {
        !number.is_empty() && number.bytes().all(|byte| byte.is_ascii_digit())
    })
}
fn sheet_id(path: &Path) -> Option<String> {
    let id = path.file_name()?.to_str()?.strip_suffix(".html")?;
    valid_id(id).then(|| id.to_owned())
}
pub fn read_sheet(path: &Path) -> Result<Vec<u8>> {
    let directory = path
        .parent()
        .ok_or_else(|| anyhow::anyhow!("sheet has no directory"))?;
    let filesystem = Filesystem {
        directory: directory.to_owned(),
        roots: vec![directory.to_owned()],
        ..Default::default()
    };
    let (body, _) = filesystem.read_bounded(path, MAX_SIZE)?;
    Ok(body)
}
impl SheetService {
    fn filesystem(&self) -> Filesystem {
        Filesystem {
            directory: self.directory.clone(),
            roots: vec![self.directory.clone()],
            ..Default::default()
        }
    }
    pub fn infos(&self) -> Vec<protocol::SheetInfo> {
        self.core.inspect(|state, _| {
            state
                .sorted_sheets()
                .into_iter()
                .map(|sheet| protocol::SheetInfo {
                    id: sheet.id.clone(),
                    title: sheet.title.clone(),
                    author: sheet.author.clone(),
                    hash: sheet.hash.clone(),
                    size: sheet.size,
                    updated: sheet.updated,
                })
                .collect()
        })
    }
    pub fn get(&self, id: &str) -> Result<(protocol::SheetInfo, Vec<u8>)> {
        if let Some(sheet) = self.infos().into_iter().find(|sheet| sheet.id == id) {
            let body = read_sheet(&path(&self.directory, id))?;
            return Ok((sheet, body));
        }
        bail!("no sheet {}", serde_json::to_string(id)?)
    }
    fn reference(&self, sheet: protocol::SheetInfo) -> tools::SheetRef {
        tools::SheetRef {
            id: sheet.id.clone(),
            title: sheet.title,
            author: sheet.author,
            size: sheet.size,
            path: path(&self.directory, &sheet.id)
                .to_string_lossy()
                .into_owned(),
        }
    }
    fn record_written(&self, state: &mut State, id: &str, title: &str, body: &[u8]) -> Result<()> {
        let author = state
            .channel
            .agents
            .get(&self.agent)
            .map(|agent| agent.name.clone())
            .ok_or_else(|| anyhow::anyhow!("agent not found"))?;
        self.core.fact_locked(
            state,
            vec![self.core.event(
                &self.agent,
                event::SHEET_WRITTEN,
                event::SheetPayload {
                    id: id.into(),
                    title: title.into(),
                    author,
                    hash: format!("{:x}", Sha256::digest(body)),
                    size: body.len() as i64,
                },
            )?],
            &CancellationToken::new(),
        )
    }
    pub fn guard(&self, subject: &Subject, base: &Path) -> Result<Option<SheetGuard>> {
        if subject.kind != Kind::Path {
            return Ok(None);
        }
        let directory = toolfs::resolve_path(Path::new(""), &self.directory);
        let mut before = BTreeMap::new();
        for value in &subject.values {
            let resolved = toolfs::resolve_path(base, Path::new(value));
            if resolved == directory || pathx::inside(&directory, &resolved).is_none() {
                continue;
            }
            let known = sheet_id(&resolved).is_some_and(|id| {
                resolved.parent() == Some(&directory)
                    && self.core.inspect(|state, _| state.sheets.contains_key(&id))
            });
            ensure!(
                known,
                "Refused: {value} is in the channel's sheets directory but is no sheet. Sheets are created with the sheet tool (action write); read and patch edit the file of a sheet that exists."
            );
            before.insert(
                resolved.clone(),
                read_sheet(&resolved).map_err(|error| anyhow::anyhow!("Refused: {error}"))?,
            );
        }
        Ok((!before.is_empty()).then(|| SheetGuard {
            filesystem: self.filesystem(),
            before,
        }))
    }
    pub fn patched(&self, subject: &Subject, base: &Path) {
        if subject.kind != Kind::Path {
            return;
        }
        let directory = toolfs::resolve_path(Path::new(""), &self.directory);
        for value in &subject.values {
            let resolved = toolfs::resolve_path(base, Path::new(value));
            let Some(id) = sheet_id(&resolved) else {
                continue;
            };
            if resolved.parent() != Some(&directory) {
                continue;
            }
            let page = read_sheet(&resolved);
            let mut state = self.core.state.lock().unwrap();
            if !state.channel.sheets.contains_key(&id) {
                continue;
            }
            match page {
                Ok(body) => {
                    let _ = self.record_written(&mut state, &id, "", &body);
                }
                Err(error)
                    if error
                        .downcast_ref::<std::io::Error>()
                        .is_some_and(|error| error.kind() == std::io::ErrorKind::NotFound) =>
                {
                    if let Ok(event) = self.core.event(
                        &self.agent,
                        event::SHEET_DELETED,
                        event::SheetPayload {
                            id,
                            ..Default::default()
                        },
                    ) {
                        let _ = self.core.fact_locked(
                            &mut state,
                            vec![event],
                            &CancellationToken::new(),
                        );
                    }
                }
                Err(_) => {}
            }
        }
    }
}
impl tools::Sheets for SheetService {
    fn list(&self) -> Vec<tools::SheetRef> {
        self.infos()
            .into_iter()
            .map(|sheet| self.reference(sheet))
            .collect()
    }
    fn write(&self, id: &str, title: &str, html: &str) -> Result<tools::SheetRef> {
        ensure!(
            html.len() <= MAX_SIZE,
            "the page is {} bytes; a sheet holds at most {MAX_SIZE}",
            html.len()
        );
        ensure!(
            title.chars().count() <= MAX_TITLE,
            "the title is {} characters; a sheet's title holds at most {MAX_TITLE}",
            title.chars().count()
        );
        let mut state = self.core.state.lock().unwrap();
        ensure!(!state.stopped, "channel is stopped");
        let id = if id.is_empty() {
            ensure!(
                state.channel.sheets.len() < MAX_SHEETS,
                "this channel already has {MAX_SHEETS} sheets: replace or delete one"
            );
            format!(
                "s{}",
                state
                    .channel
                    .sheet_seq
                    .checked_add(1)
                    .ok_or_else(|| anyhow::anyhow!("sheet identifier space exhausted"))?
            )
        } else {
            ensure!(
                valid_id(id) && state.channel.sheets.contains_key(id),
                "no sheet {}: leave id empty to create one",
                serde_json::to_string(id)?
            );
            id.into()
        };
        ensure!(
            state.channel.agents.contains_key(&self.agent),
            "agent not found"
        );
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&self.directory)?;
        self.filesystem()
            .write_atomic(&path(&self.directory, &id), html.as_bytes(), 0o600)?;
        self.record_written(&mut state, &id, title, html.as_bytes())?;
        let sheet = state
            .channel
            .sheets
            .get(&id)
            .ok_or_else(|| anyhow::anyhow!("the sheet was not recorded"))?;
        Ok(tools::SheetRef {
            id: id.clone(),
            title: sheet.title.clone(),
            author: sheet.author.clone(),
            size: sheet.size,
            path: path(&self.directory, &id).to_string_lossy().into_owned(),
        })
    }
    fn delete(&self, id: &str) -> Result<()> {
        let mut state = self.core.state.lock().unwrap();
        ensure!(!state.stopped, "channel is stopped");
        ensure!(
            valid_id(id) && state.channel.sheets.contains_key(id),
            "no sheet {}",
            serde_json::to_string(id)?
        );
        if let Err(error) = self.filesystem().remove(&path(&self.directory, id))
            && error
                .downcast_ref::<std::io::Error>()
                .is_none_or(|error| error.kind() != std::io::ErrorKind::NotFound)
        {
            return Err(error);
        }
        self.core.fact_locked(
            &mut state,
            vec![self.core.event(
                &self.agent,
                event::SHEET_DELETED,
                event::SheetPayload {
                    id: id.into(),
                    ..Default::default()
                },
            )?],
            &CancellationToken::new(),
        )
    }
}
pub struct SheetGuard {
    filesystem: Filesystem,
    before: BTreeMap<PathBuf, Vec<u8>>,
}
impl SheetGuard {
    pub fn finish(self) -> Result<String> {
        let mut notes = Vec::new();
        for (path, body) in self.before {
            if let Ok(metadata) = fs::metadata(&path)
                && metadata.len() > MAX_SIZE as u64
            {
                self.filesystem.write_atomic(&path, &body, 0o600)?;
                notes.push(format!("Undone: the patch made {} {} bytes, and a sheet holds at most {MAX_SIZE}. The sheet is as it was.",path.file_name().unwrap_or_default().to_string_lossy(),metadata.len()));
            }
        }
        Ok(notes.join("\n"))
    }
}
