//! Bounded private-pipe supervision for the existing Axum browser API.
use super::{Program, line, stop_child};
use crate::{protocol, tools::ServiceFuture};
use anyhow::{Result, ensure};
use serde_json::{Value, json};
use std::{
    collections::BTreeMap,
    path::Path,
    process::Stdio,
    sync::{
        Arc, Mutex,
        atomic::{AtomicU64, Ordering},
    },
    time::Duration,
};
use tokio::{
    io::{AsyncWriteExt, BufReader},
    sync::{mpsc, oneshot, watch},
    task::JoinSet,
};
use tokio_util::sync::CancellationToken;
const FRAME: usize = 16 << 20;
pub type Snapshot = Arc<dyn Fn() -> Result<Value> + Send + Sync>;
pub type Deliver =
    Arc<dyn Fn(Value, CancellationToken) -> ServiceFuture<'static, ()> + Send + Sync>;
struct Shared {
    output: mpsc::Sender<Arc<[u8]>>,
    pending: Mutex<BTreeMap<u64, oneshot::Sender<Value>>>,
    next: AtomicU64,
    status: Mutex<protocol::WebStatus>,
    stop: CancellationToken,
}
pub struct WebProcess {
    shared: Arc<Shared>,
    done: watch::Receiver<bool>,
    pid: i32,
}
fn frame(value: &Value) -> Result<Arc<[u8]>> {
    let mut raw = serde_json::to_vec(value)?;
    ensure!(
        raw.len() < FRAME,
        "native API control frame exceeds its bound"
    );
    raw.push(b'\n');
    Ok(raw.into())
}
impl Shared {
    async fn send(&self, value: &Value) -> Result<()> {
        let raw = frame(value)?;
        tokio::select! {biased;_=self.stop.cancelled()=>anyhow::bail!("native API stopped"),sent=tokio::time::timeout(Duration::from_secs(5),self.output.send(raw))=>{sent.map_err(|_|anyhow::anyhow!("native API control queue is full"))?.map_err(|_|anyhow::anyhow!("native API stopped"))?;Ok(())}}
    }
}
impl WebProcess {
    pub async fn start(
        program: &Program,
        data: &Path,
        port: u16,
        hosts: &[String],
        snapshot: Snapshot,
        deliver: Deliver,
        cancel: &CancellationToken,
    ) -> Result<Self> {
        ensure!(port > 0, "web port must be 1–65535");
        let mut command = program.command();
        command.args([
            "--data-dir".into(),
            data.as_os_str().to_owned(),
            "--port".into(),
            port.to_string().into(),
            "--hosts".into(),
            hosts.join(",").into(),
            "--parent-pid".into(),
            std::process::id().to_string().into(),
        ]);
        command
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit());
        let (mut child, ownership) = crate::ownedchild::spawn(|| {
            let child = command.spawn()?;
            let pid = child.id().expect("spawned API pid") as i32;
            Ok((child, pid))
        })?;
        let pid = ownership.pid();
        let mut input = child.stdin.take().expect("piped input");
        let output = child.stdout.take().expect("piped output");
        let (sending, mut receiving) = mpsc::channel::<Arc<[u8]>>(16);
        let (facts, mut updated) = watch::channel::<Option<Arc<[u8]>>>(None);
        let shared = Arc::new(Shared {
            output: sending,
            pending: Mutex::default(),
            next: AtomicU64::new(0),
            status: Mutex::new(protocol::WebStatus {
                url: format!("http://127.0.0.1:{port}/"),
                ..Default::default()
            }),
            stop: cancel.child_token(),
        });
        let (ready, readiness) = oneshot::channel();
        let (done, finished) = watch::channel(false);
        let running = shared.clone();
        tokio::spawn(async move {
            let _ownership = ownership;
            let writing = running.clone();
            let writer = tokio::spawn(async move {
                loop {
                    let raw = tokio::select! {biased;_=writing.stop.cancelled()=>break,raw=receiving.recv()=>match raw {Some(raw)=>raw,None=>break},result=updated.changed()=>{if result.is_err(){break;}let latest=updated.borrow_and_update().clone();let Some(raw)=latest else {continue};raw}};
                    if !matches!(
                        tokio::time::timeout(Duration::from_secs(5), input.write_all(&raw)).await,
                        Ok(Ok(()))
                    ) {
                        break;
                    }
                }
                writing.stop.cancel();
                let _ = input.shutdown().await;
            });
            let mut output = BufReader::new(output);
            let started = async {
                let raw = line(&mut output, FRAME)
                    .await?
                    .ok_or_else(|| anyhow::anyhow!("native API exited before readiness"))?;
                let value: Value = serde_json::from_slice(&raw)?;
                ensure!(
                    value["kind"] == "ready",
                    "native API returned invalid readiness"
                );
                running
                    .send(&json!({"kind":"snapshot","facts":snapshot()?}))
                    .await
            };
            let accepted = tokio::select! {biased;_=running.stop.cancelled()=>Err(anyhow::anyhow!("native API start cancelled")),result=tokio::time::timeout(Duration::from_secs(5),started)=>result.map_err(|_|anyhow::anyhow!("native API did not become ready")).and_then(|r|r)};
            let ready_ok = accepted.is_ok();
            let _ = ready.send(accepted.map_err(|e| e.to_string()));
            let mut workers = JoinSet::new();
            let mut snapshots = None;
            if ready_ok {
                let refreshing = running.clone();
                snapshots = Some(tokio::spawn(async move {
                    let mut tick = tokio::time::interval(Duration::from_millis(100));
                    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
                    loop {
                        tokio::select! {biased;_=refreshing.stop.cancelled()=>break,_=tick.tick()=>{
                            let latest=snapshot().and_then(|facts|frame(&json!({"kind":"snapshot","facts":facts})));
                            match latest {Ok(raw)=>{facts.send_replace(Some(raw));},Err(_)=>{refreshing.stop.cancel();break;}}
                        }}
                    }
                }));
                loop {
                    let raw = tokio::select! {biased;_=running.stop.cancelled()=>break,Some(_)=workers.join_next(),if !workers.is_empty()=>continue,raw=line(&mut output,FRAME)=>raw};
                    let Ok(Some(raw)) = raw else { break };
                    let Ok(value) = serde_json::from_slice::<Value>(&raw) else {
                        break;
                    };
                    match value["kind"].as_str() {
                        Some("control_result") => {
                            let Some(id) = value["id"].as_u64() else {
                                break;
                            };
                            let Ok(status) = serde_json::from_value::<protocol::WebStatus>(
                                value["status"].clone(),
                            ) else {
                                break;
                            };
                            *running.status.lock().unwrap() = status;
                            if let Some(sender) = running.pending.lock().unwrap().remove(&id) {
                                let _ = sender.send(value);
                            }
                        }
                        Some("delivery") if raw.len() <= 4 << 20 => {
                            let Some(id) = value["id"].as_str().map(String::from) else {
                                break;
                            };
                            if workers.len() >= 16 {
                                if running.send(&json!({"kind":"delivery_result","id":id,"error":"agent input queue is full"})).await.is_err() {break;}
                                continue;
                            }
                            let serving = running.clone();
                            let deliver = deliver.clone();
                            workers.spawn(async move {
                                let result = deliver(value, serving.stop.child_token()).await;
                                let detail =
                                    result.err().map(|e| format!("{e:#}")).unwrap_or_default();
                                if serving
                                    .send(&json!({"kind":"delivery_result","id":id,"error":detail}))
                                    .await
                                    .is_err()
                                {
                                    serving.stop.cancel();
                                }
                            });
                        }
                        _ => break,
                    }
                }
            }
            running.stop.cancel();
            running.pending.lock().unwrap().clear();
            workers.abort_all();
            while workers.join_next().await.is_some() {}
            if let Some(snapshots) = snapshots {
                let _ = snapshots.await;
            }
            let _ = writer.await;
            stop_child(&mut child).await;
            running.status.lock().unwrap().enabled = false;
            done.send_replace(true);
        });
        let process = Self {
            shared,
            done: finished,
            pid,
        };
        let accepted = tokio::select! {biased;_=cancel.cancelled()=>Err(anyhow::anyhow!("native API start cancelled")),ready=readiness=>ready.map_err(|_|anyhow::anyhow!("native API start abandoned")).and_then(|r|r.map_err(anyhow::Error::msg))};
        if let Err(error) = accepted {
            process.close().await;
            return Err(error);
        }
        Ok(process)
    }
    pub fn pid(&self) -> i32 {
        self.pid
    }
    pub fn alive(&self) -> bool {
        !*self.done.borrow() && !self.shared.stop.is_cancelled()
    }
    pub fn status(&self) -> protocol::WebStatus {
        let mut status = self.shared.status.lock().unwrap().clone();
        if !self.alive() {
            status.enabled = false;
        }
        status
    }
    pub async fn call(
        &self,
        operation: &str,
        cancel: &CancellationToken,
    ) -> Result<protocol::WebStatus> {
        ensure!(self.alive(), "native API is unavailable");
        let id = self.shared.next.fetch_add(1, Ordering::Relaxed) + 1;
        let (sender, receiver) = oneshot::channel();
        self.shared.pending.lock().unwrap().insert(id, sender);
        let _pending = Pending {
            shared: self.shared.clone(),
            id,
        };
        self.shared
            .send(&json!({"kind":"control","id":id,"op":operation}))
            .await?;
        let response = tokio::select! {biased;_=cancel.cancelled()=>anyhow::bail!("context canceled"),_=self.shared.stop.cancelled()=>anyhow::bail!("native API stopped"),received=tokio::time::timeout(Duration::from_secs(5),receiver)=>received.map_err(|_|anyhow::anyhow!("native API control timed out"))?.map_err(|_|anyhow::anyhow!("native API stopped"))?};
        let error = response["error"].as_str().unwrap_or("");
        ensure!(error.is_empty(), "{error}");
        let mut status: protocol::WebStatus = serde_json::from_value(response["status"].clone())?;
        status.open_url = response["open_url"].as_str().unwrap_or("").into();
        Ok(status)
    }
    pub fn notify(&self, value: Value) {
        if !self.alive() {
            return;
        }
        let Ok(raw) = serde_json::to_vec(&value) else {
            return;
        };
        if raw.len() > 1 << 16 {
            return;
        }
        if let Ok(raw) = frame(&json!({"kind":"notification","value":value})) {
            let _ = self.shared.output.try_send(raw);
        }
    }
    pub async fn close(&self) {
        self.shared.stop.cancel();
        let mut finished = self.done.clone();
        while !*finished.borrow_and_update() {
            if finished.changed().await.is_err() {
                break;
            }
        }
    }
}
impl Drop for WebProcess {
    fn drop(&mut self) {
        self.shared.stop.cancel();
    }
}
struct Pending {
    shared: Arc<Shared>,
    id: u64,
}
impl Drop for Pending {
    fn drop(&mut self) {
        self.shared.pending.lock().unwrap().remove(&self.id);
    }
}
