use serde_json::{Value, json};
use stavlos::{
    httpx,
    model::{
        self, CallError, Delta, DeltaSink, Response,
        stream::{self, Codec, Feed, Options, Sse},
    },
};
use std::{
    collections::BTreeMap,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::{TcpListener, TcpStream},
};
use tokio_util::sync::CancellationToken;

struct Recorder {
    values: Vec<String>,
    sink: DeltaSink,
    terminal: bool,
}
impl Recorder {
    fn new(sink: DeltaSink) -> Self {
        Self {
            values: vec![],
            sink,
            terminal: false,
        }
    }
}
impl Codec for Recorder {
    fn feed(&mut self, payload: &[u8]) -> Result<Feed, String> {
        let value = String::from_utf8_lossy(payload).into_owned();
        self.values.push(value.clone());
        (self.sink)(Delta {
            text: value.clone(),
            ..Default::default()
        });
        if let Some(error) = value.strip_prefix("fail:") {
            return Err(error.into());
        }
        if value == "terminal" {
            self.terminal = true;
        }
        Ok(if value == "stop" {
            Feed::Done
        } else {
            Feed::Continue
        })
    }
    fn response(&mut self) -> Response {
        Response {
            blocks: if self.values.is_empty() {
                vec![]
            } else {
                vec![model::Block::text(self.values.join("|"))]
            },
            stop_reason: model::STOP_END_TURN.into(),
            ..Default::default()
        }
    }
    fn terminal(&self) -> bool {
        self.terminal
    }
}
fn codec(sink: DeltaSink) -> Box<dyn Codec> {
    Box::new(Recorder::new(sink))
}
#[test]
fn original_sse_framing_is_preserved_across_every_network_chunk_boundary() {
    let cases: Value = serde_json::from_str(include_str!("data/go-sse-framing.json")).unwrap();
    for case in cases.as_array().unwrap() {
        for chunk in [1, 2, 7, 16, 65536] {
            let mut parser = Sse::default();
            let mut codec = Recorder::new(Arc::new(|_| {}));
            let mut result = Ok(false);
            for part in case["body"].as_str().unwrap().as_bytes().chunks(chunk) {
                result = parser.push(part, &mut codec);
                if !matches!(result, Ok(false)) {
                    break;
                }
            }
            if matches!(result, Ok(false)) {
                result = parser.finish(&mut codec);
            }
            assert_eq!(
                json!(codec.values),
                case["values"],
                "{} chunk {chunk}",
                case["body"]
            );
            assert_eq!(parser.lines, case["lines"].as_u64().unwrap() as usize);
            if case["error"] == "" {
                assert_eq!(result.unwrap(), case["done"].as_bool().unwrap());
            } else {
                assert_eq!(
                    result.unwrap_err().to_string(),
                    case["error"].as_str().unwrap()
                );
            }
        }
    }
    let mut parser = Sse::default();
    let mut codec = Recorder::new(Arc::new(|_| {}));
    assert!(matches!(
        parser.push(&vec![b'x'; stream::MAX_SSE_LINE], &mut codec),
        Err(stream::SseError::LineLimit)
    ));
}
async fn read_request(stream: &mut TcpStream) -> BTreeMap<String, String> {
    let mut reader = BufReader::new(stream);
    let mut line = String::new();
    reader.read_line(&mut line).await.unwrap();
    assert!(line.starts_with("POST "));
    let mut headers = BTreeMap::new();
    loop {
        line.clear();
        reader.read_line(&mut line).await.unwrap();
        if line == "\r\n" {
            break;
        }
        let (name, value) = line.split_once(':').unwrap();
        headers.insert(name.to_lowercase(), value.trim().to_owned());
    }
    let length = headers["content-length"].parse::<usize>().unwrap();
    let mut body = vec![0; length];
    reader.read_exact(&mut body).await.unwrap();
    assert_eq!(body, b"{}");
    headers
}
struct Reply {
    status: u16,
    headers: Vec<(String, String)>,
    body: String,
}
fn reply(status: u16, body: &str) -> Reply {
    Reply {
        status,
        headers: vec![],
        body: body.into(),
    }
}
async fn serve(
    replies: Vec<Reply>,
) -> (
    String,
    tokio::task::JoinHandle<Vec<BTreeMap<String, String>>>,
) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/stream", listener.local_addr().unwrap());
    let task = tokio::spawn(async move {
        let mut observed = vec![];
        for reply in replies {
            let (mut stream, _) = listener.accept().await.unwrap();
            observed.push(read_request(&mut stream).await);
            let mut headers = format!(
                "HTTP/1.1 {} Fixture\r\nContent-Length: {}\r\nConnection: close\r\n",
                reply.status,
                reply.body.len()
            );
            for (name, value) in reply.headers {
                headers.push_str(&format!("{name}: {value}\r\n"));
            }
            headers.push_str("\r\n");
            stream.write_all(headers.as_bytes()).await.unwrap();
            stream.write_all(reply.body.as_bytes()).await.unwrap();
        }
        observed
    });
    (url, task)
}
fn request(url: String) -> stream::Request {
    stream::Request {
        name: "owned fixture".into(),
        client: httpx::builder().no_proxy().build().unwrap(),
        url,
        body: b"{}".to_vec(),
        header: None,
        on_status: None,
        on_response: None,
        is_limit: None,
    }
}
fn options() -> Options {
    Options {
        retry_delay: Duration::ZERO,
        idle_timeout: Duration::from_secs(1),
        header_timeout: Duration::from_secs(1),
        ..Default::default()
    }
}
fn text(response: &Response) -> &str {
    response.blocks.first().map_or("", |block| &block.text)
}

