//! Provider-independent conversation and accounting contract.
pub mod chatcompletions;
pub mod codex;
pub mod quota;
pub mod registry;
pub mod stream;
use crate::{
    wire::{Slice, Time},
    wire_struct,
};
use serde_json::value::RawValue;
use std::sync::Arc;
pub const ROLE_USER: &str = "user";
pub const ROLE_ASSISTANT: &str = "assistant";
pub const BLOCK_TEXT: &str = "text";
pub const BLOCK_THINKING: &str = "thinking";
pub const BLOCK_TOOL_USE: &str = "tool_use";
pub const BLOCK_TOOL_RESULT: &str = "tool_result";
pub const STOP_END_TURN: &str = "end_turn";
pub const STOP_TOOL_USE: &str = "tool_use";
pub const STOP_MAX_TOKENS: &str = "max_tokens";
pub const STOP_REFUSAL: &str = "refusal";
pub const STOP_OTHER: &str = "other";
wire_struct!(Block {kind:"type":String=>required,text:"text":Arc<str> =>optional,provider_id:"provider_id":String=>optional,opaque:"opaque":Arc<str> =>optional,id:"id":String=>optional,name:"name":String=>optional,input:"input":Option<Box<RawValue>> =>raw_optional,tool_use_id:"tool_use_id":String=>optional,content:"content":Arc<str> =>optional,is_error:"is_error":bool=>optional,cancelled:"cancelled":bool=>optional});
wire_struct!(Message {role:"role":String=>required,blocks:"blocks":Slice<Block> =>required});
wire_struct!(ToolDef {name:"name":String=>required,description:"description":String=>required,schema:"schema":Option<Box<RawValue>> =>raw});
wire_struct!(Usage {input_tokens:"input_tokens":i64=>required,output_tokens:"output_tokens":i64=>required,cache_read_tokens:"cache_read_tokens":i64=>required,cache_write_tokens:"cache_write_tokens":i64=>required});
wire_struct!(UsageWindow {used_percent:"used_percent":f64=>float,minutes:"minutes":i64=>optional,resets_at:"resets_at":Time=>time_optional});
wire_struct!(PlanUsage {plan:"plan":String=>optional,windows:"windows":Slice<UsageWindow> =>required,observed:"observed":Time=>required,limited_until:"limited_until":Time=>time_optional});
#[derive(Clone, Debug, Default)]
pub struct Request {
    pub model: String,
    pub system: String,
    pub messages: Vec<Message>,
    pub tools: Vec<ToolDef>,
    pub max_tokens: i64,
    pub variant: String,
    pub cache_key: String,
}
#[derive(Clone, Debug, Default)]
pub struct Response {
    pub blocks: Vec<Block>,
    pub stop_reason: String,
    pub usage: Usage,
}
#[derive(Clone, Debug, Default)]
pub struct Delta {
    pub text: String,
    pub thinking: String,
    pub tool_name: String,
    pub reset: bool,
}
#[derive(Clone, Debug, Default)]
pub struct Capabilities {
    pub ignores_max_tokens: bool,
}
#[derive(Clone, Debug, Default)]
pub struct Info {
    pub capabilities: Capabilities,
    pub context_window: i64,
    pub max_output: i64,
    pub input_price: f64,
    pub output_price: f64,
    pub cache_read_price: f64,
    pub cache_write_price: f64,
}
impl Info {
    pub fn cost(&self, usage: &Usage) -> f64 {
        (usage.input_tokens as f64 * self.input_price
            + usage.output_tokens as f64 * self.output_price
            + usage.cache_read_tokens as f64 * self.cache_read_price
            + usage.cache_write_tokens as f64 * self.cache_write_price)
            / 1e6
    }
}
pub fn split(full: &str) -> anyhow::Result<(&str, &str)> {
    full.split_once('/')
        .filter(|(provider, id)| !provider.is_empty() && !id.is_empty())
        .ok_or_else(|| anyhow::anyhow!("model must be provider/model-id"))
}
impl Block {
    pub fn text(text: impl Into<String>) -> Self {
        Self {
            kind: BLOCK_TEXT.into(),
            text: Arc::from(text.into()),
            ..Self::default()
        }
    }
}
pub type DeltaSink = Arc<dyn Fn(Delta) + Send + Sync>;
#[derive(Clone, thiserror::Error, Debug)]
pub enum CallError {
    #[error("{0}")]
    Permanent(String),
    #[error("{0}")]
    Transient(String),
    #[error("{message}")]
    Limit {
        message: String,
        retry_after: std::time::Duration,
    },
    #[error("operation cancelled")]
    Cancelled,
}
impl CallError {
    pub fn context(self, prefix: &str) -> Self {
        match self {
            Self::Permanent(message) => Self::Permanent(format!("{prefix}: {message}")),
            Self::Transient(message) => Self::Transient(format!("{prefix}: {message}")),
            Self::Limit {
                message,
                retry_after,
            } => Self::Limit {
                message: format!("{prefix}: {message}"),
                retry_after,
            },
            Self::Cancelled => Self::Cancelled,
        }
    }
}
#[derive(Clone, Debug, thiserror::Error)]
#[error("{error}")]
pub struct Failure {
    pub response: Response,
    pub error: CallError,
}
pub type CompleteResult = Result<Response, Failure>;
pub type CompleteFuture<'a> =
    std::pin::Pin<Box<dyn std::future::Future<Output = CompleteResult> + Send + 'a>>;
pub trait Model: Send + Sync {
    fn complete(
        &self,
        request: Request,
        on_delta: Option<DeltaSink>,
        cancel: tokio_util::sync::CancellationToken,
    ) -> CompleteFuture<'_>;
}
pub trait Provider: Send + Sync {
    fn name(&self) -> &str;
    fn open(&self, model_id: &str) -> Result<Arc<dyn Model>, CallError>;
    fn variants(&self, _model_id: &str) -> Vec<String> {
        Vec::new()
    }
    fn capabilities(&self, _model_id: &str) -> Capabilities {
        Capabilities::default()
    }
}
#[derive(Clone, Default)]
pub struct Token {
    pub access: String,
    pub account_id: String,
}
impl std::fmt::Debug for Token {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Token")
            .field("access", &"[redacted]")
            .field("account_id", &self.account_id)
            .finish()
    }
}
pub type TokenFuture =
    std::pin::Pin<Box<dyn std::future::Future<Output = Result<Token, CallError>> + Send>>;
pub type TokenSource =
    Arc<dyn Fn(tokio_util::sync::CancellationToken) -> TokenFuture + Send + Sync>;
pub fn tool_schema(tool: &ToolDef) -> Box<RawValue> {
    tool.schema.clone().unwrap_or_else(|| {
        RawValue::from_string(r#"{"type":"object","properties":{}}"#.into()).unwrap()
    })
}
pub fn tool_arguments(block: &Block) -> &str {
    block.input.as_ref().map_or("{}", |raw| raw.get())
}
pub fn tool_result_text(block: &Block) -> String {
    let output = if block.content.is_empty() {
        "(no output)"
    } else {
        &block.content
    };
    if block.is_error && !output.starts_with("Error") {
        format!("Error: {output}")
    } else {
        output.into()
    }
}
