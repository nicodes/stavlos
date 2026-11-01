use super::snapshot::Source;
use crate::{policy::Set, toolname};
use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use serde_yaml_ng::Value as Yaml;
use std::{
    collections::{BTreeMap, BTreeSet},
    path::{Path, PathBuf},
};

pub const ROLE_COLORS: &[&str] = &[
    "red", "blue", "green", "yellow", "purple", "orange", "pink", "cyan",
];
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ModelSpec {
    pub id: String,
    #[serde(default, deserialize_with = "yaml_strings")]
    pub variants: Vec<String>,
}
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Role {
    pub name: String,
    pub description: String,
    #[serde(rename = "type")]
    pub kind: String,
    pub read_only: bool,
    pub models: Vec<ModelSpec>,
    pub tools: Vec<String>,
    pub tool_rules: BTreeMap<String, BTreeMap<String, String>>,
    pub skills: Vec<String>,
    pub mcp: Vec<String>,
    pub spawn: Vec<String>,
    pub max_turns: i64,
    pub color: String,
    pub body: String,
    pub source: PathBuf,
    pub layer: String,
}
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Skill {
    pub name: String,
    pub description: String,
    pub body: String,
    pub dir: PathBuf,
}
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Command {
    pub name: String,
    pub description: String,
    pub body: String,
    pub source: PathBuf,
}

