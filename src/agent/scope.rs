//! Task grants and the channel working set. Revoked grants never inherit a wider scope.
use super::state::ChannelState;
use crate::{config, pathx, protocol, toolfs};
use anyhow::{Result, ensure};
use std::{
    collections::BTreeSet,
    path::{Path, PathBuf},
};

pub fn resolve_directory(base: &Path, value: &str) -> PathBuf {
    let mut expanded = config::effective::expand_env(value.trim(), &|key| std::env::var(key).ok());
    if let Some(rest) = expanded.strip_prefix('~') {
        let (user, tail) = rest.split_once('/').unwrap_or((rest, ""));
        if let Some(home) = user_home(user) {
            expanded = home.join(tail).to_string_lossy().into_owned();
        }
    }
    pathx::clean(&base.join(expanded))
}
fn user_home(user: &str) -> Option<PathBuf> {
    if user.is_empty() {
        return std::env::var_os("HOME").map(PathBuf::from);
    }
    let name = std::ffi::CString::new(user).ok()?;
    let mut entry = std::mem::MaybeUninit::<libc::passwd>::uninit();
    let mut buffer = vec![0u8; 64 * 1024];
    let mut found = std::ptr::null_mut();
    // The returned string is copied while the passwd buffer is still alive.
    unsafe {
        if libc::getpwnam_r(
            name.as_ptr(),
            entry.as_mut_ptr(),
            buffer.as_mut_ptr().cast(),
            buffer.len(),
            &mut found,
        ) != 0
            || found.is_null()
        {
            return None;
        }
        use std::os::unix::ffi::OsStrExt;
        let directory = std::ffi::CStr::from_ptr((*found).pw_dir);
        Some(PathBuf::from(std::ffi::OsStr::from_bytes(
            directory.to_bytes(),
        )))
    }
}
pub fn directory_infos(state: &ChannelState, config: &config::Effective) -> Vec<protocol::DirInfo> {
    let mut seen = BTreeSet::from([pathx::clean(Path::new(&state.dir))]);
    let mut out = vec![protocol::DirInfo {
        path: state.dir.clone(),
        source: "channel".into(),
    }];
    let configured = config.dirs.iter().map(|dir| {
        (
            resolve_directory(Path::new(&state.dir), &dir.to_string_lossy()),
            "config",
        )
    });
    let added = state
        .dirs
        .iter()
        .map(|dir| (PathBuf::from(&dir.path), dir.source.as_str()));
    for (path, source) in configured.chain(added) {
        if seen.insert(path.clone()) {
            out.push(protocol::DirInfo {
                path: path.to_string_lossy().into_owned(),
                source: source.into(),
            });
        }
    }
    out
}
pub fn channel_directories(state: &ChannelState, config: &config::Effective) -> Vec<PathBuf> {
    directory_infos(state, config)
        .into_iter()
        .map(|dir| dir.path.into())
        .collect()
}
pub fn contains(dirs: &[PathBuf], path: &Path) -> bool {
    let path = toolfs::resolve_path(Path::new(""), path);
    dirs.iter()
        .any(|dir| pathx::inside(&toolfs::resolve_path(Path::new(""), dir), &path).is_some())
}
pub fn agent_directories(
    state: &ChannelState,
    config: &config::Effective,
    id: &str,
) -> Vec<PathBuf> {
    let channel = channel_directories(state, config);
    match state
        .agents
        .get(id)
        .and_then(|agent| agent.directories.0.as_ref())
    {
        None => channel,
        Some(grants) => grants
            .iter()
            .map(PathBuf::from)
            .filter(|dir| contains(&channel, dir))
            .collect(),
    }
}
pub fn child_scope(
    state: &ChannelState,
    config: &config::Effective,
    parent: &str,
    requested: &[String],
) -> Result<crate::wire::Slice<String>> {
    if parent.is_empty() {
        return Ok(Default::default());
    }
    let permitted = agent_directories(state, config, parent);
    ensure!(!permitted.is_empty(), "parent has no task directories");
    if requested.is_empty() {
        return Ok(vec![permitted[0].to_string_lossy().into_owned()].into());
    }
    let mut out = Vec::new();
    for directory in requested {
        let path = toolfs::resolve_path(&permitted[0], Path::new(directory));
        ensure!(
            contains(&permitted, &path),
            "child task directory is outside parent scope: {}",
            path.display()
        );
        out.push(path.to_string_lossy().into_owned());
    }
    Ok(out.into())
}
pub fn scratch_directory(paths: &crate::paths::Paths, channel: &str) -> PathBuf {
    paths
        .cache
        .parent()
        .unwrap_or(Path::new("."))
        .join("stavlos-tasks")
        .join(channel)
}