#[tokio::test]
async fn retries_refresh_headers_and_observe_every_status_before_success() {
    let (url, server) = serve(vec![
        reply(503, "temporary"),
        reply(429, "slow down"),
        reply(200, "data: success\n\ndata: [DONE]\n\n"),
    ])
    .await;
    let mut request = request(url);
    let credentials = Arc::new(AtomicUsize::new(0));
    let calls = credentials.clone();
    request.header = Some(Arc::new(move |_| {
        let attempt = calls.fetch_add(1, Ordering::SeqCst) + 1;
        Box::pin(async move {
            let mut headers = reqwest::header::HeaderMap::new();
            headers.insert(
                "authorization",
                format!("Bearer fixture-{attempt}").parse().unwrap(),
            );
            Ok(headers)
        })
    }));
    let statuses = Arc::new(AtomicUsize::new(0));
    let observed = statuses.clone();
    request.on_response = Some(Arc::new(move |_| {
        observed.fetch_add(1, Ordering::SeqCst);
    }));
    let response = stream::complete(request, &options(), CancellationToken::new(), None, codec)
        .await
        .unwrap();
    assert_eq!(text(&response), "success");
    assert_eq!(credentials.load(Ordering::SeqCst), 3);
    assert_eq!(statuses.load(Ordering::SeqCst), 3);
    let observed = server.await.unwrap();
    for (index, headers) in observed.iter().enumerate() {
        assert_eq!(
            headers["authorization"],
            format!("Bearer fixture-{}", index + 1)
        );
        assert_eq!(headers["content-type"], "application/json");
        assert_eq!(headers["accept"], "text/event-stream");
    }
}
#[tokio::test]
async fn plan_limits_are_immediate_and_retried_rate_limits_remain_distinct_from_transient_errors() {
    let mut limited = reply(
        402,
        r#"{"error":{"type":"quota","message":"out of credits"}}"#,
    );
    limited.headers.push(("Retry-After".into(), "7".into()));
    let (url, server) = serve(vec![limited]).await;
    let failure = stream::complete(
        request(url),
        &options(),
        CancellationToken::new(),
        None,
        codec,
    )
    .await
    .unwrap_err();
    match failure.error {
        CallError::Limit { retry_after, .. } => assert_eq!(retry_after, Duration::from_secs(7)),
        other => panic!("{other:?}"),
    };
    assert_eq!(server.await.unwrap().len(), 1);
    for status in [429, 503] {
        let (url, server) =
            serve((0..3).map(|_| reply(status, "ordinary refusal")).collect()).await;
        let failure = stream::complete(
            request(url),
            &options(),
            CancellationToken::new(),
            None,
            codec,
        )
        .await
        .unwrap_err();
        assert!(if status == 429 {
            matches!(failure.error, CallError::Limit { .. })
        } else {
            matches!(failure.error, CallError::Transient(_))
        });
        assert_eq!(server.await.unwrap().len(), 3);
    }
}
#[tokio::test]
async fn incomplete_and_capacity_streams_retry_with_reset_but_final_rejections_stop() {
    for first in ["data: partial\n\n", "data: fail:at capacity\n\n"] {
        let (url, server) = serve(vec![
            reply(200, first),
            reply(200, "data: final\n\ndata: [DONE]\n\n"),
        ])
        .await;
        let deltas = Arc::new(Mutex::new(vec![]));
        let sink = deltas.clone();
        let response = stream::complete(
            request(url),
            &options(),
            CancellationToken::new(),
            Some(Arc::new(move |delta| sink.lock().unwrap().push(delta))),
            codec,
        )
        .await
        .unwrap();
        assert_eq!(text(&response), "final");
        {
            let deltas = deltas.lock().unwrap();
            assert_eq!(deltas.len(), 3);
            assert!(deltas[1].reset);
            assert_eq!(deltas[2].text, "final");
        }
        assert_eq!(server.await.unwrap().len(), 2);
    }
    let (url, server) = serve(vec![reply(200, "data: fail:invalid tool\n\n")]).await;
    let failure = stream::complete(
        request(url),
        &options(),
        CancellationToken::new(),
        None,
        codec,
    )
    .await
    .unwrap_err();
    assert!(matches!(failure.error, CallError::Permanent(_)));
    assert_eq!(server.await.unwrap().len(), 1);
}
async fn stalled(cancel: bool) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/stream", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.unwrap();
        read_request(&mut stream).await;
        stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 99999\r\nConnection: close\r\n\r\ndata: partial\n\n").await.unwrap();
        let mut byte = [0];
        let _ = stream.read(&mut byte).await;
    });
    let mut options = options();
    options.mid_stream_retries = 0;
    options.idle_timeout = Duration::from_millis(50);
    let cancellation = CancellationToken::new();
    let signal = cancellation.clone();
    let sink: DeltaSink = Arc::new(move |_| {
        if cancel {
            signal.cancel();
        }
    });
    let failure = tokio::time::timeout(
        Duration::from_secs(2),
        stream::complete(request(url), &options, cancellation, Some(sink), codec),
    )
    .await
    .unwrap()
    .unwrap_err();
    assert_eq!(text(&failure.response), "partial");
    if cancel {
        assert!(matches!(failure.error, CallError::Cancelled));
    } else {
        assert!(matches!(failure.error, CallError::Transient(_)));
        assert_eq!(failure.response.stop_reason, model::STOP_OTHER);
    }
    server.await.unwrap();
}
#[tokio::test]
async fn cancellation_and_idle_timeouts_return_partial_results_and_close_the_response() {
    stalled(true).await;
    stalled(false).await;
}

