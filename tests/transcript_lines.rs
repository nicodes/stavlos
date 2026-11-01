use serde_json::{Value, json};
use stavlos::{
    protocol,
    tui::transcript::{self as t, Block, Kind, Line, glyph},
};
fn cases(raw: &str) -> Vec<Value> {
    serde_json::from_str(raw).unwrap()
}
fn lines(actual: Vec<Line>, expected: &Value) {
    assert_eq!(
        json!(actual),
        if expected.is_null() {
            json!([])
        } else {
            expected.clone()
        }
    );
}
#[test]
fn basic_lines_and_output_folding_match_go() {
    for case in cases(include_str!("data/go-transcript-text-lines.json")) {
        let body = case["input"].as_str().unwrap();
        lines(t::markdown_lines(body), &case["markdown"]);
        lines(t::output_lines(body), &case["output"]);
        lines(
            t::block(Block::User, "label", body, glyph::ASK),
            &case["user"],
        );
        lines(
            t::received(
                "scout",
                &["main".into(), "user".into()],
                "user",
                body,
                glyph::REPLY,
            ),
            &case["received"],
        );
        lines(t::error_block(body, glyph::ERROR), &case["error"]);
        assert_eq!(json!(t::thinking_line(body)), case["thinking"]);
        let mut line = Line::new(Kind::Text, body);
        line.suffix = body.into();
        lines(t::clean(vec![line]), &case["clean"]);
    }
}
#[test]
fn patch_headers_moves_hunks_and_both_expansion_limits_match_go() {
    for case in cases(include_str!("data/go-transcript-diff-lines.json")) {
        let patch = case["patch"].as_str().unwrap();
        assert_eq!(t::patch_files(patch), case["files"]);
        lines(t::diff_lines(patch), &case["lines"]);
    }
}
#[test]
fn tool_arguments_titles_addressing_and_glyphs_match_280_go_cases() {
    let cases = cases(include_str!("data/go-transcript-tool-lines.json"));
    assert_eq!(cases.len(), 280);
    for case in cases {
        let name = case["tool"].as_str().unwrap();
        let raw = case["input"].as_str().unwrap();
        assert_eq!(t::tool_arg(name, raw), case["arg"], "arg: {name} {raw}");
        assert_eq!(t::tool_line(name, raw), case["line"], "line: {name} {raw}");
        assert_eq!(t::tool_title(name), case["title"]);
        let (glyph, gap) = t::tool_glyph(name);
        assert_eq!(glyph, case["glyph"]);
        assert_eq!(gap, case["gap"]);
        let (who, body) = t::prompt_of(name, raw);
        assert_eq!(who, case["who"]);
        assert_eq!(body, case["text"]);
        assert_eq!(
            json!(t::message_recipients(raw)),
            if case["recipients"].is_null() {
                json!([])
            } else {
                case["recipients"].clone()
            }
        );
    }
}
#[test]
fn recorded_questions_and_permission_cards_match_go() {
    for case in cases(include_str!("data/go-transcript-question-lines.json")) {
        let prompt: protocol::PromptInfo = serde_json::from_value(case["prompt"].clone()).unwrap();
        let result: Option<stavlos::event::AskResolvedPayload> =
            serde_json::from_value(case["result"].clone()).unwrap();
        lines(
            t::question_lines(&prompt, result.as_ref(), case["chat"] == true),
            &case["lines"],
        );
    }
    for case in cases(include_str!("data/go-transcript-permission-lines.json")) {
        let input =
            serde_json::value::RawValue::from_string(case["input"].as_str().unwrap().into());
        if case["input"] == "{bad" {
            // The native protocol rejects an invalid raw JSON field before it reaches a card.
            assert!(input.is_err());
            continue;
        }
        let prompt = protocol::PromptInfo {
            agent: "a1".into(),
            from: "scout".into(),
            tool: "shell".into(),
            dir: "/outside".into(),
            input: Some(input.unwrap()),
            ..Default::default()
        };
        let result: Option<stavlos::event::AskResolvedPayload> =
            serde_json::from_value(case["result"].clone()).unwrap();
        lines(
            t::permission_lines(&prompt, result.as_ref(), case["chat"] == true),
            &case["lines"],
        );
    }
    let mut line = Line::new(Kind::Tool, "");
    line.tool = "message".into();
    assert!(t::is_message(&line));
    assert!(t::is_prompt_call(&line));
    assert_eq!(t::call_glyph(&line), (glyph::REPLY, " "));
}