/// This is only a prompt heuristic; the process sandbox enforces actual writes.
pub fn bash_path_candidates(command: &str, base: &Path) -> Vec<PathBuf> {
    let separators = command
        .replace("&&", " && ")
        .replace("||", " || ")
        .replace(';', " ; ")
        .replace('|', " | ");
    let tokens: Vec<_> = separators.split_whitespace().collect();
    let mut out = Vec::new();
    let mut start = true;
    let mut index = 0;
    while index < tokens.len() {
        let token = tokens[index];
        index += 1;
        if matches!(token, "|" | "||" | "&&" | ";" | "&") {
            start = true;
            continue;
        }
        if matches!(token, ">" | ">>" | "<" | "2>" | "&>" | "2>>") {
            if let Some(next) = tokens.get(index) {
                add_candidate(&mut out, base, next);
                index += 1;
            }
            continue;
        }
        let redirect = token.trim_start_matches(['1', '2', '&']);
        if redirect.starts_with(['>', '<']) {
            add_candidate(&mut out, base, redirect.trim_start_matches(['>', '<']));
            continue;
        }
        if start {
            start = false;
            if token == "cd"
                && let Some(next) = tokens.get(index)
            {
                add_candidate(&mut out, base, next);
                index += 1;
            }
            continue;
        }
        let value = if token.starts_with('-') {
            token.split_once('=').map_or(token, |(_, value)| value)
        } else {
            token
        };
        let value = expand_shell_variables(value.trim_matches(['\'', '"']), base);
        if value.starts_with(['/', '~'])
            || value == ".."
            || value.starts_with("../")
            || value.contains("/../")
        {
            add_candidate(&mut out, base, &value);
        } else if !value.is_empty() && !value.starts_with('-') {
            let resolved = toolfs::resolve_path(base, Path::new(&value));
            if pathx::inside(&toolfs::resolve_path(base, Path::new("")), &resolved).is_none() {
                add_candidate(&mut out, base, &resolved.to_string_lossy());
            }
        }
    }
    out
}
fn add_candidate(out: &mut Vec<PathBuf>, base: &Path, value: &str) {
    let mut value = value.trim_matches(['\'', '"']);
    if let Some(wildcard) = value.find(['*', '?', '[']) {
        value = &value[..wildcard];
        if let Some(slash) = value.rfind('/') {
            value = &value[..slash];
        }
    }
    if value.is_empty() || value == "-" || value == "/dev" || value.starts_with("/dev/") {
        return;
    }
    out.push(resolve_directory(base, value));
}
fn expand_shell_variables(input: &str, base: &Path) -> String {
    let environment = crate::childenv::process(&[], []);
    let lookup = |key: &str| -> String {
        if key == "PWD" {
            return base.to_string_lossy().into_owned();
        }
        crate::childenv::pairs(&environment)
            .find(|(name, _)| *name == key)
            .map(|(_, value)| value.to_string_lossy().into_owned())
            .unwrap_or_default()
    };
    let mut out = String::new();
    let mut chars = input.chars().peekable();
    while let Some(ch) = chars.next() {
        if ch != '$' {
            out.push(ch);
            continue;
        }
        let mut name = String::new();
        if chars.peek() == Some(&'{') {
            chars.next();
            let mut closed = false;
            for ch in chars.by_ref() {
                if ch == '}' {
                    closed = true;
                    break;
                }
                name.push(ch);
            }
            if !closed {
                continue;
            }
        } else if chars
            .peek()
            .is_some_and(|ch| ch.is_ascii_digit() || "*#$@!?-".contains(*ch))
        {
            name.push(chars.next().unwrap());
        } else {
            while chars
                .peek()
                .is_some_and(|ch| ch.is_ascii_alphanumeric() || *ch == '_')
            {
                name.push(chars.next().unwrap());
            }
            if name.is_empty() {
                out.push('$');
                continue;
            }
        }
        out.push_str(&lookup(&name));
    }
    out
}
pub fn grant_directory(path: &Path) -> PathBuf {
    let base = if path.is_dir() {
        path
    } else {
        path.parent().unwrap_or(path)
    };
    let home = std::env::var_os("HOME")
        .map(PathBuf::from)
        .unwrap_or_default();
    if base != path && (base == home || base == Path::new("/")) {
        return path.to_owned();
    }
    for directory in base.ancestors() {
        if directory.join(".git").exists() {
            return if directory == home || directory == Path::new("/") {
                base.to_owned()
            } else {
                directory.to_owned()
            };
        }
    }
    base.to_owned()
}
pub fn outside_directory(
    subject: &crate::policy::Subject,
    base: &Path,
    directories: &[PathBuf],
) -> Option<PathBuf> {
    let paths = match subject.kind {
        crate::policy::Kind::Command => bash_path_candidates(subject.primary(), base),
        crate::policy::Kind::Path => subject
            .values
            .iter()
            .map(|value| toolfs::resolve_path(base, Path::new(value)))
            .collect(),
        _ => Vec::new(),
    };
    paths
        .into_iter()
        .find(|path| !contains(directories, path))
        .map(|path| grant_directory(&path))
}
