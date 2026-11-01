//! Shared streaming transport: bounded SSE lines, cancellation, retries and partial responses.
use super::{CallError, CompleteResult, Delta, DeltaSink, Failure, Response, STOP_OTHER};
use reqwest::header::{ACCEPT, CONTENT_TYPE, HeaderMap, HeaderValue, RETRY_AFTER};
use std::{
    sync::{
        Arc, OnceLock,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, SystemTime},
};
use tokio_util::sync::CancellationToken;

pub const MAX_SSE_LINE: usize = 16 << 20;
pub type HeaderFuture =
    std::pin::Pin<Box<dyn std::future::Future<Output = Result<HeaderMap, CallError>> + Send>>;
pub type HeaderSource = Arc<dyn Fn(CancellationToken) -> HeaderFuture + Send + Sync>;
pub type StatusHook = Arc<dyn Fn(u16, &[u8]) -> Option<CallError> + Send + Sync>;
pub type ResponseHook = Arc<dyn Fn(&HeaderMap) + Send + Sync>;
pub type LimitCheck = Arc<dyn Fn(u16, &[u8]) -> bool + Send + Sync>;
#[derive(Clone)]
pub struct Request {
    pub name: String,
    pub client: reqwest::Client,
    pub url: String,
    pub body: Vec<u8>,
    pub header: Option<HeaderSource>,
    pub on_status: Option<StatusHook>,
    pub on_response: Option<ResponseHook>,
    pub is_limit: Option<LimitCheck>,
}
#[derive(Clone, Debug)]
pub struct Options {
    pub max_attempts: usize,
    pub mid_stream_retries: usize,
    pub retry_delay: Duration,
    pub idle_timeout: Duration,
    pub header_timeout: Duration,
}
impl Default for Options {
    fn default() -> Self {
        Self {
            max_attempts: 3,
            mid_stream_retries: 3,
            retry_delay: Duration::from_millis(500),
            idle_timeout: Duration::from_secs(120),
            header_timeout: Duration::from_secs(180),
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Feed {
    Continue,
    Done,
}
#[derive(Debug, thiserror::Error)]
pub enum SseError {
    #[error("{0}")]
    Codec(String),
    #[error("SSE line exceeds the 16 MiB limit")]
    LineLimit,
}
impl SseError {
    fn retryable(&self) -> bool {
        match self {
            Self::LineLimit => true,
            Self::Codec(message) => transient(message),
        }
    }
}
pub trait Codec: Send {
    fn feed(&mut self, payload: &[u8]) -> Result<Feed, String>;
    fn response(&mut self) -> Response;
    fn terminal(&self) -> bool;
}
#[derive(Default)]
pub struct Sse {
    line: Vec<u8>,
    data: Vec<u8>,
    pub lines: usize,
    done: bool,
}
impl Sse {
    fn flush(&mut self, codec: &mut dyn Codec) -> Result<(), SseError> {
        if self.data.is_empty() {
            return Ok(());
        }
        let payload = std::mem::take(&mut self.data);
        if String::from_utf8_lossy(&payload).trim() == "[DONE]"
            || codec.feed(&payload).map_err(SseError::Codec)? == Feed::Done
        {
            self.done = true;
        }
        Ok(())
    }
    fn line(&mut self, codec: &mut dyn Codec) -> Result<(), SseError> {
        self.lines += 1;
        let mut line = std::mem::take(&mut self.line);
        if line.last() == Some(&b'\r') {
            line.pop();
        }
        if line.is_empty() {
            return self.flush(codec);
        }
        if let Some(value) = line.strip_prefix(b"data:") {
            let value = value.strip_prefix(b" ").unwrap_or(value);
            if !self.data.is_empty() {
                self.data.push(b'\n');
            }
            self.data.extend_from_slice(value);
        }
        Ok(())
    }
    pub fn push(&mut self, mut bytes: &[u8], codec: &mut dyn Codec) -> Result<bool, SseError> {
        while !bytes.is_empty() && !self.done {
            let end = bytes.iter().position(|b| *b == b'\n');
            let count = end.unwrap_or(bytes.len());
            if self.line.len() + count >= MAX_SSE_LINE {
                return Err(SseError::LineLimit);
            }
            self.line.extend_from_slice(&bytes[..count]);
            bytes = &bytes[count..];
            if end.is_some() {
                bytes = &bytes[1..];
                self.line(codec)?;
            }
        }
        Ok(self.done)
    }
    pub fn finish(&mut self, codec: &mut dyn Codec) -> Result<bool, SseError> {
        if self.done {
            return Ok(true);
        }
        if !self.line.is_empty() {
            self.line(codec)?;
        }
        if !self.done {
            self.flush(codec)?;
        }
        Ok(self.done)
    }
}
pub fn transient(message: &str) -> bool {
    static PATTERN: OnceLock<regex::Regex> = OnceLock::new();
    PATTERN.get_or_init(||regex::Regex::new(r"(?i)at capacity|high demand|overloaded|try again|temporar|server[ _]error|internal error|service unavailable|upstream|timed? ?out|connection reset").unwrap()).is_match(message)
}
pub fn has_any(body: &[u8], marks: &[&str]) -> bool {
    let text = String::from_utf8_lossy(body).to_lowercase();
    marks.iter().any(|mark| text.contains(mark))
}
pub fn plan_limit(status: u16, body: &[u8]) -> bool {
    matches!(status, 429 | 403 | 402)
        && has_any(
            body,
            &[
                "usage_limit",
                "usage limit",
                "spending-limit",
                "spending limit",
                "limit reached",
                "limit_reached",
                "quota",
                "exceeded your current",
                "out of credits",
                "insufficient_quota",
                "insufficient balance",
                "insufficient credit",
            ],
        )
}
pub fn error_text(raw: &[u8]) -> String {
    if let Ok(value) = serde_json::from_slice::<serde_json::Value>(raw)
        && let Some(message) = value["error"]["message"].as_str().filter(|s| !s.is_empty())
    {
        let kind = value["error"]["type"].as_str().unwrap_or("");
        return if kind.is_empty() {
            message.into()
        } else {
            format!("{kind}: {message}")
        };
    }
    String::from_utf8_lossy(raw).trim().into()
}
pub fn retry_after(value: &str, now: SystemTime) -> Duration {
    let value = value.trim();
    if let Ok(seconds) = value.parse::<u64>() {
        return Duration::from_secs(seconds);
    }
    httpdate::parse_http_date(value)
        .ok()
        .and_then(|date| date.duration_since(now).ok())
        .unwrap_or_default()
}
fn jitter(delay: Duration) -> Duration {
    let nanos = delay.as_nanos().min((u64::MAX - 1) as u128) as u64;
    let random = uuid::Uuid::new_v4().as_u128() as u64;
    (delay / 2)
        .saturating_add(Duration::from_nanos(random % (nanos + 1)))
        .min(Duration::from_secs(30))
}
async fn pause(delay: Duration, cancel: &CancellationToken) -> Result<(), CallError> {
    tokio::select! {biased;_=cancel.cancelled()=>Err(CallError::Cancelled),_=tokio::time::sleep(delay)=>Ok(())}
}
struct Rejection {
    error: CallError,
    retry: bool,
    after: Duration,
    limit: bool,
}
fn rejected(error: CallError, retry: bool) -> Rejection {
    Rejection {
        error,
        retry,
        after: Duration::ZERO,
        limit: false,
    }
}
async fn once(
    request: &Request,
    options: &Options,
    cancel: &CancellationToken,
) -> Result<reqwest::Response, Rejection> {
    let mut headers = if let Some(header) = &request.header {
        tokio::select! {biased;_=cancel.cancelled()=>return Err(rejected(CallError::Cancelled,false)),result=header(cancel.clone())=>result.map_err(|error|rejected(error,false))?}
    } else {
        HeaderMap::new()
    };
    headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
    headers.insert(ACCEPT, HeaderValue::from_static("text/event-stream"));
    let sending = request
        .client
        .post(&request.url)
        .headers(headers)
        .body(request.body.clone())
        .send();
    let mut response = tokio::select! {biased;
        _=cancel.cancelled()=>return Err(rejected(CallError::Cancelled,false)),
        result=tokio::time::timeout(options.header_timeout,sending)=>match result {
            Ok(Ok(response))=>response,
            Ok(Err(error))=>return Err(rejected(CallError::Permanent(format!("{}: {}",request.name,error.without_url())),true)),
            Err(_)=>return Err(rejected(CallError::Permanent(format!("{}: timed out waiting for response headers",request.name)),true)),
        }
    };
    if let Some(observe) = &request.on_response {
        observe(response.headers());
    }
    if response.status().is_success() {
        return Ok(response);
    }
    let code = response.status().as_u16();
    let after = response
        .headers()
        .get(RETRY_AFTER)
        .and_then(|value| value.to_str().ok())
        .map(|value| retry_after(value, SystemTime::now()))
        .unwrap_or_default();
    let mut raw = Vec::new();
    while raw.len() < 2048 {
        let part = tokio::select! {biased;_=cancel.cancelled()=>return Err(rejected(CallError::Cancelled,false)),part=response.chunk()=>part};
        match part {
            Ok(Some(part)) => {
                let count = part.len().min(2048 - raw.len());
                raw.extend_from_slice(&part[..count]);
            }
            _ => break,
        }
    }
    if let Some(error) = request
        .on_status
        .as_ref()
        .and_then(|on_status| on_status(code, &raw))
    {
        return Err(rejected(error, false));
    }
    let message = format!("{}: status {code}: {}", request.name, error_text(&raw));
    let limit = request
        .is_limit
        .as_ref()
        .map_or_else(|| plan_limit(code, &raw), |check| check(code, &raw));
    if limit {
        return Err(rejected(
            CallError::Limit {
                message,
                retry_after: after,
            },
            false,
        ));
    }
    Err(Rejection {
        error: CallError::Permanent(message),
        retry: matches!(code, 408 | 409 | 429) || code >= 500,
        after,
        limit: code == 429,
    })
}
async fn post(
    request: &Request,
    options: &Options,
    cancel: &CancellationToken,
) -> Result<reqwest::Response, CallError> {
    let mut delay = options.retry_delay;
    for attempt in 1.. {
        match once(request, options, cancel).await {
            Ok(response) => return Ok(response),
            Err(rejected) => {
                if !rejected.retry {
                    return Err(rejected.error);
                }
                if attempt >= options.max_attempts {
                    return Err(if rejected.limit {
                        CallError::Limit {
                            message: rejected.error.to_string(),
                            retry_after: rejected.after,
                        }
                    } else {
                        CallError::Transient(rejected.error.to_string())
                    });
                }
                pause(
                    jitter(delay)
                        .max(rejected.after)
                        .min(Duration::from_secs(30)),
                    cancel,
                )
                .await?;
                delay = delay.saturating_mul(2);
            }
        }
    }
    unreachable!()
}
async fn attempt(
    request: &Request,
    options: &Options,
    cancel: &CancellationToken,
    codec: &mut dyn Codec,
) -> (Response, bool, Option<CallError>) {
    let mut response = match post(request, options, cancel).await {
        Ok(response) => response,
        Err(error) => return (Response::default(), false, Some(error)),
    };
    let mut sse = Sse::default();
    let mut idle = tokio::time::Instant::now() + options.idle_timeout;
    loop {
        let part = tokio::select! {biased;
            _=cancel.cancelled()=>return(codec.response(),false,Some(CallError::Cancelled)),
            part=tokio::time::timeout_at(idle,response.chunk())=>part,
        };
        let before = sse.lines;
        let ended = match part {
            Err(_) => {
                let mut response = codec.response();
                response.stop_reason = STOP_OTHER.into();
                return (
                    response,
                    true,
                    Some(CallError::Permanent(format!(
                        "{}: no data from the provider for {:?}",
                        request.name, options.idle_timeout
                    ))),
                );
            }
            Ok(Err(error)) => {
                return (
                    codec.response(),
                    true,
                    Some(CallError::Permanent(format!(
                        "{}: {}",
                        request.name,
                        error.without_url()
                    ))),
                );
            }
            Ok(Ok(Some(bytes))) => sse.push(&bytes, codec),
            Ok(Ok(None)) => match sse.finish(codec) {
                Ok(done) if done || codec.terminal() => return (codec.response(), false, None),
                Ok(_) => {
                    let mut response = codec.response();
                    response.stop_reason = STOP_OTHER.into();
                    return (
                        response,
                        true,
                        Some(CallError::Permanent(format!(
                            "{}: stream ended before the response was complete",
                            request.name
                        ))),
                    );
                }
                Err(error) => {
                    return (
                        codec.response(),
                        error.retryable(),
                        Some(CallError::Permanent(format!("{}: {error}", request.name))),
                    );
                }
            },
        };
        if sse.lines != before {
            idle = tokio::time::Instant::now() + options.idle_timeout;
        }
        match ended {
            Ok(true) => return (codec.response(), false, None),
            Ok(false) => {}
            Err(error) => {
                return (
                    codec.response(),
                    error.retryable(),
                    Some(CallError::Permanent(format!("{}: {error}", request.name))),
                );
            }
        }
    }
}
pub async fn complete(
    request: Request,
    options: &Options,
    cancel: CancellationToken,
    on_delta: Option<DeltaSink>,
    new_codec: impl Fn(DeltaSink) -> Box<dyn Codec>,
) -> CompleteResult {
    let sink = on_delta.unwrap_or_else(|| Arc::new(|_| {}));
    let mut delay = options.retry_delay;
    for attempt_number in 0.. {
        let streamed = Arc::new(AtomicBool::new(false));
        let marked = streamed.clone();
        let deltas = sink.clone();
        let mut codec = new_codec(Arc::new(move |delta| {
            marked.store(true, Ordering::Relaxed);
            deltas(delta)
        }));
        let (response, broke, error) = attempt(&request, options, &cancel, codec.as_mut()).await;
        let Some(error) = error else {
            return Ok(response);
        };
        if !broke || cancel.is_cancelled() {
            return Err(Failure { response, error });
        }
        if attempt_number >= options.mid_stream_retries {
            return Err(Failure {
                response,
                error: CallError::Transient(error.to_string()),
            });
        }
        if streamed.load(Ordering::Relaxed) {
            sink(Delta {
                reset: true,
                ..Delta::default()
            });
        }
        if let Err(error) = pause(jitter(delay), &cancel).await {
            return Err(Failure { response, error });
        }
        delay = delay.saturating_mul(2);
    }
    unreachable!()
}
