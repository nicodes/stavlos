//! Built-in tools and the owner services they are allowed to call.
mod files;
mod interaction;
pub mod markdown;
pub mod operator;
mod shell;
pub mod web;
use crate::{event, model, policy, proc, sandbox, toolfs::Filesystem, wire::Slice, wire_struct};
use anyhow::Result;
pub use interaction::{MessageInput, format_answers, message_kind, parse_questions, status_lines};
use serde_json::value::RawValue;
use sha2::{Digest as _, Sha256};
pub use shell::until_changed;
use std::{
    collections::BTreeMap,
    future::Future,
    path::PathBuf,
    pin::Pin,
    sync::{Arc, OnceLock},
    time::Duration,
};
use tokio_util::sync::CancellationToken;

pub type ServiceFuture<'a, T> = Pin<Box<dyn Future<Output = Result<T>> + Send + 'a>>;
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Output {
    pub output: String,
    pub is_error: bool,
}
impl Output {
    pub fn ok(text: impl Into<String>) -> Self {
        Self {
            output: text.into(),
            is_error: false,
        }
    }
    pub fn error(text: impl Into<String>) -> Self {
        Self {
            output: text.into(),
            is_error: true,
        }
    }
}
#[derive(Clone, Debug, Default)]
pub struct Skill {
    pub name: String,
    pub description: String,
    pub body: String,
    pub directory: PathBuf,
}
pub trait Jobs: Send + Sync {
    fn adopt(&self, command: &str, job: proc::Job, timeout: Duration) -> Result<String>;
    fn stop(&self, id: &str) -> Result<()>;
    fn has(&self, id: &str) -> bool;
}
pub trait Todos: Send + Sync {
    fn edit(&self, add: Vec<TodoAdd>, updates: Vec<TodoUpdate>) -> Result<Vec<event::TodoItem>>;
    fn list(&self) -> Vec<event::TodoItem>;
}
pub struct Answers {
    pub answers: Vec<String>,
    pub error: Option<String>,
}
pub trait Asker: Send + Sync {
    fn ask<'a>(
        &'a self,
        questions: Vec<event::Question>,
        cancel: CancellationToken,
    ) -> Pin<Box<dyn Future<Output = Answers> + Send + 'a>>;
}
pub trait Sheets: Send + Sync {
    fn write(&self, id: &str, title: &str, html: &str) -> Result<SheetRef>;
    fn list(&self) -> Vec<SheetRef>;
    fn delete(&self, id: &str) -> Result<()>;
}
pub trait Boards: Send + Sync {
    fn create<'a>(
        &'a self,
        caller: &'a str,
        name: &'a str,
        members: Vec<String>,
        cancel: CancellationToken,
    ) -> ServiceFuture<'a, String>;
    fn list<'a>(&'a self, caller: &'a str, cancel: CancellationToken) -> ServiceFuture<'a, String>;
    fn read<'a>(
        &'a self,
        caller: &'a str,
        channel: &'a str,
        from: i64,
        limit: usize,
        cancel: CancellationToken,
    ) -> ServiceFuture<'a, String>;
    fn message<'a>(
        &'a self,
        caller: &'a str,
        message: Delivery,
        cancel: CancellationToken,
    ) -> ServiceFuture<'a, String>;
}
pub struct Delivery {
    pub channel: String,
    pub to: Vec<String>,
    pub text: String,
    pub kind: String,
    pub reply_to: Vec<String>,
}
pub struct Spawn {
    pub parent: String,
    pub archetype: String,
    pub label: String,
    pub task: String,
    pub directories: Vec<String>,
}
pub trait Orchestrator: Send + Sync {
    fn spawn<'a>(
        &'a self,
        request: Spawn,
        cancel: CancellationToken,
    ) -> ServiceFuture<'a, (String, String)>;
    fn supports_scopes(&self) -> bool {
        true
    }
    fn message(&self, caller: &str, message: Delivery) -> Result<String>;
    fn cancel(&self, parent: &str, id: &str) -> Result<()>;
    fn status(&self, parent: &str, id: &str) -> Result<Vec<ChildStatus>>;
    fn can_spawn(&self, agent: &str) -> (bool, String);
    fn archetypes(&self, agent: &str) -> Vec<String>;
}
wire_struct!(TodoAdd {text:"text":String=>required,status:"status":String=>required});
wire_struct!(TodoUpdate {id:"id":String=>required,text:"text":String=>required,status:"status":String=>required});
wire_struct!(SheetRef {id:"id":String=>required,title:"title":String=>required,author:"author":String=>required,path:"path":String=>required,size:"size":i64=>required});
wire_struct!(ChildStatus {pending_replies:"pending_replies":Slice<event::ReplyRequest> =>optional,awaiting_replies:"awaiting_replies":Slice<event::ReplyRequest> =>optional,id:"id":String=>required,parent:"parent":String=>optional,you:"you":bool=>optional,name:"name":String=>required,role:"role":String=>required,state:"state":String=>required,turn:"turn":i64=>required,cost_usd:"cost_usd":f64=>required});
#[derive(Clone, Default)]
pub struct Env {
    pub filesystem: Filesystem,
    pub skills: BTreeMap<String, Skill>,
    pub agent: String,
    pub boards: Option<Arc<dyn Boards>>,
    pub orchestrator: Option<Arc<dyn Orchestrator>>,
    pub partial: Option<proc::Sink>,
    pub max_output: i64,
    pub overflow: PathBuf,
    pub jobs: Option<Arc<dyn Jobs>>,
    pub todo: Option<Arc<dyn Todos>>,
    pub ask: Option<Arc<dyn Asker>>,
    pub sheets: Option<Arc<dyn Sheets>>,
    pub pass_env: Vec<String>,
    pub sandbox: Option<sandbox::Spec>,
    pub search: web::SearchConfig,
    pub web: Option<Arc<web::Web>>,
    /// None discovers ripgrep on PATH; Some("") selects the portable walker.
    pub ripgrep: Option<PathBuf>,
}
impl Env {
    pub fn clip(&self, text: &str) -> String {
        let max = if self.max_output <= 0 {
            crate::clip::DEFAULT_MAX
        } else {
            self.max_output as usize
        };
        if text.len() <= max {
            return text.into();
        }
        let clipped = crate::clip::middle(text, max as i64);
        if self.overflow.as_os_str().is_empty() {
            return clipped;
        }
        use std::os::unix::fs::DirBuilderExt as _;
        if std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&self.overflow)
            .is_err()
        {
            return clipped;
        }
        let hash = format!("{:x}", Sha256::digest(text.as_bytes()));
        let path = self.overflow.join(format!("{}.txt", &hash[..16]));
        if crate::statefile::write_atomic(&path, text.as_bytes(), 0o600, false).is_err() {
            return clipped;
        }
        format!(
            "{clipped}\n[the whole output, {} bytes, is at {}: read it with an offset, or grep it]",
            text.len(),
            path.display()
        )
    }
}
fn decode<T: serde::de::DeserializeOwned + Default>(input: Option<&RawValue>) -> Result<T> {
    Ok(match input {
        None => T::default(),
        Some(input) => crate::wire::decode(&fold_keys(input.get().as_bytes())?)?,
    })
}
// Go's struct decoder accepts folded field names. Keep raw numbers and field order
// while folding input objects, including questions, options and todo edits.
fn fold_keys(input: &[u8]) -> Result<String> {
    let input = input.trim_ascii();
    if input.first() == Some(&b'{') {
        let mut fields = BTreeMap::new();
        for (key, value) in crate::config::jsonc::object_fields(input)? {
            let key = crate::protocol::simple_lower(&key).replace('ſ', "s");
            let value = fold_keys(&value)?;
            if value == "null"
                && fields.get(&key).is_some_and(|previous: &String| {
                    previous.starts_with('"')
                        || previous == "true"
                        || previous == "false"
                        || previous.as_bytes().first().is_some_and(u8::is_ascii_digit)
                })
            {
                continue;
            }
            fields.insert(key, value);
        }
        Ok(format!(
            "{{{}}}",
            fields
                .into_iter()
                .map(|(key, value)| format!("{}:{value}", serde_json::to_string(&key).unwrap()))
                .collect::<Vec<_>>()
                .join(",")
        ))
    } else if input.first() == Some(&b'[') {
        let values: Vec<Box<RawValue>> = serde_json::from_slice(input)?;
        Ok(format!(
            "[{}]",
            values
                .iter()
                .map(|v| fold_keys(v.get().as_bytes()))
                .collect::<Result<Vec<_>>>()?
                .join(",")
        ))
    } else {
        Ok(std::str::from_utf8(input)?.into())
    }
}
pub fn definitions() -> &'static [model::ToolDef] {
    static DEFINITIONS: OnceLock<Vec<model::ToolDef>> = OnceLock::new();
    DEFINITIONS.get_or_init(|| {
        serde_json::from_str(include_str!("definitions.json")).expect("built-in definitions")
    })
}
pub fn definition(name: &str, delegate: bool) -> Option<model::ToolDef> {
    let mut def = definitions().iter().find(|d| d.name == name)?.clone();
    if !delegate && matches!(name, "agent" | "channel") {
        let mut schema: serde_json::Value =
            serde_json::from_str(def.schema.as_ref()?.get()).ok()?;
        schema["properties"]["action"]["enum"] = if name == "agent" {
            serde_json::json!(["status"])
        } else {
            serde_json::json!(["list", "read"])
        };
        def.schema = Some(RawValue::from_string(schema.to_string()).ok()?);
    }
    Some(def)
}
pub fn policy_subject(
    name: &str,
    input: Option<&RawValue>,
    subject: policy::Subject,
) -> policy::Subject {
    use crate::toolname as t;
    let action = match t::operation_raw(name, input) {
        t::SHELL => Some("run"),
        t::AGENT_CREATE if name == t::AGENT => Some("create"),
        t::AGENT_CANCEL if name == t::AGENT => Some("cancel"),
        t::AGENT_STATUS if name == t::AGENT => Some("status"),
        t::FETCH if name == t::WEB => Some("fetch"),
        t::SEARCH if name == t::WEB => Some("search"),
        t::SHELL_KILL if name == t::SHELL => Some("kill"),
        _ => None,
    };
    if let Some(action) = action {
        policy::Subject::text(format!("{action} {}", subject.primary()))
    } else {
        subject
    }
}
/// Native file, process and owner-interaction tools. Web and operator have dedicated boundaries.
pub fn subject(name: &str, input: Option<&RawValue>) -> policy::Subject {
    match name {
        "read" | "patch" | "grep" | "glob" => files::subject(name, input),
        "shell" => shell::subject(input),
        "operator" => operator::subject(input),
        "web" => web::subject(input),
        _ => interaction::subject(name, input),
    }
}
pub async fn run(
    name: &str,
    input: Option<&RawValue>,
    env: &Env,
    cancel: CancellationToken,
) -> Output {
    let result = match name {
        "read" | "patch" | "grep" | "glob" => files::run(name, input, env, cancel).await,
        "shell" => shell::run(input, env, cancel).await,
        "operator" => Ok(operator::run(input, cancel).await),
        "web" => web::run(input, env, cancel).await,
        _ => interaction::run(name, input, env, cancel).await,
    };
    result.unwrap_or_else(|error| Output::error(format!("{error:#}")))
}
