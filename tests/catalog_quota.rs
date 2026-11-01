use serde_json::{Value, json};
use stavlos::{
    model::{Token, quota},
    modelsdev::{Catalog, Loader},
    wire::Time,
};
use std::{
    collections::BTreeMap,
    fs,
    os::unix::fs::PermissionsExt,
    time::{Duration, SystemTime},
};
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    net::TcpListener,
};
use tokio_util::sync::CancellationToken;
fn equivalent(actual: &Value, expected: &Value) {
    match (actual, expected) {
        (Value::Number(a), Value::Number(b)) => assert_eq!(a.as_f64(), b.as_f64()),
        (Value::Array(a), Value::Array(b)) => {
            assert_eq!(a.len(), b.len());
            for (a, b) in a.iter().zip(b) {
                equivalent(a, b)
            }
        }
        (Value::Object(a), Value::Object(b)) => {
            assert_eq!(a.keys().collect::<Vec<_>>(), b.keys().collect::<Vec<_>>());
            for (key, value) in a {
                equivalent(value, &b[key]);
            }
        }
        (Value::String(a), Value::String(b)) => {
            if let (Ok(a), Ok(b)) = (
                chrono::DateTime::parse_from_rfc3339(a),
                chrono::DateTime::parse_from_rfc3339(b),
            ) {
                assert_eq!(a, b);
            } else {
                assert_eq!(a, b);
            }
        }
        _ => assert_eq!(actual, expected),
    }
}
#[test]
fn original_quota_shapes_endpoints_and_numeric_prefixes_keep_their_meaning() {
    let now = Time(
        chrono::DateTime::from_timestamp(1_790_000_000, 0)
            .unwrap()
            .fixed_offset(),
    );
    let readings: Value =
        serde_json::from_str(include_str!("data/go-quota-readings.json")).unwrap();
    for case in readings.as_array().unwrap() {
        let parsed = quota::parse(
            case["provider"].as_str().unwrap(),
            case["body"].as_str().unwrap().as_bytes(),
            now,
        );
        assert_eq!(
            parsed.is_err(),
            case["error"].as_bool().unwrap(),
            "{} {}",
            case["provider"],
            case["body"]
        );
        if let Ok(mut usage) = parsed {
            quota::sort_windows(&mut usage);
            let encoded = serde_json::to_value(usage);
            assert_eq!(
                encoded.is_err(),
                case["serialization_error"].as_bool().unwrap_or(false)
            );
            if let Ok(encoded) = encoded {
                equivalent(&encoded, &case["usage"]);
            }
        }
    }
    let urls: Value = serde_json::from_str(include_str!("data/go-quota-urls.json")).unwrap();
    for case in urls.as_array().unwrap() {
        assert_eq!(
            quota::url(
                case["provider"].as_str().unwrap(),
                case["base"].as_str().unwrap()
            ),
            case["url"].as_str().unwrap()
        );
    }
    let numbers: Value = serde_json::from_str(include_str!("data/go-quota-numbers.json")).unwrap();
    for case in numbers.as_array().unwrap() {
        let actual = quota::number(&case["input"]);
        assert_eq!(
            actual.is_some(),
            case["ok"].as_bool().unwrap(),
            "{}",
            case["input"]
        );
        if let Some(actual) = actual {
            let expected = case["value"].as_str().unwrap();
            match expected {
                "NaN" => assert!(actual.is_nan()),
                "+Inf" => assert_eq!(actual, f64::INFINITY),
                "-Inf" => assert_eq!(actual, f64::NEG_INFINITY),
                _ => {
                    let expected = expected.parse::<f64>().unwrap();
                    assert_eq!(actual, expected, "{}", case["input"]);
                    assert_eq!(actual.is_sign_negative(), expected.is_sign_negative());
                }
            }
        }
    }
}
#[test]
fn catalogue_keeps_only_selected_metadata_and_roundtrips_the_original_fixture() {
    let catalog = Catalog::parse(include_bytes!("data/modelsdev-small.json"), &[]).unwrap();
    assert_eq!(catalog.models("ollama-cloud"), ["free-model", "priced"]);
    assert!(catalog.models("unknown").is_empty());
    let info = catalog.model("anthropic", "claude-sonnet-5").unwrap();
    assert_eq!(info.context_window, 1_000_000);
    assert_eq!(info.max_output, 128_000);
    assert_eq!(
        [
            info.input_price,
            info.output_price,
            info.cache_read_price,
            info.cache_write_price
        ],
        [2., 10., 0.2, 2.5]
    );
    assert!(catalog.model("anthropic", "missing").is_none());
    let source=br#"{"openai":{"env":["MUST_NOT_KEEP"],"models":{"gpt-fixture":{"id":"gpt-fixture","name":"Display name","attachment":true,"limit":{"context":400000,"output":128000}}}},"other":{"models":{"x":{"limit":"invalid but not selected"}}}}"#;
    let catalog = Catalog::parse(source, &["openai", "xai"]).unwrap();
    assert!(!catalog.covers(&["openai", "xai"]));
    let encoded = catalog.encode().unwrap();
    let encoded_text = String::from_utf8_lossy(&encoded);
    assert!(!encoded_text.contains("MUST_NOT_KEEP"));
    assert!(!encoded_text.contains("attachment"));
    assert!(!encoded_text.contains("other"));
    let roundtrip = Catalog::parse(&encoded, &[]).unwrap();
    assert_eq!(
        roundtrip.model_name("openai", "gpt-fixture"),
        "Display name"
    );
    assert_eq!(
        roundtrip
            .model("openai", "gpt-fixture")
            .unwrap()
            .context_window,
        400_000
    );
    assert!(Catalog::parse(source, &["missing"]).is_err());
    assert!(Catalog::parse(b"{}", &[]).is_err());
    let fallback = Catalog::parse(stavlos::modelsdev::FALLBACK, &[]).unwrap();
    assert!(fallback.covers(&["openai", "xai"]));
}
async fn one_response(
    status: u16,
    body: String,
) -> (String, tokio::task::JoinHandle<BTreeMap<String, String>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/usage", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut reader = BufReader::new(&mut socket);
        let mut line = String::new();
        reader.read_line(&mut line).await.unwrap();
        assert!(line.starts_with("GET "));
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
        drop(reader);
        socket.write_all(format!("HTTP/1.1 {status} Fixture\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",body.len()).as_bytes()).await.unwrap();
        headers
    });
    (url, server)
}
#[tokio::test]
async fn cache_loading_distinguishes_fresh_stale_incomplete_and_fallback_without_blocking_on_special_files()
 {
    let owned = tempfile::tempdir().unwrap();
    let cache = owned.path().join("models.json");
    let mut loader = Loader::new(cache.clone()).unwrap();
    loader.client = stavlos::httpx::builder()
        .no_proxy()
        .timeout(Duration::from_millis(100))
        .build()
        .unwrap();
    loader.url = "http://127.0.0.1:9/unavailable".into();
    let (catalog, stale) = loader
        .load(&["openai", "xai"], &CancellationToken::new())
        .await
        .unwrap();
    assert!(stale);
    assert!(catalog.covers(&["openai", "xai"]));
    let full=br#"{"openai":{"models":{"cached":{"name":"Cache fixture","limit":{"context":123,"output":7}}}},"xai":{"models":{}}}"#;
    stavlos::modelsdev::write_cache(&cache, full);
    assert_eq!(
        fs::metadata(&cache).unwrap().permissions().mode() & 0o777,
        0o600
    );
    let (catalog, stale) = loader
        .load(&["openai", "xai"], &CancellationToken::new())
        .await
        .unwrap();
    assert!(!stale);
    assert_eq!(catalog.model_name("openai", "cached"), "Cache fixture");
    fs::File::open(&cache)
        .unwrap()
        .set_times(
            fs::FileTimes::new()
                .set_modified(SystemTime::now() - Duration::from_secs(48 * 60 * 60)),
        )
        .unwrap();
    let (_, stale) = loader
        .load(&["openai", "xai"], &CancellationToken::new())
        .await
        .unwrap();
    assert!(stale);
    let partial = br#"{"openai":{"models":{"partial":{"name":"Kept incomplete"}}}}"#;
    stavlos::modelsdev::write_cache(&cache, partial);
    let (catalog, stale) = loader
        .load(&["openai", "xai"], &CancellationToken::new())
        .await
        .unwrap();
    assert!(stale);
    assert!(!catalog.covers(&["openai", "xai"]));
    assert_eq!(catalog.model_name("openai", "partial"), "Kept incomplete");
    let (url, server) = one_response(200, String::from_utf8(full.to_vec()).unwrap()).await;
    loader.url = url;
    let (catalog, stale) = loader
        .load(&["openai", "xai"], &CancellationToken::new())
        .await
        .unwrap();
    assert!(!stale);
    assert!(catalog.covers(&["openai", "xai"]));
    assert_eq!(server.await.unwrap()["user-agent"], "stavlos");
    assert!(
        Catalog::parse(&fs::read(&cache).unwrap(), &[])
            .unwrap()
            .covers(&["openai", "xai"])
    );
    let fifo = owned.path().join("fifo");
    let path = std::ffi::CString::new(fifo.as_os_str().as_encoded_bytes()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(path.as_ptr(), 0o600) }, 0);
    assert!(stavlos::modelsdev::read_cache(&fifo).is_none());
}
#[tokio::test]
async fn quota_http_uses_each_subscriptions_auth_and_reports_only_keys_for_unknown_shapes() {
    let client = stavlos::httpx::builder().no_proxy().build().unwrap();
    let token = Token {
        access: "fixture-private-credential".into(),
        account_id: "fixture-account".into(),
    };
    for (provider, body) in [
        (
            "openai",
            json!({"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}),
        ),
        (
            "zai",
            json!({"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":12}]}),
        ),
        ("xai", json!({"config":{"creditUsagePercent":9}})),
        ("kimi", json!({"usage":{"limit":100,"used":5}})),
    ] {
        let (url, server) = one_response(200, body.to_string()).await;
        let usage = quota::fetch(&client, provider, &url, &token, &CancellationToken::new())
            .await
            .unwrap();
        assert_eq!(usage.windows.len(), 1);
        assert!(!usage.observed.is_zero());
        let headers = server.await.unwrap();
        assert_eq!(
            headers["authorization"],
            if provider == "zai" {
                token.access.clone()
            } else {
                format!("Bearer {}", token.access)
            }
        );
        if provider == "openai" {
            assert_eq!(headers["chatgpt-account-id"], "fixture-account");
            assert_eq!(headers["originator"], "stavlos");
        }
        if provider == "xai" {
            assert_eq!(headers["x-grok-client-surface"], "grok-build");
            assert_eq!(headers["x-grok-client-version"], "1.0.0");
        }
    }
    let (url, server) = one_response(200, json!({"surprise":true}).to_string()).await;
    let error = quota::fetch(&client, "kimi", &url, &token, &CancellationToken::new())
        .await
        .unwrap_err();
    assert!(error.to_string().contains("keys: surprise"));
    assert!(!error.to_string().contains(&token.access));
    server.await.unwrap();
}
