//! The filtered models.dev catalogue and atomic owner-local metadata cache.
use crate::{
    model::{Capabilities, Info},
    statefile, wire_struct,
};
use anyhow::{Context, Result, bail, ensure};
use serde::{Deserialize, Serialize};
use serde_json::value::RawValue;
use std::{
    collections::BTreeMap,
    io::Read,
    os::unix::fs::{DirBuilderExt, OpenOptionsExt},
    path::{Path, PathBuf},
    time::{Duration, SystemTime},
};
use tokio_util::sync::CancellationToken;
pub const URL: &str = "https://models.dev/api.json";
pub const TTL: Duration = Duration::from_secs(24 * 60 * 60);
const FETCH_TIMEOUT: Duration = Duration::from_secs(15);
const MAX_BYTES: usize = 64 << 20;
pub const FALLBACK: &[u8] = include_bytes!("modelsdev-fallback.json");
wire_struct!(RawLimit{context:"context":i64=>required,output:"output":i64=>required});
wire_struct!(RawCost{input:"input":f64=>float,output:"output":f64=>float,cache_read:"cache_read":f64=>float,cache_write:"cache_write":f64=>float});
wire_struct!(RawModel{id:"id":String=>required,name:"name":String=>required,limit:"limit":RawLimit=>required,cost:"cost":Option<RawCost> =>optional});
#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(default)]
struct RawProvider {
    models: Option<BTreeMap<String, Option<RawModel>>>,
}
#[derive(Clone)]
pub struct Catalog {
    providers: BTreeMap<String, RawProvider>,
}
impl Catalog {
    pub fn parse(data: &[u8], keep: &[&str]) -> Result<Self> {
        let raw: BTreeMap<String, Box<RawValue>> =
            crate::wire::decode(std::str::from_utf8(data).context("modelsdev: parse")?)
                .context("modelsdev: parse")?;
        let mut providers = BTreeMap::new();
        for (id, raw) in raw {
            if !keep.is_empty() && !keep.contains(&id.as_str()) {
                continue;
            }
            let mut provider: RawProvider =
                crate::wire::decode(raw.get()).with_context(|| format!("modelsdev: parse {id}"))?;
            if let Some(models) = &mut provider.models {
                for model in models.values_mut() {
                    if model.is_none() {
                        *model = Some(RawModel::default());
                    }
                }
            }
            providers.insert(id, provider);
        }
        ensure!(!providers.is_empty(), "modelsdev: parse: empty catalog");
        Ok(Self { providers })
    }
    pub fn encode(&self) -> Result<Vec<u8>> {
        Ok(serde_json::to_vec(&self.providers)?)
    }
    pub fn model(&self, provider: &str, id: &str) -> Option<Info> {
        let model = self
            .providers
            .get(provider)?
            .models
            .as_ref()?
            .get(id)?
            .as_ref()?;
        let mut info = Info {
            context_window: model.limit.context,
            max_output: model.limit.output,
            capabilities: Capabilities::default(),
            ..Default::default()
        };
        if let Some(cost) = &model.cost {
            info.input_price = cost.input;
            info.output_price = cost.output;
            info.cache_read_price = cost.cache_read;
            info.cache_write_price = cost.cache_write;
        }
        Some(info)
    }
    pub fn models(&self, provider: &str) -> Vec<String> {
        self.providers
            .get(provider)
            .and_then(|provider| provider.models.as_ref())
            .map(|models| models.keys().cloned().collect())
            .unwrap_or_default()
    }
    pub fn model_name(&self, provider: &str, id: &str) -> String {
        self.providers
            .get(provider)
            .and_then(|provider| provider.models.as_ref())
            .and_then(|models| models.get(id))
            .and_then(|model| model.as_ref())
            .map(|model| model.name.as_str())
            .filter(|name| !name.is_empty())
            .unwrap_or(id)
            .into()
    }
    pub fn covers(&self, keep: &[&str]) -> bool {
        keep.iter()
            .all(|provider| self.providers.contains_key(*provider))
    }
}
#[derive(Clone)]
pub struct Loader {
    pub url: String,
    pub cache: PathBuf,
    pub client: reqwest::Client,
}
impl Loader {
    pub fn new(cache: PathBuf) -> Result<Self> {
        Ok(Self {
            url: URL.into(),
            cache,
            client: crate::httpx::client(Some(FETCH_TIMEOUT))?,
        })
    }
    pub async fn load(&self, keep: &[&str], cancel: &CancellationToken) -> Result<(Catalog, bool)> {
        let mut incomplete = None;
        if let Some((data, fresh)) = read_cache(&self.cache)
            && let Ok(catalog) = Catalog::parse(&data, keep)
        {
            if catalog.covers(keep) {
                return Ok((catalog, !fresh));
            }
            incomplete = Some(catalog);
        }
        let error = match self.refresh(keep, cancel).await {
            Ok(catalog) => return Ok((catalog, false)),
            Err(error) => error,
        };
        if let Some(catalog) = incomplete {
            return Ok((catalog, true));
        }
        let catalog =
            Catalog::parse(FALLBACK, keep).with_context(|| format!("modelsdev: fetch: {error}"))?;
        Ok((catalog, true))
    }
    pub async fn refresh(&self, keep: &[&str], cancel: &CancellationToken) -> Result<Catalog> {
        let request = async {
            let mut response = self
                .client
                .get(&self.url)
                .header("Accept", "application/json")
                .header("User-Agent", "stavlos")
                .send()
                .await?;
            ensure!(
                response.status() == reqwest::StatusCode::OK,
                "GET {}: {}",
                self.url,
                response.status()
            );
            let mut raw = Vec::new();
            while raw.len() < MAX_BYTES {
                let Some(part) = response.chunk().await? else {
                    break;
                };
                let count = part.len().min(MAX_BYTES - raw.len());
                raw.extend_from_slice(&part[..count]);
            }
            Ok::<_, anyhow::Error>(raw)
        };
        let data = tokio::select! {biased;_=cancel.cancelled()=>bail!("operation cancelled"),data=tokio::time::timeout(FETCH_TIMEOUT,request)=>data.context("modelsdev: fetch timed out")??};
        let catalog = Catalog::parse(&data, keep)?;
        if let Ok(encoded) = catalog.encode() {
            write_cache(&self.cache, &encoded);
        }
        Ok(catalog)
    }
}
pub fn read_cache(path: &Path) -> Option<(Vec<u8>, bool)> {
    let file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
        .ok()?;
    let metadata = file.metadata().ok()?;
    if !metadata.is_file() || metadata.len() > MAX_BYTES as u64 {
        return None;
    }
    let fresh = SystemTime::now()
        .duration_since(metadata.modified().ok()?)
        .unwrap_or_default()
        < TTL;
    let mut bytes = Vec::new();
    file.take((MAX_BYTES + 1) as u64)
        .read_to_end(&mut bytes)
        .ok()?;
    if bytes.is_empty() || bytes.len() > MAX_BYTES {
        return None;
    }
    Some((bytes, fresh))
}
pub fn write_cache(path: &Path, data: &[u8]) {
    let parent = path.parent().unwrap_or(Path::new("."));
    if std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o755)
        .create(parent)
        .is_ok()
    {
        let _ = statefile::write_atomic(path, data, 0o600, false);
    }
}
