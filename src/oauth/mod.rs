//! Owner-local subscription sign-in, token refresh and unverified display claims.
mod chatgpt;
mod grok;
mod key;
use crate::{
    nativeapi,
    wire::{Slice, Time},
    wire_struct,
};
use base64::{
    Engine as _,
    engine::general_purpose::{URL_SAFE, URL_SAFE_NO_PAD},
};
pub use chatgpt::ChatGpt;
pub use grok::Grok;
pub use key::KeyFlow;
use serde::{Deserialize, de::DeserializeOwned};
use std::{collections::BTreeMap, future::Future, pin::Pin, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;
pub const METHOD_BROWSER: &str = "browser";
pub const METHOD_DEVICE: &str = "device";
pub const METHOD_API_KEY: &str = "apikey";
#[derive(Debug, Clone, thiserror::Error)]
pub enum Error {
    #[error("login was denied")]
    Denied,
    #[error("login code expired; start again")]
    Expired,
    #[error("operation cancelled")]
    Cancelled,
    #[error("{0}")]
    Message(String),
}
pub type Result<T> = std::result::Result<T, Error>;
pub type FlowFuture<'a, T> = Pin<Box<dyn Future<Output = Result<T>> + Send + 'a>>;
fn message(error: impl std::fmt::Display) -> Error {
    Error::Message(error.to_string())
}
pub fn user_agent() -> String {
    let arch = match std::env::consts::ARCH {
        "x86_64" => "amd64",
        "aarch64" => "arm64",
        other => other,
    };
    format!("stavlos/0.1 ({} {arch})", std::env::consts::OS)
}
#[derive(Clone, Default)]
pub struct Tokens {
    pub access: String,
    pub refresh: String,
    pub id_token: String,
    pub expires_at: Time,
    pub account_id: String,
    pub email: String,
}
impl std::fmt::Debug for Tokens {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Tokens").finish_non_exhaustive()
    }
}
#[derive(Clone, Debug)]
pub struct Method {
    pub id: String,
    pub label: String,
}
impl Method {
    fn new(id: &str, label: &str) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
        }
    }
}
pub struct Pending {
    pub provider: String,
    pub method: String,
    pub url: String,
    pub code: String,
    pub instructions: String,
    pub expires_in: Duration,
    pub interval: Duration,
    device_code: String,
    device_auth_id: String,
    browser: Option<Arc<chatgpt::Browser>>,
    key: Option<tokio::sync::mpsc::Sender<String>>,
    key_receiver: tokio::sync::Mutex<Option<tokio::sync::mpsc::Receiver<String>>>,
}
impl Default for Pending {
    fn default() -> Self {
        Self {
            provider: String::new(),
            method: String::new(),
            url: String::new(),
            code: String::new(),
            instructions: String::new(),
            expires_in: Duration::ZERO,
            interval: Duration::ZERO,
            device_code: String::new(),
            device_auth_id: String::new(),
            browser: None,
            key: None,
            key_receiver: tokio::sync::Mutex::new(None),
        }
    }
}
impl Pending {
    pub fn deliver(&self, key: &str) -> Result<()> {
        let sender = self
            .key
            .as_ref()
            .ok_or_else(|| message("this login does not take a key"))?;
        let key = key.trim();
        if key.is_empty() {
            return Err(message("the key is empty"));
        }
        sender
            .try_send(key.into())
            .map_err(|_| message("this login already has a key"))
    }
    pub async fn close(&self) {
        if let Some(browser) = &self.browser {
            browser.close();
            browser.shutdown().await;
        }
    }
}
pub trait Flow: Send + Sync {
    fn provider(&self) -> &str;
    fn label(&self) -> &str;
    fn methods(&self) -> Vec<Method>;
    fn start(&self, method: String, cancel: CancellationToken) -> FlowFuture<'_, Arc<Pending>>;
    fn wait(&self, pending: Arc<Pending>, cancel: CancellationToken) -> FlowFuture<'_, Tokens>;
    fn refresh(&self, refresh: String, cancel: CancellationToken) -> FlowFuture<'_, Tokens>;
}
#[derive(Clone)]
pub struct Client {
    pub http: reqwest::Client,
    pub poll_margin: Duration,
}
impl Client {
    pub fn new() -> anyhow::Result<Self> {
        Ok(Self {
            http: crate::httpx::client(Some(Duration::from_secs(30)))?,
            poll_margin: Duration::from_secs(3),
        })
    }
    async fn send(
        &self,
        request: reqwest::RequestBuilder,
        cancel: &CancellationToken,
    ) -> Result<reqwest::Response> {
        tokio::select! {biased;_=cancel.cancelled()=>Err(Error::Cancelled),response=request.header("Accept","application/json").header("User-Agent",user_agent()).send()=>response.map_err(|error|message(error.without_url()))}
    }
    async fn form(
        &self,
        url: &str,
        form: &[(&str, &str)],
        cancel: &CancellationToken,
    ) -> Result<reqwest::Response> {
        self.send(self.http.post(url).form(form), cancel).await
    }
    async fn json(
        &self,
        url: &str,
        body: &serde_json::Value,
        cancel: &CancellationToken,
    ) -> Result<reqwest::Response> {
        self.send(self.http.post(url).json(body), cancel).await
    }
}
async fn body(
    mut response: reqwest::Response,
    limit: usize,
    cancel: &CancellationToken,
) -> Result<Vec<u8>> {
    let mut raw = Vec::new();
    while raw.len() < limit {
        let part = tokio::select! {biased;_=cancel.cancelled()=>return Err(Error::Cancelled),part=response.chunk()=>part.map_err(|error|message(error.without_url()))?};
        let Some(part) = part else { break };
        let count = part.len().min(limit - raw.len());
        raw.extend_from_slice(&part[..count]);
    }
    Ok(raw)
}
async fn decode<T: DeserializeOwned + Default>(
    response: reqwest::Response,
    cancel: &CancellationToken,
) -> Result<T> {
    let raw = body(response, (16 << 20) + 1, cancel).await?;
    if raw.len() > 16 << 20 {
        return Err(message("authorization response exceeds its bound"));
    }
    let mut values =
        serde_json::Deserializer::from_slice(&raw).into_iter::<Box<serde_json::value::RawValue>>();
    let raw = values
        .next()
        .ok_or_else(|| message("authorization response is empty"))?
        .map_err(message)?;
    crate::wire::decode(raw.get()).map_err(message)
}
async fn read_error(response: reqwest::Response, cancel: &CancellationToken) -> Result<String> {
    Ok(
        String::from_utf8_lossy(&body(response, 4096, cancel).await?)
            .trim()
            .into(),
    )
}
async fn sleep(duration: Duration, cancel: &CancellationToken) -> Result<()> {
    tokio::select! {biased;_=cancel.cancelled()=>Err(Error::Cancelled),_=tokio::time::sleep(duration)=>Ok(())}
}
#[derive(Default, Deserialize)]
#[serde(default)]
struct TokenResponse {
    #[serde(deserialize_with = "crate::wire::null_default")]
    access_token: String,
    #[serde(deserialize_with = "crate::wire::null_default")]
    refresh_token: String,
    #[serde(deserialize_with = "crate::wire::null_default")]
    id_token: String,
    #[serde(deserialize_with = "crate::wire::null_default")]
    expires_in: i64,
    #[serde(deserialize_with = "crate::wire::null_default")]
    error: String,
    #[serde(deserialize_with = "crate::wire::null_default")]
    error_description: String,
}
impl TokenResponse {
    fn tokens(self, fallback_refresh: &str) -> Result<Tokens> {
        let expiration = if self.expires_in <= 0 {
            3600
        } else {
            self.expires_in
        };
        let duration = chrono::TimeDelta::try_seconds(expiration)
            .ok_or_else(|| message("authorization expiration is outside its range"))?;
        let expires_at = chrono::Utc::now()
            .checked_add_signed(duration)
            .ok_or_else(|| message("authorization expiration is outside its range"))?;
        let mut out = Tokens {
            access: self.access_token,
            refresh: if self.refresh_token.is_empty() {
                fallback_refresh.into()
            } else {
                self.refresh_token
            },
            id_token: self.id_token,
            expires_at: Time(expires_at.fixed_offset()),
            ..Default::default()
        };
        for token in [&out.id_token, &out.access] {
            let claims = claims(token);
            if out.account_id.is_empty() {
                out.account_id = claims.account_id().into();
            }
            if out.email.is_empty() {
                out.email = claims.email;
            }
        }
        Ok(out)
    }
}
wire_struct!(Organization{id:"id":String=>required});
wire_struct!(OpenAiAuth{chatgpt_account_id:"chatgpt_account_id":String=>required});
wire_struct!(Claims{exp:"exp":i64=>required,email:"email":String=>required,chatgpt_acct:"chatgpt_account_id":String=>required,organizations:"organizations":Slice<Organization> =>required,openai_auth:"https://api.openai.com/auth":OpenAiAuth=>required});
impl Claims {
    pub fn account_id(&self) -> &str {
        if !self.chatgpt_acct.is_empty() {
            &self.chatgpt_acct
        } else if !self.openai_auth.chatgpt_account_id.is_empty() {
            &self.openai_auth.chatgpt_account_id
        } else {
            self.organizations
                .first()
                .map_or("", |organization| organization.id.as_str())
        }
    }
}
/// These unverified claims describe an account; they never grant authority.
pub fn claims(token: &str) -> Claims {
    let Some(payload) = token.split('.').nth(1) else {
        return Claims::default();
    };
    let Ok(raw) = URL_SAFE_NO_PAD
        .decode(payload.trim_end_matches('='))
        .or_else(|_| URL_SAFE.decode(payload))
    else {
        return Claims::default();
    };
    let Ok(value) = serde_json::from_slice::<serde_json::Value>(&raw) else {
        return Claims::default();
    };
    // A malformed field does not discard other valid display claims.
    let text = |key: &str| value[key].as_str().unwrap_or("").to_owned();
    Claims {
        exp: value["exp"].as_i64().unwrap_or(0),
        email: text("email"),
        chatgpt_acct: text("chatgpt_account_id"),
        openai_auth: OpenAiAuth {
            chatgpt_account_id: value["https://api.openai.com/auth"]["chatgpt_account_id"]
                .as_str()
                .unwrap_or("")
                .into(),
        },
        organizations: value["organizations"]
            .as_array()
            .map(|organizations| {
                organizations
                    .iter()
                    .map(|organization| Organization {
                        id: organization["id"].as_str().unwrap_or("").into(),
                    })
                    .collect::<Vec<_>>()
                    .into()
            })
            .unwrap_or_default(),
    }
}
pub fn expiring_at(token: &str, skew: Duration, now: Time) -> bool {
    let claims = claims(token);
    if claims.exp == 0 {
        return false;
    }
    let deadline = now.0.timestamp() as i128 * 1_000_000_000
        + now.0.timestamp_subsec_nanos() as i128
        + skew.as_nanos().min(i128::MAX as u128) as i128;
    (claims.exp as i128 * 1_000_000_000) < deadline
}
pub fn expiring(token: &str, skew: Duration) -> bool {
    expiring_at(token, skew, Time::now())
}
pub fn flows(
    callback: Option<nativeapi::Program>,
) -> anyhow::Result<BTreeMap<String, Arc<dyn Flow>>> {
    let client = Client::new()?;
    let mut chatgpt = ChatGpt::new(client.clone(), callback);
    let mut grok = Grok::new(client);
    if let Ok(issuer) = std::env::var("STAVLOS_OAUTH_OPENAI_ISSUER")
        && !issuer.is_empty()
    {
        chatgpt.issuer = issuer.trim_end_matches('/').into();
        eprintln!(
            "stavlos: STAVLOS_OAUTH_OPENAI_ISSUER is set: ChatGPT sign-ins and tokens use the configured issuer"
        );
    }
    if let Ok(base) = std::env::var("STAVLOS_OAUTH_XAI_BASE")
        && !base.is_empty()
    {
        let base = base.trim_end_matches('/');
        grok.device_url = format!("{base}/device/code");
        grok.token_url = format!("{base}/token");
        eprintln!(
            "stavlos: STAVLOS_OAUTH_XAI_BASE is set: Grok sign-ins and tokens use the configured base"
        );
    }
    Ok(BTreeMap::from([
        ("openai".into(), Arc::new(chatgpt) as Arc<dyn Flow>),
        ("xai".into(), Arc::new(grok) as Arc<dyn Flow>),
        ("zai".into(), Arc::new(KeyFlow::zai()) as Arc<dyn Flow>),
        ("kimi".into(), Arc::new(KeyFlow::kimi()) as Arc<dyn Flow>),
    ]))
}
