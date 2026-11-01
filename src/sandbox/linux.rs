use anyhow::{Context, Result, bail};
use std::{
    ffi::CString,
    fs, io,
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    path::Path,
};
const WRITE: u64 = 1 << 1;
const READ: u64 = (1 << 0) | (1 << 2) | (1 << 3);
const DIR_CHANGES: u64 = ((1 << 13) - 1) & !((1 << 4) - 1);
const REFER: u64 = 1 << 13;
const TRUNCATE: u64 = 1 << 14;
#[repr(C)]
struct Ruleset {
    fs: u64,
    net: u64,
    scoped: u64,
}
#[repr(C)]
struct PathRule {
    access: u64,
    fd: i32,
    pad: u32,
}
pub fn abi() -> i32 {
    let result = unsafe { libc::syscall(libc::SYS_landlock_create_ruleset, 0, 0, 1) };
    if result < 0 { 0 } else { result as i32 }
}
fn add_path(
    ruleset: &OwnedFd,
    path: &Path,
    file_rights: u64,
    directory_rights: u64,
    missing_ok: bool,
) -> Result<()> {
    let name = CString::new(path.as_os_str().as_encoded_bytes())?;
    let descriptor = unsafe { libc::open(name.as_ptr(), libc::O_PATH | libc::O_CLOEXEC) };
    if descriptor < 0 {
        let error = io::Error::last_os_error();
        if missing_ok || error.kind() == io::ErrorKind::NotFound {
            return Ok(());
        }
        return Err(error).with_context(|| format!("read grant {}", path.display()));
    }
    let descriptor = unsafe { OwnedFd::from_raw_fd(descriptor) };
    let mut metadata = std::mem::MaybeUninit::<libc::stat>::uninit();
    if unsafe { libc::fstat(descriptor.as_raw_fd(), metadata.as_mut_ptr()) } < 0 {
        return Err(io::Error::last_os_error().into());
    }
    let metadata = unsafe { metadata.assume_init() };
    let rights = if metadata.st_mode & libc::S_IFMT == libc::S_IFDIR {
        directory_rights
    } else {
        file_rights
    };
    let rule = PathRule {
        access: rights,
        fd: descriptor.as_raw_fd(),
        pad: 0,
    };
    if unsafe {
        libc::syscall(
            libc::SYS_landlock_add_rule,
            ruleset.as_raw_fd(),
            1,
            &rule as *const PathRule,
            0,
        )
    } < 0
    {
        return Err(io::Error::last_os_error())
            .with_context(|| format!("landlock rule for {}", path.display()));
    }
    Ok(())
}
pub fn restrict(
    writable: &[std::path::PathBuf],
    readable: &[std::path::PathBuf],
    network: bool,
) -> Result<()> {
    let version = abi();
    if version <= 0 {
        bail!("landlock unavailable");
    }
    let mut file = WRITE;
    let mut directory = WRITE | DIR_CHANGES;
    if version >= 2 {
        directory |= REFER;
    }
    if version >= 3 {
        file |= TRUNCATE;
        directory |= TRUNCATE;
    }
    let attributes = Ruleset {
        fs: directory | READ,
        net: if version >= 4 && !network { 3 } else { 0 },
        scoped: if version >= 6 { 3 } else { 0 },
    };
    let descriptor = unsafe {
        libc::syscall(
            libc::SYS_landlock_create_ruleset,
            &attributes as *const Ruleset,
            std::mem::size_of::<Ruleset>(),
            0,
        )
    };
    if descriptor < 0 {
        return Err(io::Error::last_os_error()).context("landlock ruleset");
    }
    let ruleset = unsafe { OwnedFd::from_raw_fd(descriptor as i32) };
    for path in [
        "/dev/null",
        "/dev/zero",
        "/dev/full",
        "/dev/tty",
        "/dev/ptmx",
    ]
    .iter()
    .map(Path::new)
    .chain(writable.iter().map(|p| p.as_path()))
    {
        add_path(
            &ruleset,
            path,
            file | (1 << 2) | (1 << 0),
            directory | READ,
            true,
        )?;
    }
    for path in system_readable()
        .iter()
        .map(Path::new)
        .chain(readable.iter().map(|p| p.as_path()))
    {
        add_path(&ruleset, path, (1 << 2) | (1 << 0), READ, false)?;
    }
    if unsafe { libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) } < 0 {
        return Err(io::Error::last_os_error()).context("no_new_privs");
    }
    if unsafe { libc::syscall(libc::SYS_landlock_restrict_self, ruleset.as_raw_fd(), 0) } < 0 {
        return Err(io::Error::last_os_error()).context("landlock restrict");
    }
    Ok(())
}
pub fn system_readable() -> &'static [&'static str] {
    &[
        "/bin",
        "/sbin",
        "/lib",
        "/lib64",
        "/usr/bin",
        "/usr/sbin",
        "/usr/lib",
        "/usr/lib64",
        "/usr/share",
        "/usr/local/bin",
        "/usr/local/lib",
        "/usr/local/go",
        "/etc/ld.so.cache",
        "/etc/alternatives",
        "/etc/ssl/certs",
        "/etc/resolv.conf",
        "/etc/hosts",
        "/etc/nsswitch.conf",
        "/etc/passwd",
        "/etc/group",
        "/etc/localtime",
        "/dev/urandom",
        "/dev/random",
        "/proc/self",
        "/proc/meminfo",
        "/proc/cpuinfo",
        "/sys/devices/system/cpu",
    ]
}
pub fn restrict_unix_sockets() -> Result<()> {
    #[cfg(target_arch = "x86_64")]
    let architecture = 0xc000003eu32;
    #[cfg(target_arch = "aarch64")]
    let architecture = 0xc00000b7u32;
    #[cfg(not(any(target_arch = "x86_64", target_arch = "aarch64")))]
    {
        bail!("unix socket containment unsupported on this architecture");
    }
    #[cfg(any(target_arch = "x86_64", target_arch = "aarch64"))]
    {
        let instruction = |code: u16, k: u32, jt: u8, jf: u8| libc::sock_filter { code, jt, jf, k };
        let mut filter = vec![
            instruction(0x20, 4, 0, 0),
            instruction(0x15, architecture, 1, 0),
            instruction(0x06, 0x80000000, 0, 0),
            instruction(0x20, 0, 0, 0),
        ];
        #[cfg(target_arch = "x86_64")]
        {
            filter.push(instruction(0x45, 0x40000000, 0, 1));
            filter.push(instruction(0x06, 0x00050000 | libc::ENOSYS as u32, 0, 0));
        }
        filter.extend([
            instruction(0x15, libc::SYS_socket as u32, 0, 3),
            instruction(0x20, 16, 0, 0),
            instruction(0x15, libc::AF_UNIX as u32, 0, 1),
            instruction(0x06, 0x00050000 | libc::EPERM as u32, 0, 0),
            instruction(0x06, 0x7fff0000, 0, 0),
        ]);
        let program = libc::sock_fprog {
            len: filter.len() as u16,
            filter: filter.as_mut_ptr(),
        };
        if unsafe {
            libc::prctl(
                libc::PR_SET_SECCOMP,
                2,
                &program as *const libc::sock_fprog,
                0,
                0,
            )
        } < 0
        {
            return Err(io::Error::last_os_error()).context("unix socket containment");
        }
        Ok(())
    }
}
fn mount(
    source: &str,
    destination: &Path,
    kind: &str,
    flags: libc::c_ulong,
    data: &str,
) -> Result<()> {
    let source = CString::new(source)?;
    let destination = CString::new(destination.as_os_str().as_encoded_bytes())?;
    let kind = CString::new(kind)?;
    let data = CString::new(data)?;
    if unsafe {
        libc::mount(
            source.as_ptr(),
            destination.as_ptr(),
            kind.as_ptr(),
            flags,
            data.as_ptr().cast(),
        )
    } < 0
    {
        return Err(io::Error::last_os_error().into());
    }
    Ok(())
}
fn bind_read_only(source: &str, destination: &Path) -> Result<()> {
    mount(source, destination, "", libc::MS_BIND, "")
        .with_context(|| format!("bind {}", destination.display()))?;
    let target = CString::new(destination.as_os_str().as_encoded_bytes())?;
    let mut attributes = std::mem::MaybeUninit::<libc::statvfs>::uninit();
    if unsafe { libc::statvfs(target.as_ptr(), attributes.as_mut_ptr()) } < 0 {
        return Err(io::Error::last_os_error().into());
    }
    let attributes = unsafe { attributes.assume_init() };
    let keep = attributes.f_flag as libc::c_ulong
        & (libc::MS_NOSUID
            | libc::MS_NODEV
            | libc::MS_NOEXEC
            | libc::MS_NOATIME
            | libc::MS_NODIRATIME
            | libc::MS_RELATIME);
    mount(
        "",
        destination,
        "",
        libc::MS_BIND | libc::MS_REMOUNT | libc::MS_RDONLY | keep,
        "",
    )
    .with_context(|| format!("read-only {}", destination.display()))
}
pub fn mount_view(spec: &super::Spec) -> Result<()> {
    mount("", Path::new("/"), "", libc::MS_REC | libc::MS_PRIVATE, "").context("private mounts")?;
    if !spec.tmp.as_os_str().is_empty() && spec.private_tmp {
        mount(
            spec.tmp.to_str().context("scratch path is not UTF-8")?,
            Path::new("/tmp"),
            "",
            libc::MS_BIND | libc::MS_REC,
            "",
        )?;
    }
    for path in &spec.read_only {
        if fs::symlink_metadata(path).is_ok() {
            bind_read_only(path.to_str().context("control path is not UTF-8")?, path)?;
        }
    }
    for path in &spec.hidden {
        let Ok(metadata) = fs::symlink_metadata(path) else {
            continue;
        };
        if metadata.is_dir() {
            mount(
                "tmpfs",
                path,
                "tmpfs",
                libc::MS_NOSUID | libc::MS_NODEV | libc::MS_NOEXEC | libc::MS_RDONLY,
                "size=4k,mode=0555",
            )?;
        } else if !metadata.file_type().is_symlink() {
            bind_read_only("/dev/null", path)?;
        }
    }
    Ok(())
}
#[repr(C)]
pub struct CapabilityHeader {
    pub version: u32,
    pub pid: i32,
}
#[repr(C)]
#[derive(Clone, Copy, Default)]
pub struct CapabilityData {
    pub effective: u32,
    pub permitted: u32,
    pub inheritable: u32,
}
pub fn drop_capabilities() -> Result<()> {
    if unsafe { libc::prctl(47, 4, 0, 0, 0) } < 0 {
        return Err(io::Error::last_os_error()).context("clear capabilities");
    }
    let header = CapabilityHeader {
        version: 0x20080522,
        pid: 0,
    };
    let capabilities = [CapabilityData::default(); 2];
    if unsafe {
        libc::syscall(
            libc::SYS_capset,
            &header as *const CapabilityHeader,
            capabilities.as_ptr(),
        )
    } < 0
    {
        return Err(io::Error::last_os_error()).context("drop capabilities");
    }
    Ok(())
}
pub fn namespace_flags(network: bool) -> i32 {
    libc::CLONE_NEWUSER | libc::CLONE_NEWNS | if network { 0 } else { libc::CLONE_NEWNET }
}
/// Only raw syscalls here: this runs in Command::pre_exec, after fork and before exec.
///
/// # Safety
/// Call only in the freshly forked command child; it changes that process's namespaces and credentials.
pub unsafe fn enter_namespace_child(
    flags: i32,
    uid_map: &[u8],
    gid_map: &[u8],
    parent: i32,
) -> io::Result<()> {
    unsafe fn write_file(path: &std::ffi::CStr, bytes: &[u8]) -> io::Result<()> {
        let fd = unsafe { libc::open(path.as_ptr(), libc::O_WRONLY | libc::O_CLOEXEC) };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        let mut offset = 0;
        while offset < bytes.len() {
            let n =
                unsafe { libc::write(fd, bytes[offset..].as_ptr().cast(), bytes.len() - offset) };
            if n < 0 {
                let error = io::Error::last_os_error();
                if error.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                unsafe { libc::close(fd) };
                return Err(error);
            }
            if n == 0 {
                unsafe { libc::close(fd) };
                return Err(io::ErrorKind::WriteZero.into());
            }
            offset += n as usize;
        }
        unsafe { libc::close(fd) };
        Ok(())
    }
    if unsafe { libc::unshare(flags) } < 0 {
        return Err(io::Error::last_os_error());
    }
    unsafe {
        write_file(c"/proc/self/setgroups", b"deny\n")?;
        write_file(c"/proc/self/uid_map", uid_map)?;
        write_file(c"/proc/self/gid_map", gid_map)?;
    }
    let header = CapabilityHeader {
        version: 0x20080522,
        pid: 0,
    };
    let mut capabilities = [CapabilityData::default(); 2];
    if unsafe {
        libc::syscall(
            libc::SYS_capget,
            &header as *const CapabilityHeader,
            capabilities.as_mut_ptr(),
        )
    } < 0
    {
        return Err(io::Error::last_os_error());
    }
    capabilities[0].inheritable |= 1 << 21;
    if unsafe {
        libc::syscall(
            libc::SYS_capset,
            &header as *const CapabilityHeader,
            capabilities.as_ptr(),
        )
    } < 0
    {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::prctl(47, 2, 21, 0, 0) } < 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL, 0, 0, 0) } < 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::getppid() } != parent {
        return Err(io::ErrorKind::Interrupted.into());
    }
    Ok(())
}
