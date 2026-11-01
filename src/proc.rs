//! Owned command groups with a bounded output tail and detached lifetime.
use crate::{sandbox, sandbox::Spec};
use anyhow::{Context, Result};
use std::{
    ffi::{OsStr, OsString},
    fs::File,
    io::{self, Read},
    os::{
        fd::{AsRawFd, FromRawFd, OwnedFd},
        unix::process::ExitStatusExt,
    },
    path::Path,
    process::{Child, ExitStatus, Stdio},
    sync::{Arc, Condvar, Mutex},
    time::{Duration, Instant, SystemTime},
};

pub const OUTPUT_CAP: usize = 256 * 1024;
const DROPPED: &[u8] = "… [earlier output dropped] …\n".as_bytes();
pub type Sink = Arc<dyn Fn(&[u8]) + Send + Sync>;

#[derive(Default)]
struct TailState {
    bytes: Vec<u8>,
    lines: usize,
    sink: Option<Sink>,
}
#[derive(Default)]
pub struct Tail(Mutex<TailState>);
impl Tail {
    pub fn new(sink: Option<Sink>) -> Self {
        Self(Mutex::new(TailState {
            sink,
            ..TailState::default()
        }))
    }
    pub fn write(&self, bytes: &[u8]) {
        let sink = {
            let mut state = self.0.lock().unwrap();
            state.bytes.extend_from_slice(bytes);
            state.lines += bytes.iter().filter(|b| **b == b'\n').count();
            if state.bytes.len() > OUTPUT_CAP {
                let mut keep = Vec::with_capacity(DROPPED.len() + OUTPUT_CAP / 2);
                keep.extend_from_slice(DROPPED);
                keep.extend_from_slice(&state.bytes[state.bytes.len() - OUTPUT_CAP / 2..]);
                state.bytes = keep;
            }
            state.sink.clone()
        };
        if let Some(sink) = sink {
            sink(bytes);
        }
    }
    pub fn bytes(&self) -> Vec<u8> {
        self.0.lock().unwrap().bytes.clone()
    }
    pub fn output(&self) -> String {
        String::from_utf8_lossy(&self.bytes()).into_owned()
    }
    pub fn lines(&self) -> usize {
        self.0.lock().unwrap().lines
    }
    pub fn detach(&self) {
        self.0.lock().unwrap().sink = None;
    }
}

