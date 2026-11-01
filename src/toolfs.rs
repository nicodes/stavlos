//! Open judged tool paths through their working-directory capabilities.
use crate::pathx;
use anyhow::{Context, Result, bail, ensure};
use cap_std::{
    ambient_authority,
    fs::{Dir, DirBuilder, DirBuilderExt as _, OpenOptions, OpenOptionsExt as _},
};
use std::{
    collections::BTreeMap,
    ffi::OsString,
    fs::{self, File},
    io::Write,
    os::unix::fs::{DirBuilderExt, OpenOptionsExt as _, PermissionsExt},
    path::{Path, PathBuf},
};
#[derive(Clone, Debug, Default)]
pub struct Filesystem {
    pub directory: PathBuf,
    pub roots: Vec<PathBuf>,
    pub judged: BTreeMap<String, PathBuf>,
}
/// Resolve the existing prefix, retaining literal '~' and '$HOME' path components.
pub fn resolve_path(root: &Path, path: &Path) -> PathBuf {
    let path = if path.as_os_str().is_empty() {
        root.to_owned()
    } else if path.is_absolute() {
        path.to_owned()
    } else {
        root.join(path)
    };
    let path = pathx::clean(&path);
    let mut current = path.clone();
    let mut suffix = PathBuf::new();
    loop {
        if let Ok(real) = fs::canonicalize(&current) {
            return pathx::clean(&real.join(&suffix));
        }
        let Some(parent) = current.parent() else {
            return path;
        };
        if parent == current {
            return path;
        }
        if let Some(name) = current.file_name() {
            suffix = Path::new(name).join(suffix);
        }
        current = parent.to_owned();
    }
}
impl Filesystem {
    pub fn resolve(&self, path: &str) -> PathBuf {
        self.judged
            .get(path)
            .cloned()
            .unwrap_or_else(|| resolve_path(&self.directory, Path::new(path)))
    }
    pub fn at(&self, absolute: &Path) -> Result<Option<(Dir, PathBuf)>> {
        for root in &self.roots {
            if let Some(relative) = pathx::inside(root, absolute) {
                return Ok(Some((
                    Dir::open_ambient_dir(root, ambient_authority())
                        .with_context(|| format!("open root {}", root.display()))?,
                    relative,
                )));
            }
        }
        Ok(None)
    }
    pub fn open_read(&self, absolute: &Path) -> Result<File> {
        let file = if let Some((root, relative)) = self.at(absolute)? {
            let mut options = OpenOptions::new();
            options.read(true).custom_flags(libc::O_NONBLOCK);
            root.open_with(relative, &options)?.into_std()
        } else {
            fs::OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_NONBLOCK)
                .open(absolute)?
        };
        let metadata = file.metadata()?;
        ensure!(!metadata.is_dir(), "{} is a directory", absolute.display());
        ensure!(
            metadata.is_file(),
            "{} is not a regular file",
            absolute.display()
        );
        Ok(file)
    }
    pub fn check(&self, absolute: &Path) -> Result<()> {
        if let Some((root, relative)) = self.at(absolute)? {
            root.metadata(relative)?;
        }
        Ok(())
    }
    pub fn remove(&self, absolute: &Path) -> Result<()> {
        if let Some((root, relative)) = self.at(absolute)? {
            let metadata = root.symlink_metadata(&relative)?;
            if metadata.is_dir() {
                root.remove_dir(relative)?;
            } else {
                root.remove_file(relative)?;
            }
        } else if fs::symlink_metadata(absolute)?.is_dir() {
            fs::remove_dir(absolute)?;
        } else {
            fs::remove_file(absolute)?;
        }
        Ok(())
    }
    fn parent(&self, absolute: &Path) -> Result<(Dir, OsString)> {
        let name = absolute
            .file_name()
            .context("a file path is required")?
            .to_owned();
        if let Some((root, relative)) = self.at(absolute)? {
            ensure!(relative != Path::new("."), "a file path is required");
            let parent = relative.parent().unwrap_or(Path::new("."));
            if !parent.as_os_str().is_empty() && parent != Path::new(".") {
                root.create_dir_with(parent, DirBuilder::new().recursive(true).mode(0o755))?;
            }
            return Ok((
                root.open_dir(if parent.as_os_str().is_empty() {
                    Path::new(".")
                } else {
                    parent
                })?,
                name,
            ));
        }
        let parent = absolute.parent().unwrap_or(Path::new("."));
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o755)
            .create(parent)?;
        Ok((Dir::open_ambient_dir(parent, ambient_authority())?, name))
    }
    /// Hold the checked parent through replacement; failed writes remove only their own temp file.
    pub fn write_atomic(&self, absolute: &Path, content: &[u8], mode: u32) -> Result<()> {
        let (parent, name) = self.parent(absolute)?;
        let mut temporary = OsString::from(".");
        temporary.push(&name);
        temporary.push(format!(".stavlos-{}", uuid::Uuid::new_v4().simple()));
        let result = (|| -> Result<()> {
            let mut options = OpenOptions::new();
            options.write(true).create_new(true).mode(mode & 0o7777);
            let mut file = parent.open_with(&temporary, &options)?.into_std();
            file.write_all(content)?;
            file.set_permissions(fs::Permissions::from_mode(mode & 0o7777))?;
            file.sync_all()?;
            drop(file);
            parent.rename(&temporary, &parent, &name)?;
            Ok(())
        })();
        if result.is_err() {
            let _ = parent.remove_file(&temporary);
        }
        result
    }
    pub fn read_bounded(&self, absolute: &Path, maximum: usize) -> Result<(Vec<u8>, u32)> {
        use std::io::Read;
        let file = self.open_read(absolute)?;
        let metadata = file.metadata()?;
        if metadata.len() > maximum as u64 {
            bail!("{} is too large", absolute.display())
        }
        let mode = metadata.permissions().mode() & 0o777;
        let mut bytes = Vec::new();
        file.take((maximum + 1) as u64).read_to_end(&mut bytes)?;
        ensure!(
            bytes.len() <= maximum,
            "{} is too large",
            absolute.display()
        );
        Ok((bytes, mode))
    }
}
