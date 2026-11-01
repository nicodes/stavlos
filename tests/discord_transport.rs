use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use stavlos::discord::{
    api::{Api, Missing},
    gateway,
    http::{Http, Rest},
    planning,
};
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::TcpListener,
};
use tokio_tungstenite::tungstenite::Message;
use tokio_util::sync::CancellationToken;
#[derive(Clone)]
struct Request {
    method: String,
    path: String,
    authorization: String,
    body: Value,
}
struct Reply {
    status: u16,
    headers: Vec<(String, String)>,
    body: String,
    delay: Duration,
}
impl Reply {
    fn json(value: Value) -> Self {
        Self {
            status: 200,
            headers: vec![],
            body: value.to_string(),
            delay: Duration::ZERO,
        }
    }
    fn error(status: u16) -> Self {
        Self {
            status,
            ..Self::json(json!({"ignored":"private fixture response"}))
        }
    }
}
struct Server {
    endpoint: reqwest::Url,
    requests: Arc<Mutex<Vec<Request>>>,
    task: tokio::task::JoinHandle<()>,
}
impl Drop for Server {
    fn drop(&mut self) {
        self.task.abort();
    }
}
impl Server {
    async fn new(handler: impl Fn(&Request, usize) -> Reply + Send + 'static) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("http://{}/api/v10/", listener.local_addr().unwrap())
            .parse()
            .unwrap();
        let requests = Arc::new(Mutex::new(Vec::new()));
        let recorded = requests.clone();
        let task = tokio::spawn(async move {
            loop {
                let (socket, _) = listener.accept().await.unwrap();
                let mut socket = BufReader::new(socket);
                let mut first = String::new();
                socket.read_line(&mut first).await.unwrap();
                let mut first = first.split_whitespace();
                let method = first.next().unwrap().to_string();
                let path = first.next().unwrap().to_string();
                let mut authorization = String::new();
                let mut length = 0;
                loop {
                    let mut line = String::new();
                    socket.read_line(&mut line).await.unwrap();
                    if line == "\r\n" {
                        break;
                    }
                    let (key, value) = line.split_once(':').unwrap();
                    if key.eq_ignore_ascii_case("content-length") {
                        length = value.trim().parse().unwrap();
                    }
                    if key.eq_ignore_ascii_case("authorization") {
                        authorization = value.trim().into();
                    }
                }
                assert!(length <= 1 << 20);
                let mut body = vec![0; length];
                socket.read_exact(&mut body).await.unwrap();
                let request = Request {
                    method,
                    path,
                    authorization,
                    body: serde_json::from_slice(&body).unwrap_or(Value::Null),
                };
                let n = {
                    let mut requests = recorded.lock().unwrap();
                    requests.push(request.clone());
                    requests.len()
                };
                let reply = handler(&request, n);
                tokio::time::sleep(reply.delay).await;
                let headers = reply
                    .headers
                    .iter()
                    .map(|(key, value)| format!("{key}: {value}\r\n"))
                    .collect::<String>();
                let response = format!(
                    "HTTP/1.1 {} Fixture\r\nContent-Length: {}\r\nContent-Type: application/json\r\nConnection: close\r\n{headers}\r\n{}",
                    reply.status,
                    reply.body.len(),
                    reply.body
                );
                let _ = socket.write_all(response.as_bytes()).await;
            }
        });
        Self {
            endpoint,
            requests,
            task,
        }
    }
    fn rest(&self) -> Arc<Rest> {
        Arc::new(Rest::with_endpoint("offline-bot-secret".into(), self.endpoint.clone()).unwrap())
    }
    fn http(&self) -> Http {
        Http::new(self.rest(), "bot".into(), "app".into())
    }
    fn count(&self) -> usize {
        self.requests.lock().unwrap().len()
    }
}
fn cancel() -> CancellationToken {
    CancellationToken::new()
}
#[tokio::test]
async fn errors_do_not_leak_credentials_or_retry_ambiguous_sends_and_401_stops_bot_requests() {
    let server = Server::new(|_, n| {
        Reply::error(match n {
            1 => 500,
            2 => 404,
            _ => 401,
        })
    })
    .await;
    let rest = server.rest();
    let error = rest
        .request(
            reqwest::Method::POST,
            &["webhooks", "hook", "offline-hook-secret"],
            &[],
            Some(json!({"content":"private text"})),
            false,
            cancel(),
        )
        .await
        .unwrap_err()
        .to_string();
    assert_eq!(error, "discord HTTP 500");
    assert_eq!(server.count(), 1);
    assert!(
        rest.request(
            reqwest::Method::GET,
            &["channels", "gone"],
            &[],
            None,
            true,
            cancel()
        )
        .await
        .unwrap_err()
        .is::<Missing>()
    );
    assert!(
        rest.request(
            reqwest::Method::GET,
            &["users", "@me"],
            &[],
            None,
            true,
            cancel()
        )
        .await
        .unwrap_err()
        .to_string()
        .contains("401")
    );
    assert!(
        rest.request(
            reqwest::Method::GET,
            &["guilds", "guild"],
            &[],
            None,
            true,
            cancel()
        )
        .await
        .is_err()
    );
    assert_eq!(server.count(), 3);
}
#[tokio::test]
async fn only_explicit_rate_limit_rejections_are_retried_and_bucket_reset_is_observed() {
    let server = Server::new(|_, n| {
        if n == 1 {
            Reply {
                status: 429,
                ..Reply::json(json!({"retry_after":0.025,"global":true}))
            }
        } else {
            Reply {
                headers: vec![
                    ("X-RateLimit-Bucket".into(), "bucket".into()),
                    ("X-RateLimit-Remaining".into(), "0".into()),
                    ("X-RateLimit-Reset-After".into(), "0.025".into()),
                ],
                ..Reply::json(json!({"id":"sent"}))
            }
        }
    })
    .await;
    let rest = server.rest();
    let started = tokio::time::Instant::now();
    rest.request(
        reqwest::Method::POST,
        &["channels", "channel", "messages"],
        &[],
        Some(json!({"content":"once"})),
        true,
        cancel(),
    )
    .await
    .unwrap();
    assert!(started.elapsed() >= Duration::from_millis(25));
    assert_eq!(server.count(), 2);
    let started = tokio::time::Instant::now();
    rest.request(
        reqwest::Method::POST,
        &["channels", "channel", "messages"],
        &[],
        Some(json!({"content":"next"})),
        true,
        cancel(),
    )
    .await
    .unwrap();
    assert!(started.elapsed() >= Duration::from_millis(20));
    assert!(
        server
            .requests
            .lock()
            .unwrap()
            .iter()
            .all(|r| r.authorization == "Bot offline-bot-secret")
    );
}
#[tokio::test]
async fn cancellation_interrupts_http_and_redirects_are_never_followed() {
    let server = Server::new(|_, _| Reply {
        delay: Duration::from_secs(30),
        ..Reply::json(json!({}))
    })
    .await;
    let rest = server.rest();
    let stop = cancel();
    let stopping = stop.clone();
    let task = tokio::spawn(async move {
        rest.request(
            reqwest::Method::GET,
            &["users", "@me"],
            &[],
            None,
            true,
            stopping,
        )
        .await
    });
    tokio::time::timeout(Duration::from_secs(2), async {
        while server.count() == 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    stop.cancel();
    assert!(
        tokio::time::timeout(Duration::from_millis(200), task)
            .await
            .unwrap()
            .unwrap()
            .is_err()
    );
    assert_eq!(server.count(), 1);
    let redirect = Server::new(|_, _| Reply {
        headers: vec![(
            "Location".into(),
            "http://127.0.0.1:1/credential-leak".into(),
        )],
        ..Reply::error(302)
    })
    .await;
    assert_eq!(
        redirect
            .rest()
            .request(
                reqwest::Method::GET,
                &["users", "@me"],
                &[],
                None,
                true,
                cancel()
            )
            .await
            .unwrap_err()
            .to_string(),
        "discord HTTP 302"
    );
    assert_eq!(redirect.count(), 1);
}
fn hook(name: &str) -> Value {
    json!({"id":if name=="stavlos" {"agent-hook"}else {"human-hook"},"token":"offline-hook-secret","name":name,"user":{"id":"bot"},"application_id":"app"})
}
#[tokio::test]
async fn concurrent_agent_posts_share_one_hook_and_cards_remain_owned_across_restart() {
    let creates = Arc::new(AtomicUsize::new(0));
    let created = creates.clone();
    let server = Server::new(move |r, _| {
        if r.path == "/api/v10/channels/channel/webhooks" {
            return if r.method == "GET" {
                Reply::json(json!([]))
            } else {
                created.fetch_add(1, Ordering::SeqCst);
                Reply::json(hook(r.body["name"].as_str().unwrap()))
            };
        }
        if r.path == "/api/v10/webhooks/agent-hook" {
            assert_eq!(r.authorization, "Bot offline-bot-secret");
            return Reply::json(hook("stavlos"));
        }
        if r.method == "PATCH" {
            assert_eq!(r.body["flags"], 32772);
            assert!(r.body["content"].is_null());
            assert_eq!(r.body["embeds"], json!([]));
            assert_eq!(r.body["components"][0]["type"], 10);
            return Reply::json(json!({"id":"card"}));
        }
        assert!(r.path.contains("wait=true"));
        assert!(r.path.contains("with_components=true"));
        assert_eq!(r.body["allowed_mentions"]["parse"], json!([]));
        assert!(r.authorization.is_empty());
        Reply::json(json!({"id":"card"}))
    })
    .await;
    let http = Arc::new(server.http());
    let (first, second) = tokio::join!(
        http.send(
            "channel".into(),
            "agent one".into(),
            "one".into(),
            vec![],
            cancel()
        ),
        http.send(
            "channel".into(),
            "agent two".into(),
            "two".into(),
            vec![],
            cancel()
        )
    );
    assert_eq!(first.unwrap(), "card");
    second.unwrap();
    assert_eq!(creates.load(Ordering::SeqCst), 1);
    let (id, webhook) = http
        .send_question(
            "channel".into(),
            "agent one".into(),
            "Question".into(),
            vec![json!({"type":1,"components":[]})],
            cancel(),
        )
        .await
        .unwrap();
    assert_eq!((id.as_str(), webhook.as_str()), ("card", "agent-hook"));
    let restarted = server.http();
    restarted
        .edit_webhook(webhook, id, "Resolved".into(), vec![], cancel())
        .await
        .unwrap();
    let requests = server.requests.lock().unwrap();
    let card = requests
        .iter()
        .find(|r| r.body["flags"] == 32772 && r.method == "POST")
        .unwrap();
    assert!(card.body.get("content").is_none());
    assert_eq!(card.body["components"][0]["content"], "Question");
}
#[tokio::test]
async fn foreign_question_webhooks_are_refused_and_missing_sends_invalidate_without_retry() {
    let foreign = Server::new(|r, _| {
        if r.path.ends_with("/webhooks") {
            let mut h = hook("stavlos");
            h["application_id"] = json!("other");
            Reply::json(json!([h]))
        } else {
            panic!("must not execute foreign webhook")
        }
    })
    .await;
    assert!(
        foreign
            .http()
            .send_question(
                "channel".into(),
                "agent".into(),
                "Question".into(),
                vec![],
                cancel()
            )
            .await
            .is_err()
    );
    assert_eq!(foreign.count(), 1);
    let server = Server::new(|r, _| {
        if r.path.ends_with("/webhooks") {
            Reply::json(json!([hook("stavlos")]))
        } else {
            Reply::error(404)
        }
    })
    .await;
    let http = server.http();
    for _ in 0..2 {
        assert!(
            http.send(
                "channel".into(),
                "agent".into(),
                "text".into(),
                vec![],
                cancel()
            )
            .await
            .unwrap_err()
            .is::<Missing>()
        );
    }
    assert_eq!(server.count(), 4);
}
#[tokio::test]
async fn terminal_speech_uses_a_separate_profile_and_interaction_cards_clear_legacy_fields() {
    let server=Server::new(|r,_| {
        if r.path=="/api/v10/guilds/guild/members/user" {return Reply::json(json!({"nick":"Operator","avatar":"avatar","user":{"id":"user","username":"fallback"}}));}
        if r.path=="/api/v10/channels/channel/webhooks" {return if r.method=="GET" {Reply::json(json!([hook("stavlos"),hook("stavlos-terminal")]))}else {panic!("existing hooks")};}
        if r.path.contains("human-hook") {assert_eq!(r.body["username"],"Operator");assert!(r.body["avatar_url"].as_str().unwrap().contains("guilds/guild/users/user/avatars/avatar.png"));return Reply::json(json!({"id":"human-post"}));}
        Reply::json(json!({"id":"edited"}))
    }).await;
    let http = server.http();
    http.send_user(
        "channel".into(),
        "guild".into(),
        "user".into(),
        "terminal text".into(),
        cancel(),
    )
    .await
    .unwrap();
    assert!(
        !http
            .own_webhook("channel".into(), "human-hook".into(), cancel())
            .await
            .unwrap()
    );
    let i = json!({"id":"interaction","application_id":"app","token":"offline-interaction-secret","type":3,"data":{"custom_id":"sv|prompt|allow|"}});
    http.respond(i.clone(), json!({"type":6}), cancel())
        .await
        .unwrap();
    http.response_edit(i, "Allowed".into(), vec![], cancel())
        .await
        .unwrap();
    http.response_edit(
        json!({"application_id":"app","token":"offline-interaction-secret","type":2}),
        "Status".into(),
        vec![],
        cancel(),
    )
    .await
    .unwrap();
    let requests = server.requests.lock().unwrap();
    let edits = requests
        .iter()
        .filter(|r| r.method == "PATCH")
        .collect::<Vec<_>>();
    assert_eq!(edits[0].body["flags"], 32772);
    assert_eq!(edits[0].body["embeds"], json!([]));
    assert_eq!(edits[1].body["content"], "Status");
    assert!(edits[1].body.get("flags").is_none());
}
#[tokio::test]
async fn gateway_identifies_heartbeats_dispatches_and_closes_on_cancellation() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("ws://{}", listener.local_addr().unwrap());
    let server = Server::new(move |r, _| {
        assert_eq!(r.path, "/api/v10/gateway/bot");
        Reply::json(json!({"url":url,"session_start_limit":{"remaining":1}}))
    })
    .await;
    let (heartbeat_seen, heartbeat_received) = tokio::sync::oneshot::channel();
    let websocket = tokio::spawn(async move {
        let (socket, _) = listener.accept().await.unwrap();
        socket.set_nodelay(true).unwrap();
        let mut socket = tokio_tungstenite::accept_async(socket).await.unwrap();
        socket
            .send(Message::Text(
                json!({"op":10,"d":{"heartbeat_interval":100}})
                    .to_string()
                    .into(),
            ))
            .await
            .unwrap();
        let identify: Value =
            serde_json::from_str(socket.next().await.unwrap().unwrap().to_text().unwrap()).unwrap();
        assert_eq!(identify["op"], 2);
        assert_eq!(identify["d"]["intents"], 33281);
        assert_eq!(identify["d"]["token"], "offline-bot-secret");
        for (kind, data, seq) in [
            ("READY", json!({"session_id":"offline"}), 1),
            ("MESSAGE_CREATE", json!({"content":"human","id":"post"}), 2),
            ("INTERACTION_CREATE", json!({"id":"click"}), 3),
        ] {
            socket
                .send(Message::Text(
                    json!({"op":0,"t":kind,"s":seq,"d":data}).to_string().into(),
                ))
                .await
                .unwrap();
        }
        let mut heartbeat_seen = Some(heartbeat_seen);
        while let Some(Ok(message)) = socket.next().await {
            if message.is_text() {
                let value: Value = serde_json::from_str(message.to_text().unwrap()).unwrap();
                assert_eq!(value["op"], 1);
                assert!(value["d"].is_null() || value["d"].as_u64().is_some_and(|n| n <= 3));
                socket
                    .send(Message::Text(json!({"op":11}).to_string().into()))
                    .await
                    .unwrap();
                if value["d"] == 3
                    && let Some(seen) = heartbeat_seen.take()
                {
                    seen.send(()).unwrap();
                }
            }
        }
    });
    let stop = cancel();
    let connected = Arc::new(Mutex::new(Vec::new()));
    let states = connected.clone();
    let (events, mut received) = tokio::sync::mpsc::channel(4);
    let rest = server.rest();
    let stopping = stop.clone();
    let run = tokio::spawn(async move {
        gateway::run(
            rest,
            "offline-bot-secret".into(),
            events,
            Arc::new(move |state| states.lock().unwrap().push(state)),
            stopping,
        )
        .await
    });
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(10), received.recv())
            .await
            .unwrap()
            .unwrap()
            .kind,
        "MESSAGE_CREATE"
    );
    assert_eq!(received.recv().await.unwrap().data["id"], "click");
    heartbeat_received.await.unwrap();
    stop.cancel();
    run.await.unwrap().unwrap();
    tokio::time::timeout(Duration::from_secs(2), websocket)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(*connected.lock().unwrap(), vec![true, false]);
}
#[tokio::test]
async fn exhausted_gateway_sessions_do_not_connect_and_channel_markers_remain_unambiguous() {
    let server = Server::new(|_, _| {
        Reply::json(json!({"url":"ws://127.0.0.1:1","session_start_limit":{"remaining":0}}))
    })
    .await;
    let (events, _) = tokio::sync::mpsc::channel(1);
    let error = gateway::run(
        server.rest(),
        "offline-bot-secret".into(),
        events,
        Arc::new(|_| {}),
        cancel(),
    )
    .await
    .unwrap_err();
    assert!(error.to_string().contains("session start limit"));
    let channels = serde_json::from_value::<Vec<stavlos::discord::api::Channel>>(json!([
        {"id":"category","name":"chosen","type":4},
        {"id":"one","type":0,"parent_id":"category","topic":"stavlos-channel:immutable"},
        {"id":"two","type":0,"parent_id":"category","topic":"stavlos-channel:immutable"}
    ]))
    .unwrap();
    assert_eq!(
        planning::category(&channels, "chosen").unwrap(),
        Some("category".into())
    );
    assert!(planning::markers(&channels, "category").is_err());
    let root = tempfile::tempdir().unwrap();
    let child = root.path().join("child");
    std::fs::create_dir(&child).unwrap();
    let grant = root.path().to_string_lossy().to_string();
    assert!(planning::allowed(&grant, std::slice::from_ref(&grant)));
    assert!(!planning::allowed(child.to_str().unwrap(), &[grant]));
}
