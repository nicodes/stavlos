//! Explicit child-process environment, preserving non-UTF-8 values without logging them.
use std::{
    collections::BTreeSet,
    ffi::{OsStr, OsString},
    os::unix::ffi::OsStrExt,
    sync::OnceLock,
};
const NAMES: &[&str] = &[
    "PATH",
    "HOME",
    "USER",
    "LOGNAME",
    "SHELL",
    "TERM",
    "COLORTERM",
    "TERM_PROGRAM",
    "LANG",
    "LANGUAGE",
    "TZ",
    "TMPDIR",
    "PWD",
    "HOSTNAME",
    "SHLVL",
    "EDITOR",
    "VISUAL",
    "PAGER",
    "LESS",
    "NO_COLOR",
    "FORCE_COLOR",
    "CI",
    "CC",
    "CXX",
    "CFLAGS",
    "CXXFLAGS",
    "CPPFLAGS",
    "LDFLAGS",
    "LD_LIBRARY_PATH",
    "MAKEFLAGS",
    "JAVA_HOME",
    "VIRTUAL_ENV",
    "GIT_AUTHOR_NAME",
    "GIT_AUTHOR_EMAIL",
    "GIT_COMMITTER_NAME",
    "GIT_COMMITTER_EMAIL",
];
const PREFIXES: &[&str] = &[
    "LC_",
    "XDG_CONFIG_",
    "XDG_DATA_",
    "XDG_CACHE_HOME",
    "XDG_STATE_HOME",
    "GO",
    "CARGO_",
    "RUSTUP_",
    "RUSTC",
    "NODE_",
    "NPM_CONFIG_",
    "PNPM_",
    "YARN_",
    "BUN_",
    "DENO_",
    "PYTHON",
    "PIP_",
    "UV_",
    "POETRY_",
    "CONDA_",
    "PYENV_",
    "MISE_",
    "ASDF_",
    "NVM_",
    "VOLTA_",
    "GRADLE_",
    "MAVEN_",
    "ANDROID_",
    "DOTNET_",
    "PKG_CONFIG",
];
pub fn passes(name: &str) -> bool {
    static SECRET: OnceLock<regex::Regex> = OnceLock::new();
    let secret=SECRET.get_or_init(||regex::Regex::new(r"(?i)(API[_-]?KEY|APIKEY|SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL|PRIVATE[_-]?KEY|(^|_)AUTH($|_))").unwrap());
    !name.starts_with("STAVLOS_")
        && !secret.is_match(name)
        && (NAMES.contains(&name) || PREFIXES.iter().any(|prefix| name.starts_with(prefix)))
}
pub fn url_credential(value: &[u8]) -> bool {
    static URL: OnceLock<regex::bytes::Regex> = OnceLock::new();
    // Go's Perl whitespace class is ASCII, including form feed but excluding vertical tab.
    URL.get_or_init(||regex::bytes::Regex::new(r"(?-u)^[A-Za-z][A-Za-z0-9+.-]*://[^/@\x09\x0a\x0c\x0d\x20]+:[^/@\x09\x0a\x0c\x0d\x20]+@").unwrap()).is_match(value)
}
pub fn filter(
    environment: impl IntoIterator<Item = OsString>,
    pass: &[String],
    extra: impl IntoIterator<Item = OsString>,
) -> Vec<OsString> {
    let keep: BTreeSet<_> = pass.iter().map(String::as_bytes).collect();
    let mut result = Vec::new();
    for item in environment {
        let bytes = item.as_bytes();
        let Some(separator) = bytes.iter().position(|b| *b == b'=') else {
            continue;
        };
        let (name, value) = (&bytes[..separator], &bytes[separator + 1..]);
        if keep.contains(name) || passes(&String::from_utf8_lossy(name)) && !url_credential(value) {
            result.push(item);
        }
    }
    result.extend(extra);
    result
}
pub fn process(pass: &[String], extra: impl IntoIterator<Item = OsString>) -> Vec<OsString> {
    filter(
        std::env::vars_os().map(|(name, value)| {
            let mut pair = name;
            pair.push("=");
            pair.push(value);
            pair
        }),
        pass,
        extra,
    )
}
pub fn pairs(environment: &[OsString]) -> impl Iterator<Item = (&OsStr, &OsStr)> {
    environment.iter().filter_map(|item| {
        let bytes = item.as_bytes();
        let separator = bytes.iter().position(|b| *b == b'=')?;
        Some((
            OsStr::from_bytes(&bytes[..separator]),
            OsStr::from_bytes(&bytes[separator + 1..]),
        ))
    })
}
