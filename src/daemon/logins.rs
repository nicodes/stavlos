use super::owner::Daemon;
use crate::{agent::names, model::registry, oauth, protocol};
use anyhow::{Result, ensure};
use std::{
    sync::Arc,
    time::{Duration, Instant},
};
use tokio_util::sync::CancellationToken;
pub(super) struct Login {
    provider: String,
    pending: Arc<oauth::Pending>,
    started: Instant,
}
pub fn provider_info(status: registry::Status) -> protocol::ProviderInfo {
    protocol::ProviderInfo {
        id: status.id,
        name: status.name,
        connected: status.connected,
        kind: status.kind,
        priority: status.priority,
        models: status.models as i64,
        label: status.label,
        account: status.account,
        methods: status
            .methods
            .into_iter()
            .map(|method| protocol::LoginMethod {
                id: method.id,
                label: method.label,
            })
            .collect(),
    }
}
impl Daemon {
    pub async fn login_start(
        &self,
        provider: &str,
        method: &str,
    ) -> Result<protocol::LoginStartResult> {
        let stale = {
            let mut logins = self.logins.lock().unwrap();
            let stale = logins
                .iter()
                .filter(|(_, login)| login.started.elapsed() > Duration::from_secs(1800))
                .map(|(id, _)| id.clone())
                .collect::<Vec<_>>();
            stale
                .into_iter()
                .filter_map(|id| logins.remove(&id))
                .collect::<Vec<_>>()
        };
        for login in stale {
            login.pending.close().await;
        }
        ensure!(
            self.logins.lock().unwrap().len() < 128,
            "too many sign-ins are in progress"
        );
        let flow = self.registry.flow(provider)?;
        let pending = flow.start(method.into(), self.cancel.child_token()).await?;
        let id = names::new_id("l")?;
        let result = protocol::LoginStartResult {
            id: id.clone(),
            provider: provider.into(),
            method: pending.method.clone(),
            url: pending.url.clone(),
            code: pending.code.clone(),
            instructions: pending.instructions.clone(),
            expires_in: pending.expires_in.as_secs() as i64,
        };
        self.logins.lock().unwrap().insert(
            id,
            Arc::new(Login {
                provider: provider.into(),
                pending,
                started: Instant::now(),
            }),
        );
        Ok(result)
    }
    pub fn login_key(&self, id: &str, key: &str) -> Result<()> {
        let login = self
            .logins
            .lock()
            .unwrap()
            .get(id)
            .cloned()
            .ok_or_else(|| anyhow::anyhow!("no login in progress with id {id:?}"))?;
        Ok(login.pending.deliver(key)?)
    }
    pub async fn login_wait(
        &self,
        id: &str,
        cancel: CancellationToken,
    ) -> Result<protocol::ProviderInfo> {
        let login = self
            .logins
            .lock()
            .unwrap()
            .get(id)
            .cloned()
            .ok_or_else(|| anyhow::anyhow!("no login in progress with id {id:?}"))?;
        let flow = self.registry.flow(&login.provider)?;
        match flow.wait(login.pending.clone(), cancel.clone()).await {
            Ok(tokens) => {
                self.logins.lock().unwrap().remove(id);
                login.pending.close().await;
                self.registry.save_login(&login.provider, &tokens)?;
                Ok(provider_info(
                    self.registry
                        .status(&login.provider)
                        .ok_or_else(|| anyhow::anyhow!("provider disappeared during sign-in"))?,
                ))
            }
            Err(error) => {
                if !cancel.is_cancelled() {
                    self.logins.lock().unwrap().remove(id);
                    login.pending.close().await;
                }
                Err(error.into())
            }
        }
    }
    pub(super) async fn close_logins(&self) {
        let logins = std::mem::take(&mut *self.logins.lock().unwrap());
        for (_, login) in logins {
            login.pending.close().await;
        }
    }
}
