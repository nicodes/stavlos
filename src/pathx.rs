use std::path::{Component, Path, PathBuf};

/// Lexical cleaning, without following an untrusted symlink or changing its authority.
pub fn clean(path: &Path) -> PathBuf {
    let mut out = PathBuf::new();
    for component in path.components() {
        match component {
            Component::CurDir => {}
            Component::ParentDir => {
                if out.file_name().is_some_and(|p| p != "..") {
                    out.pop();
                } else if !out.has_root() {
                    out.push("..");
                }
            }
            other => out.push(other.as_os_str()),
        }
    }
    if out.as_os_str().is_empty() {
        out.push(".");
    }
    out
}
pub fn absolute(path: &Path) -> std::io::Result<PathBuf> {
    if path.is_absolute() {
        Ok(clean(path))
    } else {
        Ok(clean(&std::env::current_dir()?.join(path)))
    }
}
/// Relative lexical path, also for repository instruction files above a channel directory.
pub fn relative(base: &Path, path: &Path) -> Option<PathBuf> {
    let base = clean(base);
    let path = clean(path);
    if base.is_absolute() != path.is_absolute() {
        return None;
    }
    let from: Vec<_> = base
        .components()
        .filter(|c| *c != Component::CurDir)
        .collect();
    let to: Vec<_> = path
        .components()
        .filter(|c| *c != Component::CurDir)
        .collect();
    let shared = from.iter().zip(&to).take_while(|(a, b)| a == b).count();
    let mut out = PathBuf::new();
    for _ in &from[shared..] {
        out.push("..");
    }
    for component in &to[shared..] {
        out.push(component.as_os_str());
    }
    if out.as_os_str().is_empty() {
        out.push(".");
    }
    Some(out)
}
pub fn inside(base: &Path, path: &Path) -> Option<PathBuf> {
    relative(base, path).filter(|p| !p.components().any(|c| c == Component::ParentDir))
}
