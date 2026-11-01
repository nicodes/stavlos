use serde_json::json;
use stavlos::{
    auth::{Credential, Store},
    model::{
        self, CallError, Capabilities, CompleteFuture, DeltaSink, Model, Provider,
        registry::{self, Endpoints, Registry, UsageTracker},
    },
    modelsdev::Catalog,
    oauth::{self, Flow},
    wire::Time,
};
use std::{
    collections::BTreeMap,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};
use tokio_util::sync::CancellationToken;
fn future_time(seconds: i64) -> Time {
    Time((chrono::Utc::now() + chrono::TimeDelta::seconds(seconds)).fixed_offset())
}
fn catalog() -> Arc<Catalog> {
    Arc::new(Catalog::parse(json!({
    "openai":{"models":{"gpt-5.4":{"name":"GPT fixture","limit":{"context":400000,"output":128000},"cost":{"input":2,"output":10}},"gpt-5.5-pro":{},"gpt-5.6":{},"gpt-6":{}}},
    "xai":{"models":{"grok-4":{},"grok-imagine":{}}},"zai-coding-plan":{"models":{"glm-5":{},"unrelated":{}}},"kimi-code-plan-global":{"models":{"kimi-k2":{}}},
    "fake":{"models":{"m":{"limit":{"context":1234,"output":77},"cost":{"input":3,"output":6}}}}
}).to_string().as_bytes(),&[]).unwrap())
}
struct FakeProvider {
    name: String,
    opens: Arc<AtomicUsize>,
}
struct FakeModel;
impl Model for FakeModel {
    fn complete(
        &self,
        _: model::Request,
        _: Option<DeltaSink>,
        _: CancellationToken,
    ) -> CompleteFuture<'_> {
        Box::pin(async { Ok(model::Response::default()) })
    }
}
impl Provider for FakeProvider {
    fn name(&self) -> &str {
        &self.name
    }
    fn open(&self, _: &str) -> Result<Arc<dyn Model>, CallError> {
        self.opens.fetch_add(1, Ordering::SeqCst);
        Ok(Arc::new(FakeModel))
    }
    fn variants(&self, _: &str) -> Vec<String> {
        vec!["fixture-variant".into()]
    }
    fn capabilities(&self, _: &str) -> Capabilities {
        Capabilities {
            ignores_max_tokens: true,
        }
    }
}
#[test]
fn subscription_lists_filter_supported_models_keep_prices_zero_and_cache_opened_models() {
    let owned = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open(owned.path().join("auth.json")));
    let registry =
        Registry::new(Some(catalog()), Some(store), oauth::flows(None).unwrap()).unwrap();
    assert_eq!(
        registry
            .list()
            .iter()
            .map(|status| status.id.as_str())
            .collect::<Vec<_>>(),
        ["openai", "xai", "zai", "kimi"]
    );
    assert!(registry.providers().is_empty());
    assert!(registry.models("", false).is_empty());
    let models = registry.models("", true);
    assert_eq!(
        models
            .iter()
            .map(|model| model.id.as_str())
            .collect::<Vec<_>>(),
        [
            "openai/gpt-5.4",
            "openai/gpt-6",
            "xai/grok-4",
            "zai/glm-5",
            "kimi/kimi-k2"
        ]
    );
    assert!(
        models
            .iter()
            .all(|model| model.info.input_price == 0. && model.info.output_price == 0.)
    );
    assert_eq!(registry.status("openai").unwrap().models, 2);
    assert!(registry.check("openai/gpt-5.4").is_err());
    registry
        .save_login(
            "openai",
            &oauth::Tokens {
                access: "fixture-access".into(),
                refresh: "fixture-refresh".into(),
                expires_at: future_time(3600),
                email: "fixture@example.invalid".into(),
                ..Default::default()
            },
        )
        .unwrap();
    assert_eq!(registry.providers(), ["openai"]);
    assert_eq!(
        registry.status("openai").unwrap().account,
        "fixture@example.invalid"
    );
    let (_, info) = registry.resolve("openai/gpt-5.4").unwrap();
    assert!(info.capabilities.ignores_max_tokens);
    assert_eq!(info.context_window, 400_000);
    assert_eq!(info.input_price, 0.);
    let opens = Arc::new(AtomicUsize::new(0));
    registry
        .register(Arc::new(FakeProvider {
            name: "fake".into(),
            opens: opens.clone(),
        }))
        .unwrap();
    assert!(
        registry
            .register(Arc::new(FakeProvider {
                name: "fake".into(),
                opens: opens.clone()
            }))
            .is_err()
    );
    let (first, info) = registry.resolve("fake/m").unwrap();
    let (second, _) = registry.resolve("fake/m").unwrap();
    assert!(Arc::ptr_eq(&first, &second));
    assert_eq!(opens.load(Ordering::SeqCst), 1);
    assert_eq!(info.context_window, 1234);
    assert_eq!(info.input_price, 3.);
    assert!(info.capabilities.ignores_max_tokens);
    assert_eq!(registry.variants("fake/m"), ["fixture-variant"]);
    registry.with_endpoints(Endpoints::default());
    let (third, _) = registry.resolve("fake/m").unwrap();
    assert!(!Arc::ptr_eq(&first, &third));
    assert_eq!(opens.load(Ordering::SeqCst), 2);
    registry.disconnect("openai").unwrap();
    assert!(!registry.status("openai").unwrap().connected);
    assert!(
        registry
            .check("unknown/m")
            .unwrap_err()
            .to_string()
            .contains("Kimi For Coding")
    );
    for (id, allowed) in [
        ("gpt-5.4", true),
        ("gpt-5.4-latest", false),
        ("gpt-5.3-codex-spark", true),
        ("gpt-5.5", true),
        ("gpt-5.5-pro", false),
        ("gpt-5.6", false),
        ("gpt-5.6-mini", true),
        ("gpt-6", true),
        ("gpt-6-pro", false),
        ("gpt-٥", false),
    ] {
        assert_eq!(registry::allowed("openai", id), allowed, "{id}");
    }
}
struct RefreshFlow {
    calls: Arc<AtomicUsize>,
}
impl Flow for RefreshFlow {
    fn provider(&self) -> &str {
        "openai"
    }
    fn label(&self) -> &str {
        "fixture subscription"
    }
    fn methods(&self) -> Vec<oauth::Method> {
        vec![]
    }
    fn start(&self, _: String, _: CancellationToken) -> oauth::FlowFuture<'_, Arc<oauth::Pending>> {
        Box::pin(async { Err(oauth::Error::Message("fixture has no sign-in".into())) })
    }
    fn wait(
        &self,
        _: Arc<oauth::Pending>,
        _: CancellationToken,
    ) -> oauth::FlowFuture<'_, oauth::Tokens> {
        Box::pin(async { Err(oauth::Error::Message("fixture has no sign-in".into())) })
    }
    fn refresh(
        &self,
        refresh: String,
        cancel: CancellationToken,
    ) -> oauth::FlowFuture<'_, oauth::Tokens> {
        Box::pin(async move {
            assert_eq!(refresh, "seed-refresh");
            self.calls.fetch_add(1, Ordering::SeqCst);
            tokio::select! {biased;_=cancel.cancelled()=>return Err(oauth::Error::Cancelled),_=tokio::time::sleep(Duration::from_millis(10))=>{}}
            Ok(oauth::Tokens {
                access: "rotated-fixture".into(),
                refresh: "next-refresh".into(),
                expires_at: future_time(3600),
                ..Default::default()
            })
        })
    }
}
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn concurrent_calls_refresh_once_preserve_identity_and_pick_up_external_credentials() {
    let owned = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open(owned.path().join("auth.json")));
    store
        .set(
            "openai",
            Credential {
                kind: "oauth".into(),
                access: "expired-fixture".into(),
                refresh: "seed-refresh".into(),
                expires: 1,
                account_id: "preserved-account".into(),
                email: "fixture@example.invalid".into(),
                ..Default::default()
            },
        )
        .unwrap();
    let calls = Arc::new(AtomicUsize::new(0));
    let flows = BTreeMap::from([(
        "openai".into(),
        Arc::new(RefreshFlow {
            calls: calls.clone(),
        }) as Arc<dyn Flow>,
    )]);
    let registry = Registry::new(None, Some(store.clone()), flows).unwrap();
    let source = registry.token_source("openai");
    let mut requests = vec![];
    for _ in 0..16 {
        let source = source.clone();
        requests.push(tokio::spawn(async move {
            source(CancellationToken::new()).await.unwrap()
        }));
    }
    for request in requests {
        let token = request.await.unwrap();
        assert_eq!(token.access, "rotated-fixture");
        assert_eq!(token.account_id, "preserved-account");
    }
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let saved = store.get("openai").unwrap();
    assert_eq!(saved.refresh, "next-refresh");
    assert_eq!(saved.email, "fixture@example.invalid");
    assert!(saved.expires > chrono::Utc::now().timestamp_millis());
    store
        .set(
            "openai",
            Credential {
                kind: "apikey".into(),
                key: "external-key-fixture".into(),
                ..Default::default()
            },
        )
        .unwrap();
    assert_eq!(
        source(CancellationToken::new()).await.unwrap().access,
        "external-key-fixture"
    );
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    registry.disconnect("openai").unwrap();
    assert!(source(CancellationToken::new()).await.is_err());
    registry
        .save_login(
            "zai",
            &oauth::Tokens {
                access: "zai-key-fixture".into(),
                ..Default::default()
            },
        )
        .unwrap();
    let saved = store.get("zai").unwrap();
    assert_eq!(saved.kind, "apikey");
    assert_eq!(saved.key, "zai-key-fixture");
    assert!(saved.refresh.is_empty());
}
#[test]
fn usage_observations_keep_newer_readings_plan_names_refusals_and_reentrant_hooks() {
    let tracker = Arc::new(UsageTracker::default());
    tracker.mark_limited("none", Time::default());
    assert!(tracker.all().is_empty());
    let calls = Arc::new(Mutex::new(vec![]));
    let received = calls.clone();
    let weak = Arc::downgrade(&tracker);
    tracker.on_usage(Some(Arc::new(move |provider, usage| {
        assert!(weak.upgrade().unwrap().all().contains_key(provider));
        received.lock().unwrap().push(usage);
    })));
    let timestamp = |seconds| {
        Time(
            chrono::DateTime::from_timestamp(seconds, 0)
                .unwrap()
                .fixed_offset(),
        )
    };
    tracker.observe(
        "openai",
        model::PlanUsage {
            plan: "pro".into(),
            observed: timestamp(200),
            ..Default::default()
        },
    );
    tracker.mark_limited("openai", timestamp(500));
    tracker.observe(
        "openai",
        model::PlanUsage {
            observed: timestamp(100),
            ..Default::default()
        },
    );
    assert_eq!(calls.lock().unwrap().len(), 1);
    tracker.observe(
        "openai",
        model::PlanUsage {
            observed: timestamp(300),
            ..Default::default()
        },
    );
    let usage = tracker.all().remove("openai").unwrap();
    assert_eq!(usage.plan, "pro");
    assert_eq!(usage.limited_until, timestamp(500));
    assert_eq!(usage.observed, timestamp(300));
    tracker.seed(
        "openai",
        model::PlanUsage {
            plan: "older".into(),
            observed: timestamp(250),
            ..Default::default()
        },
    );
    assert_eq!(tracker.all()["openai"].plan, "pro");
    tracker.seed(
        "openai",
        model::PlanUsage {
            plan: "newer".into(),
            observed: timestamp(400),
            ..Default::default()
        },
    );
    assert_eq!(tracker.all()["openai"].plan, "newer");
    assert_eq!(calls.lock().unwrap().len(), 2);
}
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn quota_polling_is_explicit_single_flight_and_rate_limited_after_failures_too() {
    use tokio::{
        io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
        net::TcpListener,
    };
    let owned = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open(owned.path().join("auth.json")));
    store
        .set(
            "zai",
            Credential {
                kind: "apikey".into(),
                key: "fixture-usage-key".into(),
                ..Default::default()
            },
        )
        .unwrap();
    let registry = Registry::new(None, Some(store), BTreeMap::new()).unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}", listener.local_addr().unwrap());
    registry.with_endpoints(Endpoints {
        zai: base,
        ..Default::default()
    });
    registry
        .poll_plan_usage("zai", Duration::ZERO, &CancellationToken::new())
        .await
        .unwrap();
    assert!(
        tokio::time::timeout(Duration::from_millis(10), listener.accept())
            .await
            .is_err()
    );
    registry.enable_plan_polling();
    let server = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut reader = BufReader::new(&mut socket);
        let mut line = String::new();
        reader.read_line(&mut line).await.unwrap();
        assert!(line.starts_with("GET /api/monitor/usage/quota/limit "));
        let mut auth = false;
        loop {
            line.clear();
            reader.read_line(&mut line).await.unwrap();
            if line == "\r\n" {
                break;
            }
            if line.to_lowercase().starts_with("authorization: ") {
                assert_eq!(
                    line.trim().split_once(':').unwrap().1.trim(),
                    "fixture-usage-key"
                );
                auth = true;
            }
        }
        assert!(auth);
        drop(reader);
        socket
            .write_all(b"HTTP/1.1 500 Fixture\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            .await
            .unwrap();
    });
    let mut polls = vec![];
    for _ in 0..16 {
        let registry = registry.clone();
        polls.push(tokio::spawn(async move {
            registry
                .poll_plan_usage("zai", Duration::from_secs(300), &CancellationToken::new())
                .await
        }));
    }
    let mut failed = 0;
    for poll in polls {
        if poll.await.unwrap().is_err() {
            failed += 1;
        }
    }
    assert_eq!(failed, 1);
    server.await.unwrap();
    registry
        .poll_plan_usage("zai", Duration::from_secs(300), &CancellationToken::new())
        .await
        .unwrap();
}
#[test]
fn wire_objects_refuse_positional_arrays_and_empty_mutations_keep_nil_slices() {
    assert!(serde_json::from_str::<model::Usage>("[]").is_err());
    assert!(serde_json::from_str::<stavlos::protocol::Request>("[]").is_err());
    assert!(serde_json::from_str::<stavlos::protocol::Error>("[]").is_err());
    assert!(serde_json::from_str::<stavlos::event::Event>("[]").is_err());
    let mut slice = stavlos::wire::Slice::<String>::default();
    slice.sort();
    assert_eq!(
        serde_json::to_value(slice).unwrap(),
        serde_json::Value::Null
    );
}
