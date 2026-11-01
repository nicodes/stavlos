//! Shared permission verdicts and role-specific process/file boundaries.
use super::{scope, state::Permits};
use crate::{
    config,
    paths::Paths,
    pathx,
    policy::{Kind, Subject, Verb},
    sandbox, shellcmd, toolfs,
};
use std::{
    collections::BTreeSet,
    fs,
    path::{Path, PathBuf},
    sync::OnceLock,
};

pub struct Facts<'a> {
    pub bare: bool,
    pub hidden: bool,
    pub ruled: Verb,
    pub compound: bool,
    pub control: bool,
    pub permitted: bool,
    pub mode: &'a str,
    pub egress: bool,
    pub outside: bool,
}
pub fn mode_verdict(mode: &str, sticky: bool, egress: bool, outside: bool) -> Verb {
    match mode {
        "yolo" if !sticky => Verb::Allow,
        "auto" if outside => Verb::Deny,
        "auto" if !sticky && !egress => Verb::Allow,
        _ => Verb::Ask,
    }
}
pub fn judge(facts: Facts<'_>) -> Verb {
    if facts.hidden {
        return Verb::Deny;
    }
    let mut verdict = facts.ruled;
    if verdict == Verb::Allow && (facts.compound || facts.control || facts.bare) {
        verdict = Verb::Ask;
    }
    if verdict == Verb::Ask && facts.bare {
        return if facts.outside && facts.mode == "auto" {
            Verb::Deny
        } else {
            Verb::Ask
        };
    }
    if verdict == Verb::Ask && !facts.control && facts.permitted {
        verdict = Verb::Allow;
    }
    if verdict != Verb::Deny && (verdict == Verb::Ask || facts.outside) {
        verdict = mode_verdict(facts.mode, facts.control, facts.egress, facts.outside);
    }
    verdict
}
pub fn url_host(raw: &str) -> String {
    let raw = raw.trim();
    if raw.is_empty() {
        return String::new();
    }
    let raw = if raw.contains("://") {
        raw.to_owned()
    } else {
        format!("https://{raw}")
    };
    if reqwest::Url::parse(&raw).is_err() {
        return String::new();
    }
    let authority = raw.split_once("://").map_or("", |(_, rest)| {
        rest.split(['/', '?', '#']).next().unwrap_or("")
    });
    if authority.contains('\\') {
        return String::new();
    }
    let host = authority
        .rsplit_once('@')
        .map_or(authority, |(_, host)| host);
    let host = if let Some(ipv6) = host.strip_prefix('[') {
        let Some((host, _)) = ipv6.split_once(']') else {
            return String::new();
        };
        host
    } else {
        host.split_once(':').map_or(host, |(host, _)| host)
    };
    // Permissions use Go's original hostname spelling, without URL transport IDNA rewriting.
    host.chars()
        .map(|ch| ch.to_lowercase().next().unwrap_or(ch))
        .collect()
}
pub fn prefix_for(kind: Kind, argument: &str) -> String {
    match kind {
        Kind::Command => shellcmd::prefix(argument),
        Kind::Url => url_host(argument),
        _ => String::new(),
    }
}
pub fn prefix_covers(kind: Kind, prefix: &str, argument: &str) -> bool {
    match kind {
        Kind::Command => shellcmd::covers(prefix, argument),
        Kind::Url => !prefix.is_empty() && url_host(argument) == prefix,
        _ => false,
    }
}
impl Permits {
    pub fn covers(&self, tool: &str, subject: &Subject) -> bool {
        !subject.values.is_empty()
            && subject.values.iter().all(|value| {
                self.calls.contains(&format!("{tool}\0{value}"))
                    || self.prefixes.get(tool).is_some_and(|prefixes| {
                        prefixes
                            .iter()
                            .any(|prefix| prefix_covers(subject.kind, prefix, value))
                    })
            })
    }
}
pub fn hosts_allow(hosts: &[String], urls: &[String]) -> bool {
    !hosts.is_empty()
        && !urls.is_empty()
        && urls.iter().all(|url| {
            let host = url_host(url);
            !host.is_empty()
                && hosts.iter().any(|allowed| {
                    allowed == "*"
                        || allowed.strip_prefix("*.").map_or_else(
                            || host == *allowed,
                            |domain| host.ends_with(&format!(".{domain}")),
                        )
                })
        })
}
pub fn egress(tool: &str, subject: &Subject) -> bool {
    subject.kind == Kind::Url
        || tool == crate::toolname::SEARCH
        || tool.starts_with(crate::toolname::MCP_PREFIX)
}
pub fn control_file(
    tool: &str,
    subject: &Subject,
    base: &Path,
    directories: &[PathBuf],
) -> Option<String> {
    if tool != "patch" {
        return None;
    }
    for value in &subject.values {
        let path = toolfs::resolve_path(base, Path::new(value));
        for directory in directories {
            let root = toolfs::resolve_path(Path::new(""), directory);
            let Some(relative) = pathx::inside(&root, &path) else {
                continue;
            };
            let control = [".stavlos", ".git", ".envrc"]
                .iter()
                .any(|control| relative == Path::new(control) || relative.starts_with(control));
            if control
                || relative
                    .file_name()
                    .is_some_and(|name| name == "AGENTS.md" || name == "CLAUDE.md")
            {
                return Some(value.clone());
            }
        }
    }
    None
}
pub fn hidden_paths(
    config: &config::Effective,
    directories: &[PathBuf],
    paths: &Paths,
    home: &Path,
    runtime: Option<&Path>,
    uid: u32,
) -> Vec<PathBuf> {
    let mut hidden = vec![
        paths.config.clone(),
        paths.data.clone(),
        paths.cache.clone(),
        paths.socket.clone(),
        PathBuf::from(format!("/run/user/{uid}")),
    ];
    hidden.extend(runtime.map(Path::to_owned));
    hidden.extend(config.sandbox.hide.iter().cloned());
    let mut out: BTreeSet<PathBuf> = hidden
        .into_iter()
        .filter(|path| !any_within(directories, path))
        .collect();
    static HOME_HIDDEN: OnceLock<Vec<String>> = OnceLock::new();
    static WORKSPACE_HIDDEN: OnceLock<Vec<String>> = OnceLock::new();
    let home_hidden = HOME_HIDDEN.get_or_init(|| {
        serde_json::from_str(include_str!("sandbox_hidden_home.json"))
            .expect("static credential names")
    });
    let workspace_hidden = WORKSPACE_HIDDEN.get_or_init(|| {
        serde_json::from_str(include_str!("sandbox_workspace_credentials.json"))
            .expect("static credential names")
    });
    for name in home_hidden {
        out.insert(home.join(name));
    }
    for directory in directories {
        let directory = pathx::clean(directory);
        if !directory.is_absolute() {
            continue;
        }
        for parent in directory.ancestors() {
            for name in workspace_hidden {
                out.insert(parent.join(name));
            }
        }
    }
    out.into_iter().collect()
}
pub fn hidden_from(
    subject: &Subject,
    base: &Path,
    hidden: &[PathBuf],
    own: &[PathBuf],
) -> Option<PathBuf> {
    if subject.kind != Kind::Path {
        return None;
    }
    for value in &subject.values {
        let path = toolfs::resolve_path(base, Path::new(value));
        if scope::contains(own, &path) {
            continue;
        }
        if let Some(hidden) = hidden
            .iter()
            .find(|hidden| scope::contains(&[(*hidden).clone()], &path))
        {
            return Some(hidden.clone());
        }
    }
    None
}
fn any_within(paths: &[PathBuf], directory: &Path) -> bool {
    paths
        .iter()
        .any(|path| pathx::inside(directory, path).is_some())
}

