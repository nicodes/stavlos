use crate::{auth::Session, database::Database};
use hmac::{Hmac, Mac};
use sha2::Sha256;
use std::{
    fs::{File, OpenOptions},
    io::Read,
    os::{
        fd::{AsRawFd, FromRawFd},
        unix::fs::OpenOptionsExt,
    },
    path::Path,
    sync::OnceLock,
};
use subtle::ConstantTimeEq;
pub fn token(session: &Session, channel: &str, id: &str) -> String {
    let mut mac = Hmac::<Sha256>::new_from_slice(&session.key).unwrap();
    mac.update(format!("{channel}\n{id}").as_bytes());
    mac.finalize()
        .into_bytes()
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}
pub fn holds(session: &Session, channel: &str, id: &str, offered: &str) -> bool {
    bool::from(
        token(session, channel, id)
            .as_bytes()
            .ct_eq(offered.as_bytes()),
    )
}
fn identifier(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-'))
}
fn directory(parent: &File, name: &str) -> std::io::Result<File> {
    let name = std::ffi::CString::new(name)?;
    let fd = unsafe {
        libc::openat(
            parent.as_raw_fd(),
            name.as_ptr(),
            libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC,
        )
    };
    if fd < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(unsafe { File::from_raw_fd(fd) })
}
fn bytes(root: &Path, channel: &str, id: &str) -> std::io::Result<Vec<u8>> {
    if !identifier(channel)
        || !id
            .strip_prefix('s')
            .is_some_and(|n| !n.is_empty() && n.bytes().all(|b| b.is_ascii_digit()))
    {
        return Err(std::io::Error::other("invalid sheet"));
    }
    let parent = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW)
        .open(root)?;
    let sheets = directory(&parent, "sheets")?;
    let channel = directory(&sheets, channel)?;
    let name = std::ffi::CString::new(format!("{id}.html"))?;
    let fd = unsafe {
        libc::openat(
            channel.as_raw_fd(),
            name.as_ptr(),
            libc::O_RDONLY | libc::O_NOFOLLOW | libc::O_NONBLOCK | libc::O_CLOEXEC,
        )
    };
    if fd < 0 {
        return Err(std::io::Error::last_os_error());
    }
    let file = unsafe { File::from_raw_fd(fd) };
    if !file.metadata()?.is_file() {
        return Err(std::io::Error::other("not a sheet file"));
    }
    let mut raw = Vec::new();
    file.take((2 << 20) + 1).read_to_end(&mut raw)?;
    if raw.len() > 2 << 20 {
        return Err(std::io::Error::other("sheet too large"));
    }
    Ok(raw)
}
pub async fn read(
    database: &Database,
    root: &Path,
    channel: &str,
    id: &str,
) -> Result<Vec<u8>, String> {
    let sheets = database.sheets(channel.into()).await?;
    let info = sheets
        .iter()
        .find(|s| s["id"].as_str() == Some(id))
        .ok_or("sheet not found")?;
    let title = info["title"].as_str().unwrap_or("").to_owned();
    let root = root.to_owned();
    let channel = channel.to_owned();
    let id = id.to_owned();
    let raw = tokio::task::spawn_blocking(move || bytes(&root, &channel, &id))
        .await
        .map_err(|_| "sheet reader failed")?
        .map_err(|_| "sheet not found")?;
    Ok(frame(&title, &raw))
}
fn escape(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('\"', "&#34;")
        .replace('\'', "&#39;")
}
pub fn frame(title: &str, raw: &[u8]) -> Vec<u8> {
    static HEAD: OnceLock<regex::bytes::Regex> = OnceLock::new();
    static HTML: OnceLock<regex::bytes::Regex> = OnceLock::new();
    let link=br#"<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/sheet.css">"#;
    if let Some(found) = HEAD
        .get_or_init(|| regex::bytes::Regex::new("(?i)<head[^>]*>").unwrap())
        .find(raw)
    {
        let mut out = raw[..found.end()].to_vec();
        out.extend_from_slice(link);
        out.extend_from_slice(&raw[found.end()..]);
        return out;
    }
    if let Some(found) = HTML
        .get_or_init(|| regex::bytes::Regex::new("(?i)<html[^>]*>").unwrap())
        .find(raw)
    {
        let mut out = raw[..found.end()].to_vec();
        out.extend_from_slice(b"<head>");
        out.extend_from_slice(link);
        out.extend_from_slice(b"</head>");
        out.extend_from_slice(&raw[found.end()..]);
        return out;
    }
    let mut out = br#"<!doctype html><html data-theme="dark"><head>"#.to_vec();
    out.extend_from_slice(link);
    out.extend_from_slice(
        format!(
            "<title>{}</title></head><body class=\"p-6\">",
            escape(title)
        )
        .as_bytes(),
    );
    out.extend_from_slice(raw);
    out.extend_from_slice(b"</body></html>");
    out
}
