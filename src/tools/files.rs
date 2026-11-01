use super::{Env, Output, decode};
use crate::{policy, toolpatch, wire_struct};
use anyhow::{Context, Result, bail, ensure};
use regex::bytes::Regex;
use serde_json::value::RawValue;
use std::{
    fs,
    io::{Read as _, Seek as _},
    path::{Path, PathBuf},
    process::Stdio,
};
use tokio::io::{AsyncBufReadExt as _, AsyncReadExt as _};
use tokio_util::sync::CancellationToken;

wire_struct!(ReadInput {path:"path":String=>required,offset:"offset":i64=>required,limit:"limit":i64=>required});
wire_struct!(PatchInput {patch:"patch":String=>required});
wire_struct!(SearchInput {pattern:"pattern":String=>required,path:"path":String=>required,glob:"glob":String=>required,ignore_case:"ignore_case":bool=>required,limit:"limit":i64=>required});
pub(super) fn subject(name: &str, input: Option<&RawValue>) -> policy::Subject {
    if name == "patch" {
        let paths = decode::<PatchInput>(input)
            .ok()
            .and_then(|a| toolpatch::parse(&a.patch).ok())
            .unwrap_or_default()
            .into_iter()
            .flat_map(|op| {
                if op.move_to.is_empty() {
                    vec![op.path]
                } else {
                    vec![op.path, op.move_to]
                }
            });
        return policy::Subject::paths(paths);
    }
    let a = decode::<ReadInput>(input).unwrap_or_default();
    policy::Subject::paths([
        if matches!(name, "grep" | "glob") && a.path.trim().is_empty() {
            ".".into()
        } else {
            a.path
        },
    ])
}
pub(super) async fn run(
    name: &str,
    input: Option<&RawValue>,
    env: &Env,
    cancel: CancellationToken,
) -> Result<Output> {
    match name {
        "read" => {
            let a = decode::<ReadInput>(input).context("bad input")?;
            let env = env.clone();
            tokio::task::spawn_blocking(move || read(a, &env, &cancel)).await?
        }
        "patch" => {
            let a = decode::<PatchInput>(input).context("bad input")?;
            let env = env.clone();
            tokio::task::spawn_blocking(move || {
                toolpatch::run(&env.filesystem, &a.patch)
                    .map(Output::ok)
                    .context("patch")
            })
            .await?
        }
        _ => {
            let a = decode::<SearchInput>(input).context("bad input")?;
            search(name, a, env, cancel).await
        }
    }
}
pub(super) fn binary(mut head: &[u8]) -> bool {
    if head.contains(&0) {
        return true;
    }
    while !head.is_empty() && std::str::from_utf8(head).is_err() {
        if head.len() < 4 || std::str::from_utf8(&head[..head.len() - 1]).is_ok() {
            head = &head[..head.len() - 1];
        } else {
            return true;
        }
    }
    false
}
fn content_type(head: &[u8]) -> &'static str {
    for (signature, kind) in [
        (&b"\x89PNG\r\n\x1a\n"[..], "image/png"),
        (&b"GIF87a"[..], "image/gif"),
        (&b"GIF89a"[..], "image/gif"),
        (&b"\xff\xd8\xff"[..], "image/jpeg"),
        (&b"%PDF-"[..], "application/pdf"),
        (&b"PK\x03\x04"[..], "application/zip"),
        (&b"\x1f\x8b\x08"[..], "application/x-gzip"),
        (&b"\0\0\x01\0"[..], "image/x-icon"),
        (&b"BM"[..], "image/bmp"),
        (&b"OggS\0"[..], "application/ogg"),
        (&b"fLaC\0\0\0\x22"[..], "audio/flac"),
        (&b"ID3"[..], "audio/mpeg"),
    ] {
        if head.starts_with(signature) {
            return kind;
        }
    }
    if head.starts_with(b"RIFF") && head.len() >= 12 {
        match &head[8..12] {
            b"WEBP" => return "image/webp",
            b"WAVE" => return "audio/wave",
            b"AVI " => return "video/avi",
            _ => {}
        }
    }
    if head
        .iter()
        .any(|b| matches!(b,0x00..=0x08|0x0b|0x0e..=0x1a|0x1c..=0x1f))
    {
        "application/octet-stream"
    } else {
        "text/plain; charset=utf-8"
    }
}
fn next_line(reader: &mut impl std::io::BufRead, maximum: usize) -> Result<Option<Vec<u8>>> {
    let mut line = Vec::new();
    loop {
        let available = reader.fill_buf()?;
        if available.is_empty() {
            if line.is_empty() {
                return Ok(None);
            }
            break;
        }
        let count = available
            .iter()
            .position(|b| *b == b'\n')
            .map_or(available.len(), |i| i + 1);
        ensure!(
            line.len() + count < maximum,
            "bufio.Scanner: token too long"
        );
        line.extend_from_slice(&available[..count]);
        let end = line.ends_with(b"\n");
        reader.consume(count);
        if end {
            line.pop();
            break;
        }
    }
    if line.ends_with(b"\r") {
        line.pop();
    }
    Ok(Some(line))
}
fn read(a: ReadInput, env: &Env, cancel: &CancellationToken) -> Result<Output> {
    let mut file = env.filesystem.open_read(&env.filesystem.resolve(&a.path))?;
    let mut head = Vec::new();
    ReadTake::read_head(&mut file, &mut head, 8192)?;
    if binary(&head) {
        bail!(
            "{} is not a text file ({}, {} bytes): read cannot show it",
            a.path,
            content_type(&head),
            file.metadata()?.len()
        );
    }
    file.rewind()?;
    let mut reader = std::io::BufReader::with_capacity(1 << 20, file);
    let offset = a.offset.max(1) as u64;
    let limit = clamp(a.limit, 2000, 5000) as u64;
    let budget = if env.max_output <= 0 {
        32 * 1024
    } else {
        env.max_output as usize
    };
    let (mut output, mut number) = (Vec::new(), 0);
    while let Some(line) = next_line(&mut reader, 16 << 20)? {
        ensure!(!cancel.is_cancelled(), "context canceled");
        number += 1;
        if number < offset {
            continue;
        }
        if number >= offset.saturating_add(limit) || output.len() > budget {
            output.extend_from_slice(
                format!("… (more lines; continue with offset={number})\n").as_bytes(),
            );
            break;
        }
        output.extend_from_slice(&line);
        output.push(b'\n');
    }
    Ok(Output::ok(if output.is_empty() {
        "(empty file)".into()
    } else {
        env.clip(&String::from_utf8_lossy(&output))
    }))
}
struct ReadTake;
impl ReadTake {
    fn read_head(file: &mut fs::File, head: &mut Vec<u8>, maximum: u64) -> Result<()> {
        file.take(maximum).read_to_end(head)?;
        Ok(())
    }
}
fn clamp(n: i64, default: usize, maximum: usize) -> usize {
    if n <= 0 {
        default
    } else {
        (n as usize).min(maximum)
    }
}
fn matcher(pattern: &str) -> impl Fn(&str) -> bool {
    let re = policy::glob(pattern.strip_prefix('/').unwrap_or(pattern), true);
    let basename = !pattern.contains('/');
    let all = pattern.is_empty();
    move |shown| {
        all || re.is_match(if basename {
            Path::new(shown)
                .file_name()
                .and_then(|s| s.to_str())
                .unwrap_or("")
        } else {
            shown
        })
    }
}
async fn search(
    name: &str,
    a: SearchInput,
    env: &Env,
    cancel: CancellationToken,
) -> Result<Output> {
    let grep = name == "grep";
    let regex = if grep {
        ensure!(
            !a.pattern.is_empty(),
            "pattern \"\" is not a regular expression: <nil>"
        );
        Some(
            Regex::new(&format!(
                "{}{}",
                if a.ignore_case { "(?i)" } else { "" },
                a.pattern
            ))
            .with_context(|| {
                format!(
                    "pattern {} is not a regular expression",
                    serde_json::to_string(&a.pattern).unwrap()
                )
            })?,
        )
    } else {
        ensure!(!a.pattern.trim().is_empty(), "a pattern is required");
        None
    };
    let limit = if grep {
        clamp(a.limit, 200, 2000)
    } else {
        clamp(a.limit, 500, 5000)
    };
    env.filesystem.check(&env.filesystem.resolve(&a.path))?;
    let executable = env
        .ripgrep
        .clone()
        .or_else(|| crate::sandbox::resolve_program(std::ffi::OsStr::new("rg")).ok())
        .filter(|p| !p.as_os_str().is_empty());
    let (mut lines, more) = if let Some(executable) = executable {
        run_rg(executable, grep, &a, env, limit, cancel).await?
    } else {
        let env = env.clone();
        let a = a.clone();
        tokio::task::spawn_blocking(move || walk(grep, &a, regex.as_ref(), &env, limit, &cancel))
            .await??
    };
    if !grep {
        lines.sort();
    }
    if lines.is_empty() {
        return Ok(Output::ok("no matches"));
    }
    let mut output = lines.join("\n");
    if more {
        output.push_str(&format!(
            "\n… (stopped at {limit} {}; narrow the pattern or the path, or raise limit)",
            if grep { "matching lines" } else { "paths" }
        ));
    }
    Ok(Output::ok(env.clip(&output)))
}
async fn run_rg(
    executable: PathBuf,
    grep: bool,
    a: &SearchInput,
    env: &Env,
    limit: usize,
    cancel: CancellationToken,
) -> Result<(Vec<String>, bool)> {
    let mut command = tokio::process::Command::new(executable);
    command.args(["--no-config", "--color", "never"]);
    if grep {
        command.args([
            "--line-number",
            "--no-heading",
            "--with-filename",
            "--max-columns",
            "500",
            "--max-columns-preview",
        ]);
        if a.ignore_case {
            command.arg("--ignore-case");
        }
        if !a.glob.is_empty() {
            command.args(["--glob", &a.glob]);
        }
        command.args(["--regexp", &a.pattern]);
    } else {
        command.args(["--files", "--glob", &a.pattern]);
    }
    // Pass the path policy judged, rather than resolving a model's path a second time.
    let judged = env.filesystem.judged.get(&a.path);
    if let Some(judged) = judged {
        command.arg("--").arg(judged);
    } else if !a.path.is_empty() {
        command.arg("--").arg(&a.path);
    }
    command.current_dir(&env.filesystem.directory).env_clear();
    for key in ["PATH", "HOME"] {
        command.env(key, std::env::var_os(key).unwrap_or_default());
    }
    let mut child = command
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()?;
    let mut output = tokio::io::BufReader::new(child.stdout.take().unwrap());
    let mut errors = child.stderr.take().unwrap();
    let error_task = tokio::spawn(async move {
        let mut head = Vec::new();
        let _ = (&mut errors).take(2048).read_to_end(&mut head).await;
        let _ = tokio::io::copy(&mut errors, &mut tokio::io::sink()).await;
        head
    });
    let mut lines = Vec::new();
    let mut more = false;
    let reading = async {
        loop {
            let mut line = Vec::new();
            loop {
                let available = output.fill_buf().await?;
                if available.is_empty() {
                    break;
                }
                let count = available
                    .iter()
                    .position(|b| *b == b'\n')
                    .map_or(available.len(), |i| i + 1);
                ensure!(
                    line.len() + count < 1 << 20,
                    "bufio.Scanner: token too long"
                );
                line.extend_from_slice(&available[..count]);
                let end = line.ends_with(b"\n");
                output.consume(count);
                if end {
                    break;
                }
            }
            if line.is_empty() {
                break;
            }
            if lines.len() == limit {
                more = true;
                break;
            }
            if line.ends_with(b"\n") {
                line.pop();
            }
            if line.ends_with(b"\r") {
                line.pop();
            }
            let mut shown = String::from_utf8_lossy(&line).into_owned();
            if let Some(judged) = judged
                && let Some(relative) = crate::pathx::inside(&env.filesystem.directory, judged)
            {
                let prefix = judged.to_string_lossy();
                if let Some(suffix) = shown.strip_prefix(prefix.as_ref()) {
                    shown = format!("{}{suffix}", relative.display());
                }
            }
            lines.push(shown.strip_prefix("./").unwrap_or(&shown).to_owned());
        }
        Ok::<_, anyhow::Error>(())
    };
    let read_result = tokio::select! {_=cancel.cancelled()=>Err(anyhow::anyhow!("context canceled")),result=reading=>result};
    if more || read_result.is_err() {
        let _ = child.kill().await;
    }
    let status = child.wait().await;
    let errors = error_task.await.unwrap_or_default();
    read_result?;
    let status = status?;
    if more || status.success() || status.code() == Some(1) && lines.is_empty() {
        return Ok((lines, more));
    }
    if !errors.is_empty() {
        bail!("{}", String::from_utf8_lossy(&errors).trim());
    }
    bail!("{status}")
}
fn walk(
    grep: bool,
    a: &SearchInput,
    regex: Option<&Regex>,
    env: &Env,
    limit: usize,
    cancel: &CancellationToken,
) -> Result<(Vec<String>, bool)> {
    let start = env.filesystem.resolve(&a.path);
    fs::metadata(&start)?;
    let matches = matcher(if grep { &a.glob } else { &a.pattern });
    let mut pending = vec![start.clone()];
    let mut lines = Vec::new();
    while let Some(path) = pending.pop() {
        ensure!(!cancel.is_cancelled(), "context canceled");
        let Ok(metadata) = fs::symlink_metadata(&path) else {
            continue;
        };
        if path != start
            && path
                .file_name()
                .is_some_and(|s| s.as_encoded_bytes().starts_with(b".") || s == "node_modules")
        {
            continue;
        }
        if metadata.is_dir() {
            let Ok(entries) = fs::read_dir(&path) else {
                continue;
            };
            let mut children: Vec<_> = entries.filter_map(Result::ok).map(|e| e.path()).collect();
            children.sort();
            pending.extend(children.into_iter().rev());
            continue;
        }
        if !metadata.is_file() || metadata.len() > 4 << 20 {
            continue;
        }
        let shown = crate::pathx::relative(&env.filesystem.directory, &path)
            .unwrap_or_else(|| path.clone())
            .to_string_lossy()
            .into_owned();
        if !matches(&shown) {
            continue;
        }
        if !grep {
            if lines.len() == limit {
                return Ok((lines, true));
            }
            lines.push(shown);
            continue;
        }
        let Ok((bytes, _)) = env.filesystem.read_bounded(&path, 4 << 20) else {
            continue;
        };
        if bytes[..bytes.len().min(8000)].contains(&0) {
            continue;
        }
        let mut reader = std::io::BufReader::new(bytes.as_slice());
        let mut number = 0;
        while let Ok(Some(line)) = next_line(&mut reader, 4 << 20) {
            number += 1;
            if regex.unwrap().is_match(&line) {
                if lines.len() == limit {
                    return Ok((lines, true));
                }
                lines.push(format!(
                    "{shown}:{number}:{}",
                    crate::clip::head(&String::from_utf8_lossy(&line), 500)
                ));
            }
        }
    }
    Ok((lines, false))
}
