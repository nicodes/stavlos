use serde_json::{Value, json, value::RawValue};
use stavlos::tools::{
    self, Env, Output,
    web::{EXA_MCP, Options, SearchConfig, Web},
};
use std::{
    collections::BTreeMap,
    fs,
    os::unix::fs::PermissionsExt as _,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};
use tokio::{
    io::{AsyncBufReadExt as _, AsyncReadExt as _, AsyncWriteExt as _, BufReader},
    net::{TcpListener, UnixListener},
};
use tokio_rustls::{
    TlsAcceptor,
    rustls::{
        self,
        pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer},
    },
};
use tokio_util::sync::CancellationToken;
fn raw(input: Value) -> Box<RawValue> {
    RawValue::from_string(input.to_string()).unwrap()
}
#[test]
fn all_original_search_reply_shapes_keep_typed_failures_and_free_mcp_framing() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-search-parsers.json")).unwrap();
    assert_eq!(cases.len(), 61);
    for (index, c) in cases.iter().enumerate() {
        let result = tools::web::parse_search(
            c["provider"].as_str().unwrap(),
            c["input"].as_str().unwrap().as_bytes(),
        );
        if c["error"].as_str().unwrap().is_empty() {
            let rows = result
                .unwrap()
                .into_iter()
                .map(|r| json!({"Title":r.title,"URL":r.url,"Snippet":r.snippet}))
                .collect::<Vec<_>>();
            assert_eq!(json!(rows), c["rows"], "search reply {index}");
        } else {
            assert!(result.is_err(), "search reply {index}: {c}");
        }
    }
}
#[test]
fn every_original_address_boundary_remains_filtered_and_url_policy_spelling_is_preserved() {
    let ips: Vec<Value> =
        serde_json::from_str(include_str!("data/go-web-public-ips.json")).unwrap();
    assert_eq!(ips.len(), 191);
    for c in ips {
        assert_eq!(
            tools::web::public_address(c["ip"].as_str().unwrap()),
            c["public"].as_bool().unwrap(),
            "{}",
            c["ip"]
        );
    }
    let urls: Vec<Value> = serde_json::from_str(include_str!("data/go-web-urls.json")).unwrap();
    for c in urls {
        let result = tools::web::normalize_text(c["input"].as_str().unwrap());
        if c["error"].as_str().unwrap().is_empty() {
            // The Go normalizer stripped IPv6 brackets with :443. Keep a valid IPv6 URL.
            let expected = if c["input"] == "https://[::1]:443/a" {
                "https://[::1]/a"
            } else {
                c["url"].as_str().unwrap()
            };
            assert_eq!(result.unwrap(), expected, "{}", c["input"]);
        } else {
            assert!(result.is_err(), "{}", c["input"]);
        }
    }
}
type Requests = Arc<Mutex<Vec<(String, String, Value)>>>;
struct Server {
    base: String,
    requests: Requests,
    stop: CancellationToken,
}
impl Drop for Server {
    fn drop(&mut self) {
        self.stop.cancel();
    }
}
async fn server() -> Server {
    let certificate = CertificateDer::from(include_bytes!("data/web-loopback-cert.der").to_vec());
    let key = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(
        include_bytes!("data/web-loopback-key.der").to_vec(),
    ));
    let config = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_no_client_auth()
    .with_single_cert(vec![certificate], key)
    .unwrap();
    let acceptor = TlsAcceptor::from(Arc::new(config));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let stop = CancellationToken::new();
    let end = stop.clone();
    let requests: Requests = Arc::new(Mutex::new(vec![]));
    let seen = requests.clone();
    tokio::spawn(async move {
        loop {
            let socket = tokio::select! {_=end.cancelled()=>break,result=listener.accept()=>result.unwrap().0};
            let acceptor = acceptor.clone();
            let seen = seen.clone();
            let end = end.clone();
            tokio::spawn(async move {
                let Ok(socket) = acceptor.accept(socket).await else {
                    return;
                };
                let mut reader = BufReader::new(socket);
                let mut headers = String::new();
                loop {
                    let mut line = String::new();
                    if reader.read_line(&mut line).await.unwrap_or(0) == 0 {
                        return;
                    }
                    if line == "\r\n" {
                        break;
                    }
                    headers.push_str(&line);
                }
                let length = headers
                    .lines()
                    .find_map(|line| {
                        line.to_ascii_lowercase()
                            .strip_prefix("content-length:")
                            .and_then(|s| s.trim().parse::<usize>().ok())
                    })
                    .unwrap_or(0);
                let mut bytes = vec![0; length];
                reader.read_exact(&mut bytes).await.unwrap();
                let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
                let path = headers
                    .lines()
                    .next()
                    .unwrap()
                    .split_whitespace()
                    .nth(1)
                    .unwrap()
                    .to_owned();
                seen.lock()
                    .unwrap()
                    .push((path.clone(), headers.clone(), body));
                let endpoint = path.split('?').next().unwrap();
                let mut extra = String::new();
                let mut status = "200 OK";
                let (kind,body)=match endpoint{
                    "/html"=>("text/html",b"<nav>noise</nav><main><h1>Title</h1><p>Keep <strong>this</strong>.</p></main>".to_vec()),
                    "/text"=>("text/plain; charset=utf-8","雪".repeat(30000).into_bytes()),
                    "/binary"=>("application/octet-stream",b"binary\0bytes".to_vec()),
                    "/large"=>("text/plain",vec![b'x';(5<<20)+10]),
                    "/redir"|"/away"|"/plain"|"/self"|"/key-trap"=>{
                        status="302 Found";let location=match endpoint{"/away"=>format!("https://localhost:{port}/html"),"/plain"=>format!("http://127.0.0.1:{port}/html"),"/self"=>"/self".into(),"/key-trap"=>"/never".into(),_=>"/html".into()};extra=format!("Location: {location}\r\n");("text/plain",vec![])
                    },
                    "/brave"=>("application/json",json!({"web":{"results":[{"title":"Brave","url":"https://example.org/a","description":"Snippet"}]}}).to_string().into_bytes()),
                    "/tavily"=>("application/json",json!({"results":[{"title":"Tavily","url":"https://example.org/b","content":"Snippet"}]}).to_string().into_bytes()),
                    "/exa"=>("application/json",json!({"results":[{"title":"Exa","url":"https://example.org/c","text":"Snippet"}]}).to_string().into_bytes()),
                    "/mcp"=>{let value=json!({"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Title: Free search\nURL: https://example.org/free\nHighlights:\nSome source text\n..."}]}});("text/event-stream",format!("event: message\ndata: {value}\n\n").into_bytes())},
                    "/slow"=>{let mut byte=[0];tokio::select!{_=end.cancelled()=>{},_=reader.read(&mut byte)=>{}}return;},
                    _=>{status="404 Not Found";("text/plain",b"not found".to_vec())},
                };
                let response = format!(
                    "HTTP/1.1 {status}\r\nContent-Type: {kind}\r\nContent-Length: {}\r\n{extra}Connection: close\r\n\r\n",
                    body.len()
                );
                let mut socket = reader.into_inner();
                let _ = socket.write_all(response.as_bytes()).await;
                let _ = socket.write_all(&body).await;
                let _ = socket.shutdown().await;
            });
        }
    });
    Server {
        base: format!("https://127.0.0.1:{port}"),
        requests,
        stop,
    }
}
fn options(server: &Server, local: bool) -> Options {
    Options {
        allow_local: local,
        root_certificates: vec![include_bytes!("data/web-loopback-cert.pem").to_vec()],
        search_endpoints: BTreeMap::from([
            ("brave".into(), format!("{}/brave", server.base)),
            ("tavily".into(), format!("{}/tavily", server.base)),
            ("exa".into(), format!("{}/exa", server.base)),
            (EXA_MCP.into(), format!("{}/mcp", server.base)),
        ]),
    }
}
#[tokio::test]
async fn real_https_fetch_normalizes_filters_redirects_caches_and_pages_unicode() {
    let server = server().await;
    let web = Arc::new(Web::new(options(&server, true)).unwrap());
    let env = Env {
        web: Some(web.clone()),
        max_output: 200000,
        ..Default::default()
    };
    let request = raw(json!({"action":"fetch","url":format!("{}/html",server.base)}));
    let output = tools::run("web", Some(&request), &env, CancellationToken::new()).await;
    assert!(!output.is_error, "{output:?}");
    assert!(
        output.output.contains("# Title\n\nKeep **this**.") && !output.output.contains("noise")
    );
    let count = server.requests.lock().unwrap().len();
    let _ = tools::run("web", Some(&request), &env, CancellationToken::new()).await;
    assert_eq!(server.requests.lock().unwrap().len(), count);
    let page = web
        .fetch(&format!("{}/redir", server.base), CancellationToken::new())
        .await
        .unwrap();
    assert!(page.url.ends_with("/html"));
    for path in ["away", "plain", "self", "binary"] {
        assert!(
            web.fetch(&format!("{}/{path}", server.base), CancellationToken::new())
                .await
                .is_err(),
            "{path}"
        );
    }
    let page = web
        .fetch(&format!("{}/large", server.base), CancellationToken::new())
        .await
        .unwrap();
    assert!(page.truncated);
    assert_eq!(page.text.len(), 100000);
    let output = tools::run(
        "web",
        Some(&raw(
            json!({"action":"fetch","url":format!("{}/text",server.base),"start":20000}),
        )),
        &env,
        CancellationToken::new(),
    )
    .await;
    assert!(!output.is_error, "{output:?}");
    assert!(output.output.contains("showing 20000–30000"));
    assert_eq!(output.output.chars().filter(|c| *c == '雪').count(), 10000);
    let output = tools::run(
        "web",
        Some(&raw(
            json!({"action":"fetch","url":format!("{}/text",server.base),"start":30000}),
        )),
        &env,
        CancellationToken::new(),
    )
    .await;
    assert!(output.is_error && output.output.contains("past the end"));
    let public = Web::new(options(&server, false)).unwrap();
    let before = server.requests.lock().unwrap().len();
    assert!(
        public
            .fetch(&format!("{}/html", server.base), CancellationToken::new())
            .await
            .is_err()
    );
    assert!(
        public
            .fetch(
                &server.base.replace("127.0.0.1", "localhost"),
                CancellationToken::new()
            )
            .await
            .is_err()
    );
    assert_eq!(server.requests.lock().unwrap().len(), before);
    let cancel = CancellationToken::new();
    let c = cancel.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(100)).await;
        c.cancel();
    });
    assert!(
        web.fetch(&format!("{}/slow", server.base), cancel)
            .await
            .is_err()
    );
}
#[tokio::test]
async fn each_search_wire_contract_works_and_redirects_never_receive_the_key() {
    let server = server().await;
    let web = Arc::new(Web::new(options(&server, true)).unwrap());
    for (provider, route, key_header) in [
        ("brave", "/brave", "x-subscription-token: fixture-key"),
        ("tavily", "/tavily", "authorization: bearer fixture-key"),
        ("exa", "/exa", "x-api-key: fixture-key"),
    ] {
        let cfg = SearchConfig {
            provider: provider.into(),
            api_key: "fixture-key".into(),
        };
        let rows = web
            .search(&cfg, "query λ", 3, CancellationToken::new())
            .await
            .unwrap();
        assert_eq!(rows.len(), 1);
        let requests = server.requests.lock().unwrap();
        let (path, headers, body) = requests
            .iter()
            .find(|(p, _, _)| p.starts_with(route))
            .unwrap();
        assert!(headers.to_ascii_lowercase().contains(key_header));
        if provider == "brave" {
            assert!(path.contains("count=3") && path.contains("q=query"));
        } else {
            assert_eq!(body["query"], "query λ");
        }
    }
    let env = Env {
        web: Some(web),
        ..Default::default()
    };
    let output = tools::run(
        "web",
        Some(&raw(json!({"action":"search","query":"sources"}))),
        &env,
        CancellationToken::new(),
    )
    .await;
    assert!(
        !output.is_error
            && output.output.contains("exa-mcp (free, no key)")
            && output.output.contains("Free search"),
        "{output:?}"
    );
    let mut opts = options(&server, true);
    opts.search_endpoints
        .insert("brave".into(), format!("{}/key-trap", server.base));
    let web = Web::new(opts).unwrap();
    assert!(
        web.search(
            &SearchConfig {
                provider: "brave".into(),
                api_key: "fixture-key".into()
            },
            "query",
            1,
            CancellationToken::new()
        )
        .await
        .is_err()
    );
    assert!(
        !server
            .requests
            .lock()
            .unwrap()
            .iter()
            .any(|(path, _, _)| path == "/never")
    );
}
#[tokio::test]
async fn protected_operator_checks_peer_before_disclosing_grants_and_bounds_outcomes() {
    for (reply, owner_offset, expected) in [
        (
            br#"{"ok":true,"result":{"status":"source_merged"}}"#.to_vec(),
            0,
            Output::ok(r#"{"status":"source_merged"}"#),
        ),
        (
            br#"{"ok":false,"reason":"spent"}"#.to_vec(),
            0,
            Output::error("operator refused: spent"),
        ),
        (
            vec![b'x'; 65537],
            0,
            Output::error(
                "operator outcome unavailable; inspect its protected grant receipt before retrying",
            ),
        ),
        (
            b"{}".to_vec(),
            1,
            Output::error("protected fleet operator identity differs"),
        ),
    ] {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("operator.sock");
        let listener = UnixListener::bind(&path).unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
        let received = Arc::new(AtomicUsize::new(0));
        let seen = received.clone();
        let task = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut bytes = vec![];
            stream.read_to_end(&mut bytes).await.unwrap();
            seen.store(bytes.len(), Ordering::SeqCst);
            let _ = stream.write_all(&reply).await;
        });
        let result = tools::operator::call(
            &path,
            b"fixture-grant",
            unsafe { libc::getuid() } + owner_offset,
            CancellationToken::new(),
        )
        .await;
        assert_eq!(result, expected);
        task.await.unwrap();
        assert_eq!(
            received.load(Ordering::SeqCst),
            if owner_offset == 0 { 13 } else { 0 }
        );
    }
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("operator.sock");
    let listener = UnixListener::bind(&path).unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o666)).unwrap();
    let result = tools::operator::call(
        &path,
        b"secret",
        unsafe { libc::getuid() },
        CancellationToken::new(),
    )
    .await;
    assert!(result.is_error && result.output.contains("unavailable"));
    assert!(
        tokio::time::timeout(Duration::from_millis(20), listener.accept())
            .await
            .is_err()
    );
    drop(listener);
    fs::remove_file(&path).unwrap();
    let listener = UnixListener::bind(&path).unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
    let cancel = CancellationToken::new();
    let c = cancel.clone();
    let task = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.unwrap();
        let mut bytes = vec![];
        stream.read_to_end(&mut bytes).await.unwrap();
        c.cancel();
    });
    let result = tools::operator::call(&path, b"fixture", unsafe { libc::getuid() }, cancel).await;
    assert!(result.is_error);
    task.await.unwrap();
}
