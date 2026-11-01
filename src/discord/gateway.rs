//! One bounded outbound Gateway session; reconnection belongs to Service.
use super::http::Rest;
use anyhow::{Result, anyhow, bail, ensure};
use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use std::{
    sync::{Arc, OnceLock},
    time::Duration,
};
use tokio::{sync::mpsc, time::Instant};
use tokio_tungstenite::tungstenite::{Message, protocol::WebSocketConfig};
use tokio_util::sync::CancellationToken;
#[derive(Clone)]
pub struct Event {
    pub kind: String,
    pub data: Value,
}
pub type Connected = Arc<dyn Fn(bool) + Send + Sync>;
struct Disconnected(Connected);
impl Drop for Disconnected {
    fn drop(&mut self) {
        (self.0)(false);
    }
}
static IDENTIFY: OnceLock<tokio::sync::Mutex<Option<Instant>>> = OnceLock::new();
pub async fn run(
    rest: Arc<Rest>,
    token: String,
    events: mpsc::Sender<Event>,
    connected: Connected,
    cancel: CancellationToken,
) -> Result<()> {
    let _disconnected = Disconnected(connected.clone());
    tokio::select! {biased;_=cancel.cancelled()=>Ok(()),result=session(rest,token,events,connected)=>result}
}
async fn session(
    rest: Arc<Rest>,
    token: String,
    events: mpsc::Sender<Event>,
    connected: Connected,
) -> Result<()> {
    let metadata = rest
        .request(
            reqwest::Method::GET,
            &["gateway", "bot"],
            &[],
            None,
            true,
            CancellationToken::new(),
        )
        .await?;
    ensure!(
        metadata["session_start_limit"]["remaining"]
            .as_u64()
            .is_some_and(|n| n > 0),
        "Discord session start limit reached; wait for its reset before connecting"
    );
    let mut url = reqwest::Url::parse(
        metadata["url"]
            .as_str()
            .ok_or_else(|| anyhow!("invalid Discord gateway URL"))?,
    )
    .map_err(|_| anyhow!("invalid Discord gateway URL"))?;
    ensure!(
        url.scheme() == "wss"
            || url.scheme() == "ws"
                && url.host_str().is_some_and(|h| h
                    .parse::<std::net::IpAddr>()
                    .is_ok_and(|ip| ip.is_loopback())),
        "Discord Gateway requires WSS"
    );
    ensure!(
        url.username().is_empty() && url.password().is_none() && url.fragment().is_none(),
        "invalid Discord gateway URL"
    );
    url.set_query(Some("v=10&encoding=json"));
    // Wait before opening the connection, so HELLO heartbeats are never delayed by this gate.
    let mut identify_gate = IDENTIFY
        .get_or_init(|| tokio::sync::Mutex::new(None))
        .lock()
        .await;
    if let Some(until) = *identify_gate {
        tokio::time::sleep_until(until).await;
    }
    let config = WebSocketConfig::default()
        .max_message_size(Some(16 * 1024 * 1024))
        .max_frame_size(Some(4 * 1024 * 1024));
    let (mut socket, _) = tokio::time::timeout(
        Duration::from_secs(20),
        tokio_tungstenite::connect_async_with_config(url.as_str(), Some(config), true),
    )
    .await
    .map_err(|_| anyhow!("Discord Gateway handshake timed out"))?
    .map_err(|_| anyhow!("Discord Gateway connection failed"))?;
    let hello = tokio::time::timeout(Duration::from_secs(20), socket.next())
        .await
        .map_err(|_| anyhow!("Discord Gateway hello timed out"))?
        .ok_or_else(|| anyhow!("Discord Gateway disconnected"))?
        .map_err(|_| anyhow!("Discord Gateway receive failed"))?;
    let hello = match hello {
        Message::Text(data) => serde_json::from_str::<Value>(&data)
            .map_err(|_| anyhow!("invalid Discord Gateway hello"))?,
        _ => bail!("invalid Discord Gateway hello"),
    };
    ensure!(hello["op"] == 10, "invalid Discord Gateway hello");
    let interval = hello["d"]["heartbeat_interval"]
        .as_u64()
        .filter(|n| *n > 0 && *n <= 300_000)
        .ok_or_else(|| anyhow!("invalid Discord heartbeat interval"))?;
    let interval = Duration::from_millis(interval);
    let mut random = [0u8; 8];
    getrandom::fill(&mut random).map_err(|_| anyhow!("Discord heartbeat jitter unavailable"))?;
    let jitter = interval.mul_f64(u64::from_le_bytes(random) as f64 / u64::MAX as f64);
    let mut heartbeat = tokio::time::interval_at(Instant::now() + jitter, interval);
    heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    let identify=json!({"op":2,"d":{"token":token,"intents":33281,"properties":{"os":std::env::consts::OS,"browser":"stavlos","device":"stavlos"}}}).to_string();
    ensure!(
        identify.len() <= 4096,
        "Discord Gateway identify payload is too large"
    );
    tokio::time::timeout(
        Duration::from_secs(10),
        socket.send(Message::Text(identify.into())),
    )
    .await
    .map_err(|_| anyhow!("Discord Gateway write timed out"))?
    .map_err(|_| anyhow!("Discord Gateway write failed"))?;
    *identify_gate = Some(Instant::now() + Duration::from_secs(5));
    drop(identify_gate);
    let deadline = Instant::now() + Duration::from_secs(20);
    let mut ready = false;
    let mut sequence = Value::Null;
    let mut awaiting_ack = false;
    loop {
        tokio::select! {biased;
            _=tokio::time::sleep_until(deadline),if !ready=>bail!("Discord Gateway ready timed out"),
            _=heartbeat.tick()=>{
                ensure!(!awaiting_ack,"Discord Gateway heartbeat was not acknowledged");
                awaiting_ack=true;
                write(&mut socket,json!({"op":1,"d":sequence})).await?;
            },
            message=socket.next()=>{
                let message=message.ok_or_else(||anyhow!("Discord Gateway disconnected"))?.map_err(|_|anyhow!("Discord Gateway receive failed"))?;
                let payload=match message {
                    Message::Text(data)=>serde_json::from_str::<Value>(&data).map_err(|_|anyhow!("invalid Discord Gateway event"))?,
                    Message::Ping(_)=>{socket.flush().await.map_err(|_|anyhow!("Discord Gateway write failed"))?;continue;},
                    Message::Pong(_)=>continue,
                    Message::Close(_)=>bail!("Discord Gateway disconnected; check bot token and privileged intents"),
                    _=>bail!("unexpected Discord Gateway encoding"),
                };
                if payload["s"].is_u64() {sequence=payload["s"].clone();}
                match payload["op"].as_i64() {
                    Some(0)=>{
                        let kind=payload["t"].as_str().ok_or_else(||anyhow!("invalid Discord Gateway dispatch"))?;
                        if kind=="READY" {ensure!(!ready,"duplicate Discord Gateway ready");ready=true;connected(true);}
                        if ready&&matches!(kind,"MESSAGE_CREATE"|"INTERACTION_CREATE") {events.try_send(Event{kind:kind.into(),data:payload["d"].clone()}).map_err(|_|anyhow!("Discord Gateway event queue is full or closed; reconnecting"))?;}
                    },
                    Some(1)=>{awaiting_ack=true;write(&mut socket,json!({"op":1,"d":sequence})).await?;},
                    Some(7|9)=>bail!("Discord Gateway requested a new connection"),
                    Some(11)=>awaiting_ack=false,
                    _=>{},
                }
            }
        }
    }
}
async fn write(
    socket: &mut tokio_tungstenite::WebSocketStream<
        tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>,
    >,
    value: Value,
) -> Result<()> {
    tokio::time::timeout(
        Duration::from_secs(10),
        socket.send(Message::Text(value.to_string().into())),
    )
    .await
    .map_err(|_| anyhow!("Discord Gateway write timed out"))?
    .map_err(|_| anyhow!("Discord Gateway write failed"))?;
    Ok(())
}
