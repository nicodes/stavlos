use serde_json::{Value, json};
use stavlos::{
    discord::{
        interaction, permission,
        questions::{self, Draft, Submission},
        status,
    },
    event, protocol,
};
fn cases(source: &str) -> Vec<Value> {
    serde_json::from_str(source).unwrap()
}
fn controls(value: &Value) -> Vec<Value> {
    serde_json::from_value(value.clone()).unwrap_or_default()
}
fn draft(prompt: &protocol::PromptInfo, value: &Value) -> Draft {
    let mut draft = Draft::new(prompt);
    draft.index = value["index"].as_u64().unwrap() as usize;
    draft.page = value["page"].as_u64().unwrap() as usize;
    draft.text = serde_json::from_value(value["text"].clone()).unwrap();
    draft.selected = serde_json::from_value(value["selected"].clone()).unwrap();
    draft
}
fn snapshot(draft: &Draft) -> Value {
    json!({"index":draft.index,"page":draft.page,"text":draft.text,"selected":draft.selected})
}
#[test]
fn question_cards_preserve_choices_custom_text_claims_and_utf16_budget_like_go() {
    for (index, case) in cases(include_str!("data/go-discord-question-views.json"))
        .into_iter()
        .enumerate()
    {
        let prompt: protocol::PromptInfo = serde_json::from_value(case["prompt"].clone()).unwrap();
        let draft = draft(&prompt, &case["draft"]);
        let (body, components) = questions::prompt_view(&prompt, &draft, "self");
        assert!(body == case["text"].as_str().unwrap(), "view {index}");
        assert_eq!(
            components,
            controls(&case["controls"]),
            "components {index}"
        );
        assert!(
            questions::answered(&prompt, Some(&draft)) == case["answered"].as_str().unwrap(),
            "answered {index}"
        );
        assert!(
            questions::answered(&prompt, None) == case["unanswered"].as_str().unwrap(),
            "unanswered {index}"
        );
    }
}
#[test]
fn question_navigation_and_legacy_selections_match_go_journeys_without_submitting() {
    for (journey, case) in cases(include_str!("data/go-discord-question-journeys.json"))
        .into_iter()
        .enumerate()
    {
        let prompt: protocol::PromptInfo = serde_json::from_value(case["prompt"].clone()).unwrap();
        let mut draft = Draft::new(&prompt);
        for (index, step) in case["steps"].as_array().unwrap().iter().enumerate() {
            let values: Vec<String> =
                serde_json::from_value(step["values"].clone()).unwrap_or_default();
            let result = draft.update(
                &prompt,
                step["action"].as_str().unwrap(),
                step["arg"].as_str().unwrap(),
                step["value"].as_str().unwrap(),
                Some(&values),
            );
            assert_eq!(
                result.err().map(|e| e.to_string()).unwrap_or_default(),
                step["error"].as_str().unwrap(),
                "journey {journey} step {index}"
            );
            assert_eq!(
                snapshot(&draft),
                step["draft"],
                "journey {journey} step {index}"
            );
            let (body, components) = questions::view(&prompt, &draft);
            assert_eq!(body, step["text"]);
            assert_eq!(components, controls(&step["controls"]));
        }
    }
}
#[test]
fn recorded_question_and_permission_outcomes_keep_the_original_decision_visible() {
    for case in cases(include_str!("data/go-discord-question-results.json")) {
        let prompt: protocol::PromptInfo = serde_json::from_value(case["prompt"].clone()).unwrap();
        let outcome: event::AskResolvedPayload =
            serde_json::from_value(case["result"].clone()).unwrap();
        assert_eq!(
            questions::recorded_result(&prompt, &outcome),
            case["expected"]
        );
    }
    for (index, case) in cases(include_str!("data/go-discord-prompt-cards.json"))
        .into_iter()
        .enumerate()
    {
        let prompt: protocol::PromptInfo = serde_json::from_value(case["prompt"].clone()).unwrap();
        let (body, components) = permission::view(&prompt, "self");
        assert_eq!(body, case["text"], "card {index}");
        assert_eq!(
            components,
            controls(&case["controls"]),
            "components {index}"
        );
        for outcome in case["outcomes"].as_array().unwrap() {
            let result: event::AskResolvedPayload =
                serde_json::from_value(outcome["result"].clone()).unwrap();
            assert_eq!(
                permission::recorded_result(&prompt, &result),
                outcome["expected"],
                "outcome card {index}"
            );
        }
    }
}
#[test]
fn status_filtering_details_and_pagination_match_go_without_changing_channel_scope() {
    for (index, case) in cases(include_str!("data/go-discord-status-views.json"))
        .into_iter()
        .enumerate()
    {
        let agents: Vec<protocol::AgentInfo> =
            serde_json::from_value::<Option<Vec<_>>>(case["agents"].clone())
                .unwrap()
                .unwrap_or_default();
        let prompts: Vec<protocol::PromptInfo> =
            serde_json::from_value(case["prompts"].clone()).unwrap();
        let (body, components) = status::view(
            "Repo *Name*",
            &agents,
            &prompts,
            "c1",
            case["all"].as_bool().unwrap(),
            case["page"].as_i64().unwrap(),
        );
        assert_eq!(body, case["text"], "status {index}");
        assert_eq!(
            components,
            controls(&case["controls"]),
            "components {index}"
        );
    }
}
#[test]
fn interaction_modals_identity_and_public_card_acknowledgements_match_go() {
    for (index, case) in cases(include_str!("data/go-discord-interaction-controls.json"))
        .into_iter()
        .enumerate()
    {
        let input = &case["interaction"];
        assert_eq!(
            interaction::modal(input),
            if case["modal"].is_null() {
                None
            } else {
                Some(case["modal"].clone())
            },
            "modal {index}"
        );
        assert_eq!(
            interaction::question(input).unwrap_or(""),
            case["prompt"],
            "prompt {index}"
        );
        assert_eq!(
            interaction::question(input).is_some(),
            case["question"].as_bool().unwrap()
        );
        assert_eq!(interaction::user(input), case["user"]);
        if interaction::question(input).is_some() {
            assert_eq!(interaction::acknowledge(input), json!({"type":6}));
        }
    }
    assert_eq!(
        interaction::user(&json!({"member":{"user":{}},"user":{"id":"fallback"}})),
        ""
    );
}
#[test]
fn a_batch_moves_to_its_missing_question_and_submits_structured_choices_once_complete() {
    let prompt = protocol::PromptInfo {
        id: "q1".into(),
        questions: vec![
            protocol::Question {
                question: "One?".into(),
                options: vec![protocol::QuestionOption {
                    label: "A, B".into(),
                    ..Default::default()
                }]
                .into(),
            },
            protocol::Question {
                question: "Two?".into(),
                ..Default::default()
            },
        ]
        .into(),
        ..Default::default()
    };
    let mut draft = Draft::new(&prompt);
    assert!(draft.submit(&prompt).is_err());
    draft.update(&prompt, "toggle", "0.0", "", None).unwrap();
    assert!(matches!(
        draft.submit(&prompt).unwrap(),
        Submission::Continue
    ));
    assert_eq!(draft.index, 1);
    draft
        .update(&prompt, "text-submit", "1", " A, B ", None)
        .unwrap();
    let Submission::Ready(result) = draft.submit(&prompt).unwrap() else {
        panic!("completed draft did not submit")
    };
    assert_eq!(result.answers.to_vec(), ["A, B", "A, B"]);
    assert_eq!(result.details[0].selected.to_vec(), [0]);
    assert!(result.details[0].custom.is_empty());
    assert!(result.details[1].selected.is_empty());
    assert_eq!(result.details[1].custom, "A, B");
}
