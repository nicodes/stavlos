use super::{permission, text};
use serde_json::{Value, json};
pub fn user(interaction: &Value) -> &str {
    if interaction["member"]["user"].is_object() {
        return interaction["member"]["user"]["id"].as_str().unwrap_or("");
    }
    interaction["user"]["id"].as_str().unwrap_or("")
}
pub fn custom_id(interaction: &Value) -> Option<&str> {
    if matches!(interaction["type"].as_u64(), Some(3 | 5)) {
        interaction["data"]["custom_id"].as_str()
    } else {
        None
    }
}
pub fn question(interaction: &Value) -> Option<&str> {
    let (id, action, _) = permission::parse_id(custom_id(interaction)?)?;
    if id.is_empty() {
        return None;
    }
    matches!(
        action,
        "questions"
            | "next"
            | "back"
            | "page-next"
            | "page-back"
            | "toggle"
            | "pick"
            | "text"
            | "text-submit"
            | "clear-text"
            | "submit"
            | "allow"
            | "always"
            | "prefix"
            | "deny-now"
            | "deny-submit"
            | "dir-submit"
            | "skip"
    )
    .then_some(id)
}
pub fn modal(interaction: &Value) -> Option<Value> {
    if interaction["type"] != 3 {
        return None;
    }
    let (id, action, arg) = permission::parse_id(custom_id(interaction)?)?;
    if id.is_empty() {
        return None;
    }
    let label = match action {
        "text" => "Custom answer",
        "deny" => "Reason",
        "dir" => "Directory to add",
        _ => return None,
    };
    let mut input = json!({"type":4,"custom_id":"value","label":label,"style":2,"required":true,"max_length":4000});
    if action == "text" {
        input["placeholder"] =
            json!("Save your custom answer here, then use Submit answer on the question card.");
    }
    Some(
        json!({"type":9,"data":{"tts":false,"content":"","embeds":null,"title":label,"custom_id":permission::component_id(id,&format!("{action}-submit"),arg),"components":[permission::row(vec![input])]}}),
    )
}
pub fn modal_value(interaction: &Value) -> String {
    let Some(rows) = interaction["data"]["components"].as_array() else {
        return String::new();
    };
    for row in rows {
        if row["type"] == 1
            && let Some(components) = row["components"].as_array()
        {
            for component in components {
                if component["type"] == 4 && component["custom_id"] == "value" {
                    return component["value"].as_str().unwrap_or("").into();
                }
            }
        }
    }
    String::new()
}
pub fn ephemeral(body: &str) -> Value {
    json!({"type":4,"data":{"content":body,"flags":64,"allowed_mentions":{"parse":[]}}})
}
pub fn acknowledge(interaction: &Value) -> Value {
    let status = interaction["type"] == 3
        && interaction["data"]["custom_id"]
            .as_str()
            .and_then(super::status::page)
            .is_some();
    if question(interaction).is_some() || status {
        json!({"type":6})
    } else {
        json!({"type":5,"data":{"flags":64}})
    }
}
pub fn card_components(body: &str, controls: &[Value]) -> Vec<Value> {
    let mut components = Vec::new();
    if !body.is_empty() {
        components.push(json!({"type":10,"content":body}));
    }
    components.extend_from_slice(controls);
    components
}
pub fn safe_reply(body: &str) -> String {
    text::clip(body, 2000)
}