pub fn role_sandbox_spec(
    config: &config::Effective,
    directories: &[PathBuf],
    paths: &Paths,
    home: &Path,
    runtime: Option<&Path>,
    channel: &str,
    readonly: bool,
) -> Option<sandbox::Spec> {
    if !config.sandbox.enabled {
        return None;
    }
    let mut spec = sandbox::Spec {
        network: config.sandbox.network,
        readable: config.sandbox.readable.clone(),
        ..Default::default()
    };
    for store in [
        ".local/share/mise/installs",
        ".rustup/toolchains",
        ".cargo/bin",
    ] {
        spec.readable.push(home.join(store));
    }
    for directory in directories {
        let directory = toolfs::resolve_path(Path::new(""), directory);
        spec.writable.push(directory.clone());
        let linked =
            fs::symlink_metadata(directory.join(".git")).is_ok_and(|metadata| metadata.is_file());
        if linked {
            spec.read_only.push(directory.join(".git"));
        }
        for control in [
            ".git/hooks",
            ".git/config",
            ".stavlos",
            ".agents",
            ".envrc",
            "AGENTS.md",
            "CLAUDE.md",
        ] {
            if linked && matches!(control, ".git/hooks" | ".git/config") {
                continue;
            }
            spec.read_only.push(directory.join(control));
        }
    }
    grant_git_metadata(&mut spec, directories, &config.sandbox.git_metadata);
    spec.read_only
        .extend(config.instruction_files.iter().cloned());
    spec.read_only.push(paths.shared_skills.clone());
    spec.readable.push(paths.shared_skills.clone());
    let resolved = spec.writable.clone();
    spec.caches = true;
    spec.writable
        .extend(config.sandbox.writable.iter().cloned());
    let scratch = scope::scratch_directory(paths, channel);
    use std::os::unix::fs::DirBuilderExt;
    if fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&scratch)
        .is_ok()
    {
        spec.tmp = scratch;
        spec.private_tmp = !any_within(&resolved, &std::env::temp_dir());
    }
    spec.hidden = hidden_paths(config, &resolved, paths, home, runtime, unsafe {
        libc::getuid()
    });
    if readonly {
        spec.network = false;
        spec.writable.retain(|path| {
            !directories.iter().any(|directory| {
                pathx::inside(path, directory).is_some() || pathx::inside(directory, path).is_some()
            }) && !config
                .sandbox
                .git_metadata
                .iter()
                .any(|metadata| pathx::inside(metadata, path).is_some())
        });
        for directory in directories {
            let directory = toolfs::resolve_path(Path::new(""), directory);
            spec.read_only.push(directory.clone());
            spec.readable.push(directory);
        }
    }
    Some(spec)
}
fn small_git_file(path: &Path) -> Option<String> {
    use std::{io::Read, os::unix::fs::OpenOptionsExt};
    let metadata = fs::symlink_metadata(path).ok()?;
    if !metadata.is_file() || metadata.len() > 4096 {
        return None;
    }
    // Recheck the held descriptor, refusing a symlink/FIFO substitution after lstat.
    let file = fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)
        .ok()?;
    let metadata = file.metadata().ok()?;
    if !metadata.is_file() || metadata.len() > 4096 {
        return None;
    }
    let mut bytes = Vec::new();
    file.take(4097).read_to_end(&mut bytes).ok()?;
    if bytes.len() > 4096 {
        return None;
    }
    String::from_utf8(bytes).ok()
}
fn exact_real(path: &Path) -> bool {
    fs::canonicalize(path).is_ok_and(|real| real == path)
}
pub fn grant_git_metadata(spec: &mut sandbox::Spec, scope: &[PathBuf], grants: &[PathBuf]) {
    for work in scope {
        let link = work.join(".git");
        let Some(body) = small_git_file(&link) else {
            continue;
        };
        let Some(own) = body.strip_prefix("gitdir: ") else {
            continue;
        };
        let own = pathx::clean(&work.join(own.trim()));
        let Some(common) = small_git_file(&own.join("commondir")) else {
            continue;
        };
        let common = pathx::clean(&own.join(common.trim()));
        if !grants.iter().any(|grant| {
            grant.is_absolute()
                && pathx::clean(grant) == *grant
                && exact_real(grant)
                && *grant == common
        }) || own.parent() != Some(common.join("worktrees").as_path())
            || !exact_real(&own)
        {
            continue;
        }
        if small_git_file(&own.join("gitdir"))
            .is_none_or(|back| pathx::clean(Path::new(back.trim())) != link)
            || small_git_file(&own.join("config.worktree")).is_none()
            || small_git_file(&common.join("config")).is_none()
            || !exact_real(&common.join("hooks"))
        {
            continue;
        }
        spec.readable.push(common.clone());
        for path in [
            own.clone(),
            common.join("objects"),
            common.join("refs"),
            common.join("logs"),
        ] {
            if exact_real(&path) {
                spec.writable.push(path);
            }
        }
        spec.read_only.extend([
            link,
            common.join("config"),
            common.join("hooks"),
            own.join("config.worktree"),
            own.join("commondir"),
            own.join("gitdir"),
        ]);
    }
}

#[derive(Default)]
pub struct RepeatCall {
    key: String,
    count: usize,
}
impl RepeatCall {
    pub fn repeated(&mut self, name: &str, input: &str, mode: &str) -> bool {
        let key = format!("{name}\0{input}");
        if key == self.key {
            self.count += 1;
        } else {
            self.key = key;
            self.count = 1;
        }
        if self.count < 3 || mode == "yolo" {
            return false;
        }
        self.count = 0;
        true
    }
    pub fn reset(&mut self) {
        *self = Self::default();
    }
}
