use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use stavlos::{
    nativeapi,
    oauth::{self, Flow},
    wire::Time,
};
use std::{
    collections::BTreeMap, fs, os::unix::fs::PermissionsExt, path::PathBuf, sync::Arc,
    time::Duration,
};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::TcpListener,
};
use tokio_util::sync::CancellationToken;

fn jwt(claims: Value) -> String {
    format!(
        "h.{}.s",
        URL_SAFE_NO_PAD.encode(serde_json::to_vec(&claims).unwrap())
    )
}
fn client() -> oauth::Client {
    oauth::Client {
        http: stavlos::httpx::builder()
            .no_proxy()
            .timeout(Duration::from_secs(3))
            .build()
            .unwrap(),
        poll_margin: Duration::ZERO,
    }
}
fn api() -> nativeapi::Program {
    let root = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    let mut candidates = Vec::new();
    if let Some(path) = std::env::var_os("COOKBOOK_NATIVE_API_BINARY") {
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
    candidates.push(root.join("crates/api/target/debug/stavlos-api"));
    candidates.push(
        std::env::current_exe()
            .unwrap()
            .parent()
            .unwrap()
            .parent()
            .unwrap()
            .join("stavlos-api"),
    );
    for path in candidates {
        if path.is_file() {
            return nativeapi::Program::external(path).unwrap();
        }
    }
    panic!(
        "build the independent API before the owner integration tests: cargo build --locked --manifest-path crates/api/Cargo.toml --bin stavlos-api"
    )
}
#[test]
fn original_unverified_claims_keep_display_precedence_partial_fields_and_nil_arrays() {
    let cases: Value = serde_json::from_str(include_str!("data/go-oauth-claims.json")).unwrap();
    for case in cases.as_array().unwrap() {
        let claims = oauth::claims(case["token"].as_str().unwrap());
        assert_eq!(
            serde_json::to_value(&claims).unwrap(),
            case["claims"],
            "{}",
            case["token"]
        );
        assert_eq!(claims.account_id(), case["account_id"].as_str().unwrap());
    }
    let now = Time(
        chrono::DateTime::from_timestamp(1_700_000_000, 0)
            .unwrap()
            .fixed_offset(),
    );
    let token = jwt(json!({"exp":1_700_000_120}));
    assert!(!oauth::expiring_at(&token, Duration::from_secs(120), now));
    assert!(oauth::expiring_at(&token, Duration::from_secs(121), now));
    assert!(!oauth::expiring_at("opaque", Duration::from_secs(999), now));
    let token = oauth::Tokens {
        access: "never-debug-access".into(),
        refresh: "never-debug-refresh".into(),
        id_token: "never-debug-id".into(),
        ..Default::default()
    };
    assert!(!format!("{token:?}").contains("never-debug"));
}
#[tokio::test]
async fn pasted_keys_stay_bound_to_their_own_plans_and_cancel_or_expire_without_network_calls() {
    for flow in [oauth::KeyFlow::zai(), oauth::KeyFlow::kimi()] {
        let cancel = CancellationToken::new();
        assert!(
            flow.start(oauth::METHOD_DEVICE.into(), cancel.clone())
                .await
                .is_err()
        );
        let pending = flow.start(String::new(), cancel.clone()).await.unwrap();
        assert_eq!(pending.provider, flow.provider());
        assert_eq!(pending.method, oauth::METHOD_API_KEY);
        assert_eq!(flow.methods()[0].id, oauth::METHOD_API_KEY);
        assert!(!pending.url.is_empty());
        assert!(pending.deliver("  ").is_err());
        pending.deliver("  fixture-owned-key  ").unwrap();
        assert!(pending.deliver("second").is_err());
        let tokens = flow.wait(pending, cancel).await.unwrap();
        assert_eq!(tokens.access, "fixture-owned-key");
        assert!(tokens.refresh.is_empty());
        assert!(tokens.expires_at.is_zero());
        assert!(
            flow.refresh(String::new(), CancellationToken::new())
                .await
                .is_err()
        );
        let cancel = CancellationToken::new();
        let pending = flow.start(String::new(), cancel.clone()).await.unwrap();
        cancel.cancel();
        assert!(matches!(
            flow.wait(pending, cancel).await,
            Err(oauth::Error::Cancelled)
        ));
        let mut pending = flow
            .start(String::new(), CancellationToken::new())
            .await
            .unwrap();
        Arc::get_mut(&mut pending).unwrap().expires_in = Duration::from_millis(1);
        assert!(matches!(
            flow.wait(pending, CancellationToken::new()).await,
            Err(oauth::Error::Expired)
        ));
    }
}
struct Incoming {
    path: String,
    headers: BTreeMap<String, String>,
    raw: Vec<u8>,
}
impl Incoming {
    fn json(&self) -> Value {
        serde_json::from_slice(&self.raw).unwrap()
    }
    fn form(&self) -> BTreeMap<String, String> {
        reqwest::Url::parse(&format!(
            "http://fixture.invalid/?{}",
            String::from_utf8_lossy(&self.raw)
        ))
        .unwrap()
        .query_pairs()
        .map(|(key, value)| (key.into_owned(), value.into_owned()))
        .collect()
    }
}
async fn incoming(socket: &mut tokio::net::TcpStream) -> Incoming {
    let mut reader = BufReader::new(socket);
    let mut line = String::new();
    reader.read_line(&mut line).await.unwrap();
    let path = line.split_whitespace().nth(1).unwrap().into();
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
    let mut raw = vec![0; headers["content-length"].parse::<usize>().unwrap()];
    reader.read_exact(&mut raw).await.unwrap();
    Incoming { path, headers, raw }
}
async fn issuer(replies: Vec<(u16, Value)>) -> (String, tokio::task::JoinHandle<Vec<Incoming>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let mut requests = vec![];
        for (status, body) in replies {
            let (mut socket, _) = listener.accept().await.unwrap();
            requests.push(incoming(&mut socket).await);
            let body = body.to_string();
            socket.write_all(format!("HTTP/1.1 {status} Fixture\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",body.len()).as_bytes()).await.unwrap();
        }
        requests
    });
    (base, server)
}
#[tokio::test]
async fn chatgpt_device_login_polls_exchanges_pkce_and_keeps_fallback_refresh_tokens() {
    let id = jwt(json!({"email":"fixture@example.invalid","chatgpt_account_id":"fixture-account"}));
    let(base,server)=issuer(vec![(200,json!({"device_auth_id":"device-fixture","user_code":"abcd-efgh","interval":"0"})),(404,Value::Null),(200,json!({"authorization_code":"code-fixture","code_verifier":"verifier-fixture"})),(200,json!({"access_token":"access-fixture","refresh_token":"refresh-fixture","id_token":id,"expires_in":3600})),(200,json!({"access_token":"rotated-fixture","expires_in":100}))]).await;
    let mut flow = oauth::ChatGpt::new(client(), None);
    flow.issuer = base;
    let mut pending = flow
        .start(oauth::METHOD_DEVICE.into(), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(pending.code, "ABCD-EFGH");
    assert_eq!(pending.interval, Duration::from_secs(5));
    Arc::get_mut(&mut pending).unwrap().interval = Duration::ZERO;
    let tokens = flow.wait(pending, CancellationToken::new()).await.unwrap();
    assert_eq!(tokens.access, "access-fixture");
    assert_eq!(tokens.account_id, "fixture-account");
    assert_eq!(tokens.email, "fixture@example.invalid");
    let refreshed = flow
        .refresh(tokens.refresh, CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(refreshed.access, "rotated-fixture");
    assert_eq!(refreshed.refresh, "refresh-fixture");
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 5);
    assert_eq!(requests[0].path, "/api/accounts/deviceauth/usercode");
    assert_eq!(
        requests[0].json()["client_id"],
        "app_EMoamEEZ73f0CkXaXp7hrann"
    );
    assert_eq!(requests[1].json()["user_code"], "ABCD-EFGH");
    let form = requests[3].form();
    assert_eq!(form["code"], "code-fixture");
    assert_eq!(form["code_verifier"], "verifier-fixture");
    assert!(form["redirect_uri"].ends_with("/deviceauth/callback"));
    assert_eq!(requests[4].form()["refresh_token"], "refresh-fixture");
    assert!(
        requests
            .iter()
            .all(|request| request.headers["user-agent"].starts_with("stavlos/0.1 (linux "))
    );
}
#[tokio::test]
async fn grok_device_login_honors_pending_slow_down_refresh_denial_and_expiration() {
    let(base,server)=issuer(vec![(200,json!({"device_code":"dc","user_code":"wxyz","verification_uri":"https://fixture.invalid/device","verification_uri_complete":"https://fixture.invalid/device?code=wxyz","expires_in":60,"interval":0})),(400,json!({"error":"authorization_pending"})),(400,json!({"error":"slow_down"})),(200,json!({"access_token":"a1","refresh_token":"r1","expires_in":3600})),(200,json!({"access_token":"a2","refresh_token":"r2","expires_in":10}))]).await;
    let mut flow = oauth::Grok::new(client());
    flow.device_url = format!("{base}/device/code");
    flow.token_url = format!("{base}/token");
    let mut pending = flow
        .start(String::new(), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(pending.code, "WXYZ");
    assert!(pending.url.contains("?code=wxyz"));
    Arc::get_mut(&mut pending).unwrap().interval = Duration::ZERO;
    let tokens = tokio::time::timeout(
        Duration::from_secs(10),
        flow.wait(pending, CancellationToken::new()),
    )
    .await
    .unwrap()
    .unwrap();
    assert_eq!(tokens.access, "a1");
    let tokens = flow
        .refresh(tokens.refresh, CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(tokens.access, "a2");
    assert_eq!(tokens.refresh, "r2");
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 5);
    assert_eq!(
        requests[0].form()["client_id"],
        "b1a00492-073a-47ea-816f-4c329264a828"
    );
    assert!(requests[0].form()["scope"].contains("grok-cli:access"));
    assert_eq!(requests[1].form()["device_code"], "dc");
    for (error, denied) in [
        ("access_denied", true),
        ("authorization_denied", true),
        ("expired_token", false),
    ] {
        let(base,server)=issuer(vec![(200,json!({"device_code":"dc","user_code":"code","verification_uri":"https://fixture.invalid/device"})),(400,json!({"error":error}))]).await;
        flow.device_url = format!("{base}/device/code");
        flow.token_url = format!("{base}/token");
        let pending = flow
            .start(String::new(), CancellationToken::new())
            .await
            .unwrap();
        let result = flow.wait(pending, CancellationToken::new()).await;
        assert!(if denied {
            matches!(result, Err(oauth::Error::Denied))
        } else {
            matches!(result, Err(oauth::Error::Expired))
        });
        assert_eq!(server.await.unwrap().len(), 2);
    }
}
#[tokio::test]
async fn browser_pkce_uses_the_independent_axum_callback_and_closing_releases_its_port() {
    let id = jwt(json!({"email":"fixture@example.invalid","chatgpt_account_id":"browser-account"}));
    let(base,server)=issuer(vec![(200,json!({"access_token":"browser-access","refresh_token":"browser-refresh","id_token":id,"expires_in":10}))]).await;
    let probe = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = probe.local_addr().unwrap().port();
    drop(probe);
    let mut flow = oauth::ChatGpt::new(client(), Some(api()));
    flow.issuer = base;
    flow.port = port;
    let pending = flow
        .start(String::new(), CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(pending.method, oauth::METHOD_BROWSER);
    let authorization = reqwest::Url::parse(&pending.url).unwrap();
    let query = authorization
        .query_pairs()
        .map(|(key, value)| (key.into_owned(), value.into_owned()))
        .collect::<BTreeMap<_, _>>();
    assert_eq!(query["code_challenge_method"], "S256");
    assert_eq!(query["originator"], "stavlos");
    assert!(query["state"].len() >= 32);
    assert!(
        flow.start(String::new(), CancellationToken::new())
            .await
            .is_err(),
        "another login must not replace the listening callback"
    );
    let browser = stavlos::httpx::builder().no_proxy().build().unwrap();
    for path in [
        "/auth/callback?code=x&state=bad",
        "/auth/callback?error=access_denied&error_description=%3Cb%3Ebad",
        "/cancel",
    ] {
        let response = browser
            .get(format!("http://127.0.0.1:{port}{path}"))
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), 400);
        assert!(!response.text().await.unwrap().contains("<b>"));
    }
    let response = browser
        .get(format!(
            "http://127.0.0.1:{port}/auth/callback?code=browser-code&state={}",
            query["state"]
        ))
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    assert!(response.text().await.unwrap().contains("Signed in"));
    let tokens = flow.wait(pending, CancellationToken::new()).await.unwrap();
    assert_eq!(tokens.access, "browser-access");
    assert_eq!(tokens.account_id, "browser-account");
    let requests = server.await.unwrap();
    let form = requests[0].form();
    assert_eq!(form["code"], "browser-code");
    assert_eq!(
        URL_SAFE_NO_PAD.encode(Sha256::digest(form["code_verifier"].as_bytes())),
        query["code_challenge"]
    );
    assert_eq!(
        form["redirect_uri"],
        format!("http://localhost:{port}/auth/callback")
    );
    let held = std::net::TcpListener::bind((std::net::Ipv4Addr::LOCALHOST, port)).unwrap();
    drop(held);
    let abandoned = flow
        .start(oauth::METHOD_BROWSER.into(), CancellationToken::new())
        .await
        .unwrap();
    abandoned.close().await;
    let held = std::net::TcpListener::bind((std::net::Ipv4Addr::LOCALHOST, port)).unwrap();
    drop(held);
    let pending = flow
        .start(oauth::METHOD_BROWSER.into(), CancellationToken::new())
        .await
        .unwrap();
    let cancel = CancellationToken::new();
    cancel.cancel();
    assert!(matches!(
        flow.wait(pending, cancel).await,
        Err(oauth::Error::Cancelled)
    ));
    let _held = std::net::TcpListener::bind((std::net::Ipv4Addr::LOCALHOST, port)).unwrap();
}
#[tokio::test]
async fn callback_state_and_results_use_private_pipes_and_bad_readiness_is_refused() {
    let owned = tempfile::tempdir().unwrap();
    let path = owned.path().join("fixture-api");
    fs::write(
        &path,
        r#"#!/usr/bin/python3
import json,pathlib,sys
root=pathlib.Path(__file__).parent
root.joinpath('argv.json').write_text(json.dumps(sys.argv))
state=json.loads(sys.stdin.readline())['state']
root.joinpath('state-proof').write_text(str(len(state)))
print(json.dumps({'ready':True}),flush=True)
print(json.dumps({'code':'fixture-code','error':''}),flush=True)
"#,
    )
    .unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o700)).unwrap();
    let program = nativeapi::Program::external(path.clone()).unwrap();
    let state = "fixture-private-authorization-state";
    let callback = nativeapi::Callback::start(&program, 1455, state, &CancellationToken::new())
        .await
        .unwrap();
    let result = callback.result().await;
    assert_eq!(result.code, "fixture-code");
    assert!(!format!("{result:?}").contains("fixture-code"));
    callback.close().await;
    let args = fs::read_to_string(owned.path().join("argv.json")).unwrap();
    assert!(!args.contains(state));
    assert!(!args.contains("fixture-code"));
    assert!(args.contains("--parent-pid"));
    assert_eq!(
        fs::read_to_string(owned.path().join("state-proof")).unwrap(),
        state.len().to_string()
    );
    fs::write(&path,"#!/usr/bin/python3\nimport json,sys\njson.loads(sys.stdin.readline())\nprint('{\"ready\":false}',flush=True)\n").unwrap();
    assert!(
        nativeapi::Callback::start(&program, 1455, state, &CancellationToken::new())
            .await
            .is_err()
    );
    assert!(nativeapi::Program::external("relative-api".into()).is_err());
}
#[tokio::test]
async fn device_errors_explain_missing_authorization_and_cancel_before_connecting() {
    let (base, server) = issuer(vec![(
        403,
        json!({"error":"device authorization disabled"}),
    )])
    .await;
    let mut flow = oauth::ChatGpt::new(client(), None);
    flow.issuer = base;
    let error = flow
        .start(oauth::METHOD_DEVICE.into(), CancellationToken::new())
        .await
        .err()
        .unwrap();
    assert!(
        error
            .to_string()
            .contains("Device code authorization for Codex")
    );
    assert_eq!(server.await.unwrap().len(), 1);
    let cancel = CancellationToken::new();
    cancel.cancel();
    assert!(matches!(
        flow.start(oauth::METHOD_DEVICE.into(), cancel).await,
        Err(oauth::Error::Cancelled)
    ));
}
