//! A reaper must never steal an exit status from an owned process supervisor.
use std::{
    collections::{BTreeMap, BTreeSet},
    io,
    sync::{Arc, Mutex, OnceLock},
    time::Duration,
};
use tokio_util::sync::CancellationToken;
fn registry() -> &'static Mutex<BTreeSet<i32>> {
    static REGISTRY: OnceLock<Mutex<BTreeSet<i32>>> = OnceLock::new();
    REGISTRY.get_or_init(Default::default)
}
struct Registration(i32);
impl Drop for Registration {
    fn drop(&mut self) {
        registry().lock().unwrap().remove(&self.0);
    }
}
#[derive(Clone)]
pub struct Owned(Arc<Registration>);
impl Owned {
    pub fn pid(&self) -> i32 {
        self.0.0
    }
}
/// Hold the same gate as the orphan reaper across process creation and registration.
pub fn spawn<T>(create: impl FnOnce() -> io::Result<(T, i32)>) -> io::Result<(T, Owned)> {
    let mut owned = registry().lock().unwrap();
    let (child, pid) = create()?;
    owned.insert(pid);
    Ok((child, Owned(Arc::new(Registration(pid)))))
}
/// (state, parent, start ticks); comm may contain spaces and closing parentheses.
pub fn identity(stat: &str) -> Option<(char, i32, u64)> {
    let end = stat.rfind(')')?;
    let fields: Vec<_> = stat[end + 1..].split_whitespace().collect();
    Some((
        fields.first()?.chars().next()?,
        fields.get(1)?.parse().ok()?,
        fields.get(19)?.parse().ok()?,
    ))
}
pub fn scan(before: &BTreeMap<i32, u64>) -> BTreeMap<i32, u64> {
    let mut seen = BTreeMap::new();
    let Ok(entries) = std::fs::read_dir("/proc") else {
        return seen;
    };
    let parent = std::process::id() as i32;
    // Never race a new child's creation, even if it exits before registration.
    let owned = registry().lock().unwrap();
    for entry in entries.flatten() {
        let Some(pid) = entry
            .file_name()
            .to_str()
            .and_then(|v| v.parse::<i32>().ok())
        else {
            continue;
        };
        if owned.contains(&pid) {
            continue;
        }
        let Ok(stat) = std::fs::read_to_string(entry.path().join("stat")) else {
            continue;
        };
        let Some(('Z', ppid, start)) = identity(&stat) else {
            continue;
        };
        if ppid != parent {
            continue;
        }
        if before.get(&pid) == Some(&start) {
            // PID reuse cannot match an earlier zombie's start time.
            let mut status = 0;
            unsafe {
                libc::waitpid(pid, &mut status, libc::WNOHANG);
            }
        } else {
            seen.insert(pid, start);
        }
    }
    seen
}
pub async fn reap(cancel: CancellationToken, every: Duration) {
    let mut interval = tokio::time::interval_at(tokio::time::Instant::now() + every, every);
    let mut seen = BTreeMap::new();
    loop {
        tokio::select! {biased;_=cancel.cancelled()=>return,_=interval.tick()=>seen=scan(&seen)}
    }
}
