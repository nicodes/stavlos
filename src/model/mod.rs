//! Provider-independent conversation and accounting contract.
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
