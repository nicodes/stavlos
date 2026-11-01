use serde_json::{Value, json};
use stavlos::{
    model::{
        self, CallError, Delta, DeltaSink, Provider as _, Request, Response,
        chatcompletions::{self, Traits},
        codex,
        stream::{Codec, Feed},
    },
    wire::Slice,
};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::TcpListener,
};
use tokio_util::sync::CancellationToken;

#[derive(Default, serde::Deserialize)]
#[serde(default, rename_all = "PascalCase")]
struct RequestInput {
    model: String,
    system: String,
    messages: Slice<model::Message>,
    tools: Slice<model::ToolDef>,
    max_tokens: i64,
    variant: String,
    cache_key: String,
}
fn request(value: &Value) -> Request {
    let value: RequestInput = serde_json::from_str(value.as_str().unwrap()).unwrap();
    Request {
        model: value.model,
        system: value.system,
        messages: value.messages.into_vec(),
        tools: value.tools.into_vec(),
        max_tokens: value.max_tokens,
        variant: value.variant,
        cache_key: value.cache_key,
    }
}
fn traits(name: &str) -> Traits {
    let mut result = Traits::default();
    match name {
        "plain" => {}
        "reasoning" => result.replays_reasoning = true,
        "body-key" => {
            result.cache_key_field = true;
            result.replays_reasoning = true;
        }
        "header-key" => {
            result.cache_header = "x-conv".into();
            result.replays_reasoning = true;
        }
        "mini-variant" | "other-model" => {
            result.variants = Some(Arc::new(|id| {
                if id.contains("mini") {
                    vec!["low".into(), "high".into()]
                } else {
                    vec![]
                }
            }))
        }
        other => panic!("unknown fixture traits {other}"),
    }
    result
}
fn response_value(response: Response) -> Value {
    json!({"Blocks":if response.blocks.is_empty(){Value::Null}else{serde_json::to_value(response.blocks).unwrap()},"StopReason":response.stop_reason,"Usage":response.usage})
}
fn delta_value(delta: &Delta) -> Value {
    json!({"Text":delta.text,"Thinking":delta.thinking,"ToolName":delta.tool_name,"Reset":delta.reset})
}
#[test]
fn all_original_provider_request_shapes_keep_reasoning_cache_routes_tools_and_variant_filtering() {
    let cases: Value =
        serde_json::from_str(include_str!("data/go-chatcompletions-bodies.json")).unwrap();
    assert_eq!(cases.as_array().unwrap().len(), 114);
    for case in cases.as_array().unwrap() {
        let provider = chatcompletions::Provider::new(
            "fixture",
            "http://unused.invalid",
            None,
            traits(case["traits"].as_str().unwrap()),
        )
        .unwrap();
        let body = provider
            .build_body(
                case["id"].as_str().unwrap(),
                &request(&case["request_input"]),
            )
            .unwrap();
        assert_eq!(
            serde_json::from_slice::<Value>(&body).unwrap(),
            case["body"],
            "{} {}",
            case["name"],
            case["traits"]
        );
    }
    let cases: Value = serde_json::from_str(include_str!("data/go-codex-bodies.json")).unwrap();
    assert_eq!(cases.as_array().unwrap().len(), 19);
    for case in cases.as_array().unwrap() {
        let body = codex::build_body(
            case["id"].as_str().unwrap(),
            &request(&case["request_input"]),
        )
        .unwrap();
        assert_eq!(
            serde_json::from_slice::<Value>(&body).unwrap(),
            case["body"],
            "{}",
            case["name"]
        );
    }
}
#[test]
fn original_incremental_provider_decoders_preserve_every_partial_response_delta_and_stop_reason() {
    for (name, source) in [
        ("chat", include_str!("data/go-chatcompletions-streams.json")),
        ("codex", include_str!("data/go-codex-streams.json")),
    ] {
        let cases: Value = serde_json::from_str(source).unwrap();
        for case in cases.as_array().unwrap() {
            let deltas = Arc::new(Mutex::new(Vec::new()));
            let recorded = deltas.clone();
            let sink: DeltaSink = Arc::new(move |delta| recorded.lock().unwrap().push(delta));
            let mut codec: Box<dyn Codec> = if name == "chat" {
                Box::new(chatcompletions::Accumulator::new(sink))
            } else {
                Box::new(codex::Accumulator::new(sink))
            };
            for (index, step) in case["steps"].as_array().unwrap().iter().enumerate() {
                let result = codec.feed(step["payload"].to_string().as_bytes());
                let actual = response_value(codec.response());
                assert_eq!(
                    actual, step["response"],
                    "{name} {} step {index}",
                    case["name"]
                );
                assert_eq!(
                    json!(
                        deltas
                            .lock()
                            .unwrap()
                            .iter()
                            .map(delta_value)
                            .collect::<Vec<_>>()
                    ),
                    step["deltas"],
                    "{name} {} step {index}",
                    case["name"]
                );
                assert_eq!(codec.terminal(), step["terminal"].as_bool().unwrap());
                assert_eq!(
                    matches!(result, Ok(Feed::Done)),
                    step["done"].as_bool().unwrap()
                );
                assert_eq!(
                    result.is_err(),
                    step["error"] != "",
                    "{name} {} step {index}",
                    case["name"]
                );
                if let Err(error) = result
                    && step["error"].as_str().unwrap().starts_with("stream error:")
                {
                    assert_eq!(error, step["error"].as_str().unwrap());
                }
            }
        }
    }
}
fn canned(name: &str) -> String {
    let source = if name == "chat" {
        include_str!("data/go-chatcompletions-streams.json")
    } else {
        include_str!("data/go-codex-streams.json")
    };
    let data: Value = serde_json::from_str(source).unwrap();
    let mut sse = String::new();
    for step in data[0]["steps"].as_array().unwrap() {
        sse.push_str("data: ");
        sse.push_str(&step["payload"].to_string());
        sse.push_str("\n\n");
    }
    sse.push_str("data: [DONE]\n\n");
    sse
}
async fn endpoint(
    status: u16,
    body: String,
    usage: bool,
) -> (
    String,
    tokio::task::JoinHandle<(BTreeMap<String, String>, Value)>,
) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/v1", listener.local_addr().unwrap());
    let task = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut reader = BufReader::new(&mut socket);
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
        let mut body_bytes = vec![0; length];
        reader.read_exact(&mut body_bytes).await.unwrap();
        drop(reader);
        let request: Value = serde_json::from_slice(&body_bytes).unwrap();
        let mut response = format!(
            "HTTP/1.1 {status} Fixture\r\nContent-Length: {}\r\nConnection: close\r\n",
            body.len()
        );
        if usage {
            response.push_str(
                "x-codex-primary-used-percent: 38.5\r\nx-codex-primary-window-minutes: 300\r\n",
            );
        }
        response.push_str("\r\n");
        socket.write_all(response.as_bytes()).await.unwrap();
        socket.write_all(body.as_bytes()).await.unwrap();
        (headers, request)
    });
    (url, task)
}
fn source() -> model::TokenSource {
    Arc::new(|_| {
        Box::pin(async {
            Ok(model::Token {
                access: "fixture-token".into(),
                account_id: "fixture-account".into(),
            })
        })
    })
}

