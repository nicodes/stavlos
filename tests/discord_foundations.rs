use serde_json::Value;
use stavlos::discord::store::{AtomicMap, BoardCursors, BridgeLock, PromptMessage, PromptStore};
use stavlos::{
    discord::{permission, text},
    protocol,
};
use std::os::unix::fs::PermissionsExt;
fn cases(raw: &str) -> Vec<Value> {
    serde_json::from_str(raw).unwrap()
}
#[test]
fn text_limits_fences_and_mentions_match_go_in_utf16_units() {
    for (index, case) in cases(include_str!("data/go-discord-text.json"))
        .into_iter()
        .enumerate()
    {
        let source = case["text"].as_str().unwrap();
        let limit = case["limit"].as_u64().unwrap() as usize;
        assert_eq!(text::units(source) as u64, case["units"].as_u64().unwrap());
        for (key, actual) in [
            ("clip", text::clip(source, limit)),
            ("markdown", text::clip_markdown(source, limit)),
            ("heading", text::question_heading(source, limit)),
            ("strip", text::strip_bot(source, "123")),
        ] {
            assert!(
                actual == case[key].as_str().unwrap(),
                "case {index}, {key}, limit {limit}"
            );
        }
    }
    for (index, case) in cases(include_str!("data/go-discord-splits.json"))
        .into_iter()
        .enumerate()
    {
        let expected: Vec<String> =
            serde_json::from_value(case["expected"].clone()).unwrap_or_default();
        assert!(
            text::split(case["text"].as_str().unwrap()) == expected,
            "split {index}"
        );
    }
}
#[test]
fn tool_labels_match_all_simple_uppercase_go_boundaries() {
    for case in cases(include_str!("data/go-discord-labels.json")) {
        let source = case["raw"].as_str().unwrap();
        assert_eq!(
            permission::label(source),
            case["expected"].as_str().unwrap(),
            "{source:?}"
        );
    }
}
#[test]
fn permission_arguments_keep_every_additional_field_and_original_json_lexemes_visible() {
    for (index, case) in cases(include_str!("data/go-discord-permission-views.json"))
        .into_iter()
        .enumerate()
    {
        let prompt = protocol::PromptInfo {
            tool: case["tool"].as_str().unwrap().into(),
            input: Some(
                serde_json::value::RawValue::from_string(case["input"].as_str().unwrap().into())
                    .unwrap(),
            ),
            dir: "/work/_thing".into(),
            ..Default::default()
        };
        assert_eq!(
            permission::heading(&prompt),
            case["heading"].as_str().unwrap(),
            "heading {index}"
        );
        assert_eq!(
            permission::subject(&prompt),
            case["subject"].as_str().unwrap(),
            "subject {index}"
        );
        assert_eq!(
            permission::json_text(case["input"].as_str().unwrap().as_bytes()),
            case["json"].as_str().unwrap(),
            "json {index}"
        );
    }
}
#[test]
fn permission_answers_and_component_ids_match_go_validation() {
    for case in cases(include_str!("data/go-discord-permission-answers.json")) {
        let prompt: protocol::PromptInfo = serde_json::from_value(case["prompt"].clone()).unwrap();
        let result = permission::answer(
            &prompt,
            case["action"].as_str().unwrap(),
            case["value"].as_str().unwrap(),
        );
        if case["error"] == "" {
            assert_eq!(
                serde_json::to_value(result.unwrap()).unwrap(),
                case["expected"]
            );
        } else {
            assert_eq!(
                result.unwrap_err().to_string(),
                case["error"].as_str().unwrap()
            );
        }
    }
    for case in cases(include_str!("data/go-discord-component-ids.json")) {
        let (prompt, action, arg) =
            permission::parse_id(case["raw"].as_str().unwrap()).unwrap_or(("", "", ""));
        assert_eq!(prompt, case["prompt"]);
        assert_eq!(action, case["action"]);
        assert_eq!(arg, case["arg"]);
    }
}

#[test]
fn prompt_and_board_state_survive_restarts_without_tokens_and_failed_writes_do_not_advance_memory()
{
    let root = tempfile::tempdir().unwrap();
    let path = root.path().join("state/prompts.json");
    let store = PromptStore::open(path.clone()).unwrap();
    let message = PromptMessage {
        channel: "discord-channel".into(),
        message: "message-id".into(),
        webhook: "webhook-id".into(),
        seq: 9,
        questions: vec![protocol::Question {
            question: "Choose?".into(),
            ..Default::default()
        }]
        .into(),
        ..Default::default()
    };
    store.set("p1", message).unwrap();
    assert_eq!(store.replay_from("discord-channel", 100), 10);
    assert_eq!(store.replay_from("another", 100), 101);
    assert_eq!(
        std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
        0o600
    );
    let reopened = PromptStore::open(path).unwrap();
    assert_eq!(reopened.snapshot()["p1"].webhook, "webhook-id");
    assert!(reopened.snapshot()["p1"].is_question());
    reopened.set("p1", PromptMessage::default()).unwrap();
    assert!(reopened.snapshot().is_empty());
    let cursor_path = root.path().join("cursor");
    let cursors = BoardCursors::open(cursor_path.clone()).unwrap();
    cursors.set("discord-channel", 50).unwrap();
    assert_eq!(
        BoardCursors::open(cursor_path.clone())
            .unwrap()
            .get("discord-channel"),
        50
    );
    std::fs::remove_file(&cursor_path).unwrap();
    std::fs::create_dir(&cursor_path).unwrap();
    assert!(cursors.set("discord-channel", 100).is_err());
    assert_eq!(cursors.get("discord-channel"), 50);
}

#[test]
fn discord_state_refuses_links_and_fifos_and_bridge_lock_is_scoped_to_guild_and_category() {
    let root = tempfile::tempdir().unwrap();
    let original = root.path().join("original");
    std::fs::write(&original, b"{}").unwrap();
    let linked = root.path().join("link");
    std::os::unix::fs::symlink(&original, &linked).unwrap();
    assert!(AtomicMap::<i64>::open(linked).is_err());
    let fifo = root.path().join("fifo");
    let cpath = std::ffi::CString::new(fifo.as_os_str().as_encoded_bytes()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(cpath.as_ptr(), 0o600) }, 0);
    assert!(AtomicMap::<i64>::open(fifo).is_err());
    let first = BridgeLock::acquire(root.path(), "1", "team").unwrap();
    assert!(BridgeLock::acquire(root.path(), "1", "team").is_err());
    let other = BridgeLock::acquire(root.path(), "1", "other-team").unwrap();
    assert_ne!(first.prompts, other.prompts);
    let expected = first.prompts.clone();
    drop(first);
    assert_eq!(
        BridgeLock::acquire(root.path(), "1", "team")
            .unwrap()
            .prompts,
        expected
    );
}