pub fn markdown_parts(content: &str) -> Result<(String, String)> {
    let normalized = content
        .strip_prefix('\u{feff}')
        .unwrap_or(content)
        .replace("\r\n", "\n");
    let Some(rest) = normalized.strip_prefix("---") else {
        return Ok((String::new(), normalized));
    };
    let rest = rest.trim_start_matches([' ', '\t', '\r']);
    let Some(rest) = rest.strip_prefix('\n') else {
        return Ok((String::new(), normalized));
    };
    let Some(end) = rest.find("\n---") else {
        bail!("unterminated frontmatter");
    };
    let body = &rest[end + 4..];
    let body = body.split_once('\n').map_or("", |(_, body)| body);
    Ok((rest[..end].to_owned(), body.to_owned()))
}
fn nullable<'de, D: serde::Deserializer<'de>, T: Deserialize<'de> + Default>(
    d: D,
) -> Result<T, D::Error> {
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}
fn present<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Option<Yaml>, D::Error> {
    Ok(Some(Yaml::deserialize(d)?))
}
#[derive(Default, Deserialize)]
#[serde(default)]
struct RoleFile {
    #[serde(deserialize_with = "nullable")]
    description: String,
    #[serde(rename = "type", deserialize_with = "nullable")]
    kind: String,
    #[serde(deserialize_with = "nullable")]
    read_only: bool,
    #[serde(deserialize_with = "present")]
    models: Option<Yaml>,
    #[serde(deserialize_with = "present")]
    tools: Option<Yaml>,
    #[serde(deserialize_with = "yaml_strings")]
    skills: Vec<String>,
    #[serde(deserialize_with = "yaml_strings")]
    mcp: Vec<String>,
    #[serde(deserialize_with = "yaml_strings")]
    spawn: Vec<String>,
    #[serde(deserialize_with = "nullable")]
    max_turns: i64,
    #[serde(deserialize_with = "nullable")]
    color: String,
    model: Option<String>,
    policy: Option<Yaml>,
    hidden: Option<bool>,
    mode: Option<String>,
    dirs: Option<Vec<String>>,
    r#loop: Option<String>,
}
fn scalar(value: &Yaml) -> Option<String> {
    match value {
        Yaml::String(s) => Some(s.clone()),
        Yaml::Bool(v) => Some(v.to_string()),
        Yaml::Number(v) => Some(v.to_string()),
        Yaml::Null => Some(String::new()),
        _ => None,
    }
}
fn yaml_strings<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Vec<String>, D::Error> {
    #[derive(Deserialize)]
    #[serde(transparent)]
    struct Text(#[serde(deserialize_with = "nullable")] String);
    Ok(Option::<Vec<Text>>::deserialize(d)?
        .unwrap_or_default()
        .into_iter()
        .map(|text| text.0)
        .collect())
}
fn parse_models(value: Option<Yaml>, meta: &str) -> Result<Vec<ModelSpec>> {
    let Some(value) = value else {
        return Ok(Vec::new());
    };
    let Yaml::Sequence(items) = value else {
        bail!("models must be a list of model ids or id/variants entries");
    };
    use std::str::FromStr;
    let document = yaml_edit::YamlFile::from_str(meta)?;
    let nodes = document
        .documents()
        .next()
        .and_then(|d| d.get("models"))
        .and_then(|n| n.as_sequence().cloned())
        .map(|s| s.values().collect::<Vec<_>>())
        .unwrap_or_default();
    let mut out = Vec::new();
    for (index, item) in items.into_iter().enumerate() {
        match item {
            Yaml::Mapping(mut mapping) => {
                let raw = nodes.get(index).and_then(|node| node.as_mapping());
                let text = |value: &Yaml, node: Option<yaml_edit::YamlNode>| -> Option<String> {
                    if value.is_null() {
                        Some(String::new())
                    } else {
                        node.as_ref()
                            .and_then(|node| node.as_scalar())
                            .map(|node| node.as_string())
                            .or_else(|| scalar(value))
                    }
                };
                if let Some(value) = mapping.get_mut(Yaml::String("id".into()))
                    && let Some(value_text) = text(value, raw.and_then(|map| map.get("id")))
                {
                    *value = Yaml::String(value_text);
                }
                if let Some(Yaml::Sequence(variants)) =
                    mapping.get_mut(Yaml::String("variants".into()))
                {
                    let raw_values = raw
                        .and_then(|map| map.get("variants"))
                        .and_then(|node| node.as_sequence().cloned())
                        .map(|sequence| sequence.values().collect::<Vec<_>>())
                        .unwrap_or_default();
                    for (index, value) in variants.iter_mut().enumerate() {
                        if let Some(value_text) = text(value, raw_values.get(index).cloned()) {
                            *value = Yaml::String(value_text);
                        }
                    }
                }
                let model: ModelSpec = serde_yaml_ng::from_value(Yaml::Mapping(mapping))?;
                if model.id.is_empty() {
                    bail!("a model entry needs an id");
                }
                out.push(model);
            }
            _ => {
                let id = if let Some(node) = nodes.get(index).and_then(|node| node.as_scalar()) {
                    node.as_string()
                } else {
                    scalar(&item).context("model entries must be ids or id/variants entries")?
                };
                out.push(ModelSpec {
                    id: id.trim().into(),
                    variants: Vec::new(),
                });
            }
        }
    }
    Ok(out)
}
fn kept_tool(name: &str) -> Option<&'static str> {
    match name {
        toolname::MESSAGE
        | toolname::AGENT
        | toolname::CHANNEL
        | toolname::AGENT_STATUS
        | toolname::ASK => Some("every agent has it; it cannot be removed"),
        toolname::AGENT_CREATE | toolname::AGENT_CANCEL => {
            Some("comes with spawn: leave spawn empty to remove it")
        }
        toolname::SHELL_KILL => Some("comes with shell: remove shell instead"),
        _ => None,
    }
}
fn checked_verb(value: &Yaml) -> Result<String> {
    let value = scalar(value).context("expected allow, ask or deny")?;
    if !matches!(value.as_str(), "allow" | "ask" | "deny") {
        bail!("expected allow, ask or deny");
    }
    Ok(value)
}
type ToolRules = BTreeMap<String, BTreeMap<String, String>>;
fn parse_tools(value: Option<Yaml>) -> Result<(Vec<String>, ToolRules)> {
    let mut removed = BTreeSet::new();
    let mut rules = ToolRules::new();
    if let Some(value) = value {
        let Yaml::Mapping(tools) = value else {
            bail!("tools must be a map of tool to verb or pattern rules");
        };
        for (name, value) in tools {
            let name = scalar(&name).context("tool name must be a scalar")?;
            let name = toolname::canonical(&name).to_owned();
            match &value {
                Yaml::Null => {}
                Yaml::String(s) if s.is_empty() => {}
                Yaml::Mapping(patterns) => {
                    let mut entries = BTreeMap::new();
                    for (pattern, value) in patterns {
                        entries.insert(
                            scalar(pattern).context("tool pattern must be a scalar")?,
                            checked_verb(value)?,
                        );
                    }
                    rules.insert(name, entries);
                }
                _ => {
                    let verb = checked_verb(&value)?;
                    if verb == "deny" && toolname::ROLE_TOOLS.contains(&name.as_str()) {
                        removed.insert(name);
                    } else {
                        if verb == "deny"
                            && let Some(reason) = kept_tool(&name)
                        {
                            bail!("{name}: {reason}");
                        }
                        rules.insert(name, BTreeMap::from([("*".into(), verb)]));
                    }
                }
            }
        }
    }
    Ok((
        toolname::ROLE_TOOLS
            .iter()
            .filter(|name| !removed.contains(**name))
            .map(|s| (*s).to_owned())
            .collect(),
        rules,
    ))
}
pub fn read_role(source: &dyn Source, path: &Path) -> Result<Role> {
    let bytes = source.read_file(path)?;
    let text = String::from_utf8_lossy(&bytes);
    let (meta, body) = markdown_parts(&text)?;
    let file: RoleFile = if meta.trim().is_empty() {
        RoleFile::default()
    } else {
        serde_yaml_ng::from_str(&meta).with_context(|| path.display().to_string())?
    };
    for (present, message) in [
        (
            file.model.is_some(),
            "model: is now models: (a list; the first entry is the default)",
        ),
        (file.policy.is_some(), "policy: is now written under tools:"),
        (file.hidden.is_some(), "hidden: is not supported; use type:"),
        (
            file.mode.is_some(),
            "mode: is now type: (primary, subagent or all)",
        ),
        (
            file.dirs.is_some(),
            "dirs: was removed: working directories belong to the channel",
        ),
        (
            file.r#loop.is_some(),
            "loop: was removed: there is one agent loop",
        ),
    ] {
        if present {
            bail!("{}: {message}", path.display());
        }
    }
    let description = file.description.trim().to_owned();
    if description.is_empty() {
        bail!("{}: description: is required", path.display());
    }
    if file.max_turns < 0 {
        bail!("max_turns: must be 0 or more");
    }
    let kind = if file.kind.is_empty() {
        "all".into()
    } else {
        file.kind
    };
    if !matches!(kind.as_str(), "primary" | "subagent" | "all") {
        bail!("type: must be primary, subagent or all");
    }
    if !file.color.is_empty() && !ROLE_COLORS.contains(&file.color.as_str()) {
        bail!("color: unknown role color");
    }
    let models = parse_models(file.models, &meta)?;
    let (tools, tool_rules) = parse_tools(file.tools)?;
    Ok(Role {
        name: markdown_name(path)?,
        description,
        kind,
        read_only: file.read_only,
        models,
        tools,
        tool_rules,
        skills: file.skills,
        mcp: file.mcp,
        spawn: file.spawn,
        max_turns: file.max_turns,
        color: file.color,
        body: body.trim().into(),
        source: path.to_owned(),
        layer: String::new(),
    })
}
fn markdown_name(path: &Path) -> Result<String> {
    let name = path
        .file_name()
        .context("Markdown definition needs a filename")?
        .to_string_lossy();
    Ok(name.strip_suffix(".md").unwrap_or(&name).to_owned())
}
pub fn skill(content: &str) -> Result<Skill> {
    #[derive(Default, Deserialize)]
    #[serde(default)]
    struct Meta {
        #[serde(deserialize_with = "nullable")]
        name: String,
        #[serde(deserialize_with = "nullable")]
        description: String,
    }
    let (meta, body) = markdown_parts(content)?;
    let metadata: Meta = if meta.trim().is_empty() {
        Meta::default()
    } else {
        serde_yaml_ng::from_str(&meta)?
    };
    Ok(Skill {
        name: metadata.name,
        description: metadata.description,
        body,
        dir: PathBuf::new(),
    })
}
pub fn read_command(source: &dyn Source, path: &Path) -> Result<Command> {
    let name = markdown_name(path)?;
    if !name.bytes().next().is_some_and(|b| b.is_ascii_lowercase())
        || !name
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b"_-".contains(&b))
    {
        bail!(
            "command name must use lowercase letters, digits, hyphens or underscores and start with a letter"
        );
    }
    let bytes = source.read_file(path)?;
    let (meta, body) = markdown_parts(&String::from_utf8_lossy(&bytes))?;
    let values: Yaml = serde_yaml_ng::from_str(&meta)?;
    let empty = serde_yaml_ng::Mapping::new();
    let values = if values.is_null() {
        &empty
    } else {
        values
            .as_mapping()
            .context("command frontmatter must be a mapping")?
    };
    for key in values.keys() {
        if key.as_str() != Some("description") {
            bail!("only description is supported in command frontmatter");
        }
    }
    let description = values
        .get(Yaml::String("description".into()))
        .and_then(Yaml::as_str)
        .filter(|s| !s.trim().is_empty())
        .context("command frontmatter needs a nonempty description")?;
    if body.trim().is_empty() {
        bail!("command needs a Markdown prompt body");
    }
    Ok(Command {
        name,
        description: description.split_whitespace().collect::<Vec<_>>().join(" "),
        body: body.trim_matches(['\r', '\n']).into(),
        source: path.to_owned(),
    })
}
impl Role {
    fn spec(&self, id: &str) -> Option<&ModelSpec> {
        self.models
            .iter()
            .find(|spec| spec.id == id || filename_match(&spec.id, id))
    }
    pub fn allows_model(&self, id: &str) -> bool {
        self.models.is_empty() || self.spec(id).is_some()
    }
    pub fn default_model(&self) -> &str {
        self.models
            .first()
            .filter(|m| !m.id.contains(['*', '?', '[']))
            .map_or("", |m| m.id.as_str())
    }
    pub fn allows_variant(&self, id: &str, variant: &str) -> bool {
        self.spec(id)
            .is_none_or(|m| m.variants.is_empty() || m.variants.iter().any(|v| v == variant))
    }
    pub fn variants(&self, id: &str) -> &[String] {
        self.spec(id).map_or(&[], |m| m.variants.as_slice())
    }
    pub fn default_variant(&self, id: &str) -> &str {
        self.variants(id).first().map_or("", String::as_str)
    }
    pub fn can_be_primary(&self) -> bool {
        self.kind != "subagent"
    }
    pub fn can_be_subagent(&self) -> bool {
        self.kind != "primary"
    }
    pub fn policy(&self) -> Result<Set> {
        super::effective::parse_policy(
            &self
                .tool_rules
                .iter()
                .map(|(name, rules)| {
                    (
                        name.clone(),
                        serde_json::to_value(rules).expect("serializable rules"),
                    )
                })
                .collect(),
        )
    }
}
#[derive(Clone)]
enum Token {
    Star,
    Any,
    Literal(char),
    Class(bool, Vec<(char, char)>),
}
/// Go filepath.Match model-whitelist syntax: slashes constrain * and ?, but not explicit classes.
pub fn filename_match(pattern: &str, value: &str) -> bool {
    fn escaped(chars: &[char], i: &mut usize) -> Option<char> {
        let ch = *chars.get(*i)?;
        if matches!(ch, '-' | ']') {
            return None;
        }
        *i += 1;
        let ch = if ch == '\\' {
            let next = *chars.get(*i)?;
            *i += 1;
            next
        } else {
            ch
        };
        if *i == chars.len() { None } else { Some(ch) }
    }
    let chars: Vec<_> = pattern.chars().collect();
    let mut i = 0;
    let mut tokens = Vec::new();
    while i < chars.len() {
        match chars[i] {
            '*' => tokens.push(Token::Star),
            '?' => tokens.push(Token::Any),
            '\\' => {
                i += 1;
                let Some(ch) = chars.get(i) else {
                    return false;
                };
                tokens.push(Token::Literal(*ch));
            }
            '[' => {
                i += 1;
                let negated = chars.get(i) == Some(&'^');
                if negated {
                    i += 1;
                }
                let mut ranges = Vec::new();
                loop {
                    if chars.get(i) == Some(&']') && !ranges.is_empty() {
                        break;
                    }
                    let Some(lo) = escaped(&chars, &mut i) else {
                        return false;
                    };
                    let mut hi = lo;
                    if chars.get(i) == Some(&'-') {
                        i += 1;
                        let Some(ch) = escaped(&chars, &mut i) else {
                            return false;
                        };
                        hi = ch;
                    }
                    ranges.push((lo, hi));
                }
                tokens.push(Token::Class(negated, ranges));
            }
            ch => tokens.push(Token::Literal(ch)),
        }
        i += 1;
    }
    let text: Vec<_> = value.chars().collect();
    let mut positions = vec![false; text.len() + 1];
    positions[0] = true;
    for token in tokens {
        let mut next = vec![false; text.len() + 1];
        match token {
            Token::Star => {
                let mut active = false;
                for at in 0..=text.len() {
                    active |= positions[at];
                    next[at] = active;
                    if text.get(at) == Some(&'/') {
                        active = false;
                    }
                }
            }
            token => {
                for at in 0..text.len() {
                    if positions[at] {
                        next[at + 1] = match &token {
                            Token::Any => text[at] != '/',
                            Token::Literal(ch) => text[at] == *ch,
                            Token::Class(negated, ranges) => {
                                ranges
                                    .iter()
                                    .any(|(lo, hi)| *lo <= text[at] && text[at] <= *hi)
                                    != *negated
                            }
                            Token::Star => unreachable!(),
                        };
                    }
                }
            }
        }
        positions = next;
    }
    positions[text.len()]
}
pub fn builtin_general() -> Role {
    Role {
        name: "general".into(),
        description: "General-purpose engineer: reads, edits, runs, and delegates".into(),
        kind: "all".into(),
        tools: toolname::ROLE_TOOLS.iter().map(|s| (*s).into()).collect(),
        spawn: vec!["general".into()],
        body: include_str!("data/builtin-general.txt").into(),
        layer: "builtin".into(),
        ..Role::default()
    }
}
