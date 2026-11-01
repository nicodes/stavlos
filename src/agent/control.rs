//! Trusted instruction attachment and bounded detection of changes to harness controls.
use super::{journal::Core, scope};
use crate::{
    config::Effective,
    instructions,
    paths::Paths,
    policy::{Kind, Subject},
    toolfs,
};
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, BTreeSet},
    fs,
    os::unix::{
        ffi::OsStrExt,
        fs::{FileTypeExt, MetadataExt},
    },
    path::{Path, PathBuf},
    sync::Mutex,
};
const CONTROL_PATHS: &[&str] = &[
    ".git/hooks",
    ".git/config",
    ".stavlos",
    ".agents",
    ".envrc",
    "AGENTS.md",
    "CLAUDE.md",
];
const MAX_ENTRIES: usize = 200;
const MAX_FILE: usize = 16 << 20;
const MAX_TOTAL: usize = 64 << 20;
pub type ControlStamp = BTreeMap<PathBuf, String>;
pub fn control_paths(config: &Effective, directories: &[PathBuf], paths: &Paths) -> Vec<PathBuf> {
    let mut seen = BTreeSet::new();
    let mut out = Vec::new();
    for path in directories
        .iter()
        .flat_map(|directory| CONTROL_PATHS.iter().map(move |name| directory.join(name)))
        .chain(std::iter::once(paths.shared_skills.clone()))
        .chain(config.instruction_files.iter().cloned())
    {
        let path = toolfs::resolve_path(Path::new(""), &path);
        if seen.insert(path.clone()) {
            out.push(path);
        }
    }
    out
}
pub fn stamp_control(config: &Effective, directories: &[PathBuf], paths: &Paths) -> ControlStamp {
    control_paths(config, directories, paths)
        .into_iter()
        .map(|path| {
            let hash = hash_path(&path);
            (path, hash)
        })
        .collect()
}
pub fn control_note(before: &ControlStamp) -> String {
    let changed: Vec<_> = before
        .iter()
        .filter(|(path, was)| hash_path(path) != **was)
        .map(|(path, _)| path.to_string_lossy().into_owned())
        .collect();
    if changed.is_empty() {
        String::new()
    } else {
        format!(
            "[this command changed {}, which steers the harness or runs code later. The human should review the change before git or a shell runs it; do not do that yourself.]",
            changed.join(", ")
        )
    }
}
fn mode_string(metadata: &fs::Metadata) -> String {
    let kind = metadata.file_type();
    let mut out = String::new();
    if kind.is_dir() {
        out.push('d');
    }
    if kind.is_symlink() {
        out.push('L');
    }
    if kind.is_block_device() || kind.is_char_device() {
        out.push('D');
    }
    if kind.is_fifo() {
        out.push('p');
    }
    if kind.is_socket() {
        out.push('S');
    }
    let mode = metadata.mode();
    if mode & 0o4000 != 0 {
        out.push('u');
    }
    if mode & 0o2000 != 0 {
        out.push('g');
    }
    if kind.is_char_device() {
        out.push('c');
    }
    if mode & 0o1000 != 0 {
        out.push('t');
    }
    if out.is_empty() {
        out.push('-');
    }
    for (index, ch) in "rwxrwxrwx".chars().enumerate() {
        out.push(if mode & (1 << (8 - index)) != 0 {
            ch
        } else {
            '-'
        });
    }
    out
}
fn content(hash: &mut Sha256, path: &Path, total: &mut usize) {
    match instructions::read_regular(path, MAX_FILE.min(MAX_TOTAL.saturating_sub(*total))) {
        Ok(bytes) => {
            *total += bytes.len();
            hash.update(bytes);
        }
        Err(_) => {
            // A FIFO never blocks a model turn. Oversized files retain a change detector.
            if let Ok(metadata) = fs::metadata(path)
                && metadata.is_file()
                && (metadata.len() > MAX_FILE as u64
                    || metadata.len() > MAX_TOTAL.saturating_sub(*total) as u64)
            {
                hash.update(b"\0bounded\0");
                hash.update(metadata.len().to_le_bytes());
                hash.update(metadata.mtime().to_le_bytes());
                hash.update(metadata.mtime_nsec().to_le_bytes());
            }
        }
    }
}
pub fn hash_path(path: &Path) -> String {
    let Ok(metadata) = fs::symlink_metadata(path) else {
        return String::new();
    };
    let mut hash = Sha256::new();
    let mut total = 0;
    if !metadata.is_dir() {
        hash.update(mode_string(&metadata));
        content(&mut hash, path, &mut total);
        return format!("{:x}", hash.finalize());
    }
    let mut stack = Vec::new();
    let children = |directory: &Path| -> Vec<PathBuf> {
        let mut out = fs::read_dir(directory)
            .into_iter()
            .flatten()
            .filter_map(|entry| entry.ok().map(|entry| entry.path()))
            .collect::<Vec<_>>();
        out.sort();
        out
    };
    stack.extend(children(path).into_iter().rev());
    let mut count = 0;
    while let Some(path) = stack.pop() {
        count += 1;
        if count > MAX_ENTRIES {
            break;
        }
        hash.update(path.as_os_str().as_bytes());
        hash.update([0]);
        if let Ok(metadata) = fs::symlink_metadata(&path) {
            hash.update(mode_string(&metadata));
            if metadata.is_file() {
                content(&mut hash, &path, &mut total);
            }
            if metadata.is_dir() {
                stack.extend(children(&path).into_iter().rev());
            }
        }
        hash.update([0]);
    }
    format!("{:x}", hash.finalize())
}
pub fn instructions_stamp(files: &[PathBuf]) -> String {
    let mut out = String::new();
    use std::fmt::Write;
    for file in files {
        if let Ok(metadata) = fs::metadata(file) {
            let nanos =
                ((metadata.mtime() as i128) * 1_000_000_000 + metadata.mtime_nsec() as i128) as i64;
            let _ = write!(out, "{}\0{}\0{nanos}\0", file.display(), metadata.len());
        } else {
            let _ = write!(out, "{}\0gone\0", file.display());
        }
    }
    out
}
pub fn trust_stamp(config: &Effective) -> String {
    instructions_stamp(
        &config
            .trust_files
            .iter()
            .map(|file| config.dir.join(file))
            .collect::<Vec<_>>(),
    )
}
#[derive(Default)]
pub struct InstructionCache {
    given: Mutex<Option<BTreeSet<String>>>,
}
pub struct Attachment {
    pub note: String,
    pub carried: Vec<String>,
    pub project_changed: bool,
}
impl InstructionCache {
    pub fn clear(&self) {
        *self.given.lock().unwrap() = None;
    }
    pub fn for_subject(
        &self,
        core: &Core,
        agent: &str,
        subject: &Subject,
        config: &Effective,
        base: &Path,
        home: &Path,
    ) -> Attachment {
        let mut out = Attachment {
            note: String::new(),
            carried: Vec::new(),
            project_changed: false,
        };
        if subject.kind != Kind::Path || !config.project_trusted {
            return out;
        }
        let trusted: BTreeSet<_> = config
            .instruction_files
            .iter()
            .map(|path| toolfs::resolve_path(Path::new(""), path))
            .collect();
        let mut found = Vec::new();
        for value in &subject.values {
            for file in instructions::between(base, &toolfs::resolve_path(base, Path::new(value))) {
                if trusted.contains(&toolfs::resolve_path(Path::new(""), &file.path)) {
                    found.push(file);
                } else {
                    out.project_changed = true;
                }
            }
        }
        if found.is_empty() {
            return out;
        }
        // Seed before taking the cache lock; cache clearing can also be requested by a channel edit.
        let prior = core.inspect(|state, _| {
            state
                .agents
                .get(agent)
                .map(|agent| agent.instructed.clone())
                .unwrap_or_default()
        });
        let mut given = self.given.lock().unwrap();
        let given = given.get_or_insert(prior);
        let mut files = Vec::new();
        for file in found {
            let path = file.path.to_string_lossy().into_owned();
            if given.insert(path.clone()) {
                out.carried.push(path);
                files.push(file);
            }
        }
        out.note = instructions::note(&files, instructions::BUDGET, home);
        out
    }
}
pub fn channel_control_stamp(core: &Core, paths: &Paths) -> ControlStamp {
    let (config, directories) = {
        let state = core.state.lock().unwrap();
        (
            state.config.clone(),
            scope::channel_directories(&state.channel, &state.config),
        )
    };
    stamp_control(&config, &directories, paths)
}
