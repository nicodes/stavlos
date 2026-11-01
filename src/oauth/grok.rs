use super::*;
pub const CLIENT_ID: &str = "b1a00492-073a-47ea-816f-4c329264a828";
const SCOPE: &str = "openid profile email offline_access grok-cli:access api:access";
const GRANT: &str = "urn:ietf:params:oauth:grant-type:device_code";
#[derive(Clone)]
pub struct Grok {
    pub device_url: String,
    pub token_url: String,
    pub client: Client,
}
impl Grok {
    pub fn new(client: Client) -> Self {
        Self {
            device_url: "https://auth.x.ai/oauth2/device/code".into(),
            token_url: "https://auth.x.ai/oauth2/token".into(),
            client,
        }
    }
}
wire_struct!(DeviceResponse{device_code:"device_code":String=>required,user_code:"user_code":String=>required,verification_uri:"verification_uri":String=>required,complete:"verification_uri_complete":String=>required,expires_in:"expires_in":f64=>float,interval:"interval":f64=>float});
impl Flow for Grok {
    fn provider(&self) -> &str {
        "xai"
    }
    fn label(&self) -> &str {
        "SuperGrok subscription"
    }
    fn methods(&self) -> Vec<Method> {
        vec![Method::new(
            METHOD_DEVICE,
            "SuperGrok subscription (device code)",
        )]
    }
    fn start(&self, method: String, cancel: CancellationToken) -> FlowFuture<'_, Arc<Pending>> {
        Box::pin(async move {
            if !method.is_empty() && method != METHOD_DEVICE {
                return Err(message(format!(
                    "grok: unsupported login method {method:?}"
                )));
            }
            let response = self
                .client
                .form(
                    &self.device_url,
                    &[
                        ("client_id", CLIENT_ID),
                        ("scope", SCOPE),
                        ("referrer", "stavlos"),
                    ],
                    &cancel,
                )
                .await?;
            let status = response.status().as_u16();
            if !response.status().is_success() {
                return Err(message(format!(
                    "grok: device code request failed ({status}): {}",
                    read_error(response, &cancel).await?
                )));
            }
            let device: DeviceResponse = decode(response, &cancel).await?;
            if device.device_code.is_empty()
                || device.user_code.is_empty()
                || device.verification_uri.is_empty()
            {
                return Err(message("grok: device code response missing fields"));
            }
            Ok(Arc::new(Pending {
                provider: "xai".into(),
                method: METHOD_DEVICE.into(),
                url: if device.complete.is_empty() {
                    device.verification_uri
                } else {
                    device.complete
                },
                code: device.user_code.trim().to_uppercase(),
                instructions: "Sign in with the X account that has your SuperGrok subscription."
                    .into(),
                expires_in: if device.expires_in > 0. {
                    Duration::from_secs(device.expires_in as u64)
                } else {
                    Duration::from_secs(300)
                },
                interval: if device.interval >= 1. {
                    Duration::from_secs(device.interval as u64)
                } else {
                    Duration::from_secs(5)
                },
                device_code: device.device_code,
                ..Default::default()
            }))
        })
    }
    fn wait(&self, pending: Arc<Pending>, cancel: CancellationToken) -> FlowFuture<'_, Tokens> {
        Box::pin(async move {
            let deadline = tokio::time::Instant::now() + pending.expires_in;
            let mut interval = pending.interval;
            while tokio::time::Instant::now() < deadline {
                let response = self
                    .client
                    .form(
                        &self.token_url,
                        &[
                            ("grant_type", GRANT),
                            ("client_id", CLIENT_ID),
                            ("device_code", &pending.device_code),
                        ],
                        &cancel,
                    )
                    .await?;
                let status = response.status().as_u16();
                let token: TokenResponse = match decode(response, &cancel).await {
                    Ok(token) => token,
                    Err(Error::Cancelled) => return Err(Error::Cancelled),
                    Err(_) => TokenResponse::default(),
                };
                if (200..300).contains(&status) && !token.access_token.is_empty() {
                    return token.tokens("");
                }
                match token.error.as_str() {
                    "authorization_pending" => {}
                    "slow_down" => interval = interval.saturating_add(Duration::from_secs(5)),
                    "access_denied" | "authorization_denied" => return Err(Error::Denied),
                    "expired_token" => return Err(Error::Expired),
                    _ => {
                        return Err(message(format!(
                            "grok: device token exchange failed ({status}): {}",
                            if token.error_description.is_empty() {
                                &token.error
                            } else {
                                &token.error_description
                            }
                        )));
                    }
                }
                sleep(interval.saturating_add(self.client.poll_margin), &cancel).await?;
            }
            Err(Error::Expired)
        })
    }
    fn refresh(&self, refresh: String, cancel: CancellationToken) -> FlowFuture<'_, Tokens> {
        Box::pin(async move {
            let response = self
                .client
                .form(
                    &self.token_url,
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
                    "grok: token refresh failed ({status}): {}",
                    read_error(response, &cancel).await?
                )));
            }
            decode::<TokenResponse>(response, &cancel)
                .await?
                .tokens(&refresh)
        })
    }
}
