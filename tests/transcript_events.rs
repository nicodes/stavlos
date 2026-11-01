use serde_json::{Value, json};
use stavlos::{event, model, tui::transcript as t};
#[test]
fn every_chat_visible_event_renderer_matches_current_go() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-transcript-event-lines.json")).unwrap();
    assert!(cases.len() > 60);
    for case in cases {
        let event = event::Event::new(
            "channel",
            "root",
            case["kind"].as_str().unwrap(),
            &case["payload"],
        )
        .unwrap();
        let expected = if case["expected"].is_null() {
            json!([])
        } else {
            case["expected"].clone()
        };
        assert_eq!(
            json!(t::event_lines(&event)),
            expected,
            "{} {}",
            case["kind"],
            case["payload"]
        );
    }
}
#[test]
fn call_headers_outputs_and_denial_annotations_match_go() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-transcript-started-lines.json")).unwrap();
    assert_eq!(cases.len(), 30);
    for case in cases {
        let name = case["name"].as_str().unwrap();
        let raw = case["raw"].as_str().unwrap();
        assert_eq!(
            json!(t::tool_started_lines(name, "call-id", raw)),
            case["expected"],
            "{name} {raw}"
        );
    }
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-transcript-denial-marks.json")).unwrap();
    for case in cases {
        assert_eq!(
            t::denial_mark(case["output"].as_str().unwrap()),
            case["expected"]
        );
    }
}
#[test]
fn malformed_event_fields_are_visible_errors_and_fence_only_assistant_text_does_not_panic() {
    let mut malformed =
        event::Event::new("channel", "root", event::TOOL_STARTED, json!({"name":true})).unwrap();
    let lines = t::event_lines(&malformed);
    assert_eq!(lines.len(), 1);
    assert_eq!(lines[0].kind, t::Kind::Error);
    assert!(lines[0].text.starts_with("(bad tool.started payload:"));
    malformed.kind = "future.event".into();
    assert!(t::event_lines(&malformed).is_empty());
    let fences = event::Event::new(
        "channel",
        "root",
        event::ASSISTANT_MESSAGE,
        event::AssistantMessagePayload {
            blocks: vec![model::Block::text("```")].into(),
            ..Default::default()
        },
    )
    .unwrap();
    assert_eq!(t::event_lines(&fences).len(), 1);
}
