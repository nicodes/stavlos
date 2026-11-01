use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use stavlos::{
    daemon::{Daemon, frontdoors::Web, web::Server},
    model::registry::Registry,
    nativeapi::{Program, web::WebProcess},
    paths::Paths,
};
use std::{
    collections::BTreeMap, os::unix::fs::PermissionsExt, path::PathBuf, sync::Arc, time::Duration,
};
use tokio_tungstenite::{
    MaybeTlsStream, WebSocketStream,
    tungstenite::{Message, client::IntoClientRequest},
};
use tokio_util::sync::CancellationToken;
type Socket = WebSocketStream<MaybeTlsStream<tokio::net::TcpStream>>;
fn api() -> Program {
    let root = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    let mut candidates = Vec::new();
    if let Some(path) = std::env::var_os("STAVLOS_API_BINARY") {
        candidates.push(PathBuf::from(path));
    }
    if let Some(path) = std::env::var_os("CARGO_TARGET_DIR") {
        let path = PathBuf::from(path);
        candidates.push(
            if path.is_absolute() {
                path
            } else {
                root.join(path)
            }
            .join("debug/stavlos-api"),
        );
    }
    candidates.push(root.join("api/target/debug/stavlos-api"));
    candidates.push(
        std::env::current_exe()
            .unwrap()
            .parent()
            .unwrap()
            .parent()
            .unwrap()
            .join("stavlos-api"),
    );
    Program::external(
        candidates
            .into_iter()
            .find(|path| path.is_file())
            .expect("build the independent api binary before owner integration tests"),
    )
    .unwrap()
}
struct Fixture {
    _root: tempfile::TempDir,
    owner: Arc<Daemon>,
    web: Arc<Server>,
    port: u16,
    channel: Arc<stavlos::agent::runtime::Channel>,
}
impl Fixture {
    async fn new() -> Self {
        let root = tempfile::tempdir().unwrap();
        let home = root.path().join("home");
        std::fs::create_dir(&home).unwrap();
        let paths = Paths {
            config: home.join("config"),
            data: home.join("data"),
            cache: home.join("cache"),
            socket: root.path().join("owner.sock"),
            shared_skills: home.join("skills"),
        };
        let owner = Daemon::open(
            paths,
            home,
            Registry::new(None, None, BTreeMap::new()).unwrap(),
        )
        .await
        .unwrap();
        let project = root.path().join("project");
        std::fs::create_dir(&project).unwrap();
        let channel = owner
            .create(
                &project.to_string_lossy(),
                "",
                "",
                "native-web-fixture",
                &CancellationToken::new(),
            )
            .unwrap();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        drop(listener);
        let web = Server::new(&owner, api(), port, Vec::new()).unwrap();
        web.install(&owner);
        Self {
            _root: root,
            owner,
            web,
            port,
            channel,
        }
    }
    fn base(&self) -> String {
        format!("http://127.0.0.1:{}", self.port)
    }
    async fn enable(&self) -> (String, String) {
        let status = self
            .web
            .call("web.enable", CancellationToken::new())
            .await
            .unwrap();
        assert!(status.enabled);
        let code = status.open_url.split_once("#code=").unwrap().1;
        let client = stavlos::httpx::builder().no_proxy().build().unwrap();
        let response = client
            .post(format!("{}/api/session", self.base()))
            .header("origin", self.base())
            .json(&json!({"code":code}))
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 200);
        let cookie = response.headers()["set-cookie"]
            .to_str()
            .unwrap()
            .split(';')
            .next()
            .unwrap()
            .to_owned();
        let body: Value = response.json().await.unwrap();
        (cookie, body["key"].as_str().unwrap().to_owned())
    }
    async fn socket(&self, cookie: &str, key: &str) -> Socket {
        let mut request = format!("ws://127.0.0.1:{}/ws", self.port)
            .into_client_request()
            .unwrap();
        request
            .headers_mut()
            .insert("origin", self.base().parse().unwrap());
        request
            .headers_mut()
            .insert("cookie", cookie.parse().unwrap());
        request.headers_mut().insert(
            "sec-websocket-protocol",
            format!("stavlos, key.{key}").parse().unwrap(),
        );
        tokio_tungstenite::connect_async(request).await.unwrap().0
    }
}
async fn next(socket: &mut Socket) -> Value {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            let message = socket.next().await.expect("websocket closed").unwrap();
            if let Message::Text(text) = message {
                return serde_json::from_str(&text).unwrap();
            }
        }
    })
    .await
    .unwrap()
}
async fn rpc(socket: &mut Socket, id: i64, method: &str, params: Value) -> Value {
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
async fn actual_axum_process_authenticates_and_keeps_browser_rpc_inside_its_scope() {
    let f = Fixture::new().await;
    assert!(
        !f.web
            .call("web.status", CancellationToken::new())
            .await
            .unwrap()
            .enabled
    );
    let (cookie, key) = f.enable().await;
    let client = stavlos::httpx::builder().no_proxy().build().unwrap();
    assert_eq!(
        client
            .get(format!("{}/api/session", f.base()))
            .send()
            .await
            .unwrap()
            .status(),
        401
    );
    let mut socket = f.socket(&cookie, &key).await;
    let status = rpc(&mut socket, 1, "daemon.status", json!({})).await;
    assert_eq!(status["result"]["channels"], 1);
    assert_eq!(status["result"]["agents"], 1);
    let list = rpc(&mut socket, 2, "channel.list", json!({})).await;
    assert_eq!(list["result"]["channels"][0]["id"], f.channel.core.id);
    for (id, method) in [
        (3, "channel.set_mode"),
        (4, "provider.list"),
        (5, "prompt.reply"),
        (6, "unknown.private"),
    ] {
        let response = rpc(&mut socket, id, method, json!({})).await;
        assert_eq!(response["error"]["code"], -32004);
    }
    let response = client
        .delete(format!("{}/api/session", f.base()))
        .header("origin", f.base())
        .header("cookie", &cookie)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 204);
    assert_eq!(
        client
            .get(format!("{}/api/session", f.base()))
            .header("cookie", &cookie)
            .send()
            .await
            .unwrap()
            .status(),
        401
    );
    f.owner.close().await.unwrap();
}
#[tokio::test]
async fn authenticated_browser_posts_are_admitted_by_the_rust_owner_and_wrong_identities_are_refused()
 {
    let f = Fixture::new().await;
    let (cookie, key) = f.enable().await;
    let mut socket = f.socket(&cookie, &key).await;
    let response = rpc(
        &mut socket,
        1,
        "channel.post",
        json!({"channel":f.channel.core.id,"text":"offline owner delivery"}),
    )
    .await;
    assert!(response.get("error").is_none(), "{response}");
    let events = f.owner.log.read(&f.channel.core.id, 1, 0).unwrap();
    let post = events
        .iter()
        .find(|event| event.kind == stavlos::event::CHAT_POSTED)
        .expect("owner persisted browser post");
    let chat: stavlos::event::ChatPayload = post.decode().unwrap();
    assert_eq!(chat.from, "human:web");
    assert_eq!(chat.text, "offline owner delivery");
    let root = f.channel.root().unwrap();
    let invalid = json!({"id":"post-invalid","request_id":"r-invalid","channel":f.channel.core.id,"targets":[root.id],"names":["forged"],"text":"refuse","message_kind":"request","method":"channel.set_mode","params":{"mode":"yolo"}});
    let before = f.owner.log.last_seq(&f.channel.core.id).unwrap();
    assert!(
        f.owner
            .deliver_web(invalid, &CancellationToken::new())
            .is_err()
    );
    assert_eq!(f.channel.info().mode, "ask");
    assert_eq!(f.owner.log.last_seq(&f.channel.core.id).unwrap(), before);
    f.owner.close().await.unwrap();
}
#[tokio::test]
async fn disabling_revokes_sessions_and_persists_the_choice_and_restart_restores_only_enabled_state()
 {
    let f = Fixture::new().await;
    let (cookie, _) = f.enable().await;
    let enabled: Value =
        serde_json::from_slice(&std::fs::read(f.owner.paths.data.join("web.json")).unwrap())
            .unwrap();
    assert_eq!(enabled["enabled"], true);
    f.web.close().await.unwrap();
    f.web.restore().await.unwrap();
    let client = stavlos::httpx::builder().no_proxy().build().unwrap();
    assert_eq!(
        client
            .get(format!("{}/api/session", f.base()))
            .header("cookie", &cookie)
            .send()
            .await
            .unwrap()
            .status(),
        401
    );
    let disabled = f
        .web
        .call("web.disable", CancellationToken::new())
        .await
        .unwrap();
    assert!(!disabled.enabled);
    let saved: Value =
        serde_json::from_slice(&std::fs::read(f.owner.paths.data.join("web.json")).unwrap())
            .unwrap();
    assert_eq!(saved["enabled"], false);
    f.web.close().await.unwrap();
    f.web.restore().await.unwrap();
    assert!(
        !f.web
            .call("web.status", CancellationToken::new())
            .await
            .unwrap()
            .enabled
    );
    assert!(
        f.web
            .call("web.open", CancellationToken::new())
            .await
            .is_err()
    );
    f.owner.close().await.unwrap();
}
#[tokio::test]
async fn private_pipe_startup_refuses_bad_readiness_and_reaps_its_child() {
    let root = tempfile::tempdir().unwrap();
    let program = root.path().join("fixture-api");
    std::fs::write(&program,"#!/usr/bin/python3\nimport os,pathlib,time\npathlib.Path(__file__).with_suffix('.pid').write_text(str(os.getpid()))\nprint('{\"kind\":\"wrong\"}',flush=True)\ntime.sleep(30)\n").unwrap();
    std::fs::set_permissions(&program, std::fs::Permissions::from_mode(0o700)).unwrap();
    let result = WebProcess::start(
        &Program::external(program.clone()).unwrap(),
        root.path(),
        4999,
        &[],
        Arc::new(|| Ok(json!({}))),
        Arc::new(|_, _| Box::pin(async { Ok(()) })),
        &CancellationToken::new(),
    )
    .await;
    assert!(result.is_err());
    let pid = std::fs::read_to_string(program.with_extension("pid"))
        .unwrap()
        .parse::<i32>()
        .unwrap();
    assert_eq!(unsafe { libc::kill(pid, 0) }, -1);
}
#[tokio::test]
async fn parent_cancellation_stops_and_reaps_the_actual_api_and_leaves_no_listener() {
    let root = tempfile::tempdir().unwrap();
    let cancel = CancellationToken::new();
    let process = WebProcess::start(
        &api(),
        root.path(),
        4999,
        &[],
        Arc::new(|| Ok(json!({}))),
        Arc::new(|_, _| Box::pin(async { Ok(()) })),
        &cancel,
    )
    .await
    .unwrap();
    let pid = process.pid();
    cancel.cancel();
    process.close().await;
    assert!(!process.alive());
    assert_eq!(unsafe { libc::kill(pid, 0) }, -1);
}

