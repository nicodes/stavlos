use axum::{
    body::{Body, to_bytes},
    http::Request,
};
use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use stavlos_api::{auth::COOKIE, config::Config, engine::Engine, web::Web};
use std::{path::PathBuf, sync::Arc, time::Duration};
use tokio::sync::{mpsc, watch};
use tokio_tungstenite::{
    MaybeTlsStream, WebSocketStream,
    tungstenite::{Message, client::IntoClientRequest},
};
use tower::ServiceExt;
struct Fixture {
    root: PathBuf,
    web: Arc<Web>,
    output: Option<mpsc::Receiver<Value>>,
    stop: watch::Sender<bool>,
    server: tokio::task::JoinHandle<std::io::Result<()>>,
}
impl Fixture {
    async fn new() -> Self {
        let root = std::env::temp_dir().join(format!("stavlos-browser-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir(&root).unwrap();
        let conn = rusqlite::Connection::open(root.join("events.db")).unwrap();
        conn.execute_batch("PRAGMA journal_mode=WAL; PRAGMA user_version=6; CREATE TABLE channels(id TEXT,name TEXT,dir TEXT,created INTEGER,archived INTEGER,title TEXT,last_seq INTEGER); CREATE TABLE events(global INTEGER PRIMARY KEY,seq INTEGER,channel TEXT,agent TEXT,type TEXT,time INTEGER,payload BLOB); CREATE TABLE usage(channel TEXT,agent TEXT,provider TEXT,time INTEGER,fresh INTEGER,cached INTEGER,output INTEGER,cost REAL);").unwrap();
        let now = chrono::Utc::now().timestamp_nanos_opt().unwrap();
        conn.execute(
            "INSERT INTO channels VALUES('c1','test',?, ?,0,'hello',4)",
            rusqlite::params![root.to_str().unwrap(), now],
        )
        .unwrap();
        for (seq, kind, payload) in [
            (1, "channel.created", json!({"name":"test"})),
            (2, "agent.spawned", json!({"name":"main"})),
            (
                3,
                "sheet.written",
                json!({"id":"s1","title":"A <fragment>","author":"a1111","size":9,"hash":"fixture"}),
            ),
            (4, "chat.posted", json!({"text":"hello"})),
        ] {
            conn.execute(
                "INSERT INTO events VALUES(?,?,'c1','',?,?,?)",
                rusqlite::params![seq, seq, kind, now, serde_json::to_vec(&payload).unwrap()],
            )
            .unwrap();
        }
        conn.execute(
            "INSERT INTO usage VALUES('c1','a1111','fixture',?,10,7,3,0.5)",
            [now],
        )
        .unwrap();
        std::fs::create_dir_all(root.join("sheets/c1")).unwrap();
        std::fs::write(root.join("sheets/c1/s1.html"), b"<div>private sheet</div>").unwrap();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        let config = Config {
            port,
            hosts: vec!["box.tailnet.ts.net".into()],
            data: root.clone(),
            parent_pid: None,
        };
        let (output, receiving) = mpsc::channel(16);
        let engine = Engine::new(output, root.clone());
        engine.update(json!({"pid":1,"build":"fixture","data_dir":root,"providers":["fixture"],"prompts":[],"subscriptions":[],"channels":[{"info":{"id":"c1","name":"test","dir":root,"created":chrono::Utc::now().to_rfc3339(),"model":"fixture/m1","root_agent":"coder","archived":false,"live_agents":2,"cost_usd":0.5,"tokens":13,"trust_pending":false,"mode":"ask"},"agents":[{"id":"a1111","channel":"c1","name":"main","state":"idle"},{"id":"a2222","channel":"c1","name":"worker","state":"idle"}]}]}));
        let (stop, stopped) = watch::channel(false);
        let web = Web::new(config, engine, stop.clone());
        let server = tokio::spawn(stavlos_api::server::run(listener, web.router(), stopped));
        Self {
            root,
            web,
            output: Some(receiving),
            stop,
            server,
        }
    }
    fn login(&self) -> (String, String) {
        let code = self.web.auth.code().unwrap();
        self.web.auth.redeem(&code).unwrap()
    }
    async fn request(
        &self,
        method: &str,
        path: &str,
        headers: &[(&str, &str)],
        body: impl Into<Body>,
    ) -> axum::response::Response {
        let mut request = Request::builder()
            .method(method)
            .uri(path)
            .header("host", format!("127.0.0.1:{}", self.web.config.port));
        for (name, value) in headers {
            if *name == "host" {
                continue;
            }
            request = request.header(*name, *value);
        }
        let mut request = request.body(body.into()).unwrap();
        if let Some((_, value)) = headers.iter().find(|(name, _)| *name == "host") {
            request.headers_mut().insert("host", value.parse().unwrap());
        }
        self.web.router().oneshot(request).await.unwrap()
    }
    fn ws_request(&self, cookie: &str, key: &str) -> axum::http::Request<()> {
        let base = self.web.config.url();
        let mut request = format!("ws://127.0.0.1:{}/ws", self.web.config.port)
            .into_client_request()
            .unwrap();
        request
            .headers_mut()
            .insert("origin", base.trim_end_matches('/').parse().unwrap());
        if !cookie.is_empty() {
            request
                .headers_mut()
                .insert("cookie", format!("{COOKIE}={cookie}").parse().unwrap());
        }
        if !key.is_empty() {
            request.headers_mut().insert(
                "sec-websocket-protocol",
                format!("stavlos, key.{key}").parse().unwrap(),
            );
        }
        request
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        self.stop.send_replace(true);
        self.web.auth.clear();
        self.server.abort();
        let _ = std::fs::remove_dir_all(&self.root);
    }
}
type Socket = WebSocketStream<MaybeTlsStream<tokio::net::TcpStream>>;
async fn next(socket: &mut Socket) -> Value {
    loop {
        let message = tokio::time::timeout(Duration::from_secs(3), socket.next())
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        if let Message::Text(text) = message {
            return serde_json::from_str(&text).unwrap();
        }
    }
}
async fn call(socket: &mut Socket, id: i64, method: &str, params: Value) -> Value {
    socket
        .send(Message::Text(
            json!({"jsonrpc":"2.0","v":1,"id":id,"method":method,"params":params})
                .to_string()
                .into(),
        ))
        .await
        .unwrap();
    loop {
        let value = next(socket).await;
        if value["id"] == id {
            return value;
        }
    }
}
#[tokio::test]
async fn host_origin_cookie_and_page_key_remain_separate_boundaries() {
    let f = Fixture::new().await;
    assert_eq!(
        f.request("GET", "/", &[("host", "evil.example")], Body::empty())
            .await
            .status(),
        421
    );
    let response = f
        .request(
            "GET",
            "/some/route",
            &[("host", "box.tailnet.ts.net")],
            Body::empty(),
        )
        .await;
    assert_eq!(response.status(), 200);
    let csp = response.headers()["content-security-policy"]
        .to_str()
        .unwrap();
    assert!(csp.contains("default-src 'none'") && csp.contains("frame-ancestors 'none'"));
    drop(response);
    assert_eq!(
        f.request("GET", "/assets/missing.js", &[], Body::empty())
            .await
            .status(),
        404
    );
    let code = f.web.auth.code().unwrap();
    for origin in [
        "",
        "null",
        "https://evil.example",
        "http://127.0.0.1",
        "http://127.0.0.1:1",
    ] {
        assert_eq!(
            f.request(
                "POST",
                "/api/session",
                &[("origin", origin)],
                json!({"code":code}).to_string()
            )
            .await
            .status(),
            403
        );
    }
    let response = f
        .request(
            "POST",
            "/api/session",
            &[("origin", "https://box.tailnet.ts.net")],
            json!({"code":code}).to_string(),
        )
        .await;
    assert_eq!(response.status(), 200);
    let cookie = response.headers()["set-cookie"]
        .to_str()
        .unwrap()
        .to_owned();
    for flag in ["HttpOnly", "SameSite=Strict", "Secure"] {
        assert!(cookie.contains(flag));
    }
    let value: Value =
        serde_json::from_slice(&to_bytes(response.into_body(), 1024).await.unwrap()).unwrap();
    let cookie = cookie.split(';').next().unwrap();
    assert_eq!(
        f.request("GET", "/api/session", &[("cookie", cookie)], Body::empty())
            .await
            .status(),
        401
    );
    assert_eq!(
        f.request(
            "GET",
            "/api/session",
            &[
                ("cookie", cookie),
                ("x-stavlos-key", value["key"].as_str().unwrap())
            ],
            Body::empty()
        )
        .await
        .status(),
        204
    );
    assert_eq!(
        f.request(
            "POST",
            "/api/session",
            &[("origin", "https://box.tailnet.ts.net")],
            json!({"code":code}).to_string()
        )
        .await
        .status(),
        401
    );
}
#[tokio::test]
async fn sheets_use_the_pinned_capability_vector_opaque_origin_and_owned_file() {
    let f = Fixture::new().await;
    let (cookie, key) = f.login();
    let session = f.web.auth.session(&cookie).unwrap();
    let cookie = format!("{COOKIE}={cookie}");
    let mut vector = session.clone();
    vector.key = stavlos_api::auth::hash("page-key");
    assert_eq!(
        stavlos_api::sheets::token(&vector, "c1", "s1"),
        "804f0abb02ac34a100da02a8251ea369465f5aad7153f1f47d1d39375e9e393c"
    );
    let token = stavlos_api::sheets::token(&session, "c1", "s1");
    let path = format!("/sheets/c1/s1?t={token}");
    assert_eq!(
        f.request("GET", &path, &[], Body::empty()).await.status(),
        401
    );
    assert_eq!(
        f.request(
            "GET",
            "/sheets/c1/s1",
            &[("cookie", &cookie), ("x-stavlos-key", &key)],
            Body::empty()
        )
        .await
        .status(),
        403
    );
    assert_eq!(
        f.request(
            "GET",
            &path,
            &[("cookie", &cookie), ("sec-fetch-dest", "document")],
            Body::empty()
        )
        .await
        .status(),
        403
    );
    let response = f
        .request(
            "GET",
            &path,
            &[("cookie", &cookie), ("sec-fetch-dest", "iframe")],
            Body::empty(),
        )
        .await;
    assert_eq!(response.status(), 200);
    let csp = response.headers()["content-security-policy"]
        .to_str()
        .unwrap();
    assert!(csp.starts_with("sandbox allow-scripts; default-src 'none'"));
    assert!(!csp.contains("allow-same-origin"));
    assert!(
        !response
            .headers()
            .contains_key("cross-origin-opener-policy")
    );
    let body = String::from_utf8(
        to_bytes(response.into_body(), 3 << 20)
            .await
            .unwrap()
            .to_vec(),
    )
    .unwrap();
    assert!(
        body.contains("A &lt;fragment&gt;")
            && body.contains("/sheet.css")
            && body.contains("private sheet")
    );
    std::fs::remove_file(f.root.join("sheets/c1/s1.html")).unwrap();
    std::os::unix::fs::symlink(f.root.join("events.db"), f.root.join("sheets/c1/s1.html")).unwrap();
    assert_eq!(
        f.request("GET", &path, &[("cookie", &cookie)], Body::empty())
            .await
            .status(),
        404
    );
    let css = f.request("GET", "/sheet.css", &[], Body::empty()).await;
    assert_eq!(
        css.headers()["cross-origin-resource-policy"],
        "cross-origin"
    );
}
#[tokio::test]
async fn real_websocket_queries_and_replay_are_rust_owned_and_revoked_on_signout() {
    let f = Fixture::new().await;
    let (cookie, key) = f.login();
    for (cookie, key) in [
        ("", ""),
        (cookie.as_str(), ""),
        (cookie.as_str(), "guessed"),
    ] {
        let error = tokio_tungstenite::connect_async(f.ws_request(cookie, key))
            .await
            .unwrap_err();
        let tokio_tungstenite::tungstenite::Error::Http(response) = error else {
            panic!("unexpected handshake error")
        };
        assert_eq!(response.status(), 401);
    }
    let (mut socket, response) = tokio_tungstenite::connect_async(f.ws_request(&cookie, &key))
        .await
        .unwrap();
    assert_eq!(response.headers()["sec-websocket-protocol"], "stavlos");
    assert_eq!(
        call(&mut socket, 1, "attach", json!({"client":"discord"})).await["result"]["version"],
        1
    );
    for (index, method) in [
        "daemon.shutdown",
        "channel.set_mode",
        "provider.login.start",
        "prompt.reply",
        "no.such.method",
    ]
    .iter()
    .enumerate()
    {
        assert_eq!(
            call(&mut socket, 10 + index as i64, method, json!({})).await["error"]["code"],
            -32004
        );
    }
    assert_eq!(
        call(&mut socket, 20, "channel.list", json!({})).await["result"]["channels"][0]["id"],
        "c1"
    );
    let usage = call(&mut socket, 21, "usage.series", json!({"buckets":1})).await;
    assert_eq!(usage["result"]["tokens"], json!([13]));
    assert_eq!(usage["result"]["cost_usd"], json!([0.5]));
    socket.send(Message::Text(json!({"jsonrpc":"2.0","v":1,"id":30,"method":"subscribe","params":{"channel":"c1","from":1}}).to_string().into())).await.unwrap();
    for seq in 1..=4 {
        let event = next(&mut socket).await;
        assert_eq!(event["method"], "event");
        assert_eq!(event["params"]["event"]["seq"], seq);
    }
    assert_eq!(next(&mut socket).await["result"]["seq"], 4);
    f.web.auth.signout(&cookie);
    let closed = tokio::time::timeout(Duration::from_secs(2), socket.next())
        .await
        .unwrap();
    assert!(closed.is_none() || matches!(closed, Some(Ok(Message::Close(_))) | Some(Err(_))));
}
#[tokio::test]
async fn resolved_posts_are_journaled_before_native_execution_and_never_replayed() {
    let mut f = Fixture::new().await;
    let (cookie, key) = f.login();
    let (mut socket, _) = tokio_tungstenite::connect_async(f.ws_request(&cookie, &key))
        .await
        .unwrap();
    let mut output = f.output.take().unwrap();
    let engine = f.web.engine.clone();
    let root = f.root.clone();
    let worker = tokio::spawn(async move {
        let plan = output.recv().await.unwrap();
        assert_eq!(plan["kind"], "delivery");
        assert_eq!(plan["targets"], json!(["a2222", "a1111"]));
        assert_eq!(plan["names"], json!(["worker", "main"]));
        assert_eq!(plan["text"], "Hello  @literal");
        let id = plan["id"].as_str().unwrap();
        assert_eq!(
            serde_json::from_slice::<Value>(
                &std::fs::read(root.join("web-inputs").join(id)).unwrap()
            )
            .unwrap(),
            plan
        );
        engine.result(id, None);
    });
    let posted = call(
        &mut socket,
        1,
        "channel.post",
        json!({"channel":"c1","text":" @worker, @main: Hello  @literal"}),
    )
    .await;
    assert_eq!(posted["result"]["to"], json!(["worker", "main"]));
    worker.await.unwrap();
    assert_eq!(
        std::fs::read_dir(f.root.join("web-inputs"))
            .unwrap()
            .count(),
        0
    );
    assert_eq!(
        call(
            &mut socket,
            2,
            "channel.post",
            json!({"channel":"c1","text":"@unknown nope"})
        )
        .await["error"]["code"],
        -32602
    );
}

#[tokio::test]
async fn browser_capacity_is_held_until_response_drop_and_disable_revokes_live_sockets() {
    let f = Fixture::new().await;
    let mut held = Vec::new();
    for _ in 0..32 {
        let response = f.request("GET", "/", &[], Body::empty()).await;
        assert_eq!(response.status(), 200);
        held.push(response);
    }
    assert_eq!(
        f.request("GET", "/", &[], Body::empty()).await.status(),
        503
    );
    held.pop();
    assert_eq!(
        f.request("GET", "/healthz", &[], Body::empty())
            .await
            .status(),
        200
    );
    drop(held);
    let (cookie, key) = f.login();
    let mut sockets = Vec::new();
    for _ in 0..8 {
        let (socket, _) = tokio_tungstenite::connect_async(f.ws_request(&cookie, &key))
            .await
            .unwrap();
        sockets.push(socket);
    }
    let error = tokio_tungstenite::connect_async(f.ws_request(&cookie, &key))
        .await
        .unwrap_err();
    let tokio_tungstenite::tungstenite::Error::Http(response) = error else {
        panic!("unexpected handshake error")
    };
    assert_eq!(response.status(), 503);
    f.web.stopped.send_replace(true);
    f.web.auth.clear();
    for mut socket in sockets {
        let closed = tokio::time::timeout(Duration::from_secs(2), socket.next())
            .await
            .unwrap();
        assert!(closed.is_none() || matches!(closed, Some(Ok(Message::Close(_))) | Some(Err(_))));
    }
    assert!(f.web.auth.session(&cookie).is_none());
}

#[tokio::test]
async fn input_operation_outlives_its_caller_and_restart_never_replays_uncertain_work() {
    let mut f = Fixture::new().await;
    let mut output = f.output.take().unwrap();
    let engine = f.web.engine.clone();
    let task = tokio::spawn(async move {
        engine.deliver(json!({"channel":"c1","text":"accepted","targets":["a1111"],"names":["main"],"message_kind":"request","board":false})).await
    });
    let plan = output.recv().await.unwrap();
    let id = plan["id"].as_str().unwrap().to_owned();
    assert!(f.root.join("web-inputs").join(&id).exists());
    task.abort();
    let _ = task.await;
    f.web
        .engine
        .result(&id, Some("execution result is uncertain; inspect history"));
    tokio::time::sleep(Duration::from_millis(20)).await;
    assert!(f.root.join("web-inputs").join(&id).exists());
    let (sender, mut receiver) = mpsc::channel(16);
    let restarted = Engine::new(sender, f.root.clone());
    tokio::time::sleep(Duration::from_millis(20)).await;
    assert!(receiver.try_recv().is_err());
    drop(restarted);
}
