//! ChatGPT subscription Responses protocol, including opaque reasoning replay.
use super::{
    Block, CallError, Capabilities, CompleteFuture, Delta, DeltaSink, Failure, PlanUsage, Request,
    Response, TokenSource, Usage, UsageWindow,
    stream::{self, Codec, Feed},
};
use crate::model;
use crate::{
    wire::{Slice, Time},
    wire_struct,
};
use serde_json::{Value, json};
use std::{collections::BTreeMap, sync::Arc};
use tokio_util::sync::CancellationToken;
pub const DEFAULT_ENDPOINT: &str = "https://chatgpt.com/backend-api/codex/responses";
pub const VARIANTS: &[&str] = &["low", "medium", "high", "xhigh"];
pub type UsageSink = Arc<dyn Fn(PlanUsage) + Send + Sync>;
#[derive(Clone)]
pub struct Provider {
    pub source: Option<TokenSource>,
    pub endpoint: String,
    pub client: reqwest::Client,
    pub on_usage: Option<UsageSink>,
    pub options: stream::Options,
}
impl Provider {
    pub fn new(
        source: Option<TokenSource>,
        endpoint: impl Into<String>,
        on_usage: Option<UsageSink>,
    ) -> anyhow::Result<Self> {
        Ok(Self {
            source,
            endpoint: endpoint.into(),
            client: crate::httpx::client(None)?,
            on_usage,
            options: stream::Options::default(),
        })
    }
}
impl model::Provider for Provider {
    fn name(&self) -> &str {
        "openai"
    }
    fn variants(&self, _: &str) -> Vec<String> {
        VARIANTS.iter().map(|s| (*s).into()).collect()
    }
    fn capabilities(&self, _: &str) -> Capabilities {
        Capabilities {
            ignores_max_tokens: true,
        }
    }
    fn open(&self, id: &str) -> Result<Arc<dyn model::Model>, CallError> {
        Ok(Arc::new(Client {
            provider: self.clone(),
            id: id.into(),
        }))
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
            let body = build_body(&self.id, &request).map_err(|error| Failure {
                response: Response::default(),
                error: CallError::Permanent(format!("codex: {error}")),
            })?;
            let source = p.source.clone();
            let cache_key = request.cache_key;
            let header: stream::HeaderSource = Arc::new(move |cancel| {
                let source = source.clone();
                let cache_key = cache_key.clone();
                Box::pin(async move {
                    let source = source.ok_or_else(|| {
                        CallError::Permanent("codex: no token source configured".into())
                    })?;
                    let token = source(cancel)
                        .await
                        .map_err(|error| error.context("codex: token"))?;
                    let mut headers = reqwest::header::HeaderMap::new();
                    headers.insert(
                        reqwest::header::AUTHORIZATION,
                        format!("Bearer {}", token.access).parse().map_err(|_| {
                            CallError::Permanent("codex: invalid bearer credential".into())
                        })?,
                    );
                    if !token.account_id.is_empty() {
                        headers.insert(
                            "chatgpt-account-id",
                            token.account_id.parse().map_err(|_| {
                                CallError::Permanent("codex: invalid account id".into())
                            })?,
                        );
                    }
                    headers.insert(
                        "originator",
                        reqwest::header::HeaderValue::from_static("stavlos"),
                    );
                    let architecture = match std::env::consts::ARCH {
                        "x86_64" => "amd64",
                        "aarch64" => "arm64",
                        other => other,
                    };
                    headers.insert(
                        reqwest::header::USER_AGENT,
                        format!("stavlos/0.1 ({} {architecture})", std::env::consts::OS)
                            .parse()
                            .unwrap(),
                    );
                    if !cache_key.is_empty() {
                        headers.insert(
                            "session-id",
                            cache_key.parse().map_err(|_| {
                                CallError::Permanent("codex: invalid session id".into())
                            })?,
                        );
                    }
                    Ok(headers)
                })
            });
            let on_usage = p.on_usage.clone();
            let observe: stream::ResponseHook = Arc::new(move |headers| {
                if let Some(sink) = &on_usage
                    && let Some(usage) = usage_from_headers(headers, Time::now())
                {
                    sink(usage);
                }
            });
            stream::complete(stream::Request{name:"codex".into(),client:p.client.clone(),url:p.endpoint.clone(),body,header:Some(header),on_status:Some(Arc::new(|status,_|if status==401{Some(CallError::Permanent("codex: unauthorized (token expired or revoked): run /providers to log in again".into()))}else{None})),on_response:Some(observe),is_limit:None},&p.options,cancel,on_delta,|sink|Box::new(Accumulator::new(sink))).await
        })
    }
}
pub fn build_body(id: &str, request: &Request) -> serde_json::Result<Vec<u8>> {
    let mut input: Vec<Value> = Vec::new();
    for message in &request.messages {
        let mut content = Vec::new();
        let (role, kind) = if message.role == model::ROLE_ASSISTANT {
            ("assistant", "output_text")
        } else {
            ("user", "input_text")
        };
        let flush = |content: &mut Vec<Value>, items: &mut Vec<Value>| {
            if !content.is_empty() {
                items.push(json!({"type":"message","role":role,"content":std::mem::take(content)}));
            }
        };
        for block in message.blocks.iter() {
            match block.kind.as_str() {
                model::BLOCK_TEXT => content.push(json!({"type":kind,"text":block.text})),
                model::BLOCK_THINKING => {
                    flush(&mut content, &mut input);
                    if !block.opaque.is_empty() {
                        let mut item = json!({"type":"reasoning","summary":[],"encrypted_content":block.opaque});
                        if !block.provider_id.is_empty() {
                            item["id"] = json!(block.provider_id);
                        }
                        input.push(item);
                    }
                }
                model::BLOCK_TOOL_USE => {
                    flush(&mut content, &mut input);
                    let mut item =
                        json!({"type":"function_call","arguments":model::tool_arguments(block)});
                    if !block.id.is_empty() {
                        item["call_id"] = json!(block.id);
                    }
                    if !block.name.is_empty() {
                        item["name"] = json!(block.name);
                    }
                    input.push(item);
                }
                model::BLOCK_TOOL_RESULT => {
                    flush(&mut content, &mut input);
                    let mut item = json!({"type":"function_call_output","output":model::tool_result_text(block)});
                    if !block.tool_use_id.is_empty() {
                        item["call_id"] = json!(block.tool_use_id);
                    }
                    input.push(item);
                }
                _ => {}
            }
        }
        flush(&mut content, &mut input);
    }
    let mut body = json!({"model":id,"input":input,"tool_choice":"auto","parallel_tool_calls":true,"store":false,"stream":true,"include":["reasoning.encrypted_content"],"reasoning":{"effort":if VARIANTS.contains(&request.variant.as_str()){request.variant.as_str()}else{"medium"},"summary":"auto"},"text":{"verbosity":"medium"}});
    if !request.system.is_empty() {
        body["instructions"] = json!(request.system);
    }
    if !request.cache_key.is_empty() {
        body["prompt_cache_key"] = json!(request.cache_key);
    }
    if !request.tools.is_empty() {
        let tools=request.tools.iter().map(|tool|{Ok(json!({"type":"function","name":tool.name,"description":tool.description,"parameters":serde_json::from_str::<Value>(model::tool_schema(tool).get())?,"strict":false}))}).collect::<serde_json::Result<Vec<_>>>()?;
        body["tools"] = json!(tools);
    }
    serde_json::to_vec(&body)
}
wire_struct!(Event{kind:"type":String=>required,output_index:"output_index":Option<i64> =>required,delta:"delta":String=>required,arguments:"arguments":String=>required,item:"item":Option<OutputItem> =>required,response:"response":Option<ResponseObject> =>required,error:"error":Option<ApiError> =>required,message:"message":String=>required});
wire_struct!(ApiError{kind:"type":String=>required,code:"code":String=>required,message:"message":String=>required});
impl ApiError {
    fn text(&self) -> String {
        if self.kind.is_empty() {
            self.message.clone()
        } else {
            format!("{}: {}", self.kind, self.message)
        }
    }
}
wire_struct!(ContentPart{kind:"type":String=>required,text:"text":String=>required});
wire_struct!(OutputItem{kind:"type":String=>required,id:"id":String=>required,role:"role":String=>required,content:"content":Slice<ContentPart> =>required,encrypted_content:"encrypted_content":String=>required,call_id:"call_id":String=>required,name:"name":String=>required,arguments:"arguments":String=>required,summary:"summary":Slice<ContentPart> =>required});
wire_struct!(ResponseObject{status:"status":String=>required,incomplete_details:"incomplete_details":Option<IncompleteDetails> =>required,output:"output":Slice<OutputItem> =>required,usage:"usage":Option<ResponseUsage> =>required,error:"error":Option<ApiError> =>required});
wire_struct!(IncompleteDetails{reason:"reason":String=>required});
wire_struct!(ResponseUsage{input_tokens:"input_tokens":i64=>required,output_tokens:"output_tokens":i64=>required,input_tokens_details:"input_tokens_details":Option<TokenDetails> =>required});
wire_struct!(TokenDetails{cached_tokens:"cached_tokens":i64=>required});
#[derive(Default)]
struct Item {
    kind: String,
    id: String,
    name: String,
    text: String,
    args: String,
    args_final: Option<String>,
    signature: String,
    announced: bool,
}
impl Item {
    fn apply(&mut self, item: &OutputItem, done: bool, sink: &DeltaSink) {
        if !item.kind.is_empty() {
            self.kind = item.kind.clone();
        }
        match item.kind.as_str() {
            "function_call" => {
                if !item.call_id.is_empty() {
                    self.id = item.call_id.clone();
                }
                if !item.name.is_empty() {
                    self.name = item.name.clone();
                    if !self.announced {
                        self.announced = true;
                        (sink)(Delta {
                            tool_name: item.name.clone(),
                            ..Default::default()
                        });
                    }
                }
                if done
                    && !item.arguments.is_empty()
                    && self.args_final.is_none()
                    && self.args.is_empty()
                {
                    self.args_final = Some(item.arguments.clone());
                }
            }
            "reasoning" => {
                if !item.id.is_empty() {
                    self.id = item.id.clone();
                }
                if !item.encrypted_content.is_empty() {
                    self.signature = item.encrypted_content.clone();
                }
                if done && self.text.is_empty() {
                    for part in item.summary.iter() {
                        self.text.push_str(&part.text);
                    }
                }
            }
            "message" => {
                if !item.id.is_empty() {
                    self.id = item.id.clone();
                }
                if done && self.text.is_empty() {
                    for part in item.content.iter() {
                        self.text.push_str(&part.text);
                    }
                }
            }
            _ => {}
        }
    }
    fn block(&self) -> Option<Block> {
        match self.kind.as_str() {
            "message" if !self.text.is_empty() => Some(Block::text(self.text.clone())),
            "reasoning" if !self.text.is_empty() || !self.signature.is_empty() => Some(Block {
                kind: model::BLOCK_THINKING.into(),
                provider_id: self.id.clone(),
                text: self.text.clone().into(),
                opaque: self.signature.clone().into(),
                ..Default::default()
            }),
            "function_call" if !self.name.is_empty() || !self.id.is_empty() => {
                let args = self.args_final.as_ref().unwrap_or(&self.args).trim();
                let input =
                    serde_json::value::RawValue::from_string(args.into()).unwrap_or_else(|_| {
                        serde_json::value::RawValue::from_string("{}".into()).unwrap()
                    });
                Some(Block {
                    kind: model::BLOCK_TOOL_USE.into(),
                    id: self.id.clone(),
                    name: self.name.clone(),
                    input: Some(input),
                    ..Default::default()
                })
            }
            _ => None,
        }
    }
}
pub struct Accumulator {
    sink: DeltaSink,
    items: BTreeMap<i64, Item>,
    order: Vec<i64>,
    usage: Usage,
    status: String,
    incomplete: String,
    final_output: Vec<OutputItem>,
}
impl Accumulator {
    pub fn new(sink: DeltaSink) -> Self {
        Self {
            sink,
            items: BTreeMap::new(),
            order: vec![],
            usage: Usage::default(),
            status: String::new(),
            incomplete: String::new(),
            final_output: vec![],
        }
    }
    fn at(&mut self, index: Option<i64>, kind: &str) -> &mut Item {
        let index = index.unwrap_or_else(|| self.order.last().copied().unwrap_or(0));
        self.items.entry(index).or_insert_with(|| {
            self.order.push(index);
            Item {
                kind: kind.into(),
                ..Default::default()
            }
        });
        let item = self.items.get_mut(&index).unwrap();
        if item.kind.is_empty() {
            item.kind = kind.into();
        }
        item
    }
}
impl Codec for Accumulator {
    fn feed(&mut self, payload: &[u8]) -> Result<Feed, String> {
        let event: Event = crate::wire::decode(&String::from_utf8_lossy(payload))
            .map_err(|error| format!("decode event: {error}"))?;
        match event.kind.as_str() {
            "response.output_text.delta" if !event.delta.is_empty() => {
                self.at(event.output_index, "message")
                    .text
                    .push_str(&event.delta);
                (self.sink)(Delta {
                    text: event.delta,
                    ..Default::default()
                });
            }
            "response.reasoning_summary_text.delta" if !event.delta.is_empty() => {
                self.at(event.output_index, "reasoning")
                    .text
                    .push_str(&event.delta);
                (self.sink)(Delta {
                    thinking: event.delta,
                    ..Default::default()
                });
            }
            "response.output_item.added" | "response.output_item.done" => {
                if let Some(item) = event.item {
                    let sink = self.sink.clone();
                    self.at(event.output_index, &item.kind).apply(
                        &item,
                        event.kind == "response.output_item.done",
                        &sink,
                    );
                }
            }
            "response.function_call_arguments.delta" => self
                .at(event.output_index, "function_call")
                .args
                .push_str(&event.delta),
            "response.function_call_arguments.done" => {
                let item = self.at(event.output_index, "function_call");
                if !event.arguments.is_empty() {
                    item.args_final = Some(event.arguments);
                }
            }
            "response.completed" | "response.incomplete" => {
                if let Some(response) = event.response {
                    self.status = response.status;
                    if let Some(details) = response.incomplete_details {
                        self.incomplete = details.reason;
                    }
                    if event.kind == "response.incomplete" && self.status.is_empty() {
                        self.status = "incomplete".into();
                    }
                    if let Some(usage) = response.usage {
                        let cached = usage
                            .input_tokens_details
                            .map_or(0, |details| details.cached_tokens);
                        self.usage = Usage {
                            input_tokens: usage.input_tokens.saturating_sub(cached).max(0),
                            output_tokens: usage.output_tokens,
                            cache_read_tokens: cached,
                            ..Default::default()
                        };
                    }
                    self.final_output = response.output.into_vec();
                }
                return Ok(Feed::Done);
            }
            "response.failed" => {
                let message = event
                    .response
                    .and_then(|response| response.error)
                    .or(event.error)
                    .map_or_else(|| "response failed".into(), |error| error.text());
                return Err(format!("stream error: {message}"));
            }
            "error" => {
                let message = event.error.map_or(event.message, |error| error.text());
                return Err(format!(
                    "stream error: {}",
                    if message.is_empty() {
                        "unknown error"
                    } else {
                        &message
                    }
                ));
            }
            _ => {}
        }
        Ok(Feed::Continue)
    }
    fn response(&mut self) -> Response {
        let mut blocks = self
            .order
            .iter()
            .filter_map(|index| self.items[index].block())
            .collect::<Vec<_>>();
        if blocks.is_empty() {
            for output in &self.final_output {
                let mut item = Item::default();
                item.apply(output, true, &self.sink);
                if let Some(block) = item.block() {
                    blocks.push(block);
                }
            }
        }
        let stop_reason = if self.status == "incomplete" && self.incomplete == "max_output_tokens" {
            model::STOP_MAX_TOKENS
        } else if blocks
            .iter()
            .any(|block| block.kind == model::BLOCK_TOOL_USE)
        {
            model::STOP_TOOL_USE
        } else {
            model::STOP_END_TURN
        };
        Response {
            blocks,
            stop_reason: stop_reason.into(),
            usage: self.usage.clone(),
        }
    }
    fn terminal(&self) -> bool {
        !self.status.is_empty()
    }
}
pub fn usage_from_headers(headers: &reqwest::header::HeaderMap, now: Time) -> Option<PlanUsage> {
    let mut usage = PlanUsage {
        observed: now,
        ..Default::default()
    };
    for name in ["primary", "secondary"] {
        let read = |field: &str| {
            headers
                .get(format!("x-codex-{name}-{field}"))
                .and_then(|value| value.to_str().ok())
                .unwrap_or("")
                .trim()
        };
        let Ok(used) = read("used-percent").parse::<f64>() else {
            continue;
        };
        if !used.is_finite() {
            continue;
        }
        let minutes = read("window-minutes")
            .parse::<i64>()
            .ok()
            .filter(|value| *value > 0)
            .unwrap_or(0);
        let resets_at = read("reset-at")
            .parse::<i64>()
            .ok()
            .filter(|value| *value > 0)
            .and_then(|seconds| chrono::DateTime::from_timestamp(seconds, 0))
            .map(|date| Time(date.with_timezone(&chrono::Local).fixed_offset()))
            .unwrap_or_default();
        if used == 0. && minutes == 0 && resets_at.is_zero() {
            continue;
        }
        usage.windows.push(UsageWindow {
            used_percent: used.clamp(0., 100.),
            minutes,
            resets_at,
        });
    }
    if usage.windows.is_empty() {
        None
    } else {
        Some(usage)
    }
}
