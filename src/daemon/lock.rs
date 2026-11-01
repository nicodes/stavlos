use anyhow::{Context, Result, ensure};
use fs2::FileExt;
use std::{
    fs::{File, OpenOptions},
    io::{Read, Seek, SeekFrom, Write},
    os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt},
    path::Path,
};
#[derive(Debug, thiserror::Error)]
#[error("another stavlosd is running on this data directory{0}")]
pub struct AlreadyRunning(pub String);
pub struct DataLock(File);
impl DataLock {
    pub fn acquire(directory: &Path) -> Result<Self> {
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(directory)?;
        let path = directory.join("stavlosd.lock");
        let mut file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
            .open(&path)
            .context("open owner lock")?;
        ensure!(
            file.metadata()?.is_file(),
            "owner lock is not a regular file"
        );
        if let Err(error) = file.try_lock_exclusive() {
            if matches!(error.kind(), std::io::ErrorKind::WouldBlock) {
                let mut bytes = String::new();
                let _ = Read::by_ref(&mut file).take(64).read_to_string(&mut bytes);
                let suffix = if bytes.parse::<u32>().is_ok() {
                    format!(" (pid {bytes})")
                } else {
                    String::new()
                };
                return Err(AlreadyRunning(suffix).into());
            }
            return Err(error.into());
        }
        file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
        file.set_len(0)?;
        file.seek(SeekFrom::Start(0))?;
        write!(&mut file, "{}", std::process::id())?;
        file.sync_all()?;
        Ok(Self(file))
    }
    pub fn file(&self) -> &File {
        &self.0
    }
}
