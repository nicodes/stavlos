use serde_json::{Value, json};
use stavlos::{
    client::{self, Client},
    protocol,
};
use std::{os::fd::AsRawFd, sync::Arc, time::Duration};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::UnixStream,
};

fn pair() -> (Arc<Client>, UnixStream) {
    let (client, peer) = UnixStream::pair().unwrap();
    (Arc::new(Client::from_stream(client).unwrap()), peer)
}
async fn line(reader: &mut (impl tokio::io::AsyncBufRead + Unpin)) -> Value {
    let mut line = String::new();
    reader.read_line(&mut line).await.unwrap();
    serde_json::from_str(&line).unwrap()
}
async fn send(writer: &mut (impl tokio::io::AsyncWrite + Unpin), value: Value) {
    let mut bytes = serde_json::to_vec(&value).unwrap();
    bytes.push(b'\n');
    writer.write_all(&bytes).await.unwrap();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn concurrent_requests_match_ids_out_of_order_and_notifications_remain_typed() {
    let (client, peer) = pair();
    let server = tokio::spawn(async move {
        let (reader, mut writer) = peer.into_split();
        let mut reader = BufReader::new(reader);
        let mut requests = Vec::new();
        for _ in 0..64 {
            let request = line(&mut reader).await;
            assert_eq!(request["jsonrpc"], "2.0");
            assert_eq!(request["v"], 1);
            assert_eq!(request["method"], "fixture.echo");
            requests.push(request);
        }
        writer.write_all(b"malformed frame\n").await.unwrap();
        send(
            &mut writer,
            json!({"jsonrpc":"2.0","method":"changed","params":{"what":"web"}}),
        )
        .await;
        for request in requests.into_iter().rev() {
            send(
                &mut writer,
                json!({"jsonrpc":"2.0","id":request["id"],"result":request["params"]}),
            )
            .await;
        }
        let request = line(&mut reader).await;
        assert_eq!(request["method"], "attach");
        assert_eq!(
            request["params"],
            json!({"client":"owned fixture","tier":"interactive"})
        );
        send(&mut writer, json!({"jsonrpc":"2.0","id":request["id"],"result":{"client_id":"fixture-client","version":1}})).await;
        // Keep the connection alive until the client has consumed its final reply.
        let mut end = [0];
        let _ = reader.read(&mut end).await;
    });
    let mut calls = Vec::new();
    for index in 0..64 {
        let client = client.clone();
        calls.push(tokio::spawn(async move {
            let result: Value = client
                .call("fixture.echo", &json!({"index":index}))
                .await
                .unwrap();
            assert_eq!(result, json!({"index":index}));
        }));
    }
    for call in calls {
        call.await.unwrap();
    }
    let notification = client.notification().await.unwrap();
    match client::decode_notification(&notification).unwrap() {
        client::Notification::Changed(changed) => assert_eq!(changed.what, "web"),
        other => panic!("unexpected notification {other:?}"),
    }
    let attached = client
        .do_method(
            protocol::methods::ATTACH,
            &protocol::AttachParams {
                client: "owned fixture".into(),
                tier: "interactive".into(),
            },
        )
        .await
        .unwrap();
    assert_eq!(attached.client_id, "fixture-client");
    assert_eq!(attached.version, 1);
    client.close();
    server.await.unwrap();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cancelling_a_queued_writer_keeps_the_socket_but_cancelling_a_partial_frame_closes_it() {
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let size: libc::c_int = 4096;
    assert_eq!(
        unsafe {
            libc::setsockopt(
                socket.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_SNDBUF,
                (&size as *const libc::c_int).cast(),
                std::mem::size_of_val(&size) as libc::socklen_t,
            )
        },
        0
    );
    let client = Arc::new(Client::from_stream(socket).unwrap());
    let writing = client.clone();
    let first = tokio::spawn(async move {
        writing
            .call::<_, Value>("blocked", &"x".repeat(2 << 20))
            .await
    });
    let mut started = [0];
    tokio::time::timeout(Duration::from_secs(5), peer.read_exact(&mut started))
        .await
        .unwrap()
        .unwrap();
    let waiting =
        tokio::time::timeout(Duration::from_millis(30), client.call_raw("queued", None)).await;
    assert!(waiting.is_err());
    assert!(
        !client.is_closed(),
        "a call cancelled before acquiring the writer must leave it intact"
    );
    first.abort();
    let _ = first.await;
    tokio::time::timeout(Duration::from_secs(1), client.closed())
        .await
        .unwrap();
    assert!(
        client.is_closed(),
        "a cancelled partial frame must close the connection"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cancelled_reply_waits_discard_late_answers_and_leave_future_calls_usable() {
    let (client, peer) = pair();
    let (ready, received) = tokio::sync::oneshot::channel();
    let (resume, resumed) = tokio::sync::oneshot::channel();
    let server = tokio::spawn(async move {
        let (reader, mut writer) = peer.into_split();
        let mut reader = BufReader::new(reader);
        let first = line(&mut reader).await;
        ready.send(()).unwrap();
        resumed.await.unwrap();
        send(
            &mut writer,
            json!({"jsonrpc":"2.0","id":first["id"],"result":{"late":true}}),
        )
        .await;
        let second = line(&mut reader).await;
        send(
            &mut writer,
            json!({"jsonrpc":"2.0","id":second["id"],"result":{"current":true}}),
        )
        .await;
        let failed = line(&mut reader).await;
        send(&mut writer,json!({"jsonrpc":"2.0","id":failed["id"],"error":{"code":-32004,"message":"fixture refusal"}})).await;
        let mut end = [0];
        let _ = reader.read(&mut end).await;
    });
    let waiting = client.clone();
    let first = tokio::spawn(async move { waiting.call_raw("first", None).await });
    received.await.unwrap();
    first.abort();
    let _ = first.await;
    assert!(!client.is_closed());
    resume.send(()).unwrap();
    let second: Value = client.call("second", &json!({})).await.unwrap();
    assert_eq!(second, json!({"current":true}));
    let refused = client.call_raw("refused", None).await.unwrap_err();
    match refused {
        client::Error::Protocol(error) => {
            assert_eq!(error.code, protocol::ERR_FORBIDDEN);
            assert_eq!(error.message, "fixture refusal");
        }
        other => panic!("{other}"),
    }
    client.close();
    server.await.unwrap();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn close_releases_pending_calls_and_a_backed_up_notification_reader() {
    let (client, peer) = pair();
    let (reader, mut writer) = peer.into_split();
    let pending = client.clone();
    let call = tokio::spawn(async move { pending.call_raw("pending", None).await });
    let mut reader = BufReader::new(reader);
    assert_eq!(line(&mut reader).await["method"], "pending");
    let frame = b"{\"jsonrpc\":\"2.0\",\"method\":\"event\",\"params\":{}}\n";
    let flood = frame.repeat(4098);
    tokio::time::timeout(Duration::from_secs(5), writer.write_all(&flood))
        .await
        .unwrap()
        .unwrap();
    client.close();
    client.close();
    assert!(
        tokio::time::timeout(Duration::from_secs(1), call)
            .await
            .unwrap()
            .unwrap()
            .is_err()
    );
    tokio::time::timeout(Duration::from_secs(1), client.closed())
        .await
        .unwrap();
}

#[tokio::test]
async fn dial_checks_a_real_owned_unix_listener_and_dropped_clients_close_the_peer() {
    let owned = tempfile::tempdir().unwrap();
    let path = owned.path().join("fixture.sock");
    let listener = tokio::net::UnixListener::bind(&path).unwrap();
    let client = Client::connect(&path).await.unwrap();
    let (mut peer, _) = listener.accept().await.unwrap();
    drop(client);
    let mut byte = [0];
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(1), peer.read(&mut byte))
            .await
            .unwrap()
            .unwrap(),
        0
    );
}
