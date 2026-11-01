use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use stavlos::{event, model, project, toolname, wire};
use std::collections::BTreeSet;
fn fixture() -> Value {
    serde_json::from_str(include_str!("data/go-model-history.json")).unwrap()
}
fn history_value(history: &[model::Message]) -> Value {
    if history.is_empty() {
        Value::Null
    } else {
        serde_json::to_value(history).unwrap()
    }
}
fn equivalent(actual: &Value, expected: &Value, context: &str) {
    match (actual, expected) {
        (Value::Number(a), Value::Number(b)) => assert_eq!(a.as_f64(), b.as_f64(), "{context}"),
        (Value::Array(a), Value::Array(b)) => {
            assert_eq!(a.len(), b.len(), "{context}");
            for (i, (a, b)) in a.iter().zip(b).enumerate() {
                equivalent(a, b, &format!("{context}[{i}]"));
            }
        }
        (Value::Object(a), Value::Object(b)) => {
            assert_eq!(
                a.keys().collect::<Vec<_>>(),
                b.keys().collect::<Vec<_>>(),
                "{context}"
            );
            for (key, value) in a {
                equivalent(value, &b[key], &format!("{context}.{key}"));
            }
        }
        _ => assert_eq!(actual, expected, "{context}"),
    }
}
#[test]
fn original_go_history_recovery_compaction_and_each_incremental_step_match() {
    let fixture = fixture();
    for case in fixture["histories"].as_array().unwrap() {
        let name = case["name"].as_str().unwrap();
        let events: Vec<event::Event> = serde_json::from_value(case["events"].clone()).unwrap();
        let mut builder = project::Builder::new();
        for (event, expected) in events.iter().zip(case["steps"].as_array().unwrap()) {
            builder.apply(event);
            let history = builder.history();
            equivalent(&history_value(&history), &expected["history"], name);
            assert_eq!(
                project::estimate_tokens(&history, "sys!", &[]),
                expected["estimate"].as_i64().unwrap(),
                "{name}"
            );
            assert_eq!(
                project::transcript(&history),
                expected["transcript"].as_str().unwrap(),
                "{name}"
            );
        }
        for expected in case["cuts"].as_array().unwrap() {
            let cut = builder.cut(expected["keep"].as_i64().unwrap());
            assert_eq!(
                cut.is_some(),
                expected["available"].as_bool().unwrap(),
                "{name}"
            );
            if let Some(cut) = cut {
                equivalent(&history_value(&cut.old), &expected["old"], name);
                assert_eq!(cut.prev, expected["prev"].as_str().unwrap(), "{name}");
                assert_eq!(
                    cut.from_seq,
                    expected["from_seq"].as_i64().unwrap(),
                    "{name}"
                );
                assert_eq!(cut.to_seq, expected["to_seq"].as_i64().unwrap(), "{name}");
                assert_eq!(cut.before, expected["before"].as_i64().unwrap(), "{name}");
                assert_eq!(
                    cut.after("Verification summary."),
                    expected["after"].as_i64().unwrap(),
                    "{name}"
                );
            }
        }
    }
}
#[test]
fn original_input_framing_and_quoted_reply_obligations_match() {
    let fixture = fixture();
    for case in fixture["inputs"].as_array().unwrap() {
        let input: event::Input = serde_json::from_value(case["input"].clone()).unwrap();
        let job: event::JobFinishedPayload = serde_json::from_value(case["job"].clone()).unwrap();
        assert_eq!(
            project::input_text(&input, &job),
            case["text"].as_str().unwrap()
        );
        assert_eq!(
            serde_json::to_value(event::input_rule(&input.kind)).unwrap(),
            case["rule"]
        );
    }
    for case in fixture["quotes"].as_array().unwrap() {
        let requests: Vec<event::ReplyRequest> =
            serde_json::from_value(case["requests"].clone()).unwrap();
        assert_eq!(
            project::pending_reply_text(&requests),
            case["text"].as_str().unwrap()
        );
    }
}
#[test]
fn original_context_clearing_retains_recent_turns_and_conversation_tools() {
    let fixture = fixture();
    let keep_tools = BTreeSet::from(["message".into(), "agent_create".into()]);
    for case in fixture["clearing"].as_array().unwrap() {
        let mut messages: Vec<model::Message> =
            serde_json::from_value(fixture["clear_base"].clone()).unwrap();
        let keep = case["keep"].as_i64().unwrap();
        let minimum = case["minimum"].as_i64().unwrap();
        let (tokens, ids) = project::clear_old(&mut messages, keep, minimum, &keep_tools);
        assert_eq!(tokens, case["tokens"].as_i64().unwrap());
        assert_eq!(json!(ids), case["ids"]);
        let (tokens, ids) = project::clear_old(&mut messages, keep, minimum, &keep_tools);
        assert_eq!(tokens, case["again_tokens"].as_i64().unwrap());
        assert_eq!(json!(ids), case["again_ids"]);
        let canonical =
            stavlos::config::jsonc::encode_go(&serde_json::to_value(messages).unwrap()).unwrap();
        assert_eq!(
            format!("{:x}", Sha256::digest(canonical.as_bytes())),
            case["after_sha256"].as_str().unwrap()
        );
    }
}
#[test]
fn original_tool_action_aliases_and_partial_json_decoding_match() {
    let cases: Value = serde_json::from_str(include_str!("data/go-tool-operations.json")).unwrap();
    for case in cases.as_array().unwrap() {
        let name = case["name"].as_str().unwrap();
        let input: &serde_json::value::RawValue =
            serde_json::from_str(case["input"].as_str().unwrap()).unwrap();
        assert_eq!(
            toolname::operation_raw(name, Some(input)),
            case["operation"].as_str().unwrap(),
            "{case}"
        );
    }
}
#[test]
fn explicit_raw_nulls_nil_slices_timestamps_and_history_copy_are_preserved() {
    let missing: model::Block = wire::decode(r#"{"type":"tool_use"}"#).unwrap();
    let explicit: model::Block = wire::decode(r#"{"type":"tool_use","input":null}"#).unwrap();
    assert!(missing.input.is_none());
    assert_eq!(explicit.input.unwrap().get(), "null");
    let nil: model::Message = wire::decode(r#"{"role":"user"}"#).unwrap();
    let empty: model::Message = wire::decode(r#"{"role":"user","blocks":[]}"#).unwrap();
    assert_eq!(
        serde_json::to_string(&nil).unwrap(),
        r#"{"role":"user","blocks":null}"#
    );
    assert_eq!(
        serde_json::to_string(&empty).unwrap(),
        r#"{"role":"user","blocks":[]}"#
    );
    let time: wire::Time =
        serde_json::from_str(r#""2026-01-02T03:04:05.000000009+01:30""#).unwrap();
    assert_eq!(
        serde_json::to_string(&time).unwrap(),
        r#""2026-01-02T03:04:05.000000009+01:30""#
    );
    let mut builder = project::Builder::new();
    builder.apply(
        &event::Event::new(
            "c",
            "a",
            event::ASSISTANT_MESSAGE,
            event::AssistantMessagePayload {
                blocks: vec![model::Block::text("owned original")].into(),
                ..Default::default()
            },
        )
        .unwrap(),
    );
    let mut copy = builder.history();
    copy[1].blocks[0].text = "local copy".into();
    assert_eq!(
        builder.history()[1].blocks[0].text.as_ref(),
        "owned original"
    );
    assert_eq!(event::ALL_TYPES.len(), 36);
    for kind in event::ALL_TYPES {
        assert_ne!(event::Kind::parse(kind), event::Kind::Unknown);
    }
    assert_eq!(event::Kind::parse("future.extension"), event::Kind::Unknown);
}
