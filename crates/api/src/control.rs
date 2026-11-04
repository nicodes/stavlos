use crate::{config::Config, engine::Engine, web::Web};
use serde_json::{Value, json};
use std::{sync::Arc, time::Duration};
use tokio::{
    io::{AsyncBufRead, AsyncBufReadExt},
    sync::{Mutex, watch},
};
type Listener = (
    watch::Sender<bool>,
    tokio::task::JoinHandle<std::io::Result<()>>,
);
pub struct Control {
    pub web: Arc<Web>,
    listener: Mutex<Option<Listener>>,
    error: Mutex<String>,
}
impl Control {
    pub fn new(config: Config, engine: Arc<Engine>) -> Arc<Self> {
        let (stop, _) = watch::channel(true);
        Arc::new(Self {
            web: Web::new(config, engine, stop),
            listener: Mutex::new(None),
            error: Mutex::new(String::new()),
        })
    }
    pub async fn enabled(&self) -> bool {
        self.listener
            .lock()
            .await
            .as_ref()
            .is_some_and(|(_, task)| !task.is_finished())
    }
    pub async fn enable(&self) -> Result<(), String> {
        let mut listener = self.listener.lock().await;
        if listener
            .as_ref()
            .is_some_and(|(_, task)| !task.is_finished())
        {
            return Ok(());
        }
        self.web.auth.clear();
        let tcp =
            tokio::net::TcpListener::bind((std::net::Ipv4Addr::LOCALHOST, self.web.config.port))
                .await
                .map_err(|e| format!("cannot listen on 127.0.0.1:{}: {e}", self.web.config.port))?;
        let (stop, stopped) = watch::channel(false);
        self.web.stopped.send_replace(false);
        let task = tokio::spawn(crate::server::run(tcp, self.web.router(), stopped));
        *listener = Some((stop, task));
        self.error.lock().await.clear();
        Ok(())
    }
    pub async fn disable(&self) {
        self.web.stopped.send_replace(true);
        self.web.auth.clear();
        let owned = self.listener.lock().await.take();
        if let Some((stop, mut task)) = owned {
            stop.send_replace(true);
            if tokio::time::timeout(Duration::from_secs(3), &mut task)
                .await
                .is_err()
            {
                task.abort();
                let _ = task.await;
            }
        }
    }
    pub async fn execute(&self, request: &Value) -> Value {
        let id = request["id"].clone();
        let op = request["op"].as_str().unwrap_or("");
        let mut open = String::new();
        let result = match op {
            "enable" => self.enable().await,
            "disable" => {
                self.disable().await;
                Ok(())
            }
            "open" | "status" => Ok(()),
            _ => Err("unsupported API control operation".into()),
        };
        let result = if result.is_ok() && op == "open" {
            if self.enabled().await {
                match self.web.auth.code() {
                    Ok(code) => {
                        open = format!("{}#code={code}", self.web.config.url());
                        Ok(())
                    }
                    Err(e) => Err(e.into()),
                }
            } else {
                Err("the web UI is disabled".into())
            }
        } else {
            result
        };
        if let Err(error) = &result {
            *self.error.lock().await = error.clone();
        }
        json!({"kind":"control_result","id":id,"status":{"enabled":self.enabled().await,"url":self.web.config.url(),"error":self.error.lock().await.clone()},"open_url":open,"error":result.err().unwrap_or_default()})
    }
}
pub async fn line<R: AsyncBufRead + Unpin>(
    reader: &mut R,
    maximum: usize,
) -> std::io::Result<Option<Vec<u8>>> {
    let mut line = Vec::new();
    loop {
        let buffer = reader.fill_buf().await?;
        if buffer.is_empty() {
            return if line.is_empty() {
                Ok(None)
            } else {
                Err(std::io::Error::other("incomplete control message"))
            };
        }
        let count = buffer
            .iter()
            .position(|b| *b == b'\n')
            .map(|p| p + 1)
            .unwrap_or(buffer.len());
        if line.len() + count > maximum {
            return Err(std::io::Error::other("control message exceeds its bound"));
        }
        let complete = buffer[count - 1] == b'\n';
        line.extend_from_slice(&buffer[..count]);
        reader.consume(count);
        if complete {
            line.pop();
            return Ok(Some(line));
        }
    }
}
pub async fn read(
    control: Arc<Control>,
    output: tokio::sync::mpsc::Sender<Value>,
) -> crate::error::Result<()> {
    let mut reader = tokio::io::BufReader::new(tokio::io::stdin());
    while let Some(raw) = line(&mut reader, 16 << 20).await? {
        let request: Value = serde_json::from_slice(&raw)
            .map_err(|_| crate::error::invalid("unreadable control message"))?;
        match request["kind"].as_str() {
            Some("snapshot") if request["facts"].is_object() => {
                control.web.engine.update(request["facts"].clone())
            }
            Some("notification") => {
                let value = request["value"].clone();
                if matches!(
                    value["method"].as_str(),
                    Some("stream" | "prompt" | "changed")
                ) {
                    control.web.engine.notify(value);
                }
            }
            Some("delivery_result") => control.web.engine.result(
                request["id"].as_str().unwrap_or(""),
                request["error"].as_str(),
            ),
            Some("control") => {
                let result = control.execute(&request).await;
                output
                    .send(result)
                    .await
                    .map_err(|_| crate::error::fail(503, "control output unavailable"))?;
            }
            _ => return Err(crate::error::invalid("unsupported control message")),
        }
    }
    Ok(())
}
