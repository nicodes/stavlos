//! Same-directory replacement, preserving existing committed-file modes when requested.
use std::{io::Write, os::unix::fs::PermissionsExt, path::Path};
pub fn write_atomic(path: &Path, data: &[u8], mode: u32, keep_mode: bool) -> std::io::Result<()> {
    let parent = path
        .parent()
        .filter(|p| !p.as_os_str().is_empty())
        .unwrap_or(Path::new("."));
    let mode = if keep_mode {
        std::fs::metadata(path)
            .map(|m| m.permissions().mode() & 0o777)
            .unwrap_or(mode)
    } else {
        mode
    };
    let name = path.file_name().unwrap_or_default().to_string_lossy();
    let mut file = tempfile::Builder::new()
        .prefix(&format!(".{name}."))
        .tempfile_in(parent)?;
    file.write_all(data)?;
    file.as_file()
        .set_permissions(std::fs::Permissions::from_mode(mode))?;
    file.as_file().sync_all()?;
    file.persist(path).map_err(|e| e.error)?;
    std::fs::File::open(parent)?.sync_all()?;
    Ok(())
}
