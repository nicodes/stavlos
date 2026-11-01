use crate::wire::{Slice, Time};
use serde::{Deserialize, Serialize};
macro_rules! vocabulary {
    ($name:ident,$default:ident,$($variant:ident=$value:literal),*$(,)?)=>{
        #[derive(Clone,Copy,Debug,PartialEq,Eq)]
        #[repr(u8)]
        pub enum $name {$($variant=$value),*}
        impl Default for $name {fn default()->Self {Self::$default}}
        impl Serialize for $name {fn serialize<S:serde::Serializer>(&self,serializer:S)->Result<S::Ok,S::Error> {serializer.serialize_u8(*self as u8)}}
        impl<'de> Deserialize<'de> for $name {fn deserialize<D:serde::Deserializer<'de>>(deserializer:D)->Result<Self,D::Error> {match u8::deserialize(deserializer)? {$($value=>Ok(Self::$variant),)*_=>Err(serde::de::Error::custom("unknown terminal line vocabulary"))}}}
    };
}
vocabulary!(
    Kind,
    Text,
    Text = 0,
    Heading = 1,
    Code = 2,
    Dim = 3,
    Tool = 4,
    ToolOut = 5,
    Notice = 6,
    Label = 7,
    Finished = 8,
    Rule = 9,
    Error = 10,
    Stream = 11,
    Model = 12,
    Think = 13,
    ToolNote = 14,
    Blank = 15
);
vocabulary!(
    Block,
    None,
    None = 0,
    User = 1,
    Steer = 2,
    Child = 3,
    Error = 4,
    Finished = 5
);
vocabulary!(Visibility, Always, Always = 0, Collapsed = 1, Expanded = 2);
vocabulary!(Tone, None, None = 0, Working = 1, Error = 2);
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "PascalCase")]
pub struct Line {
    pub kind: Kind,
    pub text: String,
    pub block: Block,
    pub vis: Visibility,
    pub running: bool,
    #[serde(rename = "Err")]
    pub error: bool,
    pub suffix: String,
    pub item: i64,
    pub lead: bool,
    pub glyph: String,
    pub tone: Tone,
    #[serde(skip)]
    pub(crate) call_id: String,
    pub tool: String,
    pub note: bool,
    pub agent: String,
    pub who: String,
    pub names: Slice<String>,
    pub diff: u8,
    pub indent: i64,
    pub turn_start: bool,
    pub at: Time,
}
impl Line {
    pub fn new(kind: Kind, text: impl Into<String>) -> Self {
        Self {
            kind,
            text: text.into(),
            ..Default::default()
        }
    }
    pub fn call_id(&self) -> &str {
        &self.call_id
    }
}
pub mod glyph {
    pub const CHILD: &str = "⑂";
    pub const REPLY: &str = "‹";
    pub const ASK: &str = "›";
    pub const SPAWN: &str = "⋙";
    pub const INFO: &str = "»";
    pub const INFO_SENT: &str = "«";
    pub const TASK: &str = "▹";
    pub const FINISHED: &str = "✓";
    pub const ERROR: &str = "!";
    pub const KILLED: &str = "⊘";
    pub const TURN: &str = "◦";
    pub const NUDGE: &str = "↻";
    pub const ASIDE: &str = "§";
    pub const MODEL: &str = "⇄";
    pub const NOTICE: &str = "»";
    pub const PROMPT: &str = "?";
    pub const ANSWER: &str = "?";
    pub const PERMISSION: &str = "!";
    pub const FAILED: &str = "✗";
    pub const COMPACTED: &str = "┄┄ compacted ┄┄";
    pub const COMPACTING: &str = "┄┄ compacting ┄┄";
    pub const FILES: &str = "◆";
    pub const READ: &str = "▤";
    pub const SEARCH: &str = "⌕";
    pub const PATCH: &str = "±";
    pub const SHELL: &str = "$";
    pub const JOB: &str = "$";
    pub const AGENTS: &str = "⑂";
    pub const CREATE: &str = "⋙";
    pub const TODO: &str = "☐";
    pub const MCP: &str = "≡";
    pub const WEB: &str = "↓";
}
pub const OUTPUT_COLLAPSED: usize = 3;
pub const OUTPUT_EXPANDED: usize = 40;
pub const DIFF_EXPANDED: usize = 200;
pub fn clean(mut lines: Vec<Line>) -> Vec<Line> {
    for line in &mut lines {
        line.text = crate::textsafe::clean(&line.text);
        line.suffix = crate::textsafe::clean(&line.suffix);
    }
    lines
}
pub fn linked(mut lines: Vec<Line>, agent: &str) -> Vec<Line> {
    for line in &mut lines {
        line.agent = agent.into();
    }
    lines
}
pub fn stamped(mut lines: Vec<Line>, created: &str) -> Vec<Line> {
    if let Ok(at) = chrono::DateTime::parse_from_rfc3339(created) {
        for line in &mut lines {
            line.at = Time(at);
        }
    }
    lines
}