#[tokio::test]
async fn redirects_never_send_bearer_headers_or_bodies_to_another_origin() {
    let trap = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let destination = format!("http://{}/trap", trap.local_addr().unwrap());
    let mut replies = vec![];
    for _ in 0..3 {
        let mut redirect = reply(307, "");
        redirect
            .headers
            .push(("Location".into(), destination.clone()));
        replies.push(redirect);
    }
    let (url, server) = serve(replies).await;
    let mut request = request(url);
    request.header = Some(Arc::new(|_| {
        Box::pin(async {
            let mut headers = reqwest::header::HeaderMap::new();
            headers.insert(
                "authorization",
                reqwest::header::HeaderValue::from_static("Bearer fixture-only"),
            );
            Ok(headers)
        })
    }));
    let failure = stream::complete(request, &options(), CancellationToken::new(), None, codec)
        .await
        .unwrap_err();
    assert!(matches!(failure.error, CallError::Transient(_)));
    assert_eq!(server.await.unwrap().len(), 3);
    assert!(
        tokio::time::timeout(Duration::from_millis(30), trap.accept())
            .await
            .is_err()
    );
}

#[test]
fn refusal_classification_and_error_text_do_not_treat_permissions_as_allowance() {
    assert!(stream::plan_limit(403, b"OUT OF CREDITS"));
    assert!(!stream::plan_limit(403, b"insufficient permissions"));
    assert!(!stream::plan_limit(401, b"quota"));
    assert_eq!(
        stream::error_text(br#"{"error":{"type":"bad","message":"fixture"}}"#),
        "bad: fixture"
    );
    assert_eq!(stream::error_text(b" plain fixture \n"), "plain fixture");
    assert!(stream::transient("server_error: at capacity"));
    assert!(!stream::transient("invalid tool"));
    let now = std::time::UNIX_EPOCH + Duration::from_secs(1_700_000_000);
    let date = httpdate::fmt_http_date(now + Duration::from_secs(21));
    assert_eq!(stream::retry_after(&date, now), Duration::from_secs(21));
    assert_eq!(stream::retry_after("-1", now), Duration::ZERO);
    assert!(
        !format!(
            "{:?}",
            model::Token {
                access: "never-print-this-fixture".into(),
                account_id: "fixture".into()
            }
        )
        .contains("never-print-this-fixture")
    );
}
