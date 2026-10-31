use crate::{
    auth::{Auth, COOKIE, Session},
    config::Config,
    database::Database,
    engine::Engine,
    rpc::{self, Client},
    static_files::Static,
};
use axum::{
    Json, Router,
    body::{Body, to_bytes},
    extract::{
        Request, State, WebSocketUpgrade,
        ws::{Message, WebSocket},
    },
    http::{HeaderMap, HeaderValue, Method, StatusCode},
    response::{IntoResponse, Response},
};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::sync::{Semaphore, watch};
pub struct Web {
    pub config: Config,
    pub auth: Auth,
    pub engine: Arc<Engine>,
    pub database: Database,
    files: Static,
    admission: Arc<Semaphore>,
    sockets: Arc<Semaphore>,
    viewers: Mutex<HashMap<[u8; 32], usize>>,
    pub stopped: watch::Sender<bool>,
}
impl Web {
    pub fn new(config: Config, engine: Arc<Engine>, stopped: watch::Sender<bool>) -> Arc<Self> {
        Arc::new(Self {
            database: Database::new(config.data.join("events.db")),
            config,
            auth: Auth::default(),
            engine,
            files: Static::default(),
            admission: Arc::new(Semaphore::new(32)),
            sockets: Arc::new(Semaphore::new(32)),
            viewers: Mutex::new(HashMap::new()),
            stopped,
        })
    }
    pub fn router(self: &Arc<Self>) -> Router {
        Router::new().fallback(handle).with_state(self.clone())
    }
}
fn text(status: StatusCode, message: &str) -> Response {
    (status, message.to_owned()).into_response()
}
fn header<'a>(headers: &'a HeaderMap, key: &str) -> &'a str {
    headers.get(key).and_then(|v| v.to_str().ok()).unwrap_or("")
}
fn cookie(headers: &HeaderMap) -> &str {
    for part in header(headers, "cookie").split(';') {
        if let Some((name, value)) = part.trim().split_once('=')
            && name == COOKIE
        {
            return value;
        }
    }
    ""
}
fn key(headers: &HeaderMap) -> &str {
    let mut key = header(headers, "x-stavlos-key");
    for protocol in header(headers, "sec-websocket-protocol")
        .split(',')
        .map(str::trim)
    {
        if let Some(value) = protocol.strip_prefix("key.") {
            key = value;
        }
    }
    key
}
struct Viewer {
    web: Arc<Web>,
    cookie: [u8; 32],
}
impl Drop for Viewer {
    fn drop(&mut self) {
        let mut viewers = self.web.viewers.lock().unwrap();
        if let Some(count) = viewers.get_mut(&self.cookie) {
            *count -= 1;
            if *count == 0 {
                viewers.remove(&self.cookie);
            }
        }
    }
}
async fn handle(State(web): State<Arc<Web>>, request: Request) -> Response {
    let Ok(permit) = web.admission.clone().try_acquire_owned() else {
        return text(StatusCode::SERVICE_UNAVAILABLE, "web UI is busy");
    };
    let host = header(request.headers(), "host").to_owned();
    if !web.config.host_allowed(&host) {
        return text(
            StatusCode::MISDIRECTED_REQUEST,
            "this name is not one the stavlos web UI answers to; list it under web.hosts in stavlos.json",
        );
    }
    let mut response = dispatch(web, request, &host).await;
    for (name, value) in [
        ("x-content-type-options", "nosniff"),
        ("referrer-policy", "no-referrer"),
        ("cross-origin-resource-policy", "same-origin"),
    ] {
        if !response.headers().contains_key(name) {
            response
                .headers_mut()
                .insert(name, HeaderValue::from_static(value));
        }
    }
    if !response.headers().contains_key("cache-control") {
        response
            .headers_mut()
            .insert("cache-control", HeaderValue::from_static("no-store"));
    }
    if !response.headers().contains_key("content-security-policy")
        || !response
            .headers()
            .get("content-security-policy")
            .unwrap()
            .to_str()
            .unwrap_or("")
            .starts_with("sandbox")
    {
        response.headers_mut().insert(
            "cross-origin-opener-policy",
            HeaderValue::from_static("same-origin"),
        );
    }
    crate::response::hold(response, permit)
}
async fn dispatch(web: Arc<Web>, request: Request, host: &str) -> Response {
    let path = request.uri().path().to_owned();
    let read = matches!(*request.method(), Method::GET | Method::HEAD);
    let is_head = *request.method() == Method::HEAD;
    if read && path == "/healthz" {
        return if is_head {
            StatusCode::OK.into_response()
        } else {
            text(StatusCode::OK, "ok\n")
        };
    }
    let cookie = cookie(request.headers()).to_owned();
    let session = web.auth.session(&cookie);
    if path == "/api/session" {
        if read {
            return if session
                .as_ref()
                .is_some_and(|s| Auth::holds(s, key(request.headers())))
            {
                StatusCode::NO_CONTENT.into_response()
            } else {
                text(StatusCode::UNAUTHORIZED, "not signed in")
            };
        }
        if !matches!(*request.method(), Method::POST | Method::DELETE) {
            return StatusCode::METHOD_NOT_ALLOWED.into_response();
        }
        let origin = header(request.headers(), "origin");
        if !web.config.origin_allowed(origin) {
            return text(StatusCode::FORBIDDEN, "origin refused");
        }
        if *request.method() == Method::DELETE {
            web.auth.signout(&cookie);
            let mut response = StatusCode::NO_CONTENT.into_response();
            response.headers_mut().insert(
                "set-cookie",
                HeaderValue::from_static(
                    "stavlos_session=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict",
                ),
            );
            return response;
        }
        let secure = origin.starts_with("https://");
        let raw = match tokio::time::timeout(
            Duration::from_secs(10),
            to_bytes(request.into_body(), 1024),
        )
        .await
        {
            Ok(Ok(raw)) => raw,
            _ => return text(StatusCode::BAD_REQUEST, "bad request"),
        };
        let value: Value = match serde_json::from_slice(&raw) {
            Ok(value) => value,
            Err(_) => return text(StatusCode::BAD_REQUEST, "bad request"),
        };
        let Some(code) = value["code"].as_str() else {
            return text(StatusCode::BAD_REQUEST, "bad request");
        };
        let Some((cookie, key)) = web.auth.redeem(code) else {
            return text(
                StatusCode::UNAUTHORIZED,
                "that code is wrong, used or expired; open the web UI from stavlos again",
            );
        };
        let mut response = Json(json!({"key":key})).into_response();
        let value = format!(
            "stavlos_session={cookie}; Path=/; Max-Age=2592000; HttpOnly; SameSite=Strict{}",
            if secure { "; Secure" } else { "" }
        );
        response
            .headers_mut()
            .insert("set-cookie", HeaderValue::from_str(&value).unwrap());
        return response;
    }
    if read && path == "/ws" {
        let Some(session) = session else {
            return text(StatusCode::UNAUTHORIZED, "not signed in");
        };
        if !Auth::holds(&session, key(request.headers())) {
            return text(StatusCode::UNAUTHORIZED, "not signed in");
        }
        if !web
            .config
            .origin_allowed(header(request.headers(), "origin"))
        {
            return text(StatusCode::FORBIDDEN, "origin refused");
        }
        let Ok(permit) = web.sockets.clone().try_acquire_owned() else {
            return text(
                StatusCode::SERVICE_UNAVAILABLE,
                "too many browser connections",
            );
        };
        let cookie_hash = crate::auth::hash(&cookie);
        {
            let mut viewers = web.viewers.lock().unwrap();
            let count = viewers.entry(cookie_hash).or_default();
            if *count >= 8 {
                return text(
                    StatusCode::SERVICE_UNAVAILABLE,
                    "too many connections for this session",
                );
            }
            *count += 1;
        }
        let viewer = Viewer {
            web: web.clone(),
            cookie: cookie_hash,
        };
        let (parts, _) = request.into_parts();
        let mut parts = parts;
        let upgrade =
            match <WebSocketUpgrade as axum::extract::FromRequestParts<()>>::from_request_parts(
                &mut parts,
                &(),
            )
            .await
            {
                Ok(upgrade) => upgrade,
                Err(rejection) => return rejection.into_response(),
            };
        return upgrade
            .protocols(["stavlos"])
            .max_message_size(4 << 20)
            .max_frame_size(4 << 20)
            .on_upgrade(move |socket| async move {
                let _permit = permit;
                let _viewer = viewer;
                session_socket(web, socket, session).await;
            });
    }
    if read && path.starts_with("/sheets/") {
        let Some(session) = session else {
            return text(StatusCode::UNAUTHORIZED, "not signed in");
        };
        let components: Vec<_> = path.trim_start_matches('/').split('/').collect();
        if components.len() != 3 {
            return StatusCode::NOT_FOUND.into_response();
        }
        let channel = components[1];
        let id = components[2];
        let offered = form_urlencoded::parse(request.uri().query().unwrap_or("").as_bytes())
            .find(|(name, _)| name == "t")
            .map(|(_, v)| v.into_owned())
            .unwrap_or_default();
        if !crate::sheets::holds(&session, channel, id, &offered) {
            return text(StatusCode::FORBIDDEN, "a sheet is opened by the app");
        }
        let destination = header(request.headers(), "sec-fetch-dest");
        if !destination.is_empty() && destination != "iframe" {
            return text(StatusCode::FORBIDDEN, "a sheet is shown inside the app");
        }
        let Ok(facts) = web.engine.facts() else {
            return StatusCode::NOT_FOUND.into_response();
        };
        if !facts["channels"].as_array().is_some_and(|channels| {
            channels
                .iter()
                .any(|c| c["info"]["id"].as_str() == Some(channel))
        }) {
            return StatusCode::NOT_FOUND.into_response();
        }
        let Ok(body) = crate::sheets::read(&web.database, &web.config.data, channel, id).await
        else {
            return StatusCode::NOT_FOUND.into_response();
        };
        let length = body.len();
        let mut response = Body::from(if is_head { Vec::new() } else { body }).into_response();
        response
            .headers_mut()
            .insert("content-length", HeaderValue::from(length));
        response.headers_mut().insert(
            "content-type",
            HeaderValue::from_static("text/html; charset=utf-8"),
        );
        let policy = format!(
            "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline' http://{host}/sheet.css https://{host}/sheet.css; img-src data: blob:; font-src data:; media-src data: blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"
        );
        response.headers_mut().insert(
            "content-security-policy",
            HeaderValue::from_str(&policy).unwrap(),
        );
        return response;
    }
    if !read {
        return StatusCode::METHOD_NOT_ALLOWED.into_response();
    }
    let mut response = web
        .files
        .response(&path, request.method(), request.headers());
    let policy = format!(
        "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self' ws://{host} wss://{host}; manifest-src 'self'; frame-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
    );
    response.headers_mut().insert(
        "content-security-policy",
        HeaderValue::from_str(&policy).unwrap(),
    );
    if path == "/sheet.css" {
        response.headers_mut().insert(
            "cross-origin-resource-policy",
            HeaderValue::from_static("cross-origin"),
        );
        response.headers_mut().insert(
            "cache-control",
            HeaderValue::from_static("public, max-age=3600"),
        );
    }
    response
}
async fn send(socket: &mut WebSocket, value: &Value) -> Result<(), ()> {
    let text = value.to_string();
    if text.len() > 8 << 20 {
        return Err(());
    }
    tokio::time::timeout(
        Duration::from_secs(10),
        socket.send(Message::Text(text.into())),
    )
    .await
    .map_err(|_| ())?
    .map_err(|_| ())
}
async fn session_socket(web: Arc<Web>, mut socket: WebSocket, session: Session) {
    let mut signed_out = session.stop.subscribe();
    let mut stopped = web.stopped.subscribe();
    if *signed_out.borrow() || *stopped.borrow() {
        return;
    }
    let expiry = tokio::time::sleep_until(tokio::time::Instant::from_std(session.expired));
    tokio::pin!(expiry);
    tokio::select! {_=signed_out.changed()=>{},_=stopped.changed()=>{},_=&mut expiry=>{},_=socket_loop(&web,&mut socket)=>{}}
    let _ = tokio::time::timeout(Duration::from_secs(1), socket.send(Message::Close(None))).await;
}
async fn replay(
    web: &Web,
    socket: &mut WebSocket,
    client: &mut Client,
    channel: &str,
    until: Option<i64>,
) -> Result<(), ()> {
    loop {
        let Some(last) = client.subscriptions.get(channel).copied() else {
            return Ok(());
        };
        if until.is_some_and(|to| last >= to) {
            return Ok(());
        }
        let events = web
            .database
            .events(
                channel.into(),
                last.saturating_add(1),
                if until.is_some() { 512 } else { 32 },
            )
            .await
            .map_err(|_| ())?;
        if events.is_empty() {
            return Ok(());
        }
        let mut delivered = 0;
        for event in events {
            let Some(seq) = event["seq"].as_i64() else {
                return Err(());
            };
            if until.is_some_and(|to| seq > to) {
                return Ok(());
            }
            client.observed(&event).map_err(|_| ())?;
            send(
                socket,
                &json!({"jsonrpc":"2.0","method":"event","params":{"event":event}}),
            )
            .await?;
            client.subscriptions.insert(channel.into(), seq);
            delivered += 1;
        }
        if until.is_none() || delivered == 0 {
            return Ok(());
        }
    }
}
async fn socket_loop(web: &Web, socket: &mut WebSocket) {
    let mut client = Client::default();
    let mut notices = web.engine.notifications.subscribe();
    let mut tick = tokio::time::interval(Duration::from_millis(30));
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
                                     message=socket.recv()=>{
                                      let raw=match message{Some(Ok(Message::Text(text)))=>text.as_bytes().to_vec(),Some(Ok(Message::Binary(bytes)))=>bytes.to_vec(),Some(Ok(Message::Ping(_)|Message::Pong(_)))=>continue,_=>return};
                                      let request:Value=match serde_json::from_slice(&raw){Ok(request)=>request,Err(_)=>{if send(socket,&json!({"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"invalid JSON"}})).await.is_err(){return;}continue;}};
                                      if !request.is_object() {if send(socket,&json!({"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}})).await.is_err(){return;}continue;}
                       let result=rpc::call(&web.engine,&web.database,&mut client,&request).await;
                                      if request["method"]=="subscribe"&&let Ok(value)=&result{let channel=request["params"]["channel"].as_str().unwrap_or("");if replay(web,socket,&mut client,channel,value["seq"].as_i64()).await.is_err(){return;}}
                                      if let Some(id)=request.get("id").filter(|id|!id.is_null()){
                                       let response=match result{Ok(result)=>json!({"jsonrpc":"2.0","id":id,"result":result}),Err(error)=>json!({"jsonrpc":"2.0","id":id,"error":error})};if send(socket,&response).await.is_err(){return;}
                                      }
                                     },
                                     _=tick.tick()=>{let channels:Vec<_>=client.subscriptions.keys().cloned().collect();for channel in channels{if replay(web,socket,&mut client,&channel,None).await.is_err(){return;}}},
                                     notice=notices.recv()=>{match notice{Ok(mut value)=>{if value["method"]=="stream"&&!value["params"]["channel"].as_str().is_some_and(|id|client.subscriptions.contains_key(id)){continue;}
                            if value["method"]=="stream" {
                let channel=value["params"]["channel"].as_str().unwrap_or("").to_owned();
                if replay(web,socket,&mut client,&channel,None).await.is_err(){return;}
                if let Ok(facts)=web.engine.facts() && client.seed_models(&facts,&channel).is_err(){return;}
        if !client.stream_allowed(&value){continue;}
                if let Some(params)=value["params"].as_object_mut(){params.remove("_epoch");}
               }
               if send(socket,&value).await.is_err(){return;}},Err(tokio::sync::broadcast::error::RecvError::Lagged(_))=>{},Err(_)=>return}}
                                    }
    }
}
