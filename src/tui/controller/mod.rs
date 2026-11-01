//! Terminal state transitions produce scoped RPC effects; network work never owns a view borrow.
mod channel;
mod commands;
mod configuration;
mod dialogs;
mod keyboard;
mod prompts;
mod replies;
use super::{editor::Editor, transcript::Transcript};
use crate::{
    paths::Paths,
    protocol::{self, methods::Method},
    wire::Time,
};
use serde::Serialize;
use serde_json::Value;
use std::{
    collections::{BTreeMap, BTreeSet},
    path::PathBuf,
    time::Duration,
};
pub const CHAT: &str = "@channel-chat";
#[derive(Clone, PartialEq, Eq)]
pub struct Scope {
    pub channel: String,
    pub generation: u64,
}
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Focus {
    Input,
    Chat,
    Sidebar,
    Tabs,
    Meta,
    Prompt,
}
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Dialog {
    Models,
    Variants,
    Roles,
    Mode,
    Providers,
    Methods,
    Channels,
    Permissions,
    Questions,
    Async,
    Todo,
    Mcp,
    Dirs,
    Discord,
    Web,
    Sandbox,
    NewName,
    NewDirectory,
    AddDirectory,
    EditDirectory,
    SetDirectory,
    DenyReason,
    PermissionDirectory,
    QuestionText,
    Login,
    LoginKey,
    Usage,
    Plan,
    Configuration,
    ConfirmLogout,
}
pub struct Choice {
    pub id: String,
    pub label: String,
    pub description: String,
}
pub struct Popup {
    pub kind: Dialog,
    pub title: String,
    pub choices: Vec<Choice>,
    pub cursor: usize,
    pub input: Editor,
    pub agent: String,
    pub context: String,
    pub detail: String,
    pub epoch: u64,
}
impl Popup {
    pub fn new(kind: Dialog, title: impl Into<String>, epoch: u64) -> Self {
        Self {
            kind,
            title: title.into(),
            choices: vec![],
            cursor: 0,
            input: Editor::default(),
            agent: String::new(),
            context: String::new(),
            detail: String::new(),
            epoch,
        }
    }
    pub fn selected(&self) -> Option<&Choice> {
        self.choices.get(self.cursor)
    }
}
pub enum Tag {
    Reconcile,
    Subscribed,
    Catalog,
    Tree(String),
    Prompts,
    Commands,
    Roles(bool),
    Models(u64),
    Variants(u64),
    Providers {
        open: bool,
        jump: String,
        epoch: u64,
    },
    LoginStart {
        provider: String,
        epoch: u64,
    },
    LoginDone(String),
    Changed(String),
    ChannelOpened {
        agent: String,
    },
    DirectoryChanged,
    DirectoryAdded(String),
    PromptAnswered(String),
    Discord(bool, u64),
    Web(bool, u64),
    Sandbox(bool, u64),
    Plans,
    Cache,
    Usage(u64),
    PlanSeries(u64),
    ConfigTree(u64),
    ConfigRead(u64),
    ConfigEdited(u64),
}
pub struct Request {
    pub scope: Option<Scope>,
    pub method: &'static str,
    pub params: Value,
    pub tag: Tag,
    pub timeout: Option<Duration>,
    pub cancellation: Option<String>,
}
pub enum Effect {
    Request(Request),
    Answer {
        scope: Scope,
        reply: protocol::PromptReplyParams,
    },
    Remember {
        channel: String,
        at: i64,
    },
    Cancel(String),
    OpenBrowser(String),
    Copy(String),
}
pub struct Reply {
    pub scope: Option<Scope>,
    pub tag: Tag,
    pub result: anyhow::Result<Value>,
}
pub fn request<P: Serialize, R>(
    scope: Option<Scope>,
    method: Method<P, R>,
    params: P,
    tag: Tag,
) -> Effect {
    Effect::Request(Request {
        scope,
        method: method.name,
        params: serde_json::to_value(params).expect("typed terminal RPC parameters"),
        tag,
        timeout: Some(Duration::from_secs(15)),
        cancellation: None,
    })
}
pub struct Channel {
    pub info: protocol::ChannelInfo,
    pub agents: Vec<protocol::AgentInfo>,
    pub selected: String,
    pub chat: bool,
    pub transcripts: BTreeMap<String, Transcript>,
    pub parents: BTreeMap<String, String>,
    pub seq: i64,
    pub replay_to: i64,
    pub history_from: i64,
    pub whole_log: bool,
    pub await_first: bool,
    pub reconciled: bool,
    pub loading: bool,
    pub input: Editor,
    pub history: Vec<String>,
    pub history_index: usize,
    pub history_draft: String,
    pub history_seq: i64,
    pub expanded: BTreeMap<String, BTreeSet<i64>>,
    pub chat_cursor: i64,
    pub scroll: usize,
    pub follow: bool,
    pub stream_epochs: BTreeMap<String, (i64, bool)>,
}
impl Channel {
    pub fn new(id: String) -> Self {
        Self {
            info: protocol::ChannelInfo {
                id,
                ..Default::default()
            },
            agents: vec![],
            selected: String::new(),
            chat: true,
            transcripts: BTreeMap::new(),
            parents: BTreeMap::new(),
            seq: 0,
            replay_to: 0,
            history_from: 0,
            whole_log: false,
            await_first: false,
            reconciled: false,
            loading: true,
            input: Editor::default(),
            history: vec![],
            history_index: 0,
            history_draft: String::new(),
            history_seq: 0,
            expanded: BTreeMap::new(),
            chat_cursor: 0,
            scroll: 0,
            follow: true,
            stream_epochs: BTreeMap::new(),
        }
    }
    pub fn transcript(&mut self, id: &str) -> &mut Transcript {
        self.transcripts.entry(id.into()).or_insert_with(|| {
            if id == CHAT {
                Transcript::chat()
            } else {
                Transcript::new()
            }
        })
    }
    pub fn view_id(&self) -> &str {
        if self.chat { CHAT } else { &self.selected }
    }
    pub fn view(&self) -> Option<&Transcript> {
        self.transcripts.get(self.view_id())
    }
    pub fn selected_agent(&self) -> Option<&protocol::AgentInfo> {
        self.agents.iter().find(|agent| agent.id == self.selected)
    }
    pub fn push_history(&mut self, text: String) {
        if self.history.last() != Some(&text) {
            self.history.push(text);
        }
        self.history_index = self.history.len();
        self.history_draft.clear();
    }
}
pub struct QuestionDraft {
    pub prompt: String,
    pub index: usize,
    pub cursor: usize,
    pub selected: Vec<BTreeSet<usize>>,
    pub custom: Vec<String>,
}
impl QuestionDraft {
    pub fn new(prompt: &protocol::PromptInfo) -> Self {
        Self {
            prompt: prompt.id.clone(),
            index: 0,
            cursor: 0,
            selected: vec![BTreeSet::new(); prompt.questions.len()],
            custom: vec![String::new(); prompt.questions.len()],
        }
    }
}
pub struct Configuration {
    pub epoch: u64,
    pub scope: protocol::ConfigScope,
    pub tree: protocol::ConfigTree,
    pub document: Option<protocol::ConfigDocument>,
    pub file_cursor: usize,
    pub field_cursor: usize,
    pub section: usize,
    pub pane: usize,
    pub collapsed: BTreeSet<String>,
    pub raw: bool,
    pub editing: bool,
    pub input: Editor,
    pub field: Vec<String>,
    pub action: String,
    pub pending: String,
    pub dirty: bool,
    pub busy: bool,
}
pub struct Controller {
    pub paths: Paths,
    pub home: PathBuf,
    pub client_id: String,
    pub scope: Scope,
    pub channel: Channel,
    pub visited: BTreeMap<String, Channel>,
    pub catalog: Vec<protocol::ChannelInfo>,
    pub trees: BTreeMap<String, Vec<protocol::AgentInfo>>,
    pub tree_closed: BTreeSet<String>,
    pub tree_all: BTreeSet<String>,
    pub sidebar_cursor: usize,
    pub show_sidebar: bool,
    pub hide_keys: bool,
    pub focus: Focus,
    pub tab_cursor: usize,
    pub meta_cursor: usize,
    pub prompts: BTreeMap<String, protocol::PromptInfo>,
    pub questions: BTreeMap<String, QuestionDraft>,
    pub permission_cursor: BTreeMap<String, usize>,
    pub prompt_busy: BTreeSet<String>,
    pub permission_drafts: BTreeMap<String, (String, String)>,
    pub mcp_open: BTreeSet<String>,
    pub active_prompt: String,
    pub providers: Vec<protocol::ProviderInfo>,
    pub roles: Vec<protocol::RoleInfo>,
    pub models: Vec<protocol::ModelInfo>,
    pub variants: Vec<String>,
    pub custom_commands: Vec<protocol::CommandInfo>,
    pub plans: Vec<protocol::PlanUsageInfo>,
    pub cache: protocol::CacheUsageResult,
    pub discord: protocol::DiscordStatus,
    pub web: protocol::WebStatus,
    pub sandbox: protocol::SandboxStatus,
    pub usage: Option<protocol::UsageSeriesResult>,
    pub plan_series: Option<protocol::PlanSeriesResult>,
    pub popup: Option<Popup>,
    pub popup_epoch: u64,
    pub config: Option<Configuration>,
    pub config_epoch: u64,
    pub login_id: String,
    pub login_provider: String,
    pub login_method: String,
    pub login: protocol::LoginStartResult,
    pub discord_epoch: u64,
    pub web_epoch: u64,
    pub sandbox_epoch: u64,
    pub palette_cursor: usize,
    pub mention_cursor: usize,
    pub status: String,
    pub status_error: bool,
    pub status_until: Time,
    pub now: Time,
    pub quit_armed: Option<Time>,
    pub cancel_armed: Option<Time>,
    pub quit: bool,
    pub tree_dirty: bool,
    pub dirty: bool,
    pub width: u16,
    pub height: u16,
}
impl Controller {
    pub fn new(paths: Paths, home: PathBuf, client_id: String, channel: String, now: Time) -> Self {
        Self {
            paths,
            home,
            client_id,
            scope: Scope {
                channel: channel.clone(),
                generation: 0,
            },
            channel: Channel::new(channel),
            visited: BTreeMap::new(),
            catalog: vec![],
            trees: BTreeMap::new(),
            tree_closed: BTreeSet::new(),
            tree_all: BTreeSet::new(),
            sidebar_cursor: 0,
            show_sidebar: true,
            hide_keys: true,
            focus: Focus::Input,
            tab_cursor: 0,
            meta_cursor: 0,
            prompts: BTreeMap::new(),
            questions: BTreeMap::new(),
            permission_cursor: BTreeMap::new(),
            prompt_busy: BTreeSet::new(),
            permission_drafts: BTreeMap::new(),
            mcp_open: BTreeSet::new(),
            active_prompt: String::new(),
            providers: vec![],
            roles: vec![],
            models: vec![],
            variants: vec![],
            custom_commands: vec![],
            plans: vec![],
            cache: Default::default(),
            discord: Default::default(),
            web: Default::default(),
            sandbox: Default::default(),
            usage: None,
            plan_series: None,
            popup: None,
            popup_epoch: 0,
            config: None,
            config_epoch: 0,
            login_id: String::new(),
            login_provider: String::new(),
            login_method: String::new(),
            login: Default::default(),
            discord_epoch: 0,
            web_epoch: 0,
            sandbox_epoch: 0,
            palette_cursor: 0,
            mention_cursor: 0,
            status: String::new(),
            status_error: false,
            status_until: Time::default(),
            now,
            quit_armed: None,
            cancel_armed: None,
            quit: false,
            tree_dirty: false,
            dirty: true,
            width: 100,
            height: 30,
        }
    }
    pub fn scoped<P: Serialize, R>(&self, method: Method<P, R>, params: P, tag: Tag) -> Effect {
        request(Some(self.scope.clone()), method, params, tag)
    }
    pub fn global<P: Serialize, R>(&self, method: Method<P, R>, params: P, tag: Tag) -> Effect {
        request(None, method, params, tag)
    }
    pub fn accepts(&self, scope: &Option<Scope>) -> bool {
        scope.as_ref().is_none_or(|scope| scope == &self.scope)
    }
    pub fn status(&mut self, text: impl Into<String>, error: bool) {
        self.status = text.into();
        self.status_error = error;
        self.status_until = Time(self.now.0 + chrono::TimeDelta::seconds(5));
        self.dirty = true;
    }
    pub fn close_popup(&mut self) -> Vec<Effect> {
        if let Some(popup) = self
            .popup
            .as_ref()
            .filter(|popup| popup.kind == Dialog::Permissions)
        {
            self.permission_cursor
                .insert(popup.context.clone(), popup.cursor);
        }
        self.popup = None;
        self.popup_epoch = self.popup_epoch.wrapping_add(1);
        self.focus = Focus::Input;
        self.dirty = true;
        if self.login_id.is_empty() {
            vec![]
        } else {
            let id = std::mem::take(&mut self.login_id);
            vec![Effect::Cancel(id)]
        }
    }
    pub fn popup(&mut self, kind: Dialog, title: impl Into<String>) -> &mut Popup {
        self.popup_epoch = self.popup_epoch.wrapping_add(1);
        self.popup = Some(Popup::new(kind, title, self.popup_epoch));
        self.dirty = true;
        self.popup.as_mut().unwrap()
    }
}
