//! Linux containment for model commands and stdio MCP servers.
pub mod linux;
use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use std::{
    ffi::{CString, OsStr, OsString},
    fs, io,
    os::unix::{
        ffi::OsStrExt,
        process::{CommandExt, ExitStatusExt},
    },
    path::{Path, PathBuf},
    process::{Command, Stdio},
    sync::OnceLock,
};
pub const HELPER_ARG0: &str = "stavlos-sandbox";
pub const SPEC_ENV: &str = "STAVLOS_SANDBOX_SPEC";
pub const STAGE_ENV: &str = "STAVLOS_SANDBOX_STAGE";
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Spec {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub readable: Vec<PathBuf>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub writable: Vec<PathBuf>,
    #[serde(rename = "readonly", skip_serializing_if = "Vec::is_empty")]
    pub read_only: Vec<PathBuf>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub hidden: Vec<PathBuf>,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub caches: bool,
    #[serde(skip_serializing_if = "empty_path")]
    pub tmp: PathBuf,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub private_tmp: bool,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub network: bool,
}
fn empty_path(path: &Path) -> bool {
    path.as_os_str().is_empty()
}
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Wire {
    #[serde(flatten)]
    pub spec: Spec,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub mounts: bool,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub probe: bool,
}
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Level {
    #[default]
    None,
    Landlock,
    Full,
}
impl std::fmt::Display for Level {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::None => "none",
            Self::Landlock => "landlock",
            Self::Full => "full",
        })
    }
}
#[derive(Clone, Debug, Default)]
pub struct Report {
    pub level: Level,
    pub missing: Vec<String>,
    pub why: String,
    pub fix: String,
}
pub fn probe_with(run: impl Fn(Wire) -> Result<()>) -> Report {
    if linux::abi() <= 0 {
        return Report {
            why: "the kernel has no Landlock".into(),
            ..Report::default()
        };
    }
    let full = run(Wire {
        mounts: true,
        probe: true,
        ..Wire::default()
    });
    if full.is_ok() {
        return Report {
            level: Level::Full,
            ..Report::default()
        };
    }
    let why = format!("full sandbox unavailable: {}", full.unwrap_err());
    match run(Wire {
        probe: true,
        ..Wire::default()
    }) {
        Ok(()) => Report {
            level: Level::Landlock,
            why,
            ..Report::default()
        },
        Err(error) => Report {
            why: format!("{why}; landlock alone: {error}"),
            ..Report::default()
        },
    }
}
pub fn probe() -> Report {
    static REPORT: OnceLock<Report> = OnceLock::new();
    REPORT
        .get_or_init(|| {
            probe_with(|wire| {
                let self_path = std::env::current_exe()?;
                let mut command = Command::new(self_path);
                command
                    .arg0(HELPER_ARG0)
                    .args(["--", "/bin/true"])
                    .env_remove(STAGE_ENV)
                    .env(SPEC_ENV, serde_json::to_string(&wire)?)
                    .stdin(Stdio::null());
                let output = command.output()?;
                if !output.status.success() {
                    bail!(
                        "{}: {}",
                        output.status,
                        String::from_utf8_lossy(&output.stderr)
                    );
                }
                Ok(())
            })
        })
        .clone()
}
pub fn explain(mut report: Report) -> Report {
    match report.level {
        Level::Full => {}
        Level::Landlock => {
            report.missing=vec!["Commands cannot hide credential and harness paths with mounts.".into(),"Commands cannot enforce read-only controls inside writable directories with mounts; changes must be reported.".into(),"Commands share the system's temporary and runtime directories, so every command asks first.".into(),"Network-off cannot isolate UDP, DNS and all other protocols without a private network namespace. Full confinement requires Landlock ABI 6 and user, mount and network namespaces.".into()];
            report.fix = user_namespace_fix();
        }
        Level::None => {
            report.missing=vec!["Nothing bounds a command: it runs with your full access. Every command asks first.".into()];
            if report.why.is_empty() {
                report.why =
                    "the kernel has no Landlock (Linux 5.13 or newer, with Landlock enabled)"
                        .into();
            }
        }
    }
    report
}
fn user_namespace_fix() -> String {
    for (path, blocked, fix) in [
        (
            "/proc/sys/kernel/apparmor_restrict_unprivileged_userns",
            "1",
            "sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0",
        ),
        (
            "/proc/sys/kernel/unprivileged_userns_clone",
            "0",
            "sudo sysctl -w kernel.unprivileged_userns_clone=1",
        ),
        (
            "/proc/sys/user/max_user_namespaces",
            "0",
            "sudo sysctl -w user.max_user_namespaces=15000",
        ),
    ] {
        if fs::read_to_string(path).is_ok_and(|value| value.trim() == blocked) {
            return fix.into();
        }
    }
    String::new()
}
pub fn resolve_program(program: &OsStr) -> Result<PathBuf> {
    if program.as_bytes().contains(&b'/') {
        return Ok(program.into());
    }
    for directory in std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()) {
        let path = directory.join(program);
        let Ok(metadata) = fs::metadata(&path) else {
            continue;
        };
        if !metadata.is_file() {
            continue;
        }
        let name = CString::new(path.as_os_str().as_bytes())?;
        if unsafe { libc::access(name.as_ptr(), libc::X_OK) } == 0 {
            if !path.is_absolute() {
                bail!("refusing executable selected from a relative PATH directory");
            }
            return Ok(path);
        }
    }
    bail!("program was not found in PATH")
}
/// Construct the command with the complete supplied environment, never inheriting a stage marker.
pub fn command(
    program: &OsStr,
    args: &[OsString],
    environment: &[OsString],
    spec: Option<&Spec>,
    level: Level,
    helper: &Path,
) -> Result<Command> {
    let program = resolve_program(program)?;
    let filtered: Vec<_> = environment
        .iter()
        .filter(|item| {
            !item
                .as_bytes()
                .starts_with(format!("{STAGE_ENV}=").as_bytes())
                && !item
                    .as_bytes()
                    .starts_with(format!("{SPEC_ENV}=").as_bytes())
        })
        .cloned()
        .collect();
    let mut command = if let Some(spec) = spec.filter(|_| level != Level::None) {
        let mut spec = spec.clone();
        spec.readable.push(program.clone());
        let wire = Wire {
            spec,
            mounts: level == Level::Full,
            probe: false,
        };
        let mut command = Command::new(helper);
        command
            .arg0(HELPER_ARG0)
            .arg("--")
            .arg(&program)
            .args(args)
            .env_clear()
            .envs(crate::childenv::pairs(&filtered))
            .env(SPEC_ENV, serde_json::to_string(&wire)?);
        command
    } else {
        let mut command = Command::new(program);
        command
            .args(args)
            .env_clear()
            .envs(crate::childenv::pairs(&filtered));
        command
    };
    command.process_group(0);
    Ok(command)
}
fn set_env(environment: &mut Vec<OsString>, name: &str, value: &OsStr) {
    let prefix = format!("{name}=");
    environment.retain(|item| !item.as_bytes().starts_with(prefix.as_bytes()));
    let mut item = OsString::from(prefix);
    item.push(value);
    environment.push(item);
}
fn cache_environment(wire: &Wire, environment: &mut Vec<OsString>) -> Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    let base = if wire.mounts && wire.spec.private_tmp {
        Path::new("/tmp")
    } else {
        wire.spec.tmp.as_path()
    };
    for (name, suffix) in [
        ("XDG_CACHE_HOME", "xdg"),
        ("GOCACHE", "go-build"),
        ("GOMODCACHE", "go-mod"),
        ("GOPATH", "go"),
        ("npm_config_cache", "npm"),
        ("CARGO_HOME", "cargo"),
        ("BUN_INSTALL_CACHE_DIR", "bun"),
        ("GRADLE_USER_HOME", "gradle"),
    ] {
        let target = base.join("caches").join(suffix);
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&target)?;
        set_env(environment, name, target.as_os_str());
    }
    Ok(())
}
extern "C" fn catch_signal(_: i32) {}
fn enter_namespaces(network: bool) -> Result<i32> {
    let self_path = std::env::current_exe()?;
    let args: Vec<_> = std::env::args_os().collect();
    let uid = unsafe { libc::getuid() };
    let gid = unsafe { libc::getgid() };
    let parent = unsafe { libc::getpid() };
    let uid_map = format!("{uid} {uid} 1\n").into_bytes();
    let gid_map = format!("{gid} {gid} 1\n").into_bytes();
    let flags = linux::namespace_flags(network);
    for signal in [libc::SIGINT, libc::SIGTERM, libc::SIGHUP, libc::SIGQUIT] {
        let mut action: libc::sigaction = unsafe { std::mem::zeroed() };
        action.sa_sigaction = catch_signal as *const () as usize;
        unsafe { libc::sigemptyset(&mut action.sa_mask) };
        if unsafe { libc::sigaction(signal, &action, std::ptr::null_mut()) } < 0 {
            return Err(io::Error::last_os_error().into());
        }
    }
    let mut child = Command::new(self_path);
    child
        .arg0(&args[0])
        .args(&args[1..])
        .env(STAGE_ENV, "2")
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    unsafe {
        child.pre_exec(move || linux::enter_namespace_child(flags, &uid_map, &gid_map, parent));
    }
    let status = child.status().context("user namespaces unavailable")?;
    Ok(status
        .code()
        .unwrap_or_else(|| 128 + status.signal().unwrap_or(0)))
}
/// Run only in the freshly exec'd helper; success execs the program and never returns.
pub fn helper(wire: Wire, args: &[OsString]) -> Result<i32> {
    let Some(program) = args.first() else {
        bail!("usage: stavlos-sandbox -- program [args]");
    };
    if wire.mounts && std::env::var_os(STAGE_ENV).is_none_or(|value| value.is_empty()) {
        if linux::abi() < 6 {
            bail!("full sandbox requires Landlock ABI 6 or newer");
        }
        return enter_namespaces(wire.spec.network);
    }
    let mut environment: Vec<_> = std::env::vars_os()
        .filter(|(key, _)| key != SPEC_ENV && key != STAGE_ENV)
        .map(|(key, value)| {
            let mut item = key;
            item.push("=");
            item.push(value);
            item
        })
        .collect();
    let mut writable = wire.spec.writable.clone();
    if wire.mounts {
        linux::mount_view(&wire.spec)?;
        linux::drop_capabilities()?;
    }
    if !wire.spec.tmp.as_os_str().is_empty() {
        let temporary = if wire.mounts && wire.spec.private_tmp {
            Path::new("/tmp")
        } else {
            wire.spec.tmp.as_path()
        };
        writable.push(temporary.to_owned());
        set_env(&mut environment, "TMPDIR", temporary.as_os_str());
    }
    if wire.spec.caches && !wire.spec.tmp.as_os_str().is_empty() {
        cache_environment(&wire, &mut environment).context("private build cache")?;
    }
    let mut readable = wire.spec.readable.clone();
    readable.extend(wire.spec.read_only.clone());
    linux::restrict(&writable, &readable, wire.spec.network)?;
    linux::restrict_unix_sockets()?;
    if wire.probe {
        return Ok(0);
    }
    let executable = CString::new(program.as_bytes())?;
    let arguments: Vec<_> = args
        .iter()
        .map(|arg| CString::new(arg.as_bytes()))
        .collect::<std::result::Result<_, _>>()?;
    let environment: Vec<_> = environment
        .iter()
        .map(|item| CString::new(item.as_bytes()))
        .collect::<std::result::Result<_, _>>()?;
    let mut argv: Vec<_> = arguments.iter().map(|s| s.as_ptr()).collect();
    argv.push(std::ptr::null());
    let mut envp: Vec<_> = environment.iter().map(|s| s.as_ptr()).collect();
    envp.push(std::ptr::null());
    unsafe { libc::execve(executable.as_ptr(), argv.as_ptr(), envp.as_ptr()) };
    Err(io::Error::last_os_error()).context("sandbox exec")
}
pub fn entry(args: &[OsString]) -> Result<i32> {
    let specification = std::env::var(SPEC_ENV).context("sandbox spec is missing")?;
    if specification.len() > 1 << 20 {
        bail!("sandbox spec is too large");
    }
    let wire: Wire = serde_json::from_str(&specification)?;
    if args.first().is_none_or(|arg| arg != "--") {
        bail!("usage: stavlos-sandbox -- program [args]");
    }
    helper(wire, &args[1..])
}
