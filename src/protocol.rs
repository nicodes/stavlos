use serde::{Deserialize, Serialize};
use serde_json::Value;
pub const VERSION: i64 = 1;
pub const MAX_LINE: usize = 4 << 20;
pub const ERR_PARSE: i64 = -32700;
pub const ERR_INVALID_REQUEST: i64 = -32600;
pub const ERR_METHOD_NOT_FOUND: i64 = -32601;
pub const ERR_INVALID_PARAMS: i64 = -32602;
pub const ERR_INTERNAL: i64 = -32603;
pub const ERR_VERSION: i64 = -32000;
pub const ERR_NOT_FOUND: i64 = -32001;
pub const ERR_CONFLICT: i64 = -32002;
pub const ERR_TRUST: i64 = -32003;
pub const ERR_FORBIDDEN: i64 = -32004;
include!("protocol_methods.rs");
#[path = "protocol_types.rs"]
mod types;
pub use types::*;
#[path = "protocol_typed_methods.rs"]
pub mod methods;
#[derive(Clone, Default, Serialize, Deserialize, Debug, thiserror::Error)]
#[error("{message}")]
pub struct Error {
    #[serde(default, deserialize_with = "crate::wire::null_default")]
    pub code: i64,
    #[serde(default, deserialize_with = "crate::wire::null_default")]
    pub message: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub data: Option<Value>,
}
impl Error {
    pub fn new(code: i64, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            data: None,
        }
    }
}
pub fn simple_lower(s: &str) -> String {
    s.chars()
        .map(|c| c.to_lowercase().next().unwrap_or(c))
        .collect()
}
pub fn recipient(s: &str) -> String {
    let r = s.trim();
    let r = simple_lower(r.strip_prefix('@').unwrap_or(r));
    if r == "human" { "user".into() } else { r }
}
pub fn recipients(value: &Value) -> Result<Vec<String>, Error> {
    let refs = match value {
        Value::Null => vec![],
        Value::String(s) => vec![s.as_str()],
        Value::Array(a) => a
            .iter()
            .map(|v| {
                v.as_str()
                    .ok_or_else(|| Error::new(ERR_INVALID_PARAMS, "recipient must be a string"))
            })
            .collect::<Result<Vec<_>, _>>()?,
        _ => {
            return Err(Error::new(
                ERR_INVALID_PARAMS,
                "recipients must be a string or array",
            ));
        }
    };
    let mut out = Vec::new();
    for r in refs {
        let r = recipient(r);
        if !out.contains(&r) {
            out.push(r)
        }
    }
    Ok(out)
}
pub fn addressees(text: &str) -> (Vec<String>, String) {
    let mut names = Vec::new();
    let mut rest = text.trim_start_matches([' ', '\t']);
    while rest.starts_with('@') {
        let end = rest.find([' ', '\t', '\n']);
        let word = &rest[..end.unwrap_or(rest.len())];
        let name = simple_lower(word[1..].trim_end_matches([',', ':', ';']));
        if name.is_empty() {
            break;
        }
        names.push(name);
        if let Some(end) = end {
            rest = rest[end..].trim_start_matches([' ', '\t', '\n']);
        } else {
            return (names, String::new());
        }
    }
    if names.is_empty() {
        (names, text.into())
    } else {
        (names, rest.into())
    }
}

#[derive(Clone, Debug, Default, Serialize)]
#[serde(transparent)]
pub struct Recipients(pub crate::wire::Slice<String>);
impl<'de> Deserialize<'de> for Recipients {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        let value = Value::deserialize(d)?;
        match value {
            Value::String(one) => Ok(Self(vec![one].into())),
            Value::Null => Ok(Self::default()),
            Value::Array(many) => {
                let strings = many
                    .into_iter()
                    .map(|value| match value {
                        Value::String(value) => Ok(value),
                        Value::Null => Ok(String::new()),
                        _ => Err(serde::de::Error::custom("recipient must be a string")),
                    })
                    .collect::<Result<Vec<_>, D::Error>>()?;
                Ok(Self(strings.into()))
            }
            _ => Err(serde::de::Error::custom(
                "recipients must be a string or array",
            )),
        }
    }
}
impl Recipients {
    pub fn normalized(&self) -> Vec<String> {
        let mut result = Vec::new();
        for name in self.0.iter().map(|name| recipient(name)) {
            if !result.contains(&name) {
                result.push(name);
            }
        }
        result
    }
}
pub fn agent_busy(state: &str) -> bool {
    state == AGENT_RUNNING || state == AGENT_BLOCKED
}
pub fn roll_up<'a>(states: impl IntoIterator<Item = &'a str>) -> &'static str {
    let mut result = CHANNEL_IDLE;
    for state in states {
        if agent_busy(state) {
            return CHANNEL_WORKING;
        }
        if state == AGENT_WAITING {
            result = CHANNEL_WAITING;
        }
    }
    result
}
pub fn mode_summary(mode: &str) -> &'static str {
    match mode {
        MODE_AUTO => "allows inside the channel's directories, denies outside them",
        MODE_YOLO => "every permission is approved, directories included",
        _ => "every permission is asked",
    }
}
impl PromptInfo {
    pub fn question_position(&self, index: usize) -> (i64, i64) {
        if self.questions.len() == 1
            && self.question_number > 0
            && self.question_total >= self.question_number
        {
            return (self.question_number, self.question_total);
        }
        (index as i64 + 1, self.questions.len() as i64)
    }
}
pub fn permission_result(
    prompt: &PromptInfo,
    resolved: &crate::event::AskResolvedPayload,
) -> String {
    use crate::event::{ASK_DEFAULTED, ASK_WITHDRAWN};
    if resolved.outcome == ASK_WITHDRAWN {
        return "Permission withdrawn".into();
    }
    let mut text = match resolved.answer.as_str() {
        ANSWER_ALLOW => "Allowed once".into(),
        ANSWER_ALLOW_ALWAYS if !prompt.dir.is_empty() => format!(
            "Allowed and added directory: {}",
            if resolved.dir.is_empty() {
                &prompt.dir
            } else {
                &resolved.dir
            }
        ),
        ANSWER_ALLOW_ALWAYS => "Allowed for this channel".into(),
        ANSWER_ALLOW_PREFIX => format!("Allowed prefix: {}", prompt.prefix),
        ANSWER_DENY => "Denied".into(),
        _ => format!("Permission resolved: {}", resolved.answer),
    };
    if resolved.outcome == ASK_DEFAULTED {
        text = format!("Defaulted · {text}");
    }
    if !resolved.reason.trim().is_empty() {
        text.push_str(" · ");
        text.push_str(&resolved.reason);
    }
    text
}
