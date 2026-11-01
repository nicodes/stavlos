use serde_json::{Value, json};
use stavlos::tui::transcript::{self as t, Transcript};
fn snapshot(transcript: &Transcript) -> Value {
    json!({"lines":transcript.all(),"tail":transcript.tail(),"items":transcript.item_count(),"committed":transcript.committed(),"waiting":transcript.waiting(),"in_turn":transcript.in_turn(),"turn_verb":transcript.turn_verb(),"running":transcript.running(),"compacting":transcript.compacting(),"question":transcript.question_item("q1").map(|n|n as i64).unwrap_or(-1),"permission":transcript.permission_item("p1").map(|n|n as i64).unwrap_or(-1)})
}
fn normalized(mut expected: Value) -> Value {
    if expected["tail"].is_null() {
        expected["tail"] = json!([]);
    }
    expected
}
#[test]
fn transcript_state_matches_every_frame_in_eight_current_go_journeys() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-transcript-state-journeys.json")).unwrap();
    assert_eq!(cases.len(), 8);
    for case in cases {
        let name = case["name"].as_str().unwrap();
        let mut transcript = if case["chat"] == true {
            Transcript::chat()
        } else {
            Transcript::new()
        };
        assert_eq!(
            snapshot(&transcript),
            normalized(case["initial"].clone()),
            "{name} initial"
        );
        for (index, step) in case["steps"].as_array().unwrap().iter().enumerate() {
            match step["op"].as_str().unwrap() {
                "event" => {
                    transcript.apply(&serde_json::from_value(step["event"].clone()).unwrap())
                }
                "stream" => transcript
                    .apply_stream(&serde_json::from_value(step["payload"].clone()).unwrap()),
                "permission" => transcript
                    .ensure_permission(serde_json::from_value(step["payload"].clone()).unwrap()),
                "question" => transcript
                    .ensure_question(serde_json::from_value(step["payload"].clone()).unwrap()),
                "child_spawned" => transcript.child_spawned(step["id"].as_str().unwrap()),
                "child_state" => transcript.child_state(
                    step["id"].as_str().unwrap(),
                    step["state"].as_str().unwrap(),
                ),
                "asker_gone" => transcript.asker_gone(step["name"].as_str().unwrap()),
                op => panic!("unknown fixture operation {op}"),
            }
            assert_eq!(
                snapshot(&transcript),
                normalized(step["expected"].clone()),
                "{name} step {index}: {step}"
            );
        }
    }
}
#[test]
fn input_items_and_item_lookup_keep_human_input_unfolded_and_model_notices_separate() {
    let mut transcript = Transcript::new();
    let queued = stavlos::event::Event::new(
        "channel",
        "root",
        stavlos::event::INPUT_QUEUED,
        stavlos::event::Input {
            id: "input".into(),
            kind: stavlos::event::INPUT_PROMPT.into(),
            text: "one\ntwo\nthree\nfour".into(),
            ..Default::default()
        },
    )
    .unwrap();
    transcript.apply(&queued);
    assert!(transcript.empty());
    transcript.apply(
        &stavlos::event::Event::new(
            "channel",
            "root",
            stavlos::event::INPUT_TAKEN,
            stavlos::event::InputTakenPayload {
                ids: vec!["input".into()].into(),
                ..Default::default()
            },
        )
        .unwrap(),
    );
    let lines = transcript.all();
    assert!(t::item_is_input(&lines, 0));
    assert!(!t::item_folds(&lines, 0));
    assert_eq!(t::item_range(&lines, 0), Some((0, lines.len() - 1)));
    assert_eq!(t::item_range(&lines, 20), None);
    assert!(t::chat_event(stavlos::event::CHAT_MESSAGE));
    assert!(!t::chat_event(stavlos::event::TOOL_STARTED));
}
