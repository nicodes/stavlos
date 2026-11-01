use crate::{protocol, wire::Slice};
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    fs::{File, OpenOptions},
    io::Read,
    os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
    sync::Mutex,
};
const MAX_FILE: u64 = 64 << 20;
#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct PromptMessage {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub permission: Option<protocol::PromptInfo>,
    #[serde(skip_serializing_if = "crate::wire::Empty::empty")]
    pub questions: Slice<protocol::Question>,
    #[serde(skip_serializing_if = "is_zero")]
    pub seq: i64,
    pub channel: String,
    pub message: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub webhook: String,
}
fn is_zero(value: &i64) -> bool {
    *value == 0
}
impl PromptMessage {
    pub fn with_prompt(mut self, prompt: &protocol::PromptInfo, seq: i64) -> Self {
        match prompt.kind.as_str() {
            protocol::PROMPT_QUESTION => {
                self.questions = if prompt.questions.is_empty() {
                    vec![protocol::Question {
                        question: prompt.question.clone(),
                        ..Default::default()
                    }]
                    .into()
                } else {
                    prompt.questions.clone()
                };
                self.seq = seq;
            }
            protocol::PROMPT_PERMISSION => {
                self.permission = Some(prompt.clone());
                self.seq = seq;
            }
            _ => {}
        }
        self
    }
    pub fn missing_prompt(&self, prompt: &protocol::PromptInfo) -> bool {
        prompt.kind == protocol::PROMPT_PERMISSION && self.permission.is_none()
            || prompt.kind == protocol::PROMPT_QUESTION && self.questions.is_empty()
    }
    pub fn is_question(&self) -> bool {
        self.permission.is_none() && (!self.webhook.is_empty() || !self.questions.is_empty())
    }
}
pub struct AtomicMap<T> {
    path: PathBuf,
    items: Mutex<BTreeMap<String, T>>,
}
impl<T: Serialize + DeserializeOwned + Clone> AtomicMap<T> {
    pub fn open(path: PathBuf) -> Result<Self> {
        let file = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
            .open(&path);
        let items = match file {
            Ok(file) => {
                ensure!(
                    file.metadata()?.is_file(),
                    "Discord state is not a regular file"
                );
                let mut raw = Vec::new();
                file.take(MAX_FILE + 1).read_to_end(&mut raw)?;
                ensure!(raw.len() as u64 <= MAX_FILE, "Discord state exceeds 64 MiB");
                serde_json::from_slice::<Option<BTreeMap<String, T>>>(&raw)?.unwrap_or_default()
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => BTreeMap::new(),
            Err(error) => return Err(error.into()),
        };
        Ok(Self {
            path,
            items: Mutex::new(items),
        })
    }
    pub fn snapshot(&self) -> BTreeMap<String, T> {
        self.items.lock().unwrap().clone()
    }
    pub fn get(&self, id: &str) -> Option<T> {
        self.items.lock().unwrap().get(id).cloned()
    }
    pub fn set(&self, id: &str, value: Option<T>) -> Result<()> {
        let mut items = self.items.lock().unwrap();
        let mut next = items.clone();
        match value {
            Some(value) => {
                next.insert(id.into(), value);
            }
            None => {
                next.remove(id);
            }
        }
        let raw = serde_json::to_vec(&next)?;
        ensure!(raw.len() as u64 <= MAX_FILE, "Discord state exceeds 64 MiB");
        if let Some(parent) = self.path.parent() {
            std::fs::DirBuilder::new()
                .recursive(true)
                .mode(0o700)
                .create(parent)?;
        }
        crate::statefile::write_atomic(&self.path, &raw, 0o600, false)?;
        *items = next;
        Ok(())
    }
}
pub struct PromptStore(pub AtomicMap<PromptMessage>);
impl PromptStore {
    pub fn open(path: PathBuf) -> Result<Self> {
        Ok(Self(AtomicMap::open(path)?))
    }
    pub fn snapshot(&self) -> BTreeMap<String, PromptMessage> {
        self.0.snapshot()
    }
    pub fn set(&self, id: &str, message: PromptMessage) -> Result<()> {
        self.0
            .set(id, (!message.message.is_empty()).then_some(message))
    }
    pub fn replay_from(&self, channel: &str, seq: i64) -> i64 {
        let mut from = seq.saturating_add(1);
        for message in self.snapshot().values() {
            if message.channel == channel && (message.is_question() || message.permission.is_some())
            {
                from = from.min(message.seq.saturating_add(1).max(1));
            }
        }
        from
    }
}
pub struct BoardCursors(pub AtomicMap<i64>);
impl BoardCursors {
    pub fn open(path: PathBuf) -> Result<Self> {
        Ok(Self(AtomicMap::open(path)?))
    }
    pub fn get(&self, id: &str) -> i64 {
        self.0.get(id).unwrap_or(0)
    }
    pub fn set(&self, id: &str, seq: i64) -> Result<()> {
        self.0.set(id, Some(seq))
    }
}
pub struct BridgeLock {
    pub prompts: PathBuf,
    pub boards: PathBuf,
    _file: File,
}
impl BridgeLock {
    pub fn acquire(data: &Path, guild: &str, category: &str) -> Result<Self> {
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(data)?;
        let key = format!(
            "{:x}",
            Sha256::digest(format!("{guild}/{category}").as_bytes())
        );
        let prompts = data.join(format!("discord-{}-prompts.json", &key[..16]));
        let lock = PathBuf::from(format!("{}.lock", prompts.display()));
        let file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
            .open(lock)?;
        ensure!(
            file.metadata()?.is_file(),
            "Discord bridge lock is not a regular file"
        );
        fs2::FileExt::try_lock_exclusive(&file).map_err(|_|anyhow::anyhow!("a Discord bridge is already running for this server/category (an old standalone stavlos-discord process or service? stop it first)"))?;
        file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
        let boards = PathBuf::from(format!("{}.boards", prompts.display()));
        Ok(Self {
            prompts,
            boards,
            _file: file,
        })
    }
}
