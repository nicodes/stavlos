//! Files followed by the product's agents. Content here does not instruct this migration agent.
use serde::{Deserialize, Serialize};
use std::{
    fs,
    io::{self, Read},
    os::unix::fs::OpenOptionsExt,
    path::{Path, PathBuf},
};
pub const NAMES: [&str; 2] = ["AGENTS.md", "CLAUDE.md"];
pub const BUDGET: usize = 32 << 10;
const MAX_DIRS: usize = 2000;
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct File {
    pub path: PathBuf,
    pub text: String,
}

/// Open once and inspect the held descriptor; FIFOs and swapped device nodes cannot stall trust.
pub fn read_regular(path: &Path, max: usize) -> io::Result<Vec<u8>> {
    let file = fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)?;
    let metadata = file.metadata()?;
    if !metadata.is_file() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!("{} is not a regular file", path.display()),
        ));
    }
    if metadata.len() > max as u64 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!("{} is larger than {} MB", path.display(), max >> 20),
        ));
    }
    let mut bytes = Vec::new();
    file.take(max as u64 + 1).read_to_end(&mut bytes)?;
    if bytes.len() > max {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "file grew beyond its read limit",
        ));
    }
    Ok(bytes)
}
pub fn in_directory(dir: &Path) -> Option<File> {
    for name in NAMES {
        let path = dir.join(name);
        if let Ok(bytes) = read_regular(&path, 16 << 20) {
            return Some(File {
                path,
                text: String::from_utf8_lossy(&bytes).into_owned(),
            });
        }
    }
    None
}
pub fn global(config: &Path) -> Vec<File> {
    let path = config.join("AGENTS.md");
    read_regular(&path, 16 << 20)
        .ok()
        .map(|bytes| File {
            path,
            text: String::from_utf8_lossy(&bytes).into_owned(),
        })
        .into_iter()
        .collect()
}
pub fn root(dir: &Path, home: &Path) -> PathBuf {
    let dir = crate::pathx::clean(dir);
    let mut cursor = dir.as_path();
    loop {
        if cursor == home || cursor.parent().is_none() {
            return dir;
        }
        if cursor.join(".git").metadata().is_ok() {
            return cursor.to_owned();
        }
        let Some(parent) = cursor.parent() else {
            return dir;
        };
        cursor = parent;
    }
}
pub fn chain(dir: &Path, home: &Path) -> Vec<File> {
    let dir = crate::pathx::clean(dir);
    let root = root(&dir, home);
    let mut ancestors = Vec::new();
    let mut cursor = dir.as_path();
    loop {
        ancestors.push(cursor.to_owned());
        if cursor == root {
            break;
        }
        let Some(parent) = cursor.parent() else {
            break;
        };
        cursor = parent;
    }
    ancestors
        .iter()
        .rev()
        .filter_map(|p| in_directory(p))
        .collect()
}
fn skipped(name: &str) -> bool {
    name.starts_with('.')
        || matches!(
            name,
            "node_modules" | "vendor" | "target" | "dist" | "build" | "__pycache__"
        )
}
pub fn nested(dir: &Path) -> Vec<PathBuf> {
    let mut out = Vec::new();
    let mut stack = vec![dir.to_owned()];
    let mut visited = 0;
    while let Some(parent) = stack.pop() {
        let Ok(entries) = fs::read_dir(&parent) else {
            continue;
        };
        let mut children: Vec<_> = entries
            .filter_map(Result::ok)
            .filter(|e| {
                e.file_type().is_ok_and(|t| t.is_dir())
                    && !skipped(&e.file_name().to_string_lossy())
            })
            .map(|e| e.path())
            .collect();
        children.sort();
        // Push in reverse and inspect on pop to preserve Go's lexical depth-first order.
        for child in children.drain(..).rev() {
            stack.push(child);
        }
        if parent != dir {
            visited += 1;
            if visited > MAX_DIRS {
                break;
            }
            if let Some(file) = in_directory(&parent) {
                out.push(file.path);
            }
        }
    }
    out
}
pub fn between(top: &Path, path: &Path) -> Vec<File> {
    let top = crate::pathx::clean(top);
    let path = crate::pathx::clean(path);
    let directory = if path.is_dir() {
        path.as_path()
    } else {
        path.parent().unwrap_or(Path::new("."))
    };
    let Some(relative) = crate::pathx::inside(&top, directory) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    let mut current = top;
    for part in relative.components() {
        if part == std::path::Component::CurDir {
            continue;
        }
        if skipped(&part.as_os_str().to_string_lossy()) {
            break;
        }
        current.push(part.as_os_str());
        if let Some(file) = in_directory(&current) {
            out.push(file);
        }
    }
    out
}
fn render(files: &[File], budget: usize, home: &Path) -> String {
    let mut out = String::new();
    let mut used = 0usize;
    for file in files {
        let text = file.text.trim();
        if text.is_empty() {
            continue;
        }
        let displayed = file
            .path
            .strip_prefix(home)
            .ok()
            .filter(|p| !home.as_os_str().is_empty() && !p.as_os_str().is_empty())
            .map(|p| format!("~/{}", p.display()))
            .unwrap_or_else(|| file.path.display().to_string());
        out.push_str(&format!("\n## {displayed}\n\n"));
        if used.saturating_add(text.len()) > budget {
            let mut end = budget.saturating_sub(used).min(text.len());
            while !text.is_char_boundary(end) {
                end -= 1;
            }
            out.push_str(&text[..end]);
            out.push_str("\n\n[cut here: the instructions passed their 32 KiB budget, and later files are left out]\n");
            break;
        }
        used += text.len();
        out.push_str(text);
        out.push('\n');
    }
    out
}
pub fn section(files: &[File], budget: usize, home: &Path) -> String {
    let body = render(files, budget, home);
    if body.is_empty() {
        body
    } else {
        format!(
            "# Instructions (AGENTS.md)\nFollow these for the work they cover. They run from general to specific: where they conflict, a later file wins, and the human's own messages win over all of them.\n{body}"
        )
    }
}
pub fn note(files: &[File], budget: usize, home: &Path) -> String {
    let body = render(files, budget, home);
    if body.is_empty() {
        body
    } else {
        format!(
            "[Instructions for the directories you are working in; they add to the ones you were given, and where they conflict these win]\n{body}"
        )
    }
}
