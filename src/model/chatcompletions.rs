//! Subscription providers using the Chat Completions streaming protocol.
use super::{
    Block, CallError, CompleteFuture, Delta, DeltaSink, Failure, Message, Request, Response,
    TokenSource, Usage,
    stream::{self, Codec, Feed},
};
use crate::model;
use crate::{wire::Slice, wire_struct};
use serde_json::value::RawValue;
use std::{collections::BTreeMap, sync::Arc};
use tokio_util::sync::CancellationToken;
pub type Variants = Arc<dyn Fn(&str) -> Vec<String> + Send + Sync>;
#[derive(Clone, Default)]
pub struct Traits {
    pub cache_header: String,
    pub cache_key_field: bool,
    pub replays_reasoning: bool,
    pub variants: Option<Variants>,
    pub is_limit: Option<stream::LimitCheck>,
}
#[derive(Clone)]
pub struct Provider {
    pub name: String,
    pub base_url: String,
    pub token: Option<TokenSource>,
    pub client: reqwest::Client,
    pub traits: Traits,
    pub options: stream::Options,
}
impl Provider {
    pub fn new(
        name: impl Into<String>,
        base_url: impl Into<String>,
        token: Option<TokenSource>,
        traits: Traits,
    ) -> anyhow::Result<Self> {
        Ok(Self {
            name: name.into(),
            base_url: base_url.into().trim_end_matches('/').into(),
            token,
            client: crate::httpx::client(None)?,
            traits,
            options: stream::Options::default(),
        })
    }
    pub fn build_body(&self, id: &str, request: &Request) -> serde_json::Result<Vec<u8>> {
        let effort = self
            .traits
            .variants
            .as_ref()
            .map(|variants| variants(id))
            .unwrap_or_default();
        let body = ChatRequest {
            model: id.into(),
            messages: to_messages(
                &request.system,
                &request.messages,
                self.traits.replays_reasoning,
            ),
            tools: request
                .tools
                .iter()
                .map(|tool| ChatTool {
                    kind: "function".into(),
                    function: ToolDefinition {
                        name: tool.name.clone(),
                        description: tool.description.clone(),
                        parameters: Some(model::tool_schema(tool)),
                    },
                })
                .collect(),
            stream: true,
            stream_options: Some(StreamOptions {
                include_usage: true,
            }),
            max_tokens: if request.max_tokens == 0 {
                16000
            } else {
                request.max_tokens
            },
            reasoning_effort: if effort.contains(&request.variant) {
                request.variant.clone()
            } else {
                String::new()
            },
            prompt_cache_key: if self.traits.cache_key_field {
                request.cache_key.clone()
            } else {
                String::new()
            },
        };
        serde_json::to_vec(&body)
    }
}
impl model::Provider for Provider {
    fn name(&self) -> &str {
        &self.name
    }
    fn open(&self, id: &str) -> Result<Arc<dyn model::Model>, CallError> {
        Ok(Arc::new(Client {
            provider: self.clone(),
            id: id.into(),
        }))
    }
    fn variants(&self, id: &str) -> Vec<String> {
        self.traits
            .variants
            .as_ref()
            .map(|variants| variants(id))
            .unwrap_or_default()
    }
}
struct Client {
    provider: Provider,
    id: String,
}
impl model::Model for Client {
    fn complete(
        &self,
        request: Request,
        on_delta: Option<DeltaSink>,
        cancel: CancellationToken,
    ) -> CompleteFuture<'_> {
        Box::pin(async move {
            let p = &self.provider;
            let body = p.build_body(&self.id, &request).map_err(|error| Failure {
                response: Response::default(),
                error: CallError::Permanent(format!("{}: {error}", p.name)),
            })?;
            let token = p.token.clone();
            let name = p.name.clone();
            let cache_header = p.traits.cache_header.clone();
            let cache_key = request.cache_key;
            let header: stream::HeaderSource = Arc::new(move |cancel| {
                let token = token.clone();
                let name = name.clone();
                let cache_header = cache_header.clone();
                let cache_key = cache_key.clone();
                Box::pin(async move {
                    let mut headers = reqwest::header::HeaderMap::new();
                    if let Some(token) = token {
                        let token = token(cancel).await.map_err(|error| error.context(&name))?;
                        headers.insert(
                            reqwest::header::AUTHORIZATION,
                            format!("Bearer {}", token.access).parse().map_err(|_| {
                                CallError::Permanent(format!("{name}: invalid bearer credential"))
                            })?,
                        );
                    }
                    if !cache_key.is_empty() && !cache_header.is_empty() {
                        let key = reqwest::header::HeaderName::from_bytes(cache_header.as_bytes())
                            .map_err(|_| {
                                CallError::Permanent(format!("{name}: invalid cache header name"))
                            })?;
                        headers.insert(
                            key,
                            cache_key.parse().map_err(|_| {
                                CallError::Permanent(format!("{name}: invalid cache key"))
                            })?,
                        );
                    }
                    Ok(headers)
                })
            });
            stream::complete(
                stream::Request {
                    name: p.name.clone(),
                    client: p.client.clone(),
                    url: format!("{}/chat/completions", p.base_url),
                    body,
                    header: Some(header),
                    on_status: None,
                    on_response: None,
                    is_limit: p.traits.is_limit.clone(),
                },
                &p.options,
                cancel,
                on_delta,
                |sink| Box::new(Accumulator::new(sink)),
            )
            .await
        })
    }
}
wire_struct!(ChatRequest{model:"model":String=>required,messages:"messages":Slice<ChatMessage> =>required,tools:"tools":Slice<ChatTool> =>optional,stream:"stream":bool=>required,stream_options:"stream_options":Option<StreamOptions> =>optional,max_tokens:"max_tokens":i64=>optional,reasoning_effort:"reasoning_effort":String=>optional,prompt_cache_key:"prompt_cache_key":String=>optional});
wire_struct!(StreamOptions{include_usage:"include_usage":bool=>required});
wire_struct!(ChatMessage{role:"role":String=>required,content:"content":Option<String> =>required,reasoning_content:"reasoning_content":String=>optional,tool_calls:"tool_calls":Slice<ToolCall> =>optional,tool_call_id:"tool_call_id":String=>optional});
wire_struct!(ToolCall{index:"index":i64=>optional,id:"id":String=>optional,kind:"type":String=>optional,function:"function":ToolFunction=>required});
wire_struct!(ToolFunction{name:"name":String=>optional,arguments:"arguments":String=>optional});
wire_struct!(ChatTool{kind:"type":String=>required,function:"function":ToolDefinition=>required});
wire_struct!(ToolDefinition{name:"name":String=>required,description:"description":String=>optional,parameters:"parameters":Option<Box<RawValue>> =>raw});
wire_struct!(ChatChunk{choices:"choices":Slice<ChunkChoice> =>required,usage:"usage":Option<ChunkUsage> =>required,error:"error":Option<ApiError> =>required});
wire_struct!(ChunkChoice{index:"index":i64=>required,delta:"delta":ChunkDelta=>required,finish_reason:"finish_reason":Option<String> =>required});
wire_struct!(ChunkDelta{content:"content":Option<String> =>required,reasoning_content:"reasoning_content":Option<String> =>required,reasoning:"reasoning":Option<String> =>required,tool_calls:"tool_calls":Slice<ToolCall> =>required});
wire_struct!(ChunkUsage{prompt_tokens:"prompt_tokens":i64=>required,completion_tokens:"completion_tokens":i64=>required,prompt_tokens_details:"prompt_tokens_details":Option<TokenDetails> =>required});
wire_struct!(TokenDetails{cached_tokens:"cached_tokens":i64=>required});
wire_struct!(ApiError{message:"message":String=>required,kind:"type":String=>required,code:"code":Option<serde_json::Value> =>required});
fn nullable<T>(values: Vec<T>) -> Slice<T> {
    if values.is_empty() {
        Slice::default()
    } else {
        values.into()
    }
}
fn to_messages(system: &str, messages: &[Message], replay_reasoning: bool) -> Slice<ChatMessage> {
    let mut result = Vec::new();
    if !system.is_empty() {
        result.push(ChatMessage {
            role: "system".into(),
            content: Some(system.into()),
            ..Default::default()
        });
    }
    let mut merged: Vec<Message> = Vec::new();
    for message in messages {
        if let Some(previous) = merged
            .last_mut()
            .filter(|previous| previous.role == message.role)
        {
            previous.blocks.extend(message.blocks.iter().cloned());
        } else {
            merged.push(message.clone());
        }
    }
    for message in merged {
        let mut text = Vec::new();
        let mut thinking = Vec::new();
        let mut calls = Vec::new();
        if message.role == model::ROLE_ASSISTANT {
            for block in message.blocks.iter() {
                match block.kind.as_str() {
                    model::BLOCK_THINKING
                        if replay_reasoning
                            && block.opaque.is_empty()
                            && !block.text.is_empty() =>
                    {
                        thinking.push(block.text.as_ref())
                    }
                    model::BLOCK_TEXT if !block.text.is_empty() => text.push(block.text.as_ref()),
                    model::BLOCK_TOOL_USE => calls.push(ToolCall {
                        id: block.id.clone(),
                        kind: "function".into(),
                        function: ToolFunction {
                            name: block.name.clone(),
                            arguments: model::tool_arguments(block).into(),
                        },
                        ..Default::default()
                    }),
                    _ => {}
                }
            }
            result.push(ChatMessage {
                role: "assistant".into(),
                content: if text.is_empty() {
                    None
                } else {
                    Some(text.join("\n"))
                },
                reasoning_content: thinking.join("\n"),
                tool_calls: nullable(calls),
                ..Default::default()
            });
        } else {
            for block in message.blocks.iter() {
                match block.kind.as_str() {
                    model::BLOCK_TOOL_RESULT => result.push(ChatMessage {
                        role: "tool".into(),
                        tool_call_id: block.tool_use_id.clone(),
                        content: Some(model::tool_result_text(block)),
                        ..Default::default()
                    }),
                    model::BLOCK_TEXT if !block.text.is_empty() => text.push(block.text.as_ref()),
                    _ => {}
                }
            }
            if !text.is_empty() {
                result.push(ChatMessage {
                    role: "user".into(),
                    content: Some(text.join("\n")),
                    ..Default::default()
                });
            }
        }
    }
    nullable(result)
}
fn finish_reason(reason: &str, has_calls: bool) -> &'static str {
    match reason {
        "tool_calls" | "function_call" => model::STOP_TOOL_USE,
        "stop" | "" => {
            if has_calls {
                model::STOP_TOOL_USE
            } else {
                model::STOP_END_TURN
            }
        }
        "length" => model::STOP_MAX_TOKENS,
        "content_filter" => model::STOP_REFUSAL,
        _ => model::STOP_OTHER,
    }
}
#[derive(Default)]
struct CallBuilder {
    id: String,
    name: String,
    args: String,
}
pub struct Accumulator {
    sink: DeltaSink,
    text: String,
    thinking: String,
    calls: BTreeMap<i64, CallBuilder>,
    finish: String,
    usage: Usage,
}
impl Accumulator {
    pub fn new(sink: DeltaSink) -> Self {
        Self {
            sink,
            text: String::new(),
            thinking: String::new(),
            calls: BTreeMap::new(),
            finish: String::new(),
            usage: Usage::default(),
        }
    }
}
impl Codec for Accumulator {
    fn feed(&mut self, payload: &[u8]) -> Result<Feed, String> {
        let chunk: ChatChunk = crate::wire::decode(&String::from_utf8_lossy(payload))
            .map_err(|error| format!("decode chunk: {error}"))?;
        if let Some(error) = chunk.error {
            return Err(format!("stream error: {}", error.message));
        }
        if let Some(usage) = chunk.usage {
            self.usage = Usage {
                input_tokens: usage
                    .prompt_tokens
                    .saturating_sub(
                        usage
                            .prompt_tokens_details
                            .as_ref()
                            .map_or(0, |details| details.cached_tokens),
                    )
                    .max(0),
                output_tokens: usage.completion_tokens,
                cache_read_tokens: usage
                    .prompt_tokens_details
                    .map_or(0, |details| details.cached_tokens),
                ..Default::default()
            };
        }
        for choice in chunk.choices.into_vec() {
            if choice.index != 0 {
                continue;
            }
            let delta = choice.delta;
            if let Some(content) = delta.content.filter(|s| !s.is_empty()) {
                self.text.push_str(&content);
                (self.sink)(Delta {
                    text: content,
                    ..Default::default()
                });
            }
            if let Some(thinking) = delta
                .reasoning_content
                .or(delta.reasoning)
                .filter(|s| !s.is_empty())
            {
                self.thinking.push_str(&thinking);
                (self.sink)(Delta {
                    thinking,
                    ..Default::default()
                });
            }
            for tool in delta.tool_calls.into_vec() {
                let call = self.calls.entry(tool.index).or_default();
                if !tool.id.is_empty() {
                    call.id = tool.id;
                }
                if !tool.function.name.is_empty() {
                    if call.name.is_empty() {
                        (self.sink)(Delta {
                            tool_name: tool.function.name.clone(),
                            ..Default::default()
                        });
                    }
                    call.name = tool.function.name;
                }
                call.args.push_str(&tool.function.arguments);
            }
            if let Some(finish) = choice.finish_reason.filter(|s| !s.is_empty()) {
                self.finish = finish;
            }
        }
        Ok(Feed::Continue)
    }
    fn response(&mut self) -> Response {
        let mut blocks = Vec::new();
        if !self.thinking.is_empty() {
            blocks.push(Block {
                kind: model::BLOCK_THINKING.into(),
                text: self.thinking.clone().into(),
                ..Default::default()
            });
        }
        if !self.text.is_empty() {
            blocks.push(Block::text(self.text.clone()));
        }
        for (index, call) in self.calls.values().enumerate() {
            let args = call.args.trim();
            let input = RawValue::from_string(args.into())
                .unwrap_or_else(|_| RawValue::from_string("{}".into()).unwrap());
            blocks.push(Block {
                kind: model::BLOCK_TOOL_USE.into(),
                id: if call.id.is_empty() {
                    format!("call_{index}")
                } else {
                    call.id.clone()
                },
                name: call.name.clone(),
                input: Some(input),
                ..Default::default()
            });
        }
        Response {
            blocks,
            stop_reason: finish_reason(&self.finish, !self.calls.is_empty()).into(),
            usage: self.usage.clone(),
        }
    }
    fn terminal(&self) -> bool {
        !self.finish.is_empty()
    }
}
