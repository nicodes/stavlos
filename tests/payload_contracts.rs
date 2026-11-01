use serde_json::Value;
use stavlos::{event, model, wire};
fn roundtrip<T: serde::Serialize + serde::de::DeserializeOwned + Default>(
    input: &str,
) -> serde_json::Result<Value> {
    serde_json::to_value(wire::decode::<T>(input)?)
}
fn equivalent(actual: &Value, expected: &Value, context: &str) {
    match (actual, expected) {
        (Value::Number(a), Value::Number(b)) => assert_eq!(a.as_f64(), b.as_f64(), "{context}"),
        (Value::Array(a), Value::Array(b)) => {
            assert_eq!(a.len(), b.len(), "{context}");
            for (index, (a, b)) in a.iter().zip(b).enumerate() {
                equivalent(a, b, &format!("{context}[{index}]"));
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
fn all_original_go_event_and_model_payloads_roundtrip_with_identical_values() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-event-model-contracts.json")).unwrap();
    assert_eq!(cases.len(), 190);
    for case in cases {
        let name = case["name"].as_str().unwrap();
        let input = case["input"].as_str().unwrap();
        let actual = match name {
            "event.AgentSpawnedPayload" => roundtrip::<event::AgentSpawnedPayload>(input),
            "event.AgentUpdatedPayload" => roundtrip::<event::AgentUpdatedPayload>(input),
            "event.AskRequestedPayload" => roundtrip::<event::AskRequestedPayload>(input),
            "event.AskResolvedPayload" => roundtrip::<event::AskResolvedPayload>(input),
            "event.AssistantMessagePayload" => roundtrip::<event::AssistantMessagePayload>(input),
            "event.BoardInfo" => roundtrip::<event::BoardInfo>(input),
            "event.ChannelCreatedPayload" => roundtrip::<event::ChannelCreatedPayload>(input),
            "event.ChannelUpdatedPayload" => roundtrip::<event::ChannelUpdatedPayload>(input),
            "event.ChatPayload" => roundtrip::<event::ChatPayload>(input),
            "event.CompactionPayload" => roundtrip::<event::CompactionPayload>(input),
            "event.DirPayload" => roundtrip::<event::DirPayload>(input),
            "event.Input" => roundtrip::<event::Input>(input),
            "event.InputTakenPayload" => roundtrip::<event::InputTakenPayload>(input),
            "event.JobFinishedPayload" => roundtrip::<event::JobFinishedPayload>(input),
            "event.JobStartedPayload" => roundtrip::<event::JobStartedPayload>(input),
            "event.JobStoppedPayload" => roundtrip::<event::JobStoppedPayload>(input),
            "event.MCPFailedPayload" => roundtrip::<event::McpFailedPayload>(input),
            "event.MCPRefPayload" => roundtrip::<event::McpRefPayload>(input),
            "event.MCPStartedPayload" => roundtrip::<event::McpStartedPayload>(input),
            "event.ModelCallPayload" => roundtrip::<event::ModelCallPayload>(input),
            "event.PermitPayload" => roundtrip::<event::PermitPayload>(input),
            "event.Question" => roundtrip::<event::Question>(input),
            "event.QuestionAnswer" => roundtrip::<event::QuestionAnswer>(input),
            "event.QuestionOption" => roundtrip::<event::QuestionOption>(input),
            "event.ReplyRequest" => roundtrip::<event::ReplyRequest>(input),
            "event.SheetPayload" => roundtrip::<event::SheetPayload>(input),
            "event.TodoItem" => roundtrip::<event::TodoItem>(input),
            "event.TodoPayload" => roundtrip::<event::TodoPayload>(input),
            "event.ToolFinishedPayload" => roundtrip::<event::ToolFinishedPayload>(input),
            "event.ToolStartedPayload" => roundtrip::<event::ToolStartedPayload>(input),
            "event.TurnEndedPayload" => roundtrip::<event::TurnEndedPayload>(input),
            "event.TurnPayload" => roundtrip::<event::TurnPayload>(input),
            "model.Block" => roundtrip::<model::Block>(input),
            "model.Message" => roundtrip::<model::Message>(input),
            "model.PlanUsage" => roundtrip::<model::PlanUsage>(input),
            "model.ToolDef" => roundtrip::<model::ToolDef>(input),
            "model.Usage" => roundtrip::<model::Usage>(input),
            "model.UsageWindow" => roundtrip::<model::UsageWindow>(input),
            _ => panic!("unknown original contract {name}"),
        };
        assert_eq!(
            actual.is_ok(),
            case["accepted"].as_bool().unwrap(),
            "{name} {input}: {:?}",
            actual.as_ref().err()
        );
        if let Ok(actual) = actual {
            equivalent(&actual, &case["output"], name);
        }
    }
}
#[test]
fn nonfinite_accounting_is_refused_before_a_journal_record_can_be_built() {
    let payload = event::AssistantMessagePayload {
        cost_usd: f64::NAN,
        ..Default::default()
    };
    assert!(event::Event::new("c", "a", event::ASSISTANT_MESSAGE, payload).is_err());
    let payload = event::ModelCallPayload {
        cost_usd: f64::INFINITY,
        ..Default::default()
    };
    assert!(event::Event::new("c", "a", event::MODEL_CALL_COMPLETED, payload).is_err());
    assert!(
        serde_json::to_string(&model::UsageWindow {
            used_percent: f64::NEG_INFINITY,
            ..Default::default()
        })
        .is_err()
    );
}
