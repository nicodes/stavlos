use serde_json::{Value, json};
use stavlos::protocol;
fn roundtrip<T: serde::Serialize + serde::de::DeserializeOwned + Default>(input: &Value) -> Value {
    let decoded: T = stavlos::wire::decode(&input.to_string()).unwrap();
    serde_json::to_value(decoded).unwrap()
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
fn every_original_owner_wire_shape_retains_nil_empty_null_and_nested_values() {
    let cases: Value = serde_json::from_str(include_str!("data/go-protocol-codecs.json")).unwrap();
    for case in cases.as_array().unwrap() {
        let input = &case["input"];
        let actual = match case["name"].as_str().unwrap() {
            "AgentInfo" => roundtrip::<protocol::AgentInfo>(input),
            "AgentTreeParams" => roundtrip::<protocol::AgentTreeParams>(input),
            "AgentTreeResult" => roundtrip::<protocol::AgentTreeResult>(input),
            "AgentSendParams" => roundtrip::<protocol::AgentSendParams>(input),
            "AgentSpawnParams" => roundtrip::<protocol::AgentSpawnParams>(input),
            "AgentSpawnResult" => roundtrip::<protocol::AgentSpawnResult>(input),
            "AgentSetModelParams" => roundtrip::<protocol::AgentSetModelParams>(input),
            "AgentSetRoleParams" => roundtrip::<protocol::AgentSetRoleParams>(input),
            "AgentCompactParams" => roundtrip::<protocol::AgentCompactParams>(input),
            "AgentCompactResult" => roundtrip::<protocol::AgentCompactResult>(input),
            "AgentSetVariantParams" => roundtrip::<protocol::AgentSetVariantParams>(input),
            "VariantsParams" => roundtrip::<protocol::VariantsParams>(input),
            "VariantsResult" => roundtrip::<protocol::VariantsResult>(input),
            "RolesParams" => roundtrip::<protocol::RolesParams>(input),
            "RoleInfo" => roundtrip::<protocol::RoleInfo>(input),
            "ModelSpec" => roundtrip::<protocol::ModelSpec>(input),
            "RolesResult" => roundtrip::<protocol::RolesResult>(input),
            "ProviderInfo" => roundtrip::<protocol::ProviderInfo>(input),
            "LoginMethod" => roundtrip::<protocol::LoginMethod>(input),
            "LoginStartParams" => roundtrip::<protocol::LoginStartParams>(input),
            "ProviderListParams" => roundtrip::<protocol::ProviderListParams>(input),
            "ProviderListResult" => roundtrip::<protocol::ProviderListResult>(input),
            "LoginStartResult" => roundtrip::<protocol::LoginStartResult>(input),
            "LoginWaitParams" => roundtrip::<protocol::LoginWaitParams>(input),
            "LoginKeyParams" => roundtrip::<protocol::LoginKeyParams>(input),
            "ProviderRef" => roundtrip::<protocol::ProviderRef>(input),
            "ModelInfo" => roundtrip::<protocol::ModelInfo>(input),
            "ModelListParams" => roundtrip::<protocol::ModelListParams>(input),
            "ModelListResult" => roundtrip::<protocol::ModelListResult>(input),
            "EventNotification" => roundtrip::<protocol::EventNotification>(input),
            "StreamNotification" => roundtrip::<protocol::StreamNotification>(input),
            "PromptNotification" => roundtrip::<protocol::PromptNotification>(input),
            "DirInfo" => roundtrip::<protocol::DirInfo>(input),
            "MCPInfo" => roundtrip::<protocol::MCPInfo>(input),
            "JobInfo" => roundtrip::<protocol::JobInfo>(input),
            "ChannelInfo" => roundtrip::<protocol::ChannelInfo>(input),
            "ChannelBudget" => roundtrip::<protocol::ChannelBudget>(input),
            "ChannelListParams" => roundtrip::<protocol::ChannelListParams>(input),
            "ChannelListResult" => roundtrip::<protocol::ChannelListResult>(input),
            "ChannelCreateParams" => roundtrip::<protocol::ChannelCreateParams>(input),
            "ChannelRef" => roundtrip::<protocol::ChannelRef>(input),
            "ChannelRenameParams" => roundtrip::<protocol::ChannelRenameParams>(input),
            "ChannelSetModelParams" => roundtrip::<protocol::ChannelSetModelParams>(input),
            "ChannelSetModeParams" => roundtrip::<protocol::ChannelSetModeParams>(input),
            "ChannelSetRecapParams" => roundtrip::<protocol::ChannelSetRecapParams>(input),
            "ChannelPostParams" => roundtrip::<protocol::ChannelPostParams>(input),
            "ChannelPostResult" => roundtrip::<protocol::ChannelPostResult>(input),
            "ChannelDirParams" => roundtrip::<protocol::ChannelDirParams>(input),
            "PromptInfo" => roundtrip::<protocol::PromptInfo>(input),
            "PromptListParams" => roundtrip::<protocol::PromptListParams>(input),
            "PromptListResult" => roundtrip::<protocol::PromptListResult>(input),
            "PromptClaimParams" => roundtrip::<protocol::PromptClaimParams>(input),
            "PromptReplyParams" => roundtrip::<protocol::PromptReplyParams>(input),
            "TrustStatusParams" => roundtrip::<protocol::TrustStatusParams>(input),
            "TrustStatusResult" => roundtrip::<protocol::TrustStatusResult>(input),
            "TrustReplyParams" => roundtrip::<protocol::TrustReplyParams>(input),
            "SandboxStatus" => roundtrip::<protocol::SandboxStatus>(input),
            "SandboxSetParams" => roundtrip::<protocol::SandboxSetParams>(input),
            "CommandInfo" => roundtrip::<protocol::CommandInfo>(input),
            "CommandListResult" => roundtrip::<protocol::CommandListResult>(input),
            "CommandRunParams" => roundtrip::<protocol::CommandRunParams>(input),
            "ConfigScope" => roundtrip::<protocol::ConfigScope>(input),
            "ConfigEntry" => roundtrip::<protocol::ConfigEntry>(input),
            "ConfigTree" => roundtrip::<protocol::ConfigTree>(input),
            "ConfigField" => roundtrip::<protocol::ConfigField>(input),
            "ConfigFileParams" => roundtrip::<protocol::ConfigFileParams>(input),
            "ConfigDocument" => roundtrip::<protocol::ConfigDocument>(input),
            "ConfigEditParams" => roundtrip::<protocol::ConfigEditParams>(input),
            "ConfigEditResult" => roundtrip::<protocol::ConfigEditResult>(input),
            "None" => roundtrip::<protocol::None>(input),
            "SubscribeResult" => roundtrip::<protocol::SubscribeResult>(input),
            "UsageSeriesParams" => roundtrip::<protocol::UsageSeriesParams>(input),
            "UsageSeriesResult" => roundtrip::<protocol::UsageSeriesResult>(input),
            "PlanUsageResult" => roundtrip::<protocol::PlanUsageResult>(input),
            "PlanUsageInfo" => roundtrip::<protocol::PlanUsageInfo>(input),
            "UsageWindowInfo" => roundtrip::<protocol::UsageWindowInfo>(input),
            "CacheUsageParams" => roundtrip::<protocol::CacheUsageParams>(input),
            "CacheUsageResult" => roundtrip::<protocol::CacheUsageResult>(input),
            "CacheProviderUsage" => roundtrip::<protocol::CacheProviderUsage>(input),
            "PlanUsagePoint" => roundtrip::<protocol::PlanUsagePoint>(input),
            "PlanSeriesParams" => roundtrip::<protocol::PlanSeriesParams>(input),
            "PlanSeriesResult" => roundtrip::<protocol::PlanSeriesResult>(input),
            "ChangedNotification" => roundtrip::<protocol::ChangedNotification>(input),
            "AttachParams" => roundtrip::<protocol::AttachParams>(input),
            "AttachResult" => roundtrip::<protocol::AttachResult>(input),
            "DaemonStatusResult" => roundtrip::<protocol::DaemonStatusResult>(input),
            "DiscordStatus" => roundtrip::<protocol::DiscordStatus>(input),
            "WebStatus" => roundtrip::<protocol::WebStatus>(input),
            "SheetInfo" => roundtrip::<protocol::SheetInfo>(input),
            "SheetListResult" => roundtrip::<protocol::SheetListResult>(input),
            "SubscribeParams" => roundtrip::<protocol::SubscribeParams>(input),
            "ReconcileResult" => roundtrip::<protocol::ReconcileResult>(input),
            "Request" => roundtrip::<protocol::Request>(input),
            "Response" => roundtrip::<protocol::Response>(input),
            "Error" => roundtrip::<protocol::Error>(input),
            "Recipients" => roundtrip::<protocol::Recipients>(input),
            name => panic!("unhandled original wire type {name}"),
        };
        equivalent(
            &actual,
            &case["expected"],
            &format!("{} {}", case["name"], case["case"]),
        );
    }
}
#[test]
fn protocol_decisions_keep_question_numbering_and_permission_outcomes() {
    let mut prompt = protocol::PromptInfo {
        questions: vec![protocol::Question::default()].into(),
        question_number: 2,
        question_total: 4,
        dir: "/owned/project".into(),
        prefix: "git status".into(),
        ..Default::default()
    };
    assert_eq!(prompt.question_position(0), (2, 4));
    let mut resolved = stavlos::event::AskResolvedPayload {
        answer: protocol::ANSWER_ALLOW_ALWAYS.into(),
        ..Default::default()
    };
    assert_eq!(
        protocol::permission_result(&prompt, &resolved),
        "Allowed and added directory: /owned/project"
    );
    resolved.outcome = stavlos::event::ASK_DEFAULTED.into();
    resolved.reason = "owner note".into();
    resolved.dir = "/owned/other".into();
    assert_eq!(
        protocol::permission_result(&prompt, &resolved),
        "Defaulted · Allowed and added directory: /owned/other · owner note"
    );
    resolved.outcome = stavlos::event::ASK_WITHDRAWN.into();
    assert_eq!(
        protocol::permission_result(&prompt, &resolved),
        "Permission withdrawn"
    );
    prompt.questions.push(protocol::Question::default());
    assert_eq!(prompt.question_position(1), (2, 2));
    let recipients: protocol::Recipients =
        serde_json::from_value(json!([" @Human ", "user", null, "@AGENT"])).unwrap();
    assert_eq!(recipients.normalized(), ["user", "", "agent"]);
    assert_eq!(protocol::roll_up(["idle", "waiting"]), "waiting");
    assert_eq!(protocol::roll_up(["waiting", "blocked"]), "working");
    assert!(
        serde_json::to_string(&protocol::PlanSeriesResult {
            used_percent: vec![f64::NAN].into(),
            ..Default::default()
        })
        .is_err()
    );
}
