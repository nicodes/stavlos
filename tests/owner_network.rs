use stavlos::{
    daemon::{
        hub::{Client, Hub},
        server::{self, Connection, Dispatch, Handler, Scope, Socket},
    },
    event::Event,
    eventlog::Log,
    protocol,
};
use std::{os::unix::fs::PermissionsExt, sync::Arc, time::Duration};
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    net::UnixStream,
};
use tokio_util::sync::CancellationToken;
fn request(method: &str) -> protocol::Request {
    protocol::Request {
        v: 1,
        method: method.into(),
        ..Default::default()
    }
}
#[test]
fn scopes_are_fixed_by_transport_and_cover_every_typed_method() {
    for scope in [Scope::Owner, Scope::Bridge, Scope::Web] {
        let connection = Connection {
            client: Client::new().unwrap(),
            cancel: CancellationToken::new(),
            scope,
            descendant: false,
        };
        let routes: std::collections::BTreeMap<String, u8> =
            serde_json::from_str(include_str!("data/go-owner-routes.json")).unwrap();
        assert_eq!(routes.len(), 57);
        for (method, expected) in routes {
            let route = server::route_scope(&method).expect(&method);
            assert_eq!(route as u8, expected, "{method}");
            assert_eq!(
                connection.admit(&request(&method)).is_ok(),
                scope <= route,
                "{method} {scope:?}"
            );
        }
        assert_eq!(
            connection
                .admit(&request("secret.unknown"))
                .unwrap_err()
                .code,
            if scope == Scope::Owner {
                protocol::ERR_METHOD_NOT_FOUND
            } else {
                protocol::ERR_FORBIDDEN
            }
        );
        let mut old = request("daemon.status");
        old.v = 0;
        assert_eq!(
            connection.admit(&old).unwrap_err().code,
            protocol::ERR_VERSION
        );
        connection
            .client
            .attach("owner", protocol::TIER_FALLBACK, scope == Scope::Web);
        assert_eq!(connection.scope, scope);
        if scope == Scope::Web {
            assert_eq!(connection.client.identity().0, "web");
        }
    }
    let connection = Connection {
        client: Client::new().unwrap(),
        cancel: CancellationToken::new(),
        scope: Scope::Owner,
        descendant: true,
    };
    assert_eq!(
        connection
            .admit(&request("daemon.status"))
            .unwrap_err()
            .code,
        protocol::ERR_FORBIDDEN
    );
}
struct Echo;
impl Handler for Echo {
    fn dispatch<'a>(&'a self, _: &'a Connection, request: protocol::Request) -> Dispatch<'a> {
        Box::pin(async move {
            if request
                .params
                .as_ref()
                .is_some_and(|v| v.get().contains("slow"))
            {
                tokio::time::sleep(Duration::from_millis(30)).await;
            }
            Ok(serde_json::json!({"method":request.method}))
        })
    }
}
async fn response(reader: &mut BufReader<tokio::net::unix::OwnedReadHalf>) -> serde_json::Value {
    let mut line = String::new();
    tokio::time::timeout(Duration::from_secs(3), reader.read_line(&mut line))
        .await
        .unwrap()
        .unwrap();
    serde_json::from_str(&line).unwrap()
}
#[tokio::test]
async fn request_ids_parse_errors_notifications_and_out_of_order_replies_survive_the_transport() {
    let (client, server) = UnixStream::pair().unwrap();
    let cancel = CancellationToken::new();
    let hub = Arc::new(Hub::default());
    let worker = tokio::spawn(server::serve_connection(
        server,
        Arc::new(Echo),
        hub.clone(),
        cancel.clone(),
        Scope::Owner,
        false,
    ));
    let (read, mut write) = client.into_split();
    let mut read = BufReader::new(read);
    write.write_all(b"{broken}\n{\"v\":1,\"id\":\"slow-id\",\"method\":\"daemon.status\",\"params\":{\"slow\":true}}\n{\"v\":1,\"id\":12345678901234567890,\"method\":\"daemon.status\"}\n{\"v\":1,\"method\":\"daemon.status\"}\n").await.unwrap();
    let bad = response(&mut read).await;
    assert_eq!(bad["error"]["code"], protocol::ERR_PARSE);
    let fast = response(&mut read).await;
    assert_eq!(fast["id"].as_u64(), Some(12345678901234567890));
    let slow = response(&mut read).await;
    assert_eq!(slow["id"], "slow-id");
    cancel.cancel();
    worker.await.unwrap();
    assert!(hub.clients().is_empty());
}
#[tokio::test]
async fn a_browser_cannot_use_attach_or_a_fabricated_method_to_gain_owner_methods() {
    let (client, server) = UnixStream::pair().unwrap();
    let cancel = CancellationToken::new();
    let worker = tokio::spawn(server::serve_connection(
        server,
        Arc::new(Echo),
        Arc::new(Hub::default()),
        cancel.clone(),
        Scope::Web,
        false,
    ));
    let (read, mut write) = client.into_split();
    let mut read = BufReader::new(read);
    for (id, method) in [
        (1, "channel.set_mode"),
        (2, "unknown.private"),
        (3, "prompt.reply"),
        (4, "daemon.status"),
    ] {
        write.write_all(format!("{{\"v\":1,\"id\":{id},\"method\":\"{method}\",\"params\":{{\"client\":\"owner\"}}}}\n").as_bytes()).await.unwrap();
        let result = response(&mut read).await;
        if id == 4 {
            assert!(result.get("result").is_some());
        } else {
            assert_eq!(result["error"]["code"], protocol::ERR_FORBIDDEN);
        }
    }
    cancel.cancel();
    worker.await.unwrap();
}
#[tokio::test]
async fn oversized_and_disconnected_clients_release_their_transport() {
    let (mut client, server) = UnixStream::pair().unwrap();
    let hub = Arc::new(Hub::default());
    let worker = tokio::spawn(server::serve_connection(
        server,
        Arc::new(Echo),
        hub.clone(),
        CancellationToken::new(),
        Scope::Owner,
        false,
    ));
    let bytes = vec![b'x'; protocol::MAX_LINE + 1];
    let _ = client.write_all(&bytes).await;
    tokio::time::timeout(Duration::from_secs(3), worker)
        .await
        .unwrap()
        .unwrap();
    assert!(hub.clients().is_empty());
}
#[tokio::test]
async fn a_published_owner_socket_is_private_and_never_replaces_regular_files() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("owner.sock");
    let socket = Socket::bind(&path).unwrap();
    assert_eq!(
        std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
        0o600
    );
    let cancel = CancellationToken::new();
    let stopping = cancel.clone();
    let hub = Arc::new(Hub::default());
    let worker = tokio::spawn(async move {
        socket
            .serve(Arc::new(Echo), hub, stopping, Scope::Bridge)
            .await
            .unwrap();
    });
    let client = UnixStream::connect(&path).await.unwrap();
    let (read, mut write) = client.into_split();
    let mut read = BufReader::new(read);
    write
        .write_all(b"{\"v\":1,\"id\":1,\"method\":\"channel.set_mode\"}\n")
        .await
        .unwrap();
    assert_eq!(
        response(&mut read).await["error"]["code"],
        protocol::ERR_FORBIDDEN
    );
    cancel.cancel();
    worker.await.unwrap();
    assert!(!path.exists());
    std::fs::write(&path, "keep").unwrap();
    assert!(Socket::bind(&path).is_err());
    assert_eq!(std::fs::read_to_string(path).unwrap(), "keep");
}
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn journal_replay_hands_off_to_live_delivery_in_order_without_duplicates() {
    let dir = tempfile::tempdir().unwrap();
    let hub = Arc::new(Hub::default());
    let notify = hub.clone();
    let log = Log::open(
        dir.path().join("events.db"),
        false,
        Some(Arc::new(move |events| notify.committed(events))),
    )
    .unwrap();
    let client = Client::new().unwrap();
    hub.add(client.clone());
    let append = |n| {
        log.append(vec![
            Event::new("c", "", "fixture", serde_json::json!({"n":n})).unwrap(),
        ])
        .unwrap()
    };
    for n in 0..600 {
        append(n);
    }
    let last = client
        .subscribe(&log, "c", 1, &CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(last, 600);
    for n in 600..610 {
        append(n);
    }
    for seq in 1..=610 {
        let line = client.queue.recv(&CancellationToken::new()).await.unwrap();
        let value: serde_json::Value = serde_json::from_slice(&line).unwrap();
        assert_eq!(value["params"]["event"]["seq"], seq);
    }
    assert_eq!(client.queue.usage().0, 0);
    // Repeating a subscription deliberately replays its requested suffix exactly once.
    assert_eq!(
        client
            .subscribe(&log, "c", 609, &CancellationToken::new())
            .await
            .unwrap(),
        610
    );
    assert_eq!(client.queue.usage().0, 2);
    client.unsubscribe("c");
    append(610);
    assert_eq!(client.queue.usage().0, 2);
    log.close().unwrap();
}