#[tokio::test]
async fn adapters_use_the_real_transport_with_subscription_auth_cache_routing_and_passive_usage() {
    let (url, server) = endpoint(200, canned("chat"), false).await;
    let provider = chatcompletions::Provider::new(
        "fixture",
        url,
        Some(source()),
        Traits {
            cache_header: "x-conv".into(),
            ..Default::default()
        },
    )
    .unwrap();
    let response = provider
        .open("m")
        .unwrap()
        .complete(
            Request {
                cache_key: "agent-fixture".into(),
                ..Default::default()
            },
            None,
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert_eq!(response.blocks.len(), 3);
    assert_eq!(response.blocks[1].text.as_ref(), "Hello world");
    assert_eq!(response.usage.input_tokens, 20);
    assert_eq!(response.usage.cache_read_tokens, 100);
    let (headers, body) = server.await.unwrap();
    assert_eq!(headers["authorization"], "Bearer fixture-token");
    assert_eq!(headers["x-conv"], "agent-fixture");
    assert_eq!(body["stream"], true);

    let (url, server) = endpoint(200, canned("codex"), true).await;
    let reported = Arc::new(Mutex::new(vec![]));
    let recorded = reported.clone();
    let provider = codex::Provider::new(
        Some(source()),
        url,
        Some(Arc::new(move |usage| recorded.lock().unwrap().push(usage))),
    )
    .unwrap();
    assert!(provider.capabilities("m").ignores_max_tokens);
    assert_eq!(provider.variants("m"), ["low", "medium", "high", "xhigh"]);
    let response = provider
        .open("m")
        .unwrap()
        .complete(
            Request {
                cache_key: "agent-fixture".into(),
                max_tokens: 7,
                ..Default::default()
            },
            None,
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert_eq!(response.blocks.len(), 3);
    assert_eq!(response.blocks[0].opaque.as_ref(), "enc1");
    assert_eq!(response.blocks[1].text.as_ref(), "Hello world");
    assert_eq!(response.usage.input_tokens, 100);
    assert_eq!(reported.lock().unwrap().len(), 1);
    let (headers, body) = server.await.unwrap();
    assert_eq!(headers["authorization"], "Bearer fixture-token");
    assert_eq!(headers["chatgpt-account-id"], "fixture-account");
    assert_eq!(headers["session-id"], "agent-fixture");
    assert_eq!(headers["originator"], "stavlos");
    assert!(headers["user-agent"].starts_with("stavlos/0.1 (linux "));
    assert!(body.get("max_output_tokens").is_none());
    assert_eq!(body["prompt_cache_key"], "agent-fixture");
}
#[tokio::test]
async fn rejected_codex_tokens_explain_reauthentication_and_still_report_response_usage() {
    let (url, server) = endpoint(401, "fixture refusal".into(), true).await;
    let reported = Arc::new(Mutex::new(vec![]));
    let recorded = reported.clone();
    let provider = codex::Provider::new(
        Some(source()),
        url,
        Some(Arc::new(move |usage| recorded.lock().unwrap().push(usage))),
    )
    .unwrap();
    let failure = provider
        .open("m")
        .unwrap()
        .complete(Request::default(), None, CancellationToken::new())
        .await
        .unwrap_err();
    assert!(matches!(failure.error, CallError::Permanent(_)));
    assert!(failure.error.to_string().contains("run /providers"));
    assert_eq!(reported.lock().unwrap().len(), 1);
    server.await.unwrap();
}
#[test]
fn allowance_headers_refuse_nonfinite_values_and_keep_zero_used_windows_with_real_data() {
    let now = stavlos::wire::Time(
        chrono::DateTime::from_timestamp(1_700_000_000, 0)
            .unwrap()
            .fixed_offset(),
    );
    let mut headers = reqwest::header::HeaderMap::new();
    headers.insert("x-codex-primary-used-percent", "0".parse().unwrap());
    assert!(codex::usage_from_headers(&headers, now).is_none());
    headers.insert("x-codex-primary-window-minutes", "300".parse().unwrap());
    let usage = codex::usage_from_headers(&headers, now).unwrap();
    assert_eq!(usage.windows.len(), 1);
    assert_eq!(usage.windows[0].used_percent, 0.);
    assert_eq!(usage.windows[0].minutes, 300);
    headers.insert("x-codex-primary-used-percent", "NaN".parse().unwrap());
    assert!(codex::usage_from_headers(&headers, now).is_none());
    headers.insert("x-codex-secondary-used-percent", "120.5".parse().unwrap());
    headers.insert("x-codex-secondary-reset-at", "1800500000".parse().unwrap());
    let usage = codex::usage_from_headers(&headers, now).unwrap();
    assert_eq!(usage.windows[0].used_percent, 100.);
    assert_eq!(usage.windows[0].resets_at.0.timestamp(), 1_800_500_000);
    assert_eq!(usage.observed, now);
    let error = CallError::Limit {
        message: "fixture".into(),
        retry_after: Duration::from_secs(7),
    }
    .context("provider");
    assert!(matches!(error,CallError::Limit{retry_after,..}if retry_after==Duration::from_secs(7)));
}
