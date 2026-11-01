//! Subscription and explicitly registered providers, rotating credentials and plan observations.
use super::{
    CallError, CompleteFuture, DeltaSink, Info, Model, PlanUsage, Provider, Token, TokenSource,
    chatcompletions, codex, quota,
};
use crate::model;
use crate::{
    auth::{Credential, Store},
    modelsdev::{Catalog, Loader},
    oauth,
    wire::Time,
};
use anyhow::{Result, bail, ensure};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex, OnceLock, RwLock},
    time::Duration,
};
use tokio_util::sync::CancellationToken;
pub const CATALOG_IDS: &[&str] = &["openai", "xai", "zai-coding-plan", "kimi-code-plan-global"];
const REFRESH_SKEW: Duration = Duration::from_secs(120);
const PLAN_POLL_EVERY: Duration = Duration::from_secs(300);
#[derive(Clone, Copy)]
struct Subscription {
    id: &'static str,
    name: &'static str,
    priority: i64,
    catalog: &'static str,
    key: bool,
}
const SUBSCRIPTIONS: [Subscription; 4] = [
    Subscription {
        id: "openai",
        name: "ChatGPT",
        priority: 0,
        catalog: "openai",
        key: false,
    },
    Subscription {
        id: "xai",
        name: "Grok",
        priority: 1,
        catalog: "xai",
        key: false,
    },
    Subscription {
        id: "zai",
        name: "Z.ai Coding Plan",
        priority: 2,
        catalog: "zai-coding-plan",
        key: true,
    },
    Subscription {
        id: "kimi",
        name: "Kimi For Coding",
        priority: 3,
        catalog: "kimi-code-plan-global",
        key: true,
    },
];
fn subscription(id: &str) -> Option<Subscription> {
    SUBSCRIPTIONS.iter().copied().find(|sub| sub.id == id)
}
pub fn subscription_name(id: &str) -> &str {
    subscription(id).map_or(id, |sub| sub.name)
}
pub fn supported() -> String {
    SUBSCRIPTIONS
        .iter()
        .map(|sub| format!("{} ({})", sub.id, sub.name))
        .collect::<Vec<_>>()
        .join(", ")
}
pub fn allowed(provider: &str, id: &str) -> bool {
    match provider {
        "openai" => {
            if matches!(
                id,
                "gpt-5.5" | "gpt-5.3-codex-spark" | "gpt-5.4" | "gpt-5.4-mini"
            ) {
                return true;
            }
            if matches!(id, "gpt-5.5-pro" | "gpt-5.6") || id.contains("-pro") {
                return false;
            }
            static VERSION: OnceLock<regex::Regex> = OnceLock::new();
            let Some(version) = VERSION
                .get_or_init(|| regex::Regex::new(r"^gpt-([0-9]+)(?:\.([0-9]+))?").unwrap())
                .captures(id)
            else {
                return false;
            };
            let major = version[1].parse::<i64>().unwrap_or(0);
            let minor = version
                .get(2)
                .and_then(|part| part.as_str().parse::<i64>().ok())
                .unwrap_or(0);
            major > 5 || major == 5 && minor > 4
        }
        "xai" => id.starts_with("grok") && !id.contains("imagine"),
        "zai" => id.starts_with("glm"),
        "kimi" => true,
        _ => false,
    }
}
#[derive(Clone, Debug, Default)]
pub struct Status {
    pub id: String,
    pub name: String,
    pub kind: String,
    pub label: String,
    pub connected: bool,
    pub account: String,
    pub priority: i64,
    pub models: usize,
    pub methods: Vec<oauth::Method>,
}
#[derive(Clone, Debug)]
pub struct ModelEntry {
    pub id: String,
    pub provider: String,
    pub name: String,
    pub info: Info,
}
#[derive(Clone, Default)]
pub struct Endpoints {
    pub codex: String,
    pub xai: String,
    pub zai: String,
    pub kimi: String,
}
impl Endpoints {
    fn get(&self, provider: &str) -> &str {
        match provider {
            "openai" => {
                if self.codex.is_empty() {
                    codex::DEFAULT_ENDPOINT
                } else {
                    &self.codex
                }
            }
            "xai" => {
                if self.xai.is_empty() {
                    "https://api.x.ai/v1"
                } else {
                    &self.xai
                }
            }
            "zai" => {
                if self.zai.is_empty() {
                    "https://api.z.ai/api/coding/paas/v4"
                } else {
                    &self.zai
                }
            }
            "kimi" => {
                if self.kimi.is_empty() {
                    "https://api.kimi.ai/coding/v1"
                } else {
                    &self.kimi
                }
            }
            _ => "",
        }
    }
}
struct Credentials {
    store: Option<Arc<Store>>,
    flows: BTreeMap<String, Arc<dyn oauth::Flow>>,
    refreshing: Mutex<BTreeMap<String, Arc<tokio::sync::Mutex<()>>>>,
}
impl Credentials {
    fn credential(&self, provider: &str) -> Option<Credential> {
        let credential = self.store.as_ref()?.get(provider)?;
        if credential.kind == "apikey" && !credential.key.is_empty()
            || credential.kind == "oauth" && !credential.refresh.is_empty()
        {
            Some(credential)
        } else {
            None
        }
    }
    fn flow(&self, provider: &str) -> Result<Arc<dyn oauth::Flow>> {
        self.flows.get(provider).cloned().ok_or_else(|| {
            anyhow::anyhow!(
                "unknown provider {provider:?}: stavlos supports {}",
                supported()
            )
        })
    }
    fn source(self: &Arc<Self>, provider: &str) -> TokenSource {
        let service = self.clone();
        let provider = provider.to_owned();
        Arc::new(move |cancel| {
            let service = service.clone();
            let provider = provider.clone();
            Box::pin(async move { service.token(&provider, &cancel).await })
        })
    }
    async fn token(
        &self,
        provider: &str,
        cancel: &CancellationToken,
    ) -> std::result::Result<Token, CallError> {
        let fail = |error| CallError::Permanent(error);
        let credential = self.credential(provider).ok_or_else(|| {
            fail(format!(
                "provider {provider:?} is not connected: run /provider to sign in"
            ))
        })?;
        if credential.kind == "apikey" {
            return Ok(Token {
                access: credential.key,
                ..Default::default()
            });
        }
        let soon = chrono::Utc::now()
            .timestamp_millis()
            .saturating_add(REFRESH_SKEW.as_millis() as i64);
        if credential.expires != 0
            && credential.expires > soon
            && !oauth::expiring(&credential.access, REFRESH_SKEW)
        {
            return Ok(Token {
                access: credential.access,
                account_id: credential.account_id,
            });
        }
        let lock = self
            .refreshing
            .lock()
            .unwrap()
            .entry(provider.into())
            .or_insert_with(|| Arc::new(tokio::sync::Mutex::new(())))
            .clone();
        let _refresh = tokio::select! {biased;_=cancel.cancelled()=>return Err(CallError::Cancelled),guard=lock.lock()=>guard};
        let store = self.store.as_ref().unwrap();
        if let Some(newer) = store.get(provider)
            && newer.access != credential.access
            && newer.expires
                > chrono::Utc::now()
                    .timestamp_millis()
                    .saturating_add(REFRESH_SKEW.as_millis() as i64)
        {
            return Ok(Token {
                access: newer.access,
                account_id: newer.account_id,
            });
        }
        let flow = self
            .flow(provider)
            .map_err(|error| fail(error.to_string()))?;
        let mut tokens = flow
            .refresh(credential.refresh, cancel.clone())
            .await
            .map_err(|error| {
                if matches!(error, oauth::Error::Cancelled) {
                    CallError::Cancelled
                } else {
                    fail(format!(
                        "{provider} session expired; run /provider to sign in again ({error})"
                    ))
                }
            })?;
        if tokens.account_id.is_empty() {
            tokens.account_id = credential.account_id;
        }
        if tokens.email.is_empty() {
            tokens.email = credential.email;
        }
        store
            .set(provider, credential_for(provider, &tokens))
            .map_err(|error| fail(error.to_string()))?;
        Ok(Token {
            access: tokens.access,
            account_id: tokens.account_id,
        })
    }
}
pub fn credential_for(provider: &str, tokens: &oauth::Tokens) -> Credential {
    if subscription(provider).is_some_and(|sub| sub.key) {
        Credential {
            kind: "apikey".into(),
            key: tokens.access.clone(),
            ..Default::default()
        }
    } else {
        Credential {
            kind: "oauth".into(),
            access: tokens.access.clone(),
            refresh: tokens.refresh.clone(),
            expires: tokens.expires_at.0.timestamp_millis(),
            account_id: tokens.account_id.clone(),
            email: tokens.email.clone(),
            ..Default::default()
        }
    }
}
pub type UsageHook = Arc<dyn Fn(&str, PlanUsage) + Send + Sync>;
#[derive(Default)]
struct UsageState {
    usage: BTreeMap<String, PlanUsage>,
    hook: Option<UsageHook>,
    polls: bool,
    polled: BTreeMap<String, std::time::Instant>,
}
#[derive(Default)]
pub struct UsageTracker {
    state: Mutex<UsageState>,
}
impl UsageTracker {
    pub fn observe(&self, provider: &str, mut usage: PlanUsage) {
        let hook = {
            let mut state = self.state.lock().unwrap();
            if let Some(old) = state.usage.get(provider) {
                if old.observed > usage.observed {
                    return;
                }
                if usage.plan.is_empty() {
                    usage.plan = old.plan.clone();
                }
                if usage.limited_until.is_zero() {
                    usage.limited_until = old.limited_until;
                }
            }
            state.usage.insert(provider.into(), usage.clone());
            state.hook.clone()
        };
        if let Some(hook) = hook {
            hook(provider, usage);
        }
    }
    pub fn mark_limited(&self, provider: &str, until: Time) {
        let mut state = self.state.lock().unwrap();
        let mut usage = state.usage.get(provider).cloned().unwrap_or_default();
        if until > usage.limited_until {
            usage.limited_until = until;
            state.usage.insert(provider.into(), usage);
        }
    }
    pub fn seed(&self, provider: &str, usage: PlanUsage) {
        let mut state = self.state.lock().unwrap();
        if state
            .usage
            .get(provider)
            .is_none_or(|old| usage.observed > old.observed)
        {
            state.usage.insert(provider.into(), usage);
        }
    }
    pub fn on_usage(&self, hook: Option<UsageHook>) {
        self.state.lock().unwrap().hook = hook;
    }
    pub fn all(&self) -> BTreeMap<String, PlanUsage> {
        self.state.lock().unwrap().usage.clone()
    }
}
struct Poller {
    credentials: Arc<Credentials>,
    usage: Arc<UsageTracker>,
    endpoints: Arc<RwLock<Endpoints>>,
    http: reqwest::Client,
}
impl Poller {
    async fn poll(
        &self,
        provider: &str,
        min_age: Duration,
        cancel: &CancellationToken,
    ) -> Result<()> {
        if !self.usage.state.lock().unwrap().polls {
            return Ok(());
        }
        let endpoint = quota::url(provider, self.endpoints.read().unwrap().get(provider));
        if endpoint.is_empty() || self.credentials.credential(provider).is_none() {
            return Ok(());
        }
        {
            let mut state = self.usage.state.lock().unwrap();
            let now = std::time::Instant::now();
            if let Some(polled) = state.polled.get(provider)
                && now.duration_since(*polled) < min_age
            {
                return Ok(());
            }
            state.polled.insert(provider.into(), now);
        }
        let token = self
            .credentials
            .token(provider, cancel)
            .await
            .map_err(|error| anyhow::anyhow!("{provider} plan usage: {error}"))?;
        let usage = quota::fetch(&self.http, provider, &endpoint, &token, cancel).await?;
        self.usage.observe(provider, usage);
        Ok(())
    }
    fn after(self: &Arc<Self>, provider: String) {
        let poller = self.clone();
        tokio::spawn(async move {
            let cancel = CancellationToken::new();
            if let Ok(Err(error)) = tokio::time::timeout(
                Duration::from_secs(15),
                poller.poll(&provider, PLAN_POLL_EVERY, &cancel),
            )
            .await
            {
                eprintln!("{error}");
            }
        });
    }
}
#[derive(Default)]
struct Providers {
    explicit: BTreeMap<String, Arc<dyn Provider>>,
    subscriptions: BTreeMap<String, Arc<dyn Provider>>,
    opened: BTreeMap<String, Arc<dyn Model>>,
}
pub struct Registry {
    catalog: RwLock<Option<Arc<Catalog>>>,
    credentials: Arc<Credentials>,
    providers: Mutex<Providers>,
    endpoints: Arc<RwLock<Endpoints>>,
    pub usage: Arc<UsageTracker>,
    poller: Arc<Poller>,
}
impl Registry {
    pub fn new(
        catalog: Option<Arc<Catalog>>,
        store: Option<Arc<Store>>,
        flows: BTreeMap<String, Arc<dyn oauth::Flow>>,
    ) -> Result<Arc<Self>> {
        let credentials = Arc::new(Credentials {
            store,
            flows,
            refreshing: Mutex::default(),
        });
        let usage = Arc::new(UsageTracker::default());
        let endpoints = Arc::new(RwLock::new(Endpoints::default()));
        let poller = Arc::new(Poller {
            credentials: credentials.clone(),
            usage: usage.clone(),
            endpoints: endpoints.clone(),
            http: crate::httpx::client(Some(Duration::from_secs(15)))?,
        });
        Ok(Arc::new(Self {
            catalog: RwLock::new(catalog),
            credentials,
            providers: Mutex::default(),
            endpoints,
            usage,
            poller,
        }))
    }
    pub async fn default(
        loader: Loader,
        store: Arc<Store>,
        callback: Option<crate::nativeapi::Program>,
        cancel: &CancellationToken,
    ) -> Result<Arc<Self>> {
        let (catalog, stale) = loader.load(CATALOG_IDS, cancel).await?;
        let registry = Self::new(
            Some(Arc::new(catalog)),
            Some(store),
            oauth::flows(callback)?,
        )?;
        if stale {
            let weak = Arc::downgrade(&registry);
            tokio::spawn(async move {
                if let Ok(catalog) = loader.refresh(CATALOG_IDS, &CancellationToken::new()).await
                    && let Some(registry) = weak.upgrade()
                {
                    registry.set_catalog(Some(Arc::new(catalog)));
                }
            });
        }
        Ok(registry)
    }
    pub fn catalog(&self) -> Option<Arc<Catalog>> {
        self.catalog.read().unwrap().clone()
    }
    pub fn set_catalog(&self, catalog: Option<Arc<Catalog>>) {
        if catalog.is_some() {
            *self.catalog.write().unwrap() = catalog;
        }
    }
    pub fn store(&self) -> Option<Arc<Store>> {
        self.credentials.store.clone()
    }
    pub fn flow(&self, provider: &str) -> Result<Arc<dyn oauth::Flow>> {
        self.credentials.flow(provider)
    }
    pub fn token_source(&self, provider: &str) -> TokenSource {
        self.credentials.source(provider)
    }
    pub fn with_endpoints(&self, endpoints: Endpoints) {
        *self.endpoints.write().unwrap() = endpoints;
        let mut providers = self.providers.lock().unwrap();
        providers.subscriptions.clear();
        providers.opened.clear();
    }
    pub fn register(&self, provider: Arc<dyn Provider>) -> Result<()> {
        let name = provider.name().to_owned();
        ensure!(!name.is_empty(), "registry: provider has empty name");
        let mut providers = self.providers.lock().unwrap();
        ensure!(
            !providers.explicit.contains_key(&name),
            "registry: provider {name:?} registered twice"
        );
        providers.explicit.insert(name, provider);
        Ok(())
    }
    pub fn list(&self) -> Vec<Status> {
        let counts = self.models("", true).into_iter().fold(
            BTreeMap::<String, usize>::new(),
            |mut counts, model| {
                *counts.entry(model.provider).or_default() += 1;
                counts
            },
        );
        let mut result = SUBSCRIPTIONS
            .iter()
            .map(|sub| self.subscription_status(*sub, counts.get(sub.id).copied().unwrap_or(0)))
            .collect::<Vec<_>>();
        for name in self.providers.lock().unwrap().explicit.keys() {
            result.push(plugin_status(name));
        }
        result.sort_by(|a, b| {
            a.priority
                .cmp(&b.priority)
                .then_with(|| a.name.cmp(&b.name))
        });
        result
    }
    pub fn status(&self, name: &str) -> Option<Status> {
        if self.providers.lock().unwrap().explicit.contains_key(name) {
            return Some(plugin_status(name));
        }
        subscription(name).map(|sub| self.subscription_status(sub, self.models(name, true).len()))
    }
    fn subscription_status(&self, sub: Subscription, models: usize) -> Status {
        let mut status = Status {
            id: sub.id.into(),
            name: sub.name.into(),
            kind: "subscription".into(),
            priority: sub.priority,
            models,
            ..Default::default()
        };
        if let Some(flow) = self.credentials.flows.get(sub.id) {
            status.label = flow.label().into();
            status.methods = flow.methods();
        }
        if let Some(credential) = self.credentials.credential(sub.id) {
            status.connected = true;
            status.account = if credential.email.is_empty() {
                credential.account_id
            } else {
                credential.email
            };
        }
        status
    }
    pub fn providers(&self) -> Vec<String> {
        self.list()
            .into_iter()
            .filter(|status| status.connected)
            .map(|status| status.id)
            .collect()
    }
    pub fn save_login(&self, provider: &str, tokens: &oauth::Tokens) -> Result<()> {
        let store = self
            .store()
            .ok_or_else(|| anyhow::anyhow!("no credential store"))?;
        store.set(provider, credential_for(provider, tokens))?;
        self.invalidate(provider);
        Ok(())
    }
    pub fn disconnect(&self, provider: &str) -> Result<()> {
        let store = self
            .store()
            .ok_or_else(|| anyhow::anyhow!("no credential store"))?;
        store.remove(provider)?;
        self.invalidate(provider);
        Ok(())
    }
    pub fn invalidate(&self, provider: &str) {
        let prefix = format!("{provider}/");
        self.providers
            .lock()
            .unwrap()
            .opened
            .retain(|id, _| !id.starts_with(&prefix));
    }
    pub fn models(&self, provider: &str, all: bool) -> Vec<ModelEntry> {
        let Some(catalog) = self.catalog() else {
            return vec![];
        };
        let mut models = Vec::new();
        for sub in SUBSCRIPTIONS {
            if !provider.is_empty() && provider != sub.id
                || !all && self.credentials.credential(sub.id).is_none()
            {
                continue;
            }
            for id in catalog.models(sub.catalog) {
                if allowed(sub.id, &id) {
                    let info =
                        subscription_info(catalog.model(sub.catalog, &id).unwrap_or_default());
                    models.push(ModelEntry {
                        id: format!("{}/{id}", sub.id),
                        provider: sub.id.into(),
                        name: catalog.model_name(sub.catalog, &id),
                        info,
                    });
                }
            }
        }
        models
    }
    fn lookup(&self, full: &str) -> Result<(Arc<dyn Provider>, String)> {
        let (name, id) = model::split(full)?;
        if let Some(provider) = self.providers.lock().unwrap().explicit.get(name).cloned() {
            return Ok((provider, id.into()));
        }
        let sub = subscription(name).ok_or_else(|| {
            anyhow::anyhow!(
                "unknown provider {name:?}: stavlos supports {}; run /provider",
                supported()
            )
        })?;
        ensure!(
            self.credentials.credential(name).is_some(),
            "{} is not connected: run /provider to sign in with your {} subscription",
            sub.name,
            sub.name
        );
        let mut providers = self.providers.lock().unwrap();
        if let Some(provider) = providers.subscriptions.get(name) {
            return Ok((provider.clone(), id.into()));
        }
        let endpoint = self.endpoints.read().unwrap().get(name).to_owned();
        let source = self.credentials.source(name);
        let provider: Arc<dyn Provider> = if name == "openai" {
            let tracker = self.usage.clone();
            Arc::new(codex::Provider::new(
                Some(source),
                endpoint,
                Some(Arc::new(move |usage| tracker.observe("openai", usage))),
            )?)
        } else {
            let traits = match name {
                "xai" => chatcompletions::Traits {
                    cache_header: "x-grok-conv-id".into(),
                    replays_reasoning: true,
                    variants: Some(Arc::new(|id| {
                        if id.contains("mini") {
                            vec!["low".into(), "high".into()]
                        } else {
                            vec![]
                        }
                    })),
                    is_limit: Some(Arc::new(|status, body| {
                        status == 403
                            && super::stream::has_any(body, &["spending-limit", "out of credits"])
                            || super::stream::plan_limit(status, body)
                    })),
                    ..Default::default()
                },
                "zai" => chatcompletions::Traits {
                    replays_reasoning: true,
                    ..Default::default()
                },
                "kimi" => chatcompletions::Traits {
                    cache_key_field: true,
                    replays_reasoning: true,
                    ..Default::default()
                },
                _ => bail!("unknown subscription"),
            };
            Arc::new(chatcompletions::Provider::new(
                name,
                endpoint,
                Some(source),
                traits,
            )?)
        };
        providers
            .subscriptions
            .insert(name.into(), provider.clone());
        Ok((provider, id.into()))
    }
    pub fn check(&self, full: &str) -> Result<()> {
        self.lookup(full).map(|_| ())
    }
    pub fn variants(&self, full: &str) -> Vec<String> {
        self.lookup(full)
            .map(|(provider, id)| provider.variants(&id))
            .unwrap_or_default()
    }
    pub fn resolve(&self, full: &str) -> Result<(Arc<dyn Model>, Info)> {
        let (provider, id) = self.lookup(full)?;
        let name = model::split(full)?.0;
        let mut info = self
            .catalog()
            .and_then(|catalog| {
                catalog.model(
                    subscription(provider.name()).map_or(provider.name(), |sub| sub.catalog),
                    &id,
                )
            })
            .unwrap_or_default();
        if subscription(name).is_some() {
            info = subscription_info(info);
        }
        info.capabilities = provider.capabilities(&id);
        let mut providers = self.providers.lock().unwrap();
        if let Some(model) = providers.opened.get(full) {
            return Ok((model.clone(), info));
        }
        let opened = provider
            .open(&id)
            .map_err(|error| anyhow::anyhow!("model {full:?}: {error}"))?;
        let model: Arc<dyn Model> = if subscription(name).is_some() {
            Arc::new(PolledModel {
                model: opened,
                poller: self.poller.clone(),
                provider: name.into(),
            })
        } else {
            opened
        };
        providers.opened.insert(full.into(), model.clone());
        Ok((model, info))
    }
    pub fn enable_plan_polling(&self) {
        self.usage.state.lock().unwrap().polls = true;
    }
    pub async fn poll_plan_usage(
        &self,
        provider: &str,
        min_age: Duration,
        cancel: &CancellationToken,
    ) -> Result<()> {
        self.poller.poll(provider, min_age, cancel).await
    }
    pub async fn poll_all_plan_usage(&self, min_age: Duration, cancel: &CancellationToken) {
        for sub in SUBSCRIPTIONS {
            if let Err(error) = self.poller.poll(sub.id, min_age, cancel).await {
                eprintln!("{error}");
            }
        }
    }
}
fn plugin_status(name: &str) -> Status {
    Status {
        id: name.into(),
        name: name.into(),
        kind: "plugin".into(),
        connected: true,
        priority: 50,
        ..Default::default()
    }
}
fn subscription_info(mut info: Info) -> Info {
    info.input_price = 0.;
    info.output_price = 0.;
    info.cache_read_price = 0.;
    info.cache_write_price = 0.;
    info
}
struct PolledModel {
    model: Arc<dyn Model>,
    poller: Arc<Poller>,
    provider: String,
}
impl Model for PolledModel {
    fn complete(
        &self,
        request: model::Request,
        on_delta: Option<DeltaSink>,
        cancel: CancellationToken,
    ) -> CompleteFuture<'_> {
        Box::pin(async move {
            let result = self.model.complete(request, on_delta, cancel).await;
            self.poller.after(self.provider.clone());
            result
        })
    }
}
