use super::*;
use sha2::{Digest, Sha256};
use std::sync::atomic::{AtomicBool, Ordering};
pub const CLIENT_ID: &str = "app_EMoamEEZ73f0CkXaXp7hrann";
pub const ISSUER: &str = "https://auth.openai.com";
#[derive(Clone)]
pub struct ChatGpt {
    pub issuer: String,
    pub port: u16,
    pub client: Client,
    pub callback: Option<nativeapi::Program>,
}
impl ChatGpt {
    pub fn new(client: Client, callback: Option<nativeapi::Program>) -> Self {
        Self {
            issuer: ISSUER.into(),
            port: 1455,
            client,
            callback,
        }
    }
    async fn exchange(
        &self,
        code: &str,
        redirect: &str,
        verifier: &str,
        cancel: &CancellationToken,
    ) -> Result<Tokens> {
        let response = self
            .client
            .form(
                &format!("{}/oauth/token", self.issuer),
                &[
                    ("grant_type", "authorization_code"),
                    ("code", code),
                    ("redirect_uri", redirect),
                    ("client_id", CLIENT_ID),
                    ("code_verifier", verifier),
                ],
                cancel,
            )
            .await?;
        let status = response.status().as_u16();
        if !response.status().is_success() {
            return Err(message(format!(
                "chatgpt: token exchange failed ({status}): {}",
                read_error(response, cancel).await?
            )));
        }
        decode::<TokenResponse>(response, cancel).await?.tokens("")
    }
    async fn start_browser(&self, cancel: &CancellationToken) -> Result<Arc<Pending>> {
        let verifier = random(64)?;
        let challenge = URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()));
        let state = random(32)?;
        let port = if self.port == 0 { 1455 } else { self.port };
        let redirect = format!("http://localhost:{port}/auth/callback");
        let program = self
            .callback
            .clone()
            .map(Ok)
            .unwrap_or_else(nativeapi::Program::source)
            .map_err(message)?;
        let native=nativeapi::Callback::start(&program,port,&state,cancel).await.map_err(|error|message(format!("chatgpt: cannot start the Rust callback on localhost:{port} ({error}); is another login running? The headless method does not need the port")))?;
        let browser = Arc::new(Browser {
            native: Arc::new(native),
            verifier,
            redirect: redirect.clone(),
            expired: AtomicBool::new(false),
            stop_timer: CancellationToken::new(),
        });
        let weak = Arc::downgrade(&browser);
        let stop = browser.stop_timer.clone();
        tokio::spawn(async move {
            tokio::select! {biased;_=stop.cancelled()=>{},_=tokio::time::sleep(Duration::from_secs(600))=>{if let Some(browser)=weak.upgrade(){browser.expired.store(true,Ordering::SeqCst);browser.native.cancel();browser.native.close().await;}}}
        });
        let mut url =
            reqwest::Url::parse(&format!("{}/oauth/authorize", self.issuer)).map_err(message)?;
        let params = BTreeMap::from([
            ("response_type", "code"),
            ("client_id", CLIENT_ID),
            ("redirect_uri", redirect.as_str()),
            ("scope", "openid profile email offline_access"),
            ("code_challenge", challenge.as_str()),
            ("code_challenge_method", "S256"),
            ("id_token_add_organizations", "true"),
            ("codex_cli_simplified_flow", "true"),
            ("state", state.as_str()),
            ("originator", "stavlos"),
        ]);
        url.query_pairs_mut().extend_pairs(params);
        Ok(Arc::new(Pending {
            provider: "openai".into(),
            method: METHOD_BROWSER.into(),
            url: url.into(),
            instructions:
                "Complete the sign-in in your browser; it will redirect back to this machine."
                    .into(),
            expires_in: Duration::from_secs(600),
            browser: Some(browser),
            ..Default::default()
        }))
    }
    async fn wait_browser(
        &self,
        pending: Arc<Pending>,
        cancel: &CancellationToken,
    ) -> Result<Tokens> {
        let browser = pending.browser.as_ref().unwrap();
        let _release = browser.native.cancellation().drop_guard();
        let received = tokio::select! {biased;_=cancel.cancelled()=>Err(Error::Cancelled),_=tokio::time::sleep(pending.expires_in)=>Err(Error::Expired),received=browser.native.result()=>{
            if browser.expired.load(Ordering::SeqCst)||received.error=="expired"{Err(Error::Expired)}else if !received.error.is_empty(){Err(message(received.error))}else{Ok(received.code)}
        }};
        browser.stop_timer.cancel();
        browser.native.close().await;
        self.exchange(&received?, &browser.redirect, &browser.verifier, cancel)
            .await
    }
    async fn start_device(&self, cancel: &CancellationToken) -> Result<Arc<Pending>> {
        let response = self
            .client
            .json(
                &format!("{}/api/accounts/deviceauth/usercode", self.issuer),
                &serde_json::json!({"client_id":CLIENT_ID}),
                cancel,
            )
            .await?;
        let status = response.status().as_u16();
        if !response.status().is_success() {
            let error = read_error(response, cancel).await?;
            return Err(message(if matches!(status, 400 | 403) {
                format!(
                    "chatgpt: {error} (ChatGPT → Settings → Security → enable \"Device code authorization for Codex\", or use the browser method)"
                )
            } else {
                format!("chatgpt: device code request failed ({status}): {error}")
            }));
        }
        let device: DeviceResponse = decode(response, cancel).await?;
        if device.device_auth_id.is_empty() || device.user_code.is_empty() {
            return Err(message("chatgpt: device code response missing fields"));
        }
        let interval = device
            .interval
            .as_ref()
            .map(|value| value.get().trim_matches('"').parse::<u64>())
            .and_then(|value| value.ok())
            .filter(|value| *value > 0)
            .unwrap_or(5);
        Ok(Arc::new(Pending {
            provider: "openai".into(),
            method: METHOD_DEVICE.into(),
            url: format!("{}/codex/device", self.issuer),
            code: device.user_code.trim().to_uppercase(),
            instructions:
                "Sign in with the ChatGPT account that has your Plus or Pro subscription.".into(),
            expires_in: Duration::from_secs(900),
            interval: Duration::from_secs(interval),
            device_auth_id: device.device_auth_id,
            ..Default::default()
        }))
    }
    async fn wait_device(
        &self,
        pending: Arc<Pending>,
        cancel: &CancellationToken,
    ) -> Result<Tokens> {
        let deadline = tokio::time::Instant::now() + pending.expires_in;
        while tokio::time::Instant::now() < deadline {
            let response=self.client.json(&format!("{}/api/accounts/deviceauth/token",self.issuer),&serde_json::json!({"device_auth_id":pending.device_auth_id,"user_code":pending.code}),cancel).await?;
            let status = response.status().as_u16();
            if response.status().is_success() {
                let token: DeviceToken = decode(response, cancel).await?;
                return self
                    .exchange(
                        &token.authorization_code,
                        &format!("{}/deviceauth/callback", self.issuer),
                        &token.code_verifier,
                        cancel,
                    )
                    .await;
            }
            if !matches!(status, 403 | 404) {
                return Err(message(format!(
                    "chatgpt: device authorization failed ({status}): {}",
                    read_error(response, cancel).await?
                )));
            }
            drop(response);
            sleep(
                pending.interval.saturating_add(self.client.poll_margin),
                cancel,
            )
            .await?;
        }
        Err(Error::Expired)
    }
}
pub(super) struct Browser {
    native: Arc<nativeapi::Callback>,
    verifier: String,
    redirect: String,
    expired: AtomicBool,
    stop_timer: CancellationToken,
}
impl Browser {
    pub(super) fn close(&self) {
        self.stop_timer.cancel();
        self.native.cancel();
    }
    pub(super) async fn shutdown(&self) {
        self.native.close().await;
    }
}
impl Drop for Browser {
    fn drop(&mut self) {
        self.close();
    }
}
fn random(length: usize) -> Result<String> {
    let mut bytes = vec![0; length];
    getrandom::fill(&mut bytes).map_err(message)?;
    Ok(URL_SAFE_NO_PAD.encode(bytes))
}
wire_struct!(DeviceResponse{device_auth_id:"device_auth_id":String=>required,user_code:"user_code":String=>required,interval:"interval":Option<Box<serde_json::value::RawValue>> =>raw});
wire_struct!(DeviceToken{authorization_code:"authorization_code":String=>required,code_verifier:"code_verifier":String=>required});
impl Flow for ChatGpt {
    fn provider(&self) -> &str {
        "openai"
    }
    fn label(&self) -> &str {
        "ChatGPT Plus/Pro subscription"
    }
    fn methods(&self) -> Vec<Method> {
        vec![
            Method::new(METHOD_BROWSER, "ChatGPT Plus/Pro (browser)"),
            Method::new(METHOD_DEVICE, "ChatGPT Plus/Pro (headless: URL + code)"),
        ]
    }
    fn start(&self, method: String, cancel: CancellationToken) -> FlowFuture<'_, Arc<Pending>> {
        Box::pin(async move {
            match method.as_str() {
                "" | METHOD_BROWSER => self.start_browser(&cancel).await,
                METHOD_DEVICE => self.start_device(&cancel).await,
                _ => Err(message(format!(
                    "chatgpt: unsupported login method {method:?}"
                ))),
            }
        })
    }
    fn wait(&self, pending: Arc<Pending>, cancel: CancellationToken) -> FlowFuture<'_, Tokens> {
        Box::pin(async move {
            if pending.browser.is_some() {
                self.wait_browser(pending, &cancel).await
            } else {
                self.wait_device(pending, &cancel).await
            }
        })
    }
    fn refresh(&self, refresh: String, cancel: CancellationToken) -> FlowFuture<'_, Tokens> {
        Box::pin(async move {
            let response = self
                .client
                .form(
                    &format!("{}/oauth/token", self.issuer),
                    &[
                        ("grant_type", "refresh_token"),
                        ("refresh_token", &refresh),
                        ("client_id", CLIENT_ID),
                    ],
                    &cancel,
                )
                .await?;
            let status = response.status().as_u16();
            if !response.status().is_success() {
                return Err(message(format!(
                    "chatgpt: token refresh failed ({status}): {}",
                    read_error(response, &cancel).await?
                )));
            }
            decode::<TokenResponse>(response, &cancel)
                .await?
                .tokens(&refresh)
        })
    }
}
