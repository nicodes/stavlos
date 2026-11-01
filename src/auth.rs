//! Owner-local subscription credentials. Secrets never appear in Debug output.
use crate::statefile;
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    io::Read,
    os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
    sync::Mutex,
    time::SystemTime,
};
#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Credential {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub added: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub access: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub refresh: String,
    #[serde(skip_serializing_if = "is_zero")]
    pub expires: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub account_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub email: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub key: String,
}
fn is_zero(n: &i64) -> bool {
    *n == 0
}
impl std::fmt::Debug for Credential {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Credential")
            .field("type", &self.kind)
            .finish_non_exhaustive()
    }
}
impl Credential {
    pub fn secret(&self) -> &str {
        if self.kind == "apikey" {
            &self.key
        } else {
            &self.access
        }
    }
}
#[derive(Default)]
struct Cache {
    values: BTreeMap<String, Credential>,
    stamp: Option<(SystemTime, u64)>,
    loaded: bool,
}
pub struct Store {
    path: PathBuf,
    cache: Mutex<Cache>,
}
fn norm(p: &str) -> &str {
    p.trim().trim_end_matches('/')
}
impl Store {
    pub fn open(path: PathBuf) -> Self {
        Self {
            path,
            cache: Mutex::new(Cache::default()),
        }
    }
    pub fn path(&self) -> &Path {
        &self.path
    }
    fn load(&self, cache: &mut Cache) -> Result<()> {
        let file = match std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK)
            .open(&self.path)
        {
            Ok(f) => f,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                *cache = Cache {
                    loaded: true,
                    ..Cache::default()
                };
                return Ok(());
            }
            Err(_) => anyhow::bail!("cannot read credential store"),
        };
        let metadata = file.metadata()?;
        ensure!(
            metadata.is_file() && metadata.len() <= 16 << 20,
            "invalid credential store"
        );
        if metadata.permissions().mode() & 0o077 != 0 {
            file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
        }
        let stamp = Some((metadata.modified()?, metadata.len()));
        if cache.loaded && cache.stamp == stamp {
            return Ok(());
        }
        let mut bytes = Vec::new();
        file.take((16 << 20) + 1).read_to_end(&mut bytes)?;
        ensure!(bytes.len() <= 16 << 20, "credential store exceeds limit");
        let values = if bytes.iter().all(|b| b.is_ascii_whitespace()) {
            BTreeMap::new()
        } else {
            serde_json::from_slice(&bytes)
                .map_err(|_| anyhow::anyhow!("invalid credential store"))?
        };
        *cache = Cache {
            values,
            stamp,
            loaded: true,
        };
        Ok(())
    }
    fn save(&self, cache: &mut Cache, values: BTreeMap<String, Credential>) -> Result<()> {
        let dir = self
            .path
            .parent()
            .filter(|p| !p.as_os_str().is_empty())
            .unwrap_or(Path::new("."));
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(dir)?;
        let mut bytes = serde_json::to_vec_pretty(&values)?;
        bytes.push(b'\n');
        if let Err(e) = statefile::write_atomic(&self.path, &bytes, 0o600, false) {
            cache.loaded = false;
            return Err(e.into());
        }
        cache.loaded = false;
        self.load(cache)
    }
    pub fn all(&self) -> Result<BTreeMap<String, Credential>> {
        let mut cache = self
            .cache
            .lock()
            .map_err(|_| anyhow::anyhow!("credential store lock failed"))?;
        self.load(&mut cache)?;
        Ok(cache.values.clone())
    }
    pub fn get(&self, provider: &str) -> Option<Credential> {
        self.all().ok()?.remove(norm(provider))
    }
    pub fn set(&self, provider: &str, mut credential: Credential) -> Result<()> {
        let mut cache = self
            .cache
            .lock()
            .map_err(|_| anyhow::anyhow!("credential store lock failed"))?;
        self.load(&mut cache)?;
        if credential.kind.is_empty() {
            credential.kind = "oauth".into()
        }
        if credential.added.is_empty() {
            credential.added = chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true)
        }
        let mut values = cache.values.clone();
        values.insert(norm(provider).into(), credential);
        self.save(&mut cache, values)
    }
    pub fn remove(&self, provider: &str) -> Result<()> {
        let mut cache = self
            .cache
            .lock()
            .map_err(|_| anyhow::anyhow!("credential store lock failed"))?;
        self.load(&mut cache)?;
        let mut values = cache.values.clone();
        values.remove(norm(provider));
        self.save(&mut cache, values)
    }
    pub fn providers(&self) -> Vec<String> {
        self.all().unwrap_or_default().into_keys().collect()
    }
}