#[derive(Clone, Debug)]
pub struct Completion {
    pub status: Option<ExitStatus>,
    pub error: Option<String>,
}
impl Completion {
    pub fn success(&self) -> bool {
        self.error.is_none() && self.status.is_some_and(|status| status.success())
    }
    pub fn exit_code(&self) -> i32 {
        self.status
            .map(|s| s.code().unwrap_or(128 + s.signal().unwrap_or(0)))
            .unwrap_or(1)
    }
}
struct State {
    tail: Tail,
    completion: Mutex<Option<Completion>>,
    finished: Condvar,
    changed: tokio::sync::watch::Sender<bool>,
}
#[derive(Clone)]
pub struct Job {
    pid: i32,
    started: SystemTime,
    state: Arc<State>,
}
impl Job {
    /// The job outlives the initiating tool call and every dropped handle.
    pub fn start(
        command: &str,
        directory: &Path,
        environment: &[OsString],
        sink: Option<Sink>,
        spec: Option<&Spec>,
    ) -> Result<Self> {
        let level = if spec.is_some() {
            sandbox::probe().level
        } else {
            sandbox::Level::None
        };
        Self::start_with_helper(
            command,
            directory,
            environment,
            sink,
            spec,
            level,
            &std::env::current_exe()?,
        )
    }
    pub fn start_with_helper(
        command: &str,
        directory: &Path,
        environment: &[OsString],
        sink: Option<Sink>,
        spec: Option<&Spec>,
        level: sandbox::Level,
        helper: &Path,
    ) -> Result<Self> {
        let mut command = sandbox::command(
            OsStr::new("bash"),
            &["-c".into(), command.into()],
            environment,
            spec,
            level,
            helper,
        )?;
        command.current_dir(directory);
        // One shared kernel pipe retains stdout/stderr write ordering.
        let mut descriptors = [-1; 2];
        if unsafe { libc::pipe2(descriptors.as_mut_ptr(), libc::O_CLOEXEC) } < 0 {
            return Err(io::Error::last_os_error()).context("command output pipe");
        }
        let read = unsafe { OwnedFd::from_raw_fd(descriptors[0]) };
        let write = unsafe { OwnedFd::from_raw_fd(descriptors[1]) };
        command
            .stdin(Stdio::null())
            .stdout(Stdio::from(write.try_clone()?))
            .stderr(Stdio::from(write));
        let child = command.spawn().context("start command")?;
        drop(command);
        let pid = child.id() as i32;
        let started = SystemTime::now();
        let flags = unsafe { libc::fcntl(read.as_raw_fd(), libc::F_GETFL) };
        if flags < 0
            || unsafe { libc::fcntl(read.as_raw_fd(), libc::F_SETFL, flags | libc::O_NONBLOCK) } < 0
        {
            let error = io::Error::last_os_error();
            let mut child = child;
            unsafe {
                libc::kill(-pid, libc::SIGKILL);
            }
            let _ = child.wait();
            return Err(error).context("command output nonblocking mode");
        }
        let state = Arc::new(State {
            tail: Tail::new(sink),
            completion: Mutex::new(None),
            finished: Condvar::new(),
            changed: tokio::sync::watch::channel(false).0,
        });
        // Keep ownership recoverable if the OS refuses the supervising thread.
        let child = Arc::new(Mutex::new(Some(child)));
        let child_worker = child.clone();
        let worker_state = state.clone();
        if let Err(error) = std::thread::Builder::new()
            .name(format!("stavlos-job-{pid}"))
            .spawn(move || {
                let child = child_worker.lock().unwrap().take().unwrap();
                let completion = supervise(child, File::from(read), &worker_state.tail);
                *worker_state.completion.lock().unwrap() = Some(completion);
                worker_state.finished.notify_all();
                worker_state.changed.send_replace(true);
            })
        {
            if let Some(mut child) = child.lock().unwrap().take() {
                unsafe {
                    libc::kill(-pid, libc::SIGKILL);
                }
                let _ = child.wait();
            }
            return Err(error).context("command supervisor");
        }
        Ok(Self {
            pid,
            started,
            state,
        })
    }
    pub fn pid(&self) -> i32 {
        self.pid
    }
    pub fn started(&self) -> SystemTime {
        self.started
    }
    pub fn output(&self) -> String {
        self.state.tail.output()
    }
    pub fn output_bytes(&self) -> Vec<u8> {
        self.state.tail.bytes()
    }
    pub fn lines(&self) -> usize {
        self.state.tail.lines()
    }
    pub fn detach(&self) {
        self.state.tail.detach();
    }
    pub fn completion(&self) -> Option<Completion> {
        self.state.completion.lock().unwrap().clone()
    }
    pub fn is_done(&self) -> bool {
        self.completion().is_some()
    }
    pub fn wait(&self) -> Completion {
        let mut completion = self.state.completion.lock().unwrap();
        while completion.is_none() {
            completion = self.state.finished.wait(completion).unwrap();
        }
        completion.clone().unwrap()
    }
    pub fn wait_timeout(&self, timeout: Duration) -> Option<Completion> {
        let completion = self.state.completion.lock().unwrap();
        let (completion, _) = self
            .state
            .finished
            .wait_timeout_while(completion, timeout, |value| value.is_none())
            .unwrap();
        completion.clone()
    }
    pub async fn wait_async(&self) -> Completion {
        let mut changed = self.state.changed.subscribe();
        loop {
            if let Some(completion) = self.completion() {
                return completion;
            }
            if changed.changed().await.is_err() {
                return self.wait();
            }
        }
    }
    pub fn kill(&self) {
        unsafe {
            libc::kill(-self.pid, libc::SIGKILL);
        }
    }
}
fn supervise(mut child: Child, mut pipe: File, tail: &Tail) -> Completion {
    let mut status = None;
    let mut deadline = None;
    let mut error = None;
    let mut eof = false;
    let mut buffer = [0u8; 32768];
    loop {
        if status.is_none() {
            match child.try_wait() {
                Ok(Some(exited)) => {
                    status = Some(exited);
                    deadline = Some(Instant::now() + Duration::from_secs(2));
                }
                Ok(None) => {}
                Err(failed) => {
                    error = Some(failed.to_string());
                    break;
                }
            }
        }
        if !eof {
            // Limit each drain so an endlessly writing descendant cannot postpone the deadline.
            for _ in 0..32 {
                match pipe.read(&mut buffer) {
                    Ok(0) => {
                        eof = true;
                        break;
                    }
                    Ok(count) => tail.write(&buffer[..count]),
                    Err(failed) if failed.kind() == io::ErrorKind::Interrupted => continue,
                    Err(failed) if failed.kind() == io::ErrorKind::WouldBlock => break,
                    Err(failed) => {
                        error = Some(failed.to_string());
                        eof = true;
                        break;
                    }
                }
            }
        }
        if eof && status.is_some() {
            break;
        }
        if deadline.is_some_and(|deadline| Instant::now() >= deadline) {
            error.get_or_insert_with(|| {
                "command exited but its output pipes did not close within 2s".into()
            });
            break;
        }
        if eof {
            // Output is closed; only the owned leader remains to reap.
            match child.wait() {
                Ok(exited) => status = Some(exited),
                Err(failed) => error = Some(failed.to_string()),
            }
            break;
        }
        let mut descriptor = libc::pollfd {
            fd: pipe.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        };
        if unsafe { libc::poll(&mut descriptor, 1, 25) } < 0 {
            let failed = io::Error::last_os_error();
            if failed.kind() != io::ErrorKind::Interrupted {
                error = Some(failed.to_string());
                break;
            }
        }
    }
    if status.is_none() {
        // Unexpected I/O/wait errors never strand an owned command group.
        unsafe {
            libc::kill(-(child.id() as i32), libc::SIGKILL);
        }
        match child.wait() {
            Ok(exited) => status = Some(exited),
            Err(failed) => {
                error.get_or_insert_with(|| failed.to_string());
            }
        }
    }
    Completion { status, error }
}
