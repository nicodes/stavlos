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
#[derive(Clone, Serialize, Deserialize)]
pub struct Request {
    pub jsonrpc: String,
    pub v: i64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub id: Option<Value>,
    pub method: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub params: Option<Value>,
}
#[derive(Clone, Serialize, Deserialize, Debug, thiserror::Error)]
#[error("{message}")]
pub struct Error {
    pub code: i64,
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
