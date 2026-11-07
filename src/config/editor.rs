use super::{
    effective::{Loader, read_file},
    frontmatter::{ROLE_COLORS, markdown_parts},
    snapshot::Disk,
    types::*,
};
use anyhow::{Context, Result, bail, ensure};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    ffi::CString,
    fs,
    io::{self, Read},
    os::{
        fd::{AsRawFd, FromRawFd},
        unix::fs::{OpenOptionsExt, symlink},
    },
    path::{Path, PathBuf},
    str::FromStr,
    sync::Mutex,
};
const MAX_FILE: usize = 4 << 20;
static WRITE_LOCK: Mutex<()> = Mutex::new(());
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ConfigEntry {
    pub path: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub directory: bool,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ConfigTree {
    pub root: String,
    pub revision: String,
    #[serde(
        default,
        deserialize_with = "null_vector",
        serialize_with = "nil_empty_vector"
    )]
    pub files: Vec<ConfigEntry>,
}
fn null_vector<'de, D: serde::Deserializer<'de>, T: Deserialize<'de>>(
    d: D,
) -> Result<Vec<T>, D::Error> {
    Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
}
fn nil_empty_vector<S: serde::Serializer, T: Serialize>(
    values: &Vec<T>,
    s: S,
) -> Result<S::Ok, S::Error> {
    if values.is_empty() {
        s.serialize_none()
    } else {
        values.serialize(s)
    }
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ConfigField {
    pub path: Vec<String>,
    pub kind: String,
    pub value: String,
    pub set: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub choices: Vec<String>,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ConfigDocument {
    pub path: String,
    pub content: String,
    pub exists: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub fields: Vec<ConfigField>,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct ConfigEdit {
    pub path: String,
    pub revision: String,
    pub action: String,
    pub content: String,
    pub destination: String,
    pub field_path: Vec<String>,
    pub field_value: Option<Box<serde_json::value::RawValue>>,
}

pub fn editor_path(root: &Path, relative: &str) -> Result<PathBuf> {
    let relative = Path::new(relative);
    let clean = crate::pathx::clean(relative);
    if relative.as_os_str().is_empty()
        || relative.is_absolute()
        || clean == Path::new(".")
        || clean
            .components()
            .any(|c| c == std::path::Component::ParentDir)
    {
        bail!("choose a relative path inside the config directory");
    }
    let path = root.join(clean);
    let mut cursor = path.as_path();
    while cursor != root {
        match fs::symlink_metadata(cursor) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                bail!("symlinked paths are not editable: {}", cursor.display())
            }
            Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e.into()),
            _ => {}
        }
        let Some(parent) = cursor.parent() else {
            break;
        };
        cursor = parent;
    }
    Ok(path)
}
fn walk(root: &Path) -> Result<Vec<PathBuf>> {
    let mut paths = Vec::new();
    let mut pending = vec![root.to_owned()];
    while let Some(path) = pending.pop() {
        let metadata = match fs::symlink_metadata(&path) {
            Ok(m) => m,
            Err(e) if path == root && e.kind() == io::ErrorKind::NotFound => continue,
            Err(e) => return Err(e.into()),
        };
        if metadata.is_dir() {
            let mut children = fs::read_dir(&path)?
                .map(|e| e.map(|e| e.path()))
                .collect::<io::Result<Vec<_>>>()?;
            children.sort();
            pending.extend(children.into_iter().rev());
        }
        if path != root {
            paths.push(path);
        }
    }
    Ok(paths)
}
pub fn tree(root: &Path) -> Result<ConfigTree> {
    let mut hash = Sha256::new();
    let mut files = Vec::new();
    for path in walk(root)? {
        let relative = path
            .strip_prefix(root)?
            .to_str()
            .context("configuration path is not UTF-8")?;
        let metadata = fs::symlink_metadata(&path)?;
        files.push(ConfigEntry {
            path: relative.into(),
            directory: metadata.is_dir(),
        });
        let kind = if metadata.is_dir() {
            "d---------"
        } else if metadata.file_type().is_symlink() {
            "L---------"
        } else if metadata.is_file() {
            "----------"
        } else {
            bail!("configuration tree contains a non-regular file");
        };
        hash.update(format!("{relative}\0{kind}\0"));
        if metadata.is_dir() {
            continue;
        }
        if metadata.file_type().is_symlink() {
            use std::os::unix::ffi::OsStrExt;
            hash.update(fs::read_link(&path)?.as_os_str().as_bytes());
            continue;
        }
        let mut file = fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK | libc::O_NOFOLLOW)
            .open(&path)?;
        let held = file.metadata()?;
        if !held.is_file() {
            bail!("configuration tree contains a non-regular file");
        }
        hash.update(format!("{}\0", held.len()));
        let mut buffer = [0u8; 16384];
        loop {
            let n = file.read(&mut buffer)?;
            if n == 0 {
                break;
            }
            hash.update(&buffer[..n]);
        }
    }
    Ok(ConfigTree {
        root: root.to_string_lossy().into_owned(),
        revision: format!("{:x}", hash.finalize()),
        files,
    })
}
pub fn template(path: &str) -> String {
    if matches!(path, "stavlos.toml" | "stavlos.local.toml") {
        "# Stavlos configuration\n".into()
    } else if path.starts_with("commands/") && path.ends_with(".md") {
        "---\ndescription: Describe this command\n---\n\nWrite the prompt here.\n".into()
    } else if path.starts_with("agents/") && path.ends_with(".md") {
        "---\ndescription: Describe this agent\ntype: all\n---\n\nDescribe the agent's role here.\n"
            .into()
    } else if path.starts_with("skills/")
        && Path::new(path).file_name().is_some_and(|n| n == "SKILL.md")
    {
        format!(
            "---\nname: {}\ndescription: Describe this skill\n---\n\nWrite the skill instructions here.\n",
            Path::new(path)
                .parent()
                .and_then(Path::file_name)
                .unwrap_or_default()
                .to_string_lossy()
        )
    } else {
        String::new()
    }
}
pub fn read(root: &Path, path: &str) -> Result<ConfigDocument> {
    let full = editor_path(root, path)?;
    let (bytes, exists) = match crate::instructions::read_regular(&full, MAX_FILE) {
        Ok(bytes) => (bytes, true),
        Err(e) if e.kind() == io::ErrorKind::NotFound => (template(path).into_bytes(), false),
        Err(e) => return Err(e.into()),
    };
    if bytes.len() > MAX_FILE || bytes.contains(&0) {
        bail!("file is not editable UTF-8 text (maximum 4 MiB)");
    }
    let content = String::from_utf8(bytes).context("file is not editable UTF-8 text")?;
    let path = crate::pathx::clean(Path::new(path))
        .to_string_lossy()
        .into_owned();
    Ok(ConfigDocument {
        fields: fields(&path, &content),
        path,
        content,
        exists,
    })
}
fn schema(name: &str) -> &'static [(&'static str, &'static str)] {
    match name {
        "File" => File::TYPES,
        "Limits" => Limits::TYPES,
        "Escalation" => Escalation::TYPES,
        "Compaction" => Compaction::TYPES,
        "Search" => Search::TYPES,
        "EnvConfig" => EnvConfig::TYPES,
        "SandboxConfig" => SandboxConfig::TYPES,
        "Discord" => Discord::TYPES,
        "Web" => Web::TYPES,
        _ => &[],
    }
}
fn json_fields(name: &str, content: &[u8], prefix: &[String]) -> Vec<ConfigField> {
    let mut values: BTreeMap<_, _> = super::jsonc::object_fields(content)
        .unwrap_or_default()
        .into_iter()
        .collect();
    let mut out = Vec::new();
    for (key, ty) in schema(name) {
        let raw = values.remove(*key);
        let ty = ty.replace(' ', "");
        let unwrapped = ty
            .strip_prefix("Option<")
            .and_then(|s| s.strip_suffix('>'))
            .unwrap_or(&ty);
        let mut path = prefix.to_vec();
        path.push((*key).into());
        if !schema(unwrapped).is_empty() {
            out.extend(json_fields(
                unwrapped,
                raw.as_deref().unwrap_or(b"{}"),
                &path,
            ));
            continue;
        }
        let kind = match unwrapped {
            "String" => "string",
            "bool" => "boolean",
            "i64" | "f64" => "number",
            _ => "json",
        };
        let choices = match path
            .iter()
            .map(String::as_str)
            .collect::<Vec<_>>()
            .as_slice()
        {
            ["mode"] => vec!["ask".into(), "auto".into(), "yolo".into()],
            ["escalation", "default"] => vec!["deny".into(), "allow".into()],
            _ => Vec::new(),
        };
        out.push(ConfigField {
            path,
            kind: kind.into(),
            value: raw
                .as_ref()
                .map(|b| String::from_utf8_lossy(b).into_owned())
                .unwrap_or_default(),
            set: raw.is_some(),
            choices,
        });
    }
    for (name, raw) in values {
        let mut path = prefix.to_vec();
        path.push(name);
        out.push(ConfigField {
            path,
            kind: "json".into(),
            value: String::from_utf8_lossy(&raw).into_owned(),
            set: true,
            choices: Vec::new(),
        });
    }
    out
}
pub fn fields(path: &str, content: &str) -> Vec<ConfigField> {
    if matches!(path, "stavlos.toml" | "stavlos.local.toml") {
        return match crate::source_toml::parse(content) {
            Ok(value) => json_fields("File", &serde_json::to_vec(&value).unwrap_or_default(), &[]),
            Err(_) => Vec::new(),
        };
    }
    let specifications: &[(&str, &str)] = if path.starts_with("agents/") && path.ends_with(".md") {
        &[
            ("description", "string"),
            ("type", "string"),
            ("models", "json"),
            ("tools", "json"),
            ("skills", "json"),
            ("mcp", "json"),
            ("spawn", "json"),
            ("max_turns", "number"),
            ("color", "string"),
        ]
    } else if path.starts_with("commands/") && path.ends_with(".md") {
        &[("description", "string")]
    } else if path.starts_with("skills/")
        && Path::new(path).file_name().is_some_and(|n| n == "SKILL.md")
    {
        &[("name", "string"), ("description", "string")]
    } else {
        return Vec::new();
    };
    let Ok((meta, body)) = markdown_parts(content) else {
        return Vec::new();
    };
    let Ok(values) = serde_yaml_ng::from_str::<serde_yaml_ng::Value>(&meta) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for (name, kind) in specifications {
        let value = values
            .get(*name)
            .and_then(|value| serde_json::to_value(value).ok())
            .and_then(|value| super::jsonc::encode_go(&value).ok());
        let choices = match *name {
            "type" => vec!["all".into(), "primary".into(), "subagent".into()],
            "color" => std::iter::once(String::new())
                .chain(ROLE_COLORS.iter().map(|s| (*s).into()))
                .collect(),
            _ => Vec::new(),
        };
        out.push(ConfigField {
            path: vec![(*name).into()],
            kind: (*kind).into(),
            set: value.is_some(),
            value: value.unwrap_or_default(),
            choices,
        });
    }
    out.push(ConfigField {
        path: vec!["$body".into()],
        kind: "body".into(),
        value: body,
        set: true,
        choices: Vec::new(),
    });
    out
}
pub fn edit_field(path: &str, content: &str, field: &[String], value: &[u8]) -> Result<String> {
    if matches!(path, "stavlos.toml" | "stavlos.local.toml") {
        return super::toml_source::edit(content, field, value);
    }
    if field.len() != 1 {
        bail!("select a frontmatter field or the prompt body");
    }
    let (mut meta, mut body) = markdown_parts(content)?;
    if field[0] == "$body" {
        body = serde_json::from_slice(value)?;
    } else {
        let parsed: serde_yaml_ng::Value = serde_yaml_ng::from_str(&meta)?;
        if !parsed.is_null() && !parsed.is_mapping() {
            bail!("frontmatter must be a mapping");
        }
        let _: Box<serde_json::value::RawValue> = serde_json::from_slice(value)?;
        let file = yaml_edit::YamlFile::from_str(&format!("{meta}\n"))?;
        let document = file
            .documents()
            .next()
            .context("frontmatter document is missing")?;
        let replacement = yaml_edit::Document::from_str(std::str::from_utf8(value)?)?;
        if let Some(mapping) = document.as_mapping() {
            mapping.set(field[0].as_str(), replacement);
            meta = file.to_string();
        } else {
            let document = yaml_edit::Document::new_mapping();
            document.set(field[0].as_str(), replacement);
            meta = document.to_string();
        }
    }
    Ok(format!(
        "---\n{}\n---\n{body}",
        meta.trim_end_matches(['\r', '\n'])
    ))
}
fn private_directories(path: &Path) -> io::Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    let mut builder = fs::DirBuilder::new();
    builder.recursive(true).mode(0o700);
    builder.create(path)
}
pub fn write_config_file(path: &Path, bytes: &[u8], keep_mode: bool) -> Result<()> {
    private_directories(
        path.parent()
            .filter(|p| !p.as_os_str().is_empty())
            .unwrap_or(Path::new(".")),
    )?;
    let actual = if fs::symlink_metadata(path).is_ok_and(|m| m.file_type().is_symlink()) {
        path.canonicalize()?
    } else {
        path.to_owned()
    };
    crate::statefile::write_atomic(&actual, bytes, 0o600, keep_mode)?;
    Ok(())
}
pub fn set_global_field(root: &Path, path: &[String], value: &[u8]) -> Result<()> {
    let _guard = WRITE_LOCK
        .lock()
        .map_err(|_| anyhow::anyhow!("configuration write lock failed"))?;
    let file = root.join("stavlos.toml");
    let content = match crate::instructions::read_regular(&file, 16 << 20) {
        Ok(b) => b,
        Err(e) if e.kind() == io::ErrorKind::NotFound => {
            ensure!(
                root.join("stavlos.json")
                    .symlink_metadata()
                    .is_err_and(|e| e.kind() == io::ErrorKind::NotFound),
                "legacy stavlos.json requires explicit conversion before writing TOML; legacy JSONC v1 must be converted manually; no automatic import"
            );
            Vec::new()
        }
        Err(e) => return Err(e.into()),
    };
    let edited =
        super::toml_source::edit(std::str::from_utf8(&content)?, path, value)?.into_bytes();
    if content != edited {
        write_config_file(&file, &edited, false)?;
    }
    Ok(())
}
pub fn set_discord_enabled(root: &Path, enabled: bool) -> Result<()> {
    let _guard = WRITE_LOCK
        .lock()
        .map_err(|_| anyhow::anyhow!("configuration write lock failed"))?;
    let file = root.join("stavlos.toml");
    let content = match crate::instructions::read_regular(&file, 16 << 20) {
        Ok(b) => b,
        Err(e) if e.kind() == io::ErrorKind::NotFound && !enabled => return Ok(()),
        Err(e) => return Err(e.into()),
    };
    let source = std::str::from_utf8(&content)?;
    let current = crate::source_toml::parse(source).map_err(anyhow::Error::msg)?;
    if !current
        .get("discord")
        .is_some_and(serde_json::Value::is_object)
    {
        ensure!(
            !enabled,
            "add a discord block to the global stavlos.toml first"
        );
        return Ok(());
    }
    let edited = super::toml_source::edit(
        source,
        &["discord".into(), "enabled".into()],
        if enabled { b"true" } else { b"false" },
    )?
    .into_bytes();
    if content != edited {
        write_config_file(&file, &edited, false)?;
    }
    Ok(())
}
fn copy_tree(root: &Path, to: &Path) -> Result<()> {
    for path in walk(root)? {
        let relative = path.strip_prefix(root)?;
        let target = to.join(relative);
        let metadata = fs::symlink_metadata(&path)?;
        if metadata.is_dir() {
            private_directories(&target)?;
        } else if metadata.file_type().is_symlink() {
            let link = fs::read_link(&path)?;
            symlink(
                if link.is_absolute() {
                    link
                } else {
                    path.parent().unwrap().join(link)
                },
                target,
            )?;
        } else if metadata.is_file() {
            let bytes = crate::instructions::read_regular(&path, 64 << 20)?;
            private_directories(target.parent().unwrap())?;
            crate::statefile::write_atomic(&target, &bytes, 0o600, false)?;
        } else {
            bail!("configuration tree contains a non-regular file");
        }
    }
    Ok(())
}
fn preview_mutation(root: &Path, edit: &ConfigEdit) -> Result<()> {
    let path = editor_path(root, &edit.path)?;
    match edit.action.as_str() {
        "write" => {
            private_directories(path.parent().unwrap())?;
            fs::write(path, &edit.content)?;
        }
        "rename" => {
            let destination = editor_path(root, &edit.destination)?;
            private_directories(destination.parent().unwrap())?;
            fs::rename(path, destination)?;
        }
        "delete" => {
            if path.is_dir() {
                fs::remove_dir_all(path)?;
            } else {
                fs::remove_file(path)?;
            }
        }
        _ => bail!("unknown config operation"),
    };
    Ok(())
}
fn validate(root: &Path, system: bool, loader: &Loader<'_>) -> Result<()> {
    for name in ["stavlos.toml", "stavlos.local.toml"] {
        match fs::read(root.join(name)) {
            Ok(bytes) => {
                crate::source_toml::parse(std::str::from_utf8(&bytes)?)
                    .map_err(anyhow::Error::msg)?;
            }
            Err(e) if e.kind() == io::ErrorKind::NotFound => {}
            Err(e) => return Err(e.into()),
        }
    }
    if system {
        let mut paths = loader.paths.clone();
        paths.config = root.to_owned();
        Loader {
            paths: &paths,
            home: loader.home,
            lookup: loader.lookup,
        }
        .global()?;
    } else {
        let mut effective = loader.global()?;
        for name in ["stavlos.toml", "stavlos.local.toml"] {
            effective.apply(&read_file(&Disk, &root.join(name))?, "project", loader)?;
        }
        effective.allow_search();
        effective.load_roles(&Disk, &root.join("agents"), "project")?;
        effective.load_skills(&Disk, &root.join("skills"))?;
        effective.load_commands(&Disk, &root.join("commands"))?;
    }
    Ok(())
}
// Parents remain held across the mutation: a path swapped after validation cannot redirect a write.
fn held_parent(root: &Path, relative: &str, create: bool) -> Result<(fs::File, CString)> {
    editor_path(root, relative)?;
    if create {
        private_directories(root)?;
    }
    let mut directory = fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW)
        .open(root)?;
    let clean = crate::pathx::clean(Path::new(relative));
    let name = CString::new(
        clean
            .file_name()
            .context("configuration filename is missing")?
            .as_encoded_bytes(),
    )?;
    for component in clean.parent().unwrap_or(Path::new("")).components() {
        if component == std::path::Component::CurDir {
            continue;
        }
        let name = CString::new(component.as_os_str().as_encoded_bytes())?;
        let mut fd = unsafe {
            libc::openat(
                directory.as_raw_fd(),
                name.as_ptr(),
                libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC,
            )
        };
        if fd < 0 && create && io::Error::last_os_error().kind() == io::ErrorKind::NotFound {
            if unsafe { libc::mkdirat(directory.as_raw_fd(), name.as_ptr(), 0o700) } < 0
                && io::Error::last_os_error().kind() != io::ErrorKind::AlreadyExists
            {
                return Err(io::Error::last_os_error().into());
            }
            fd = unsafe {
                libc::openat(
                    directory.as_raw_fd(),
                    name.as_ptr(),
                    libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                )
            };
        }
        if fd < 0 {
            return Err(io::Error::last_os_error().into());
        }
        directory = unsafe { fs::File::from_raw_fd(fd) };
    }
    Ok((directory, name))
}
fn commit_mutation(root: &Path, edit: &ConfigEdit) -> Result<()> {
    let (parent, name) = held_parent(root, &edit.path, edit.action == "write")?;
    let anchored = PathBuf::from(format!("/proc/self/fd/{}", parent.as_raw_fd()));
    match edit.action.as_str() {
        "write" => crate::statefile::write_atomic(
            &anchored.join(std::ffi::OsStr::from_bytes(name.as_bytes())),
            edit.content.as_bytes(),
            0o600,
            true,
        )?,
        "rename" => {
            let (destination, target) = held_parent(root, &edit.destination, true)?;
            if unsafe {
                libc::syscall(
                    libc::SYS_renameat2,
                    parent.as_raw_fd(),
                    name.as_ptr(),
                    destination.as_raw_fd(),
                    target.as_ptr(),
                    libc::RENAME_NOREPLACE,
                )
            } < 0
            {
                return Err(io::Error::last_os_error().into());
            }
            parent.sync_all()?;
            destination.sync_all()?;
        }
        "delete" => {
            let trash = tempfile::Builder::new()
                .prefix(".stavlos-delete-")
                .tempdir_in(&anchored)?;
            let target = trash.path().join("removed");
            fs::rename(
                anchored.join(std::ffi::OsStr::from_bytes(name.as_bytes())),
                target,
            )?;
            parent.sync_all()?;
        }
        _ => bail!("unknown config operation"),
    }
    Ok(())
}
use std::os::unix::ffi::OsStrExt;
pub fn apply(
    root: &Path,
    system: bool,
    edit: &ConfigEdit,
    loader: &Loader<'_>,
) -> Result<ConfigTree> {
    let _guard = WRITE_LOCK
        .lock()
        .map_err(|_| anyhow::anyhow!("configuration write lock failed"))?;
    let before = tree(root)?;
    if edit.revision.is_empty() || edit.revision != before.revision {
        bail!("config files changed on disk; reload before saving");
    }
    editor_path(root, &edit.path)?;
    if edit.action == "rename" {
        let destination = editor_path(root, &edit.destination)?;
        if !fs::symlink_metadata(destination).is_err_and(|e| e.kind() == io::ErrorKind::NotFound) {
            bail!("destination already exists or is unavailable");
        }
    }
    let mut edit = edit.clone();
    if !edit.field_path.is_empty() {
        let document = read(root, &edit.path)?;
        edit.content = edit_field(
            &edit.path,
            &document.content,
            &edit.field_path,
            edit.field_value
                .as_ref()
                .context("field value is required")?
                .get()
                .as_bytes(),
        )?;
    }
    if edit.content.len() > MAX_FILE || edit.action == "write" && edit.content.contains('\0') {
        bail!("config editor writes UTF-8 text without NUL bytes, maximum 4 MiB");
    }
    let preview = tempfile::Builder::new()
        .prefix("stavlos-config-preview-")
        .tempdir()?;
    copy_tree(root, preview.path())?;
    preview_mutation(preview.path(), &edit)?;
    validate(preview.path(), system, loader)?;
    if tree(root)?.revision != before.revision {
        bail!("config files changed during validation; reload before saving");
    }
    commit_mutation(root, &edit)?;
    tree(root)
}

/// Legacy-v1 schema view for explicit imports and the immutable native parity inventory.
pub fn legacy_jsonc_fields_v1(path: &str, content: &str) -> Vec<ConfigField> {
    if matches!(path, "stavlos.json" | "stavlos.local.json") {
        return match super::jsonc::mask(content.as_bytes()) {
            Ok(clean) => json_fields("File", &clean, &[]),
            Err(_) => Vec::new(),
        };
    }
    fields(path, content)
}
