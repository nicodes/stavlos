//! Staged Codex patches. Every section is validated before any replacement.
use crate::toolfs::Filesystem;
use anyhow::{Context, Result, bail, ensure};
use std::{collections::BTreeMap, io::Read, os::unix::fs::PermissionsExt, path::PathBuf};

#[derive(Clone, Debug, Default, serde::Serialize)]
pub struct Operation {
    pub kind: String,
    pub path: String,
    pub move_to: String,
    pub added: Vec<String>,
    pub hunks: Vec<Hunk>,
}
#[derive(Clone, Debug, Default, serde::Serialize)]
pub struct Hunk {
    pub anchor: String,
    pub lines: Vec<(char, String)>,
    pub eof: bool,
}
fn flush(operation: &mut Option<Operation>, hunk: &mut Option<Hunk>) {
    if let (Some(operation), Some(hunk)) = (operation.as_mut(), hunk.take())
        && !hunk.lines.is_empty()
    {
        operation.hunks.push(hunk);
    }
}
pub fn parse(text: &str) -> Result<Vec<Operation>> {
    let text = text.replace("\r\n", "\n");
    let lines: Vec<_> = text.split('\n').collect();
    let start = lines
        .iter()
        .position(|l| l.trim() == "*** Begin Patch")
        .context("missing \"*** Begin Patch\"")?;
    let end = lines
        .iter()
        .rposition(|l| l.trim() == "*** End Patch")
        .filter(|end| *end >= start)
        .context("missing \"*** End Patch\"")?;
    let (mut operations, mut current, mut hunk) = (Vec::new(), None::<Operation>, None::<Hunk>);
    for line in &lines[start + 1..end] {
        let header = [
            ("*** Add File: ", "add"),
            ("*** Delete File: ", "delete"),
            ("*** Update File: ", "update"),
        ]
        .into_iter()
        .find_map(|(prefix, kind)| line.strip_prefix(prefix).map(|path| (kind, path)));
        if let Some((kind, path)) = header {
            flush(&mut current, &mut hunk);
            if let Some(previous) = current.take() {
                operations.push(previous);
            }
            current = Some(Operation {
                kind: kind.into(),
                path: path.trim().into(),
                ..Default::default()
            });
        } else if let Some(path) = line.strip_prefix("*** Move to: ") {
            let current = current
                .as_mut()
                .filter(|op| op.kind == "update")
                .context("\"*** Move to:\" must follow an Update File header")?;
            current.move_to = path.trim().into();
        } else if line.trim() == "*** End of File" {
            if let Some(hunk) = hunk.as_mut() {
                hunk.eof = true;
            }
        } else if let Some(anchor) = line.strip_prefix("@@") {
            ensure!(
                current.as_ref().is_some_and(|op| op.kind == "update"),
                "hunk outside an Update File section"
            );
            flush(&mut current, &mut hunk);
            hunk = Some(Hunk {
                anchor: anchor.trim().into(),
                ..Default::default()
            });
        } else if let Some(current) = current.as_mut() {
            match current.kind.as_str() {
                "add" => {
                    if let Some(added) = line.strip_prefix('+') {
                        current.added.push(added.into());
                    } else {
                        ensure!(
                            line.trim().is_empty(),
                            "add file {}: line must start with '+': {}",
                            current.path,
                            quote(line)
                        );
                    }
                }
                "delete" => ensure!(
                    line.trim().is_empty(),
                    "delete file {}: unexpected content",
                    current.path
                ),
                _ => {
                    let hunk = hunk.get_or_insert_with(Hunk::default);
                    let (op, text) = match line.as_bytes().first() {
                        Some(b' ' | b'-' | b'+') => (line.as_bytes()[0] as char, &line[1..]),
                        _ => (' ', *line),
                    };
                    hunk.lines.push((op, text.into()));
                }
            }
        } else {
            ensure!(
                line.trim().is_empty(),
                "unexpected line before any file header: {}",
                quote(line)
            );
        }
    }
    flush(&mut current, &mut hunk);
    if let Some(current) = current {
        operations.push(current);
    }
    for op in &operations {
        ensure!(!op.path.is_empty(), "a {} section names no file", op.kind);
        ensure!(
            op.kind != "update" || !op.hunks.is_empty() || !op.move_to.is_empty(),
            "update file {} has no hunks",
            op.path
        );
    }
    Ok(operations)
}
fn quote(text: &str) -> String {
    serde_json::to_string(text).expect("string encoding")
}
fn trimmed(bytes: &[u8], pass: usize) -> &[u8] {
    match pass {
        0 => bytes,
        1 => bytes.trim_ascii_end_matches(),
        _ => {
            // Invalid UTF-8 is content, never silently replaced by a patch.
            if let Ok(text) = std::str::from_utf8(bytes) {
                text.trim().as_bytes()
            } else {
                bytes.trim_ascii()
            }
        }
    }
}
trait TrimSpaceTab {
    fn trim_ascii_end_matches(&self) -> &[u8];
}
impl TrimSpaceTab for [u8] {
    fn trim_ascii_end_matches(&self) -> &[u8] {
        let end = self
            .iter()
            .rposition(|b| !matches!(b, b' ' | b'\t'))
            .map_or(0, |i| i + 1);
        &self[..end]
    }
}
fn find_run(lines: &[Vec<u8>], old: &[Vec<u8>], from: usize) -> Option<usize> {
    (0..3).find_map(|pass| {
        (from..=lines.len().saturating_sub(old.len())).find(|at| {
            *at + old.len() <= lines.len()
                && old
                    .iter()
                    .enumerate()
                    .all(|(j, value)| trimmed(&lines[*at + j], pass) == trimmed(value, pass))
        })
    })
}
impl Hunk {
    fn old_new(&self) -> (Vec<Vec<u8>>, Vec<Vec<u8>>) {
        let (mut old, mut new) = (Vec::new(), Vec::new());
        for (op, text) in &self.lines {
            if *op != '+' {
                old.push(text.as_bytes().to_vec());
            }
            if *op != '-' {
                new.push(text.as_bytes().to_vec());
            }
        }
        while old.last().is_some_and(Vec::is_empty) && new.last().is_some_and(Vec::is_empty) {
            old.pop();
            new.pop();
        }
        (old, new)
    }
    fn locate(&self, lines: &[Vec<u8>], old: &[Vec<u8>], mut from: usize) -> Result<usize> {
        let anchor = if self.anchor.is_empty() {
            None
        } else {
            (0..3).find_map(|pass| {
                (from..lines.len())
                    .find(|i| trimmed(&lines[*i], pass) == trimmed(self.anchor.as_bytes(), pass))
            })
        };
        if old.is_empty() {
            return if self.anchor.is_empty() {
                Ok(lines.len())
            } else {
                anchor
                    .map(|i| i + 1)
                    .with_context(|| format!("anchor not found: {}", self.anchor))
            };
        }
        if let Some(anchor) = anchor {
            from = anchor;
        }
        let mut at = find_run(lines, old, from)
            .or_else(|| {
                if from > 0 {
                    find_run(lines, old, 0)
                } else {
                    None
                }
            })
            .with_context(|| {
                format!(
                    "context not found:\n{}",
                    String::from_utf8_lossy(&old.join(&b'\n'))
                )
            })?;
        if self.eof
            && at + old.len() != lines.len()
            && old.len() <= lines.len()
            && let Some(alternative) = find_run(lines, old, lines.len() - old.len())
        {
            at = alternative;
        }
        Ok(at)
    }
}
pub fn apply_hunks(content: &[u8], hunks: &[Hunk]) -> Result<Vec<u8>> {
    let crlf = content.windows(2).any(|w| w == b"\r\n");
    let normalized = if crlf {
        replace_crlf(content)
    } else {
        content.to_vec()
    };
    let trailing = normalized.ends_with(b"\n");
    let body = normalized.strip_suffix(b"\n").unwrap_or(&normalized);
    let mut lines: Vec<Vec<u8>> = if normalized.is_empty() {
        vec![]
    } else {
        body.split(|b| *b == b'\n').map(Vec::from).collect()
    };
    let mut from = 0;
    for (index, hunk) in hunks.iter().enumerate() {
        let (old, mut new) = hunk.old_new();
        let at = hunk
            .locate(&lines, &old, from)
            .with_context(|| format!("hunk {}", index + 1))?;
        if !old.is_empty() {
            new.clear();
            let mut oi = 0;
            for (op, text) in &hunk.lines {
                match op {
                    ' ' => {
                        if oi < old.len() {
                            new.push(lines[at + oi].clone());
                        }
                        oi += 1;
                    }
                    '-' => oi += 1,
                    _ => new.push(text.as_bytes().to_vec()),
                }
            }
        }
        from = at + new.len();
        lines.splice(at..at + old.len(), new);
    }
    let mut result = lines.join(&b'\n');
    if trailing || !result.is_empty() {
        result.push(b'\n');
    }
    if crlf {
        let mut restored = Vec::with_capacity(result.len());
        for byte in result {
            if byte == b'\n' {
                restored.push(b'\r');
            }
            restored.push(byte);
        }
        return Ok(restored);
    }
    Ok(result)
}
fn replace_crlf(bytes: &[u8]) -> Vec<u8> {
    bytes
        .iter()
        .enumerate()
        .filter_map(|(i, b)| {
            if *b == b'\r' && bytes.get(i + 1) == Some(&b'\n') {
                None
            } else {
                Some(*b)
            }
        })
        .collect()
}
#[derive(Debug)]
struct Staged {
    path: PathBuf,
    exists: bool,
    content: Vec<u8>,
    mode: u32,
    previous: Option<Vec<u8>>,
}
pub struct Plan<'a> {
    filesystem: &'a Filesystem,
    files: Vec<Staged>,
    indices: BTreeMap<PathBuf, usize>,
}
impl<'a> Plan<'a> {
    fn file(&mut self, shown: &str) -> Result<usize> {
        let path = self.filesystem.resolve(shown);
        if let Some(index) = self.indices.get(&path) {
            return Ok(*index);
        }
        let (previous, mode) = match self.filesystem.open_read(&path) {
            Ok(mut file) => {
                let mut bytes = Vec::new();
                file.read_to_end(&mut bytes)?;
                (Some(bytes), file.metadata()?.permissions().mode() & 0o777)
            }
            Err(error)
                if error
                    .downcast_ref::<std::io::Error>()
                    .is_some_and(|e| e.kind() == std::io::ErrorKind::NotFound) =>
            {
                (None, 0o644)
            }
            Err(error) => return Err(error).with_context(|| shown.to_owned()),
        };
        let index = self.files.len();
        self.indices.insert(path.clone(), index);
        self.files.push(Staged {
            path,
            exists: previous.is_some(),
            content: previous.clone().unwrap_or_default(),
            mode,
            previous,
        });
        Ok(index)
    }
    pub fn stage(filesystem: &'a Filesystem, operations: &[Operation]) -> Result<Self> {
        let mut plan = Self {
            filesystem,
            files: vec![],
            indices: BTreeMap::new(),
        };
        for op in operations {
            let index = plan.file(&op.path)?;
            match op.kind.as_str() {
                "add" => {
                    ensure!(
                        !plan.files[index].exists,
                        "{} already exists (delete it first in the same patch to replace it)",
                        op.path
                    );
                    plan.files[index].exists = true;
                    plan.files[index].content = (op.added.join("\n") + "\n").into_bytes();
                }
                "delete" => {
                    ensure!(plan.files[index].exists, "{}: no such file", op.path);
                    plan.files[index].exists = false;
                }
                "update" => {
                    ensure!(plan.files[index].exists, "{}: no such file", op.path);
                    let output = apply_hunks(&plan.files[index].content, &op.hunks)
                        .with_context(|| op.path.clone())?;
                    if op.move_to.is_empty() {
                        plan.files[index].content = output;
                    } else {
                        let to = plan.file(&op.move_to)?;
                        ensure!(
                            !plan.files[to].exists,
                            "cannot move {} to {}: it already exists",
                            op.path,
                            op.move_to
                        );
                        plan.files[index].exists = false;
                        plan.files[to].exists = true;
                        plan.files[to].content = output;
                        plan.files[to].mode = plan.files[index].mode;
                    }
                }
                _ => bail!("unknown patch operation {}", op.kind),
            }
        }
        Ok(plan)
    }
    pub fn write(self) -> Result<()> {
        let mut done: Vec<&Staged> = vec![];
        for file in &self.files {
            if !file.exists || file.previous.as_ref() == Some(&file.content) {
                continue;
            }
            if let Err(error) = self
                .filesystem
                .write_atomic(&file.path, &file.content, file.mode)
            {
                for previous in done.into_iter().rev() {
                    if let Some(content) = &previous.previous {
                        let _ =
                            self.filesystem
                                .write_atomic(&previous.path, content, previous.mode);
                    } else {
                        let _ = self.filesystem.remove(&previous.path);
                    }
                }
                return Err(error);
            }
            done.push(file);
        }
        for file in &self.files {
            if file.previous.is_some() && !file.exists {
                self.filesystem
                    .remove(&file.path)
                    .context("the other changes were applied")?;
            }
        }
        Ok(())
    }
}
pub fn summary(operations: &[Operation]) -> String {
    operations
        .iter()
        .map(|op| match op.kind.as_str() {
            "add" => format!("added {} ({} lines)", op.path, op.added.len()),
            "delete" => format!("deleted {}", op.path),
            _ if !op.move_to.is_empty() => format!("updated {} → {}", op.path, op.move_to),
            _ => format!("updated {} ({} hunk(s))", op.path, op.hunks.len()),
        })
        .collect::<Vec<_>>()
        .join("\n")
}
pub fn run(filesystem: &Filesystem, text: &str) -> Result<String> {
    let operations = parse(text)?;
    ensure!(!operations.is_empty(), "no file sections");
    Plan::stage(filesystem, &operations)?.write()?;
    Ok(summary(&operations))
}
