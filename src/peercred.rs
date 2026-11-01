//! Kernel credentials and process ancestry for the Unix owner socket.
use anyhow::{Context, Result, bail};
use std::{fs, io, os::fd::AsRawFd};
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Cred {
    pub pid: i32,
    pub uid: u32,
}
pub fn of(socket: &impl AsRawFd) -> Result<Cred> {
    let mut credential = std::mem::MaybeUninit::<libc::ucred>::uninit();
    let mut size = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
    if unsafe {
        libc::getsockopt(
            socket.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            credential.as_mut_ptr().cast(),
            &mut size,
        )
    } < 0
    {
        return Err(io::Error::last_os_error().into());
    }
    if size as usize != std::mem::size_of::<libc::ucred>() {
        bail!("unexpected peer credential size");
    }
    let credential = unsafe { credential.assume_init() };
    Ok(Cred {
        pid: credential.pid,
        uid: credential.uid,
    })
}
pub fn of_self(socket: &impl AsRawFd) -> Result<Cred> {
    let credential = of(socket)?;
    if credential.uid != unsafe { libc::getuid() } {
        bail!(
            "the socket's peer runs as another user (uid {})",
            credential.uid
        );
    }
    Ok(credential)
}
pub fn parent_from_stat(stat: &str) -> Result<i32> {
    let end = stat.rfind(')').context("unexpected /proc stat format")?;
    stat[end + 1..]
        .split_whitespace()
        .nth(1)
        .context("unexpected /proc stat format")?
        .parse()
        .context("invalid /proc parent pid")
}
pub fn parent_of(pid: i32) -> Result<i32> {
    parent_from_stat(&fs::read_to_string(format!("/proc/{pid}/stat"))?)
}
pub fn descends_from(pid: i32, ancestor: i32) -> Result<bool> {
    descends_with(pid, ancestor, parent_of)
}
pub fn descends_with(
    mut pid: i32,
    ancestor: i32,
    parent: impl Fn(i32) -> Result<i32>,
) -> Result<bool> {
    for _ in 0..128 {
        if pid <= 1 {
            return Ok(false);
        }
        let next =
            parent(pid).with_context(|| format!("the ancestry of process {pid} cannot be read"))?;
        if next == ancestor {
            return Ok(true);
        }
        pid = next;
    }
    if pid > 1 {
        bail!("the ancestry of process {pid} is deeper than this walks");
    }
    Ok(false)
}
/// Called only by the owner daemon before it starts model-controlled descendants.
pub fn harden_owner() -> Result<()> {
    if unsafe { libc::prctl(libc::PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) } < 0 {
        return Err(io::Error::last_os_error().into());
    }
    if unsafe { libc::prctl(libc::PR_SET_DUMPABLE, 0, 0, 0, 0) } < 0 {
        return Err(io::Error::last_os_error().into());
    }
    Ok(())
}
