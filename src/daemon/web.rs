use super::{
    frontdoors,
    hub::Feed,
    owner::{Daemon, ModelEpoch},
};
use crate::{
    agent::routing::{PreparedPost, Routing},
    nativeapi::{
        Program,
        web::{Deliver, Snapshot, WebProcess},
    },
    protocol,
    tools::ServiceFuture,
};
use anyhow::{Result, ensure};
use serde_json::{Value, json};
use std::{
    collections::BTreeMap,
    io::Read,
    os::unix::fs::OpenOptionsExt,
    sync::{Arc, Mutex, Weak},
};
use tokio_util::sync::CancellationToken;
pub struct Server {
    owner: Weak<Daemon>,
    program: Program,
    port: u16,
    hosts: Vec<String>,
    operation: tokio::sync::Mutex<()>,
    process: Mutex<Option<Arc<WebProcess>>>,
    epochs: Arc<Mutex<BTreeMap<String, ModelEpoch>>>,
}
impl Server {
    pub fn new(
        owner: &Arc<Daemon>,
        program: Program,
        port: u16,
        hosts: Vec<String>,
    ) -> Result<Arc<Self>> {
        ensure!(port > 0, "web port must be 1–65535");
        Ok(Arc::new(Self {
            owner: Arc::downgrade(owner),
            program,
            port,
            hosts,
            operation: tokio::sync::Mutex::new(()),
            process: Mutex::new(None),
            epochs: owner.model_epochs.clone(),
        }))
    }
    pub fn install(self: &Arc<Self>, owner: &Daemon) {
        let feed: Arc<dyn Feed> = self.clone();
        owner.hub.feed("web".into(), Arc::downgrade(&feed));
        *owner.web.write().unwrap() = Some(self.clone());
    }
    fn process(&self) -> Option<Arc<WebProcess>> {
        self.process.lock().unwrap().clone()
    }
    fn status(&self) -> protocol::WebStatus {
        self.process()
            .map(|process| process.status())
            .unwrap_or_else(|| protocol::WebStatus {
                url: format!("http://127.0.0.1:{}/", self.port),
                ..Default::default()
            })
    }
    async fn ensure_started(&self) -> Result<Arc<WebProcess>> {
        if let Some(process) = self.process()
            && process.alive()
        {
            return Ok(process);
        }
        let owner = self
            .owner
            .upgrade()
            .ok_or_else(|| anyhow::anyhow!("daemon is stopped"))?;
        let weak = self.owner.clone();
        let snapshot: Snapshot = Arc::new(move || {
            weak.upgrade()
                .ok_or_else(|| anyhow::anyhow!("daemon is stopped"))?
                .observations()
        });
        let weak = self.owner.clone();
        let deliver: Deliver = Arc::new(move |value, cancel| {
            let weak = weak.clone();
            Box::pin(async move {
                weak.upgrade()
                    .ok_or_else(|| anyhow::anyhow!("daemon is stopped"))?
                    .deliver_web(value, &cancel)
            })
        });
        let process = Arc::new(
            WebProcess::start(
                &self.program,
                &owner.paths.data,
                self.port,
                &self.hosts,
                snapshot,
                deliver,
                &owner.cancel,
            )
            .await?,
        );
        *self.process.lock().unwrap() = Some(process.clone());
        Ok(process)
    }
    fn save(&self, enabled: bool) {
        let Some(owner) = self.owner.upgrade() else {
            return;
        };
        let raw: &[u8] = if enabled {
            b"{\"enabled\":true}"
        } else {
            b"{\"enabled\":false}"
        };
        if let Err(error) =
            crate::statefile::write_atomic(&owner.paths.data.join("web.json"), raw, 0o600, false)
        {
            eprintln!("web: saving state: {error}");
        }
    }
    pub async fn restore(&self) -> Result<()> {
        let _operation = self.operation.lock().await;
        let owner = self
            .owner
            .upgrade()
            .ok_or_else(|| anyhow::anyhow!("daemon is stopped"))?;
        let file = match std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
            .open(owner.paths.data.join("web.json"))
        {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(error) => return Err(error.into()),
        };
        ensure!(
            file.metadata()?.is_file(),
            "web state is not a regular file"
        );
        let mut raw = Vec::new();
        file.take(4097).read_to_end(&mut raw)?;
        ensure!(raw.len() <= 4096, "web state exceeds its bound");
        let state: Value = serde_json::from_slice(&raw)?;
        if state["enabled"] == true {
            self.ensure_started()
                .await?
                .call("enable", &owner.cancel)
                .await?;
        }
        Ok(())
    }
}
impl Feed for Server {
    fn notify(&self, line: &[u8]) {
        if line.len() > 1 << 16 {
            return;
        }
        let Some(process) = self.process() else {
            return;
        };
        let Ok(mut value) = serde_json::from_slice::<Value>(line) else {
            return;
        };
        if value["method"] == "stream" {
            let id = value["params"]["agent"].as_str().unwrap_or("");
            let sequence = self.epochs.lock().unwrap().get(id).map(|epoch| epoch.seq);
            if let Some(sequence) = sequence {
                value["params"]["_epoch"] = json!(sequence);
            }
        }
        process.notify(value);
    }
}
impl frontdoors::Web for Server {
    fn call<'a>(
        &'a self,
        method: &'a str,
        cancel: CancellationToken,
    ) -> ServiceFuture<'a, protocol::WebStatus> {
        Box::pin(async move {
            let _operation = tokio::select! {guard=self.operation.lock()=>guard,_=cancel.cancelled()=>anyhow::bail!("context canceled")};
            let result = match method {
                protocol::MWEB_STATUS => Ok(self.status()),
                protocol::MWEB_ENABLE => {
                    let process = self.ensure_started().await?;
                    process.call("enable", &cancel).await?;
                    self.save(true);
                    process.call("open", &cancel).await
                }
                protocol::MWEB_DISABLE => {
                    let status = if let Some(process) = self.process()
                        && process.alive()
                    {
                        process.call("disable", &cancel).await?
                    } else {
                        self.status()
                    };
                    self.save(false);
                    Ok(status)
                }
                protocol::MWEB_OPEN => {
                    let process = self
                        .process()
                        .ok_or_else(|| anyhow::anyhow!("the web UI is disabled"))?;
                    process.call("open", &cancel).await
                }
                _ => anyhow::bail!("unsupported web operation"),
            };
            if matches!(method, protocol::MWEB_ENABLE | protocol::MWEB_DISABLE)
                && let Some(owner) = self.owner.upgrade()
            {
                owner.changed(protocol::CHANGED_WEB);
            }
            result
        })
    }
    fn close(&self) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            let _operation = self.operation.lock().await;
            let process = self.process.lock().unwrap().take();
            if let Some(process) = process {
                process.close().await;
            }
            Ok(())
        })
    }
}
crate::wire_struct!(Delivery {
    id:"id":String=>required,request_id:"request_id":String=>required,channel:"channel":String=>required,
    targets:"targets":crate::wire::Slice<String> =>required,names:"names":crate::wire::Slice<String> =>required,
    text:"text":String=>required,kind:"message_kind":String=>required,
});
impl Daemon {
    pub fn observations(&self) -> Result<Value> {
        let channels = self
            .channels()
            .into_iter()
            .map(|channel| json!({"info":channel.info(),"agents":channel.tree()}))
            .collect::<Vec<_>>();
        let subscriptions = self
            .registry
            .usage
            .all()
            .into_iter()
            .map(|(id, usage)| {
                let status = self.registry.status(&id);
                let connected = status.as_ref().is_some_and(|status| status.connected);
                let name = status
                    .map(|status| status.name)
                    .unwrap_or_else(|| id.clone());
                json!({"id":id,"name":name,"connected":connected,"usage":usage})
            })
            .collect::<Vec<_>>();
        Ok(
            json!({"pid":std::process::id(),"build":option_env!("STAVLOS_BUILD").unwrap_or(env!("CARGO_PKG_VERSION")),"data_dir":self.paths.data.to_string_lossy(),"providers":self.registry.providers(),"channels":channels,"prompts":self.prompts.pending(""),"subscriptions":subscriptions,"model_epochs":self.model_epochs.lock().unwrap().clone()}),
        )
    }
    pub fn deliver_web(&self, value: Value, cancel: &CancellationToken) -> Result<()> {
        let delivery: Delivery = serde_json::from_value(value)?;
        let channel = self.channel(&delivery.channel)?;
        let prepared = PreparedPost {
            id: delivery.id,
            request_id: delivery.request_id,
            text: delivery.text,
            kind: delivery.kind,
            targets: delivery.targets.into_vec(),
            names: delivery.names.into_vec(),
        };
        if let Some(board) = channel.info().board {
            let home = self.channel(&board.source)?;
            Routing {
                core: home.core.clone(),
            }
            .accept_prepared_board(&channel.core, prepared, cancel)
        } else {
            Routing {
                core: channel.core.clone(),
            }
            .accept_prepared(prepared, cancel)
        }
    }
}
