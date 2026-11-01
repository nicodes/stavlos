//! Supervise the independent Rust API over private inherited pipes.
use anyhow::{Context, Result, bail, ensure};
use serde::{Deserialize, Serialize};
use std::{
    ffi::OsString,
    os::unix::{fs::PermissionsExt, process::CommandExt},
    path::{Path, PathBuf},
    process::Stdio,
    time::Duration,
};
use tokio::{
    io::{AsyncBufRead, AsyncBufReadExt, AsyncWriteExt, BufReader},
    process::Command,
    sync::{oneshot, watch},
};
use tokio_util::sync::CancellationToken;

pub const HELPER_ARG0: &str = "stavlos-api";
#[derive(Clone, Debug)]
pub struct Program {
    path: PathBuf,
    bundled: bool,
}
impl Program {
    pub fn external(path: PathBuf) -> Result<Self> {
        ensure!(path.is_absolute(), "STAVLOS_API_BINARY must be absolute");
        let available = std::fs::metadata(&path)
            .is_ok_and(|metadata| metadata.is_file() && metadata.permissions().mode() & 0o111 != 0);
        ensure!(
            available,
            "native Rust API is unavailable; use a release CLI, run mise run build, or set STAVLOS_API_BINARY to the built api executable"
        );
        Ok(Self {
            path,
            bundled: false,
        })
    }
    pub fn source() -> Result<Self> {
        let path =
            if let Some(path) = std::env::var_os("STAVLOS_API_BINARY").filter(|s| !s.is_empty()) {
                path.into()
            } else {
                std::env::current_exe()?
                    .parent()
                    .unwrap_or(Path::new("."))
                    .join(HELPER_ARG0)
            };
        Self::external(path)
    }
    /// The CLI dispatches this argv[0] before starting its owner runtime.
    pub fn bundled(path: PathBuf) -> Result<Self> {
        let mut program = Self::external(path)?;
        program.bundled = true;
        Ok(program)
    }
    pub fn path(&self) -> &Path {
        &self.path
    }
    pub fn command(&self) -> Command {
        let mut command = Command::new(&self.path);
        if self.bundled {
            command.as_std_mut().arg0(HELPER_ARG0);
        }
        command.kill_on_drop(true);
        command
    }
}
#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct CallbackResult {
    pub code: String,
    pub error: String,
}
impl std::fmt::Debug for CallbackResult {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CallbackResult")
            .field("error", &self.error)
            .finish_non_exhaustive()
    }
}
pub struct Callback {
    result: watch::Receiver<Option<CallbackResult>>,
    done: watch::Receiver<bool>,
    stop: CancellationToken,
}
impl Drop for Callback {
    fn drop(&mut self) {
        self.stop.cancel();
    }
}
pub(crate) async fn line(
    reader: &mut (impl AsyncBufRead + Unpin),
    limit: usize,
) -> std::io::Result<Option<Vec<u8>>> {
    let mut bytes = Vec::new();
    loop {
        let available = reader.fill_buf().await?;
        if available.is_empty() {
            return Ok(if bytes.is_empty() { None } else { Some(bytes) });
        }
        let end = available.iter().position(|b| *b == b'\n');
        let count = end.map_or(available.len(), |index| index + 1);
        if bytes.len() + count > limit {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "native API control frame exceeds its bound",
            ));
        }
        bytes.extend_from_slice(&available[..count]);
        reader.consume(count);
        if end.is_some() {
            return Ok(Some(bytes));
        }
    }
}
async fn stop_child(child: &mut tokio::process::Child) {
    if let Some(pid) = child.id() {
        unsafe {
            libc::kill(pid as i32, libc::SIGINT);
        }
    }
    if tokio::time::timeout(Duration::from_secs(4), child.wait())
        .await
        .is_err()
    {
        let _ = child.kill().await;
    }
}
impl Callback {
    pub async fn start(
        program: &Program,
        port: u16,
        state: &str,
        cancel: &CancellationToken,
    ) -> Result<Self> {
        let mut command = program.command();
        command.args([
            OsString::from("oauth-callback"),
            "--port".into(),
            port.to_string().into(),
            "--parent-pid".into(),
            std::process::id().to_string().into(),
        ]);
        command
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit());
        let mut child = command
            .spawn()
            .context("native OAuth callback did not start")?;
        let mut input = child
            .stdin
            .take()
            .context("native OAuth callback input is unavailable")?;
        let output = child
            .stdout
            .take()
            .context("native OAuth callback output is unavailable")?;
        let mut setup = serde_json::to_vec(&serde_json::json!({"state":state}))?;
        setup.push(b'\n');
        tokio::select! {biased;
            _=cancel.cancelled()=>{child.kill().await?;bail!("operation cancelled")},
            written=tokio::time::timeout(Duration::from_secs(5),input.write_all(&setup))=>{written.context("native OAuth callback setup expired")??;}
        }
        input.shutdown().await?;
        drop(input);
        let (ready, readiness) = oneshot::channel::<std::result::Result<(), String>>();
        let (result, result_rx) = watch::channel(None);
        let (done, done_rx) = watch::channel(false);
        let stop = CancellationToken::new();
        let stopping = stop.clone();
        tokio::spawn(async move {
            let mut output = BufReader::new(output);
            let first = tokio::select! {biased;_=stopping.cancelled()=>Err(anyhow::anyhow!("native OAuth callback cancelled")),first=tokio::time::timeout(Duration::from_secs(5),line(&mut output,32<<10))=>match first{Ok(Ok(Some(raw)))=>serde_json::from_slice::<serde_json::Value>(&raw).map_err(anyhow::Error::from),_=>Err(anyhow::anyhow!("native OAuth callback did not start"))}};
            let accepted = first.is_ok_and(|value| value["ready"] == true);
            if !accepted {
                let _ = ready.send(Err(
                    "native OAuth callback returned invalid readiness".into()
                ));
                stop_child(&mut child).await;
                done.send_replace(true);
                return;
            }
            let _ = ready.send(Ok(()));
            let received = tokio::select! {biased;
                _=stopping.cancelled()=>CallbackResult{error:"login abandoned".into(),..Default::default()},
                raw=line(&mut output,32<<10)=>match raw{
                    Ok(Some(raw))=>serde_json::from_slice(&raw).unwrap_or_else(|_|CallbackResult{error:"native OAuth callback ended without a result".into(),..Default::default()}),
                    _=>CallbackResult{error:"native OAuth callback ended without a result".into(),..Default::default()},
                }
            };
            result.send_replace(Some(received));
            tokio::select! {biased;_=stopping.cancelled()=>stop_child(&mut child).await,_=child.wait()=>{}}
            done.send_replace(true);
        });
        let callback = Self {
            result: result_rx,
            done: done_rx,
            stop,
        };
        tokio::select! {biased;
            _=cancel.cancelled()=>{callback.close().await;bail!("operation cancelled")},
            ready=readiness=>match ready{Ok(Ok(()))=>Ok(callback),Ok(Err(error))=>{callback.close().await;Err(anyhow::anyhow!(error))},Err(_)=>{callback.close().await;bail!("native OAuth callback did not become ready")}}
        }
    }
    pub fn cancel(&self) {
        self.stop.cancel();
    }
    pub fn cancellation(&self) -> CancellationToken {
        self.stop.clone()
    }
    pub async fn result(&self) -> CallbackResult {
        let mut result = self.result.clone();
        loop {
            if let Some(value) = result.borrow().clone() {
                return value;
            }
            if result.changed().await.is_err() {
                return CallbackResult {
                    error: "native OAuth callback ended without a result".into(),
                    ..Default::default()
                };
            }
        }
    }
    pub async fn close(&self) {
        self.stop.cancel();
        let mut done = self.done.clone();
        while !*done.borrow() {
            if done.changed().await.is_err() {
                break;
            }
        }
    }
}