#[tokio::test]
async fn native_stream_epochs_allow_live_deltas_and_withhold_deltas_after_completion() {
    let f = Fixture::new().await;
    let (cookie, key) = f.enable().await;
    let mut socket = f.socket(&cookie, &key).await;
    let root = f.channel.root().unwrap();
    assert!(
        rpc(
            &mut socket,
            1,
            "subscribe",
            json!({"channel":f.channel.core.id,"from":1})
        )
        .await
        .get("error")
        .is_none()
    );
    let make =
        |kind| stavlos::event::Event::new(&f.channel.core.id, &root.id, kind, json!({})).unwrap();
    f.owner
        .log
        .append(vec![make(stavlos::event::MODEL_CALL_STARTED)])
        .unwrap();
    f.channel
        .core
        .stream(stavlos::protocol::StreamNotification {
            channel: f.channel.core.id.clone(),
            agent: root.id.clone(),
            text: "live fixture delta".into(),
            turn: 1,
            ..Default::default()
        });
    loop {
        let value = next(&mut socket).await;
        if value["method"] == "stream" {
            assert_eq!(value["params"]["text"], "live fixture delta");
            assert!(value["params"].get("_epoch").is_none());
            break;
        }
    }
    f.owner
        .log
        .append(vec![make(stavlos::event::MODEL_CALL_COMPLETED)])
        .unwrap();
    f.channel
        .core
        .stream(stavlos::protocol::StreamNotification {
            channel: f.channel.core.id.clone(),
            agent: root.id.clone(),
            text: "stale fixture delta".into(),
            turn: 1,
            ..Default::default()
        });
    f.owner.log.append(vec![make("fixture.flush")]).unwrap();
    f.owner.changed("stream-fixture-finished");
    loop {
        let value = next(&mut socket).await;
        assert_ne!(value["method"], "stream");
        if value["method"] == "changed" && value["params"]["what"] == "stream-fixture-finished" {
            break;
        }
    }
    f.owner.close().await.unwrap();
}
