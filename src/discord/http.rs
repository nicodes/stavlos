//! Bounded Discord REST, with explicit rate-limit retries and no ambiguous-send retries.
use super::{
    api::{Api, Channel, ChannelCreate, Message, Missing, User},
    interaction, text,
};
use crate::{httpx, tools::ServiceFuture};
use anyhow::{Result, anyhow, bail, ensure};
use reqwest::{Method, Url};
use serde::Deserialize;
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;
const BODY_LIMIT: usize = 4 * 1024 * 1024;
const CARD_FLAGS: u64 = 32772;
#[derive(Default)]
struct Rate {
    ready: Option<Instant>,
}
#[derive(Default)]
struct Rates {
    routes: HashMap<String, Arc<tokio::sync::Mutex<Rate>>>,
    buckets: HashMap<String, Arc<tokio::sync::Mutex<Rate>>>,
    global: Option<Instant>,
}
pub struct Rest {
    client: reqwest::Client,
    base: Url,
    token: String,
    invalid: AtomicBool,
    rates: Mutex<Rates>,
}
fn failed() -> anyhow::Error {
    anyhow!("discord request failed (check connection, token and gateway intents)")
}
fn status_error(code: u16) -> anyhow::Error {
    match code {
        401 => anyhow!("discord rejected the bot token (HTTP 401)"),
        403 => anyhow!("discord denied access; check server and bot permissions (HTTP 403)"),
        404 => Missing.into(),
        _ => anyhow!("discord HTTP {code}"),
    }
}
fn seconds(value: Option<&str>) -> Option<Duration> {
    let value = value?.parse::<f64>().ok()?;
    (value.is_finite() && (0.0..=86400.0).contains(&value)).then(|| Duration::from_secs_f64(value))
}
async fn wait(until: Option<Instant>) {
    if let Some(until) = until {
        tokio::time::sleep_until(until).await;
    }
}
impl Rest {
    pub fn new(token: String) -> Result<Self> {
        Self::with_endpoint(token, Url::parse("https://discord.com/api/v10/")?)
    }
    /// Explicit loopback endpoint permits local HTTP protocol fixtures.
    pub fn with_endpoint(token: String, base: Url) -> Result<Self> {
        ensure!(!token.is_empty(), "Discord token is empty");
        ensure!(
            base.scheme() == "https"
                || base.host_str().is_some_and(|h| h
                    .parse::<std::net::IpAddr>()
                    .is_ok_and(|ip| ip.is_loopback())),
            "Discord endpoint requires HTTPS"
        );
        ensure!(
            base.username().is_empty() && base.password().is_none(),
            "Discord endpoint must not contain credentials"
        );
        let client = httpx::builder()
            .redirect(reqwest::redirect::Policy::none())
            .timeout(Duration::from_secs(20))
            .user_agent("Stavlos (https://stavlos.ai, 1)")
            .build()?;
        Ok(Self {
            client,
            base,
            token,
            invalid: AtomicBool::new(false),
            rates: Mutex::new(Rates::default()),
        })
    }
    pub async fn request(
        &self,
        method: Method,
        path: &[&str],
        query: &[(&str, &str)],
        body: Option<Value>,
        authenticated: bool,
        cancel: CancellationToken,
    ) -> Result<Value> {
        tokio::select! {biased;_=cancel.cancelled()=>bail!("context canceled"),result=tokio::time::timeout(Duration::from_secs(20),self.perform(method,path,query,body,authenticated))=>result.map_err(|_|anyhow!("discord request timed out"))?}
    }
    async fn perform(
        &self,
        method: Method,
        path: &[&str],
        query: &[(&str, &str)],
        body: Option<Value>,
        authenticated: bool,
    ) -> Result<Value> {
        if authenticated && self.invalid.load(Ordering::Acquire) {
            return Err(status_error(401));
        }
        let mut url = self.base.clone();
        {
            let mut segments = url
                .path_segments_mut()
                .map_err(|_| anyhow!("invalid Discord endpoint"))?;
            segments.pop_if_empty();
            for segment in path {
                segments.push(segment);
            }
        }
        if !query.is_empty() {
            url.query_pairs_mut().extend_pairs(query.iter().copied());
        }
        // Discord bucket hashes exclude the major channel/guild/webhook resource.
        let major = if path.len() >= 2 && matches!(path[0], "channels" | "guilds" | "webhooks") {
            format!("{}/{}", path[0], path[1])
        } else {
            String::new()
        };
        let route = format!(
            "{}:{}:{}",
            method,
            major,
            path.iter()
                .enumerate()
                .map(|(i, p)| {
                    if i == 0
                        || matches!(
                            *p,
                            "messages"
                                | "webhooks"
                                | "typing"
                                | "members"
                                | "commands"
                                | "applications"
                                | "callback"
                                | "@me"
                                | "@original"
                        )
                    {
                        *p
                    } else {
                        ":id"
                    }
                })
                .collect::<Vec<_>>()
                .join("/")
        );
        let rate = {
            let mut rates = self.rates.lock().unwrap();
            rates.routes.entry(route.clone()).or_default().clone()
        };
        let mut rate_guard = rate.lock().await;
        for attempt in 0..5 {
            wait(rate_guard.ready).await;
            let global = self.rates.lock().unwrap().global;
            wait(global).await;
            if authenticated && self.invalid.load(Ordering::Acquire) {
                return Err(status_error(401));
            }
            let mut request = self.client.request(method.clone(), url.clone());
            if authenticated {
                request = request.header(
                    reqwest::header::AUTHORIZATION,
                    format!("Bot {}", self.token),
                );
            }
            if let Some(body) = &body {
                request = request.json(body);
            }
            let mut response = request.send().await.map_err(|_| failed())?;
            let code = response.status().as_u16();
            let headers = response.headers().clone();
            let mut data = Vec::new();
            while let Some(chunk) = response.chunk().await.map_err(|_| failed())? {
                ensure!(
                    data.len() + chunk.len() <= BODY_LIMIT,
                    "Discord response is too large"
                );
                data.extend_from_slice(&chunk);
            }
            let header = |name: &str| headers.get(name).and_then(|v| v.to_str().ok());
            if let Some(bucket) = header("x-ratelimit-bucket") {
                let mut rates = self.rates.lock().unwrap();
                let shared = rates
                    .buckets
                    .entry(format!("{major}:{bucket}"))
                    .or_insert_with(|| rate.clone())
                    .clone();
                rates.routes.insert(route.clone(), shared);
            }
            if header("x-ratelimit-remaining") == Some("0") {
                rate_guard.ready =
                    seconds(header("x-ratelimit-reset-after")).map(|d| Instant::now() + d);
            }
            if code == 429 && attempt < 4 {
                let response: Value = serde_json::from_slice(&data).unwrap_or(Value::Null);
                let delay = seconds(header("retry-after"))
                    .or_else(|| {
                        response["retry_after"]
                            .as_f64()
                            .and_then(|d| seconds(Some(&d.to_string())))
                    })
                    .ok_or_else(|| status_error(429))?
                    .max(Duration::from_millis(1));
                let ready = Instant::now() + delay;
                if response["global"] == true || header("x-ratelimit-global") == Some("true") {
                    self.rates.lock().unwrap().global = Some(ready);
                } else {
                    rate_guard.ready = Some(ready);
                }
                continue;
            }
            if !(200..300).contains(&code) {
                if code == 401 && authenticated {
                    self.invalid.store(true, Ordering::Release);
                }
                return Err(status_error(code));
            }
            return if data.is_empty() {
                Ok(Value::Null)
            } else {
                serde_json::from_slice(&data).map_err(|_| anyhow!("invalid Discord response"))
            };
        }
        Err(status_error(429))
    }
}
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
struct Webhook {
    id: String,
    token: String,
    name: String,
    user: Option<User>,
    application_id: Option<String>,
}
#[derive(Clone)]
struct Profile {
    name: String,
    avatar: String,
    expires: Instant,
}
#[derive(Default)]
struct Cache {
    hooks: HashMap<String, Webhook>,
    filling: HashMap<String, Arc<tokio::sync::Mutex<()>>>,
    profiles: HashMap<String, Option<Profile>>,
    profile_expiry: HashMap<String, Instant>,
}
pub struct Http {
    pub rest: Arc<Rest>,
    pub bot: String,
    pub application: String,
    cache: Mutex<Cache>,
}
impl Http {
    pub fn new(rest: Arc<Rest>, bot: String, application: String) -> Self {
        Self {
            rest,
            bot,
            application,
            cache: Mutex::new(Cache::default()),
        }
    }
    pub async fn identify(
        rest: &Rest,
        guild: &str,
        cancel: CancellationToken,
    ) -> Result<(User, String, String)> {
        let user: User = serde_json::from_value(
            rest.request(
                Method::GET,
                &["users", "@me"],
                &[],
                None,
                true,
                cancel.clone(),
            )
            .await?,
        )
        .map_err(|_| anyhow!("invalid Discord user response"))?;
        let guild_info = rest
            .request(
                Method::GET,
                &["guilds", guild],
                &[],
                None,
                true,
                cancel.clone(),
            )
            .await
            .map_err(|e| {
                if e.is::<Missing>() {
                    anyhow!("discord server not found; check the guild ID and invite the bot")
                } else {
                    e
                }
            })?;
        let application = rest
            .request(
                Method::GET,
                &["oauth2", "applications", "@me"],
                &[],
                None,
                true,
                cancel,
            )
            .await?;
        let id = application["id"]
            .as_str()
            .filter(|id| !id.is_empty())
            .ok_or_else(|| anyhow!("invalid Discord application response"))?;
        ensure!(!user.id.is_empty(), "invalid Discord user response");
        Ok((
            user,
            guild_info["name"].as_str().unwrap_or("").into(),
            id.into(),
        ))
    }
    pub async fn register_commands(&self, guild: &str, cancel: CancellationToken) -> Result<()> {
        self.rest.request(Method::PUT,&["applications",&self.application,"guilds",guild,"commands"],&[],Some(json!([
            {"name":"status","description":"Show this channel's stavlos agents","options":[{"type":1,"name":"active","description":"Show non-idle agents"},{"type":1,"name":"all","description":"Show all agents, including idle agents"}]},
            {"name":"cancel","description":"Cancel an agent's current turn","options":[{"type":3,"name":"agent","description":"Agent name (default main)"}]}
        ])),true,cancel).await?;
        Ok(())
    }
    async fn hook(&self, channel: &str, name: &str, cancel: CancellationToken) -> Result<Webhook> {
        let key = format!("{channel}:{name}");
        let filling = {
            let mut cache = self.cache.lock().unwrap();
            if let Some(hook) = cache.hooks.get(&key) {
                return Ok(hook.clone());
            }
            cache.filling.entry(key.clone()).or_default().clone()
        };
        let _filling = tokio::select! {biased;_=cancel.cancelled()=>bail!("context canceled"),guard=filling.lock()=>guard};
        if let Some(hook) = self.cache.lock().unwrap().hooks.get(&key) {
            return Ok(hook.clone());
        }
        let hooks: Vec<Webhook> = decode(
            self.rest
                .request(
                    Method::GET,
                    &["channels", channel, "webhooks"],
                    &[],
                    None,
                    true,
                    cancel.clone(),
                )
                .await?,
        )?;
        let hook = match hooks
            .into_iter()
            .find(|h| h.name == name && h.user.as_ref().is_some_and(|u| u.id == self.bot))
        {
            Some(hook) => hook,
            None => decode(
                self.rest
                    .request(
                        Method::POST,
                        &["channels", channel, "webhooks"],
                        &[],
                        Some(json!({"name":name})),
                        true,
                        cancel,
                    )
                    .await?,
            )?,
        };
        ensure!(
            !hook.id.is_empty() && !hook.token.is_empty(),
            "Discord webhook has no credential"
        );
        self.cache.lock().unwrap().hooks.insert(key, hook.clone());
        Ok(hook)
    }
    fn forget(&self, id: &str) {
        self.cache.lock().unwrap().hooks.retain(|_, h| h.id != id);
    }
    async fn owned(&self, id: &str, cancel: CancellationToken) -> Result<Webhook> {
        if let Some(hook) = self.cache.lock().unwrap().hooks.values().find(|h| {
            h.id == id
                && h.application_id.as_deref() == Some(&self.application)
                && !h.token.is_empty()
        }) {
            return Ok(hook.clone());
        }
        let hook: Webhook = decode(
            self.rest
                .request(Method::GET, &["webhooks", id], &[], None, true, cancel)
                .await?,
        )?;
        ensure!(
            hook.application_id.as_deref() == Some(&self.application) && !hook.token.is_empty(),
            "cannot edit a question webhook owned by another application"
        );
        self.cache
            .lock()
            .unwrap()
            .hooks
            .insert(format!("id:{id}"), hook.clone());
        Ok(hook)
    }
    async fn execute(
        &self,
        channel: &str,
        name: &str,
        payload: Value,
        cancel: CancellationToken,
    ) -> Result<String> {
        let hook = self.hook(channel, name, cancel.clone()).await?;
        let result = self
            .rest
            .request(
                Method::POST,
                &["webhooks", &hook.id, &hook.token],
                &[("wait", "true"), ("with_components", "true")],
                Some(payload),
                false,
                cancel,
            )
            .await;
        match result {
            Ok(value) => message_id(value),
            Err(error) => {
                if error.is::<Missing>() {
                    self.forget(&hook.id);
                }
                Err(error)
            }
        }
    }
    async fn edit_card(
        &self,
        path: &[&str],
        authenticated: bool,
        body: &str,
        controls: &[Value],
        cancel: CancellationToken,
    ) -> Result<()> {
        // V2 migration requires embeds:[] (null is rejected by current Discord).
        let payload = json!({"flags":CARD_FLAGS,"content":null,"embeds":[],"components":interaction::card_components(body,controls),"allowed_mentions":{"parse":[]}});
        self.rest
            .request(
                Method::PATCH,
                path,
                &[("with_components", "true")],
                Some(payload),
                authenticated,
                cancel,
            )
            .await?;
        Ok(())
    }
    async fn profile(&self, guild: &str, id: &str, cancel: CancellationToken) -> Option<Profile> {
        let key = format!("{guild}:{id}");
        {
            let cache = self.cache.lock().unwrap();
            if cache
                .profile_expiry
                .get(&key)
                .is_some_and(|t| *t > Instant::now())
            {
                return cache.profiles.get(&key).cloned().flatten();
            }
        }
        let result = self.fetch_profile(guild, id, cancel).await.ok();
        let expiry = result
            .as_ref()
            .map(|p| p.expires)
            .unwrap_or_else(|| Instant::now() + Duration::from_secs(60));
        let mut cache = self.cache.lock().unwrap();
        cache.profiles.insert(key.clone(), result.clone());
        cache.profile_expiry.insert(key, expiry);
        result
    }
    async fn fetch_profile(
        &self,
        guild: &str,
        id: &str,
        cancel: CancellationToken,
    ) -> Result<Profile> {
        if let Ok(member) = self
            .rest
            .request(
                Method::GET,
                &["guilds", guild, "members", id],
                &[],
                None,
                true,
                cancel.clone(),
            )
            .await
            && let Ok(user) = serde_json::from_value::<User>(member["user"].clone())
        {
            let name = member["nick"]
                .as_str()
                .filter(|s| !s.is_empty())
                .unwrap_or(user.name());
            if !name.is_empty() {
                let avatar=member["avatar"].as_str().filter(|s|!s.is_empty()).map(|a|format!("https://cdn.discordapp.com/guilds/{guild}/users/{id}/avatars/{a}.{}?size=256",if a.starts_with("a_") {"gif"}else {"png"})).unwrap_or_else(||avatar(&user));
                return Ok(Profile {
                    name: name.into(),
                    avatar,
                    expires: Instant::now() + Duration::from_secs(300),
                });
            }
        }
        let user: User = decode(
            self.rest
                .request(Method::GET, &["users", id], &[], None, true, cancel)
                .await?,
        )?;
        ensure!(!user.name().is_empty(), "discord user has no display name");
        Ok(Profile {
            name: user.name().into(),
            avatar: avatar(&user),
            expires: Instant::now() + Duration::from_secs(300),
        })
    }
}
fn decode<T: serde::de::DeserializeOwned>(value: Value) -> Result<T> {
    serde_json::from_value(value).map_err(|_| anyhow!("invalid Discord response"))
}
fn message_id(value: Value) -> Result<String> {
    value["id"]
        .as_str()
        .filter(|s| !s.is_empty())
        .map(str::to_string)
        .ok_or_else(|| anyhow!("invalid Discord message response"))
}
fn avatar(user: &User) -> String {
    if let Some(hash) = user.avatar.as_deref().filter(|s| !s.is_empty()) {
        return format!(
            "https://cdn.discordapp.com/avatars/{}/{hash}.{}?size=256",
            user.id,
            if hash.starts_with("a_") { "gif" } else { "png" }
        );
    }
    let index = if user.discriminator.is_empty() || user.discriminator == "0" {
        (user.id.parse::<u64>().unwrap_or(0) >> 22) % 6
    } else {
        user.discriminator.parse::<u64>().unwrap_or(0) % 5
    };
    format!("https://cdn.discordapp.com/embed/avatars/{index}.png")
}
impl Api for Http {
    fn channels(
        &self,
        guild: String,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, Vec<Channel>> {
        Box::pin(async move {
            decode(
                self.rest
                    .request(
                        Method::GET,
                        &["guilds", &guild, "channels"],
                        &[],
                        None,
                        true,
                        cancel,
                    )
                    .await?,
            )
        })
    }
    fn create(
        &self,
        guild: String,
        data: ChannelCreate,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, Channel> {
        Box::pin(async move {
            decode(
                self.rest
                    .request(
                        Method::POST,
                        &["guilds", &guild, "channels"],
                        &[],
                        Some(serde_json::to_value(data)?),
                        true,
                        cancel,
                    )
                    .await?,
            )
        })
    }
    fn rename(
        &self,
        channel: String,
        name: String,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            self.rest
                .request(
                    Method::PATCH,
                    &["channels", &channel],
                    &[],
                    Some(json!({"name":name})),
                    true,
                    cancel,
                )
                .await?;
            Ok(())
        })
    }
    fn send(
        &self,
        channel: String,
        agent: String,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, String> {
        Box::pin(async move {
            let mut payload = json!({"content":body,"components":controls,"flags":4,"allowed_mentions":{"parse":[]}});
            if !agent.is_empty() {
                payload["username"] = json!(text::clip(&agent, 80));
                return self.execute(&channel, "stavlos", payload, cancel).await;
            }
            message_id(
                self.rest
                    .request(
                        Method::POST,
                        &["channels", &channel, "messages"],
                        &[],
                        Some(payload),
                        true,
                        cancel,
                    )
                    .await?,
            )
        })
    }
    fn send_question(
        &self,
        channel: String,
        agent: String,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, (String, String)> {
        Box::pin(async move {
            let hook = self.hook(&channel, "stavlos", cancel.clone()).await?;
            ensure!(
                hook.application_id.as_deref() == Some(&self.application),
                "question webhook is not owned by this Discord application"
            );
            let payload = json!({"username":text::clip(&agent,80),"flags":CARD_FLAGS,"components":interaction::card_components(&body,&controls),"allowed_mentions":{"parse":[]}});
            let id = self.execute(&channel, "stavlos", payload, cancel).await?;
            Ok((id, hook.id))
        })
    }
    fn send_user(
        &self,
        channel: String,
        guild: String,
        user: String,
        body: String,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, String> {
        Box::pin(async move {
            if let Some(profile) = self.profile(&guild, &user, cancel.clone()).await {
                return self.execute(&channel,"stavlos-terminal",json!({"username":text::clip(&profile.name,80),"avatar_url":profile.avatar,"content":body,"flags":4,"allowed_mentions":{"parse":[]}}),cancel).await;
            }
            self.send(
                channel,
                String::new(),
                format!("**You (terminal)**\n{body}"),
                vec![],
                cancel,
            )
            .await
        })
    }
    fn edit(
        &self,
        channel: String,
        message: String,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            self.edit_card(
                &["channels", &channel, "messages", &message],
                true,
                &body,
                &controls,
                cancel,
            )
            .await
        })
    }
    fn edit_webhook(
        &self,
        webhook: String,
        message: String,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            let hook = self.owned(&webhook, cancel.clone()).await?;
            let result = self
                .edit_card(
                    &["webhooks", &hook.id, &hook.token, "messages", &message],
                    false,
                    &body,
                    &controls,
                    cancel,
                )
                .await;
            if result.as_ref().is_err_and(|e| e.is::<Missing>()) {
                self.forget(&webhook);
            }
            result
        })
    }
    fn message(
        &self,
        channel: String,
        message: String,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, Message> {
        Box::pin(async move {
            decode(
                self.rest
                    .request(
                        Method::GET,
                        &["channels", &channel, "messages", &message],
                        &[],
                        None,
                        true,
                        cancel,
                    )
                    .await?,
            )
        })
    }
    fn own_webhook(
        &self,
        channel: String,
        webhook: String,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, bool> {
        Box::pin(async move {
            Ok(self
                .hook(&channel, "stavlos", cancel)
                .await
                .is_ok_and(|h| h.id == webhook))
        })
    }
    fn typing(&self, channel: String, cancel: CancellationToken) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            self.rest
                .request(
                    Method::POST,
                    &["channels", &channel, "typing"],
                    &[],
                    None,
                    true,
                    cancel,
                )
                .await?;
            Ok(())
        })
    }
    fn respond(
        &self,
        interaction: Value,
        response: Value,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            let id = interaction["id"]
                .as_str()
                .ok_or_else(|| anyhow!("missing Discord interaction ID"))?;
            let token = interaction["token"]
                .as_str()
                .ok_or_else(|| anyhow!("missing Discord interaction token"))?;
            self.rest
                .request(
                    Method::POST,
                    &["interactions", id, token, "callback"],
                    &[],
                    Some(response),
                    false,
                    cancel,
                )
                .await?;
            Ok(())
        })
    }
    fn response_edit(
        &self,
        interaction: Value,
        body: String,
        controls: Vec<Value>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            let app = interaction["application_id"]
                .as_str()
                .ok_or_else(|| anyhow!("missing Discord application ID"))?;
            let token = interaction["token"]
                .as_str()
                .ok_or_else(|| anyhow!("missing Discord interaction token"))?;
            let path = ["webhooks", app, token, "messages", "@original"];
            if interaction::question(&interaction).is_some()
                || interaction["message"]["flags"].as_u64().unwrap_or(0) & 32768 != 0
            {
                return self.edit_card(&path, false, &body, &controls, cancel).await;
            }
            self.rest.request(Method::PATCH,&path,&[],Some(json!({"content":body,"components":controls,"allowed_mentions":{"parse":[]}})),false,cancel).await?;
            Ok(())
        })
    }
}
