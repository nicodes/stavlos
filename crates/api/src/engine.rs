use serde_json::{Value, json};
use std::{
    collections::HashMap,
    fs::{self, File, OpenOptions},
    io::Write,
    os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt},
    path::PathBuf,
    sync::{Arc, Mutex, RwLock},
    time::{Duration, Instant},
};
use tokio::sync::{Semaphore, broadcast, mpsc, oneshot};
#[derive(Clone)]
pub struct Facts {
    pub value: Arc<Value>,
    at: Instant,
}
pub struct Engine {
    facts: RwLock<Option<Facts>>,
    pub notifications: broadcast::Sender<Value>,
    output: mpsc::Sender<Value>,
    pending: Mutex<HashMap<String, oneshot::Sender<Result<(), String>>>>,
    slots: Arc<Semaphore>,
    inbox: PathBuf,
    files: Mutex<()>,
}
impl Engine {
    pub fn new(output: mpsc::Sender<Value>, data: PathBuf) -> Arc<Self> {
        let (notifications, _) = broadcast::channel(64);
        Arc::new(Self {
            facts: RwLock::new(None),
            notifications,
            output,
            pending: Mutex::new(HashMap::new()),
            slots: Arc::new(Semaphore::new(16)),
            inbox: data.join("web-inputs"),
            files: Mutex::new(()),
        })
    }
    pub fn update(&self, value: Value) {
        *self.facts.write().unwrap() = Some(Facts {
            value: Arc::new(value),
            at: Instant::now(),
        });
    }
    pub fn facts(&self) -> Result<Arc<Value>, String> {
        let facts = self.facts.read().unwrap();
        let facts = facts
            .as_ref()
            .ok_or("agent runtime has not published its state")?;
        if facts.at.elapsed() > Duration::from_secs(5) {
            return Err("agent runtime state is stale".into());
        }
        Ok(facts.value.clone())
    }
    pub fn result(&self, id: &str, error: Option<&str>) {
        if let Some(sender) = self.pending.lock().unwrap().remove(id) {
            let _ = sender.send(
                error
                    .filter(|s| !s.is_empty())
                    .map_or(Ok(()), |e| Err(e.to_owned())),
            );
        }
    }
    pub fn notify(&self, value: Value) {
        if value.to_string().len() <= 1 << 16 {
            let _ = self.notifications.send(value);
        }
    }
    fn persist(&self, id: &str, raw: &[u8]) -> Result<PathBuf, String> {
        let _lock = self.files.lock().unwrap();
        match fs::create_dir(&self.inbox) {
            Ok(()) => fs::set_permissions(&self.inbox, fs::Permissions::from_mode(0o700))
                .map_err(|_| "cannot secure input journal")?,
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {}
            Err(_) => return Err("input journal is unavailable".into()),
        };
        let metadata =
            fs::symlink_metadata(&self.inbox).map_err(|_| "input journal is unavailable")?;
        if !metadata.is_dir()
            || metadata.uid() != unsafe { libc::geteuid() }
            || metadata.mode() & 0o077 != 0
        {
            return Err("input journal is not private".into());
        }
        let mut count = 0;
        let mut size = 0u64;
        for entry in fs::read_dir(&self.inbox).map_err(|_| "input journal is unavailable")? {
            let entry = entry.map_err(|_| "input journal is unavailable")?;
            let meta = entry
                .path()
                .symlink_metadata()
                .map_err(|_| "input journal is unavailable")?;
            count += 1;
            size = size.saturating_add(meta.len());
            if !meta.is_file() || count >= 128 || size + raw.len() as u64 > 64 << 20 {
                return Err(
                    "input journal needs reconciliation before more messages can be accepted"
                        .into(),
                );
            }
        }
        let path = self.inbox.join(id);
        let temporary = self.inbox.join(format!(".tmp-{}", uuid::Uuid::new_v4()));
        let result = (|| {
            let mut file = OpenOptions::new()
                .write(true)
                .create_new(true)
                .mode(0o600)
                .custom_flags(libc::O_NOFOLLOW)
                .open(&temporary)?;
            file.write_all(raw)?;
            file.sync_all()?;
            fs::rename(&temporary, &path)?;
            File::open(&self.inbox)?.sync_all()?;
            Ok::<_, std::io::Error>(())
        })();
        if result.is_err() {
            let _ = fs::remove_file(&temporary);
            return Err("could not durably record input; inspect history before retrying".into());
        }
        Ok(path)
    }
    pub async fn deliver(self: &Arc<Self>, mut plan: Value) -> Result<(), String> {
        let permit = self
            .slots
            .clone()
            .try_acquire_owned()
            .map_err(|_| "agent input queue is full")?;
        let id = format!("post{}", uuid::Uuid::new_v4().simple());
        plan["id"] = id.clone().into();
        plan["request_id"] = if plan["board"] == true {
            if plan["message_kind"] == "request" {
                format!("r{}", uuid::Uuid::new_v4().simple()).into()
            } else {
                "".into()
            }
        } else {
            id.clone().into()
        };
        plan["kind"] = "delivery".into();
        let raw = serde_json::to_vec(&plan).map_err(|_| "could not record message")?;
        if raw.len() > 4 << 20 {
            return Err("message is too large".into());
        }
        let owner = self.clone();
        let (task_sender, task_receiver) = oneshot::channel();
        // The operation is owned after admission, even if its WebSocket disconnects.
        // An unacknowledged journal entry is retained and never replayed automatically.
        tokio::spawn(async move {
            let _permit = permit;
            let file = {
                let owner = owner.clone();
                let id = id.clone();
                tokio::task::spawn_blocking(move || owner.persist(&id, &raw)).await
            };
            let path = match file {
                Ok(Ok(path)) => path,
                Ok(Err(error)) => {
                    let _ = task_sender.send(Err(error));
                    return;
                }
                Err(_) => {
                    let _ = task_sender.send(Err("input journal failed".into()));
                    return;
                }
            };
            let (sender, receiver) = oneshot::channel();
            owner.pending.lock().unwrap().insert(id.clone(), sender);
            let result = if owner.output.try_send(plan).is_err() {
                owner.pending.lock().unwrap().remove(&id);
                let _ = fs::remove_file(&path);
                Err("agent runtime input queue is full".into())
            } else {
                match tokio::time::timeout(Duration::from_secs(30), receiver).await {
                    Ok(Ok(result)) => result,
                    _ => Err(
                        "message acknowledgement was lost; inspect channel history before retrying"
                            .into(),
                    ),
                }
            };
            owner.pending.lock().unwrap().remove(&id);
            if result.is_ok() {
                let _ = fs::remove_file(path);
                if let Ok(directory) = File::open(&owner.inbox) {
                    let _ = directory.sync_all();
                }
            }
            let _ = task_sender.send(result);
        });
        task_receiver
            .await
            .map_err(|_| "agent input operation ended without acknowledgement".to_string())?
    }
    pub fn ready_message() -> Value {
        json!({"kind":"ready","protocol":1})
    }
}
