use super::{
    frontmatter::{Command, Role, Skill},
    snapshot::{Disk, Snapshot, Source},
    types::*,
};
use crate::{
    instructions,
    paths::Paths,
    policy::{Layered, Rule, Set, Subject, Verb},
    toolname,
};
use anyhow::{Context, Result, bail};
use regex::Regex;
use serde_json::Value;
use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::OnceLock,
};

#[derive(Clone, Default)]
pub struct EscalationState {
    pub claim_timeout_ns: i64,
    pub answer_timeout_ns: i64,
    pub default: Verb,
}
#[derive(Clone, Default)]
pub struct CompactionState {
    pub threshold: f64,
    pub max_tokens: i64,
    pub clear_tokens: i64,
    pub keep_tokens: i64,
    pub max_tool_output: usize,
}
#[derive(Clone, Default)]
pub struct SandboxState {
    pub enabled: bool,
    pub network: bool,
    pub readable: Vec<PathBuf>,
    pub writable: Vec<PathBuf>,
    pub hide: Vec<PathBuf>,
    pub git_metadata: Vec<PathBuf>,
}
#[derive(Clone)]
pub struct Effective {
    pub dir: PathBuf,
    pub model: String,
    pub models: Vec<String>,
    pub root_agent: String,
    pub mode: String,
    pub limits: Limits,
    pub escalation: EscalationState,
    pub compaction: CompactionState,
    pub mcp: BTreeMap<String, Mcp>,
    pub search: Search,
    pub discord: Option<Discord>,
    pub web: Option<Web>,
    pub pass_env: Vec<String>,
    pub policy: Layered,
    pub roles: BTreeMap<String, Role>,
    pub skills: BTreeMap<String, Skill>,
    pub commands: BTreeMap<String, Command>,
    pub instructions: Vec<instructions::File>,
    pub project_trusted: bool,
    pub instruction_files: Vec<PathBuf>,
    pub plugins: Vec<String>,
    pub reminders: bool,
    pub resume_after_limit: bool,
    pub sandbox: SandboxState,
    pub dirs: Vec<PathBuf>,
    pub hosts: Vec<String>,
    pub trust_pending: bool,
    pub trust_hash: String,
    pub trust_files: Vec<String>,
    search_ruled: bool,
}
impl Default for Effective {
    fn default() -> Self {
        Self {
            dir: PathBuf::new(),
            model: String::new(),
            models: Vec::new(),
            root_agent: String::new(),
            mode: String::new(),
            limits: Limits::default(),
            escalation: EscalationState::default(),
            compaction: CompactionState::default(),
            mcp: BTreeMap::new(),
            search: Search::default(),
            discord: None,
            web: None,
            pass_env: Vec::new(),
            policy: Layered::new(Set::default(), []),
            roles: BTreeMap::new(),
            skills: BTreeMap::new(),
            commands: BTreeMap::new(),
            instructions: Vec::new(),
            project_trusted: false,
            instruction_files: Vec::new(),
            plugins: Vec::new(),
            reminders: false,
            resume_after_limit: false,
            sandbox: SandboxState::default(),
            dirs: Vec::new(),
            hosts: Vec::new(),
            trust_pending: false,
            trust_hash: String::new(),
            trust_files: Vec::new(),
            search_ruled: false,
        }
    }
}
pub type EnvLookup<'a> = &'a dyn Fn(&str) -> Option<String>;
pub type TrustLookup<'a> = &'a dyn Fn(&Path, &str) -> bool;
fn env_reference() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}").unwrap())
}
pub fn expand_env(value: &str, lookup: EnvLookup<'_>) -> String {
    env_reference()
        .replace_all(value, |capture: &regex::Captures<'_>| {
            lookup(&capture[1]).unwrap_or_default()
        })
        .into_owned()
}
pub fn expand_path(value: &str, home: &Path, lookup: EnvLookup<'_>) -> PathBuf {
    let expanded = expand_env(value.trim(), lookup);
    let path = if !home.as_os_str().is_empty() && expanded == "~" {
        home.to_owned()
    } else if !home.as_os_str().is_empty() && expanded.starts_with("~/") {
        home.join(&expanded[2..])
    } else {
        PathBuf::from(expanded)
    };
    crate::pathx::clean(&path)
}
fn lower_go(value: &str) -> String {
    value
        .chars()
        .map(|c| c.to_lowercase().next().unwrap_or(c))
        .collect()
}
pub fn working_directory(
    base: &Path,
    value: &str,
    home: &Path,
    lookup: EnvLookup<'_>,
) -> Result<PathBuf> {
    if value.trim().is_empty() {
        bail!("a default directory is required");
    }
    let path = expand_path(value, home, lookup);
    let path = if path.is_absolute() {
        path
    } else {
        if !base.is_absolute() {
            bail!("an absolute default directory is required");
        }
        base.join(path)
    };
    let resolved = path
        .canonicalize()
        .context("default directory unavailable")?;
    if !resolved.is_dir() {
        bail!(
            "default directory is not a directory: {}",
            resolved.display()
        );
    }
    Ok(resolved)
}
pub fn defaults() -> File {
    let mut policy = BTreeMap::new();
    for tool in [
        toolname::READ,
        toolname::GREP,
        toolname::GLOB,
        toolname::SKILL,
        toolname::AGENT,
        toolname::MESSAGE,
        toolname::CHANNEL,
        toolname::SHELL_KILL,
        toolname::TODO,
        toolname::ASK,
    ] {
        policy.insert(tool.into(), Value::String("allow".into()));
    }
    for tool in [
        toolname::SHEET,
        toolname::SHELL,
        toolname::PATCH,
        toolname::FETCH,
        toolname::SEARCH,
        toolname::OPERATOR,
    ] {
        policy.insert(tool.into(), Value::String("ask".into()));
    }
    File {
        root_agent: "general".into(),
        mode: "ask".into(),
        limits: Some(Limits {
            max_depth: 3,
            max_agents: 6,
            max_calls_per_turn: 200,
            max_channel_calls: 400,
            ..Limits::default()
        }),
        escalation: Some(Escalation {
            claim_timeout: "30s".into(),
            answer_timeout: "3m".into(),
            default: "deny".into(),
        }),
        compaction: Some(Compaction {
            threshold: 0.9,
            max_tokens: 150_000,
            clear_tokens: 40_000,
            keep_tokens: 15_000,
            max_tool_output: "50kb".into(),
        }),
        reminders: Some(true),
        resume_after_limit: Some(true),
        sandbox: Some(SandboxConfig {
            enabled: Some(true),
            network: Some(true),
            ..SandboxConfig::default()
        }),
        policy,
        ..File::default()
    }
}
fn verb(value: &Value) -> Result<Verb> {
    serde_json::from_value(value.clone())
        .map_err(|_| anyhow::anyhow!("want a verb: allow, ask or deny"))
}
pub fn parse_policy(input: &BTreeMap<String, Value>) -> Result<Set> {
    let mut rules = Vec::new();
    for (tool, value) in input {
        let mut add = |pattern: &str, value: &Value| -> Result<()> {
            let (tool, pattern) = toolname::policy_rule(tool, pattern);
            rules.push(Rule {
                tool,
                pattern,
                verb: verb(value)?,
            });
            Ok(())
        };
        match value {
            Value::String(_) => add("*", value).with_context(|| format!("policy.{tool}"))?,
            Value::Object(patterns) => {
                for (pattern, value) in patterns {
                    add(pattern, value).with_context(|| format!("policy.{tool}.{pattern}"))?;
                }
            }
            _ => bail!("policy.{tool}: want a verb or a pattern-to-verb object"),
        }
    }
    Ok(Set::new(rules))
}
fn search_rules(policy: &BTreeMap<String, Value>) -> bool {
    let named = |name: &str| policy.get(name).is_some_and(|v| !v.is_null());
    named(toolname::SEARCH)
        || named("web_search")
        || match policy.get(toolname::WEB) {
            Some(Value::String(_)) => true,
            Some(Value::Object(patterns)) => patterns.keys().any(|p| !p.starts_with("fetch ")),
            _ => false,
        }
}
pub fn read_file(source: &dyn Source, path: &Path) -> Result<File> {
    let bytes = match source.read_file(path) {
        Ok(bytes) => bytes,
        Err(e)
            if e.downcast_ref::<std::io::Error>()
                .is_some_and(|e| e.kind() == std::io::ErrorKind::NotFound) =>
        {
            return Ok(File::default());
        }
        Err(e) => return Err(e),
    };
    let stripped = super::jsonc::strip(&bytes);
    let mut decoder = serde_json::Deserializer::from_slice(&stripped)
        .into_iter::<Box<serde_json::value::RawValue>>();
    let value = decoder.next().context("configuration is empty")??;
    super::types::decode(value.get().as_bytes()).with_context(|| path.display().to_string())
}
pub struct Loader<'a> {
    pub paths: &'a Paths,
    pub home: &'a Path,
    pub lookup: EnvLookup<'a>,
}
impl Loader<'_> {
    pub fn global(&self) -> Result<Effective> {
        let mut effective = Effective::default();
        effective.apply(&defaults(), "defaults", self)?;
        let general = super::frontmatter::builtin_general();
        effective.roles.insert(general.name.clone(), general);
        effective
            .apply(
                &read_file(&Disk, &self.paths.config.join("stavlos.json"))?,
                "global",
                self,
            )
            .context("global config")?;
        effective.allow_search();
        effective.instructions = instructions::global(&self.paths.config);
        effective.load_roles(&Disk, &self.paths.config.join("agents"), "global")?;
        effective.load_skills(&Disk, &self.paths.shared_skills)?;
        effective.load_skills(&Disk, &self.paths.config.join("skills"))?;
        effective.load_commands(&Disk, &self.paths.config.join("commands"))?;
        Ok(effective)
    }
    pub fn load(&self, dir: &Path, trust: Option<TrustLookup<'_>>) -> Result<Effective> {
        let dir = crate::pathx::absolute(dir)?;
        let mut effective = self.global()?;
        effective.dir = dir.clone();
        let snapshot = Snapshot::take(&dir, self.home)?;
        if snapshot.files.is_empty() {
            return Ok(effective);
        }
        effective.trust_hash = snapshot.hash.clone();
        effective.trust_files = snapshot.files.clone();
        effective.instruction_files = snapshot
            .files
            .iter()
            .filter(|file| {
                !file.starts_with(".stavlos")
                    && Path::new(file)
                        .file_name()
                        .is_some_and(|n| instructions::NAMES.iter().any(|name| n == *name))
            })
            .map(|file| crate::pathx::clean(&dir.join(file)))
            .collect();
        if trust.is_some_and(|trust| trust(&dir, &snapshot.hash)) {
            let project = Paths::project(&dir);
            for (name, layer) in [("stavlos.json", "project"), ("stavlos.local.json", "local")] {
                effective
                    .apply(&read_file(&snapshot, &project.join(name))?, layer, self)
                    .with_context(|| format!("{layer} config"))?;
            }
            effective.allow_search();
            effective.load_roles(&snapshot, &project.join("agents"), "project")?;
            effective.load_skills(&snapshot, &Paths::project_skills(&dir))?;
            effective.load_skills(&snapshot, &project.join("skills"))?;
            effective.load_commands(&snapshot, &project.join("commands"))?;
            effective.project_trusted = true;
            effective.instructions.extend(snapshot.chain);
        } else {
            effective.trust_pending = true;
        }
        Ok(effective)
    }
}
impl Effective {
    pub(super) fn allow_search(&mut self) {
        if !self.search.provider.is_empty() && !self.search_ruled {
            self.policy.base = self.policy.base.merge(&Set::new([Rule {
                tool: toolname::WEB.into(),
                pattern: "search *".into(),
                verb: Verb::Allow,
            }]));
        }
    }
    pub fn apply(&mut self, file: &File, layer: &str, loader: &Loader<'_>) -> Result<()> {
        if file.discord.is_some() {
            if layer != "global" {
                bail!("discord is global-only");
            }
            self.discord = file.discord.clone();
        }
        if let Some(web) = &file.web {
            if layer != "global" {
                bail!("web is global-only");
            }
            if !(0..=65535).contains(&web.port) {
                bail!("web.port is not a port");
            }
            self.web = Some(web.clone());
        }
        if layer != "defaults" && search_rules(&file.policy) {
            self.search_ruled = true;
        }
        for plugin in &file.plugins {
            if !self.plugins.contains(plugin) {
                self.plugins.push(plugin.clone());
            }
        }
        if let Some(models) = &file.models {
            for id in models {
                if !id
                    .split_once('/')
                    .is_some_and(|(p, m)| !p.is_empty() && !m.is_empty())
                {
                    bail!("models: model must be provider/model-id");
                }
            }
            self.models = models.clone();
        }
        if !file.model.is_empty() {
            self.model = file.model.clone();
        }
        if !file.root_agent.is_empty() {
            self.root_agent = file.root_agent.clone();
        }
        if !file.mode.is_empty() {
            if !matches!(file.mode.as_str(), "ask" | "auto" | "yolo") {
                bail!("mode: ask, auto or yolo");
            }
            self.mode = file.mode.clone();
        }
        if let Some(enabled) = file.reminders {
            self.reminders = enabled;
        }
        if let Some(enabled) = file.resume_after_limit {
            self.resume_after_limit = enabled;
        }
        let expand = |p: &String| expand_path(p, loader.home, loader.lookup);
        if let Some(sandbox) = &file.sandbox {
            if let Some(enabled) = sandbox.enabled {
                self.sandbox.enabled = enabled;
            }
            if let Some(network) = sandbox.network {
                self.sandbox.network = network;
            }
            self.sandbox
                .readable
                .extend(sandbox.readable.iter().map(expand));
            self.sandbox
                .writable
                .extend(sandbox.writable.iter().map(expand));
            self.sandbox.hide.extend(sandbox.hide.iter().map(expand));
            self.sandbox
                .git_metadata
                .extend(sandbox.git_metadata.iter().map(expand));
        }
        self.dirs.extend(file.dirs.iter().map(expand));
        for host in &file.hosts {
            let host = lower_go(host.trim());
            let stripped = host.strip_prefix("*.").unwrap_or(&host);
            if host != "*"
                && (stripped.is_empty() || stripped.contains(['/', ':', '*', '@', '?', '#', ' ']))
            {
                bail!("hosts: expected a host name, *.example.com or *");
            }
            self.hosts.push(host);
        }
        if let Some(limits) = &file.limits {
            self.apply_limits(limits)?;
        }
        if let Some(escalation) = &file.escalation {
            for (value, target) in [
                (
                    &escalation.claim_timeout,
                    &mut self.escalation.claim_timeout_ns,
                ),
                (
                    &escalation.answer_timeout,
                    &mut self.escalation.answer_timeout_ns,
                ),
            ] {
                if !value.is_empty() {
                    let duration = crate::duration::parse(value)?;
                    if duration <= 0 {
                        bail!("escalation timeout must be positive");
                    }
                    *target = duration;
                }
            }
            if !escalation.default.is_empty() {
                self.escalation.default = match escalation.default.as_str() {
                    "allow" => Verb::Allow,
                    "deny" => Verb::Deny,
                    _ => bail!("escalation.default: allow or deny"),
                };
            }
        }
        if let Some(compaction) = &file.compaction {
            self.apply_compaction(compaction)?;
        }
        if let Some(search) = &file.search {
            if !search.provider.is_empty() {
                let provider = lower_go(&search.provider);
                if !matches!(provider.as_str(), "brave" | "tavily" | "exa") {
                    bail!("search.provider: brave, tavily or exa");
                }
                self.search.provider = provider;
            }
            if !search.api_key.is_empty() {
                if !search.api_key.contains("${env:") {
                    eprintln!(
                        "stavlos: search.apiKey is written into the {layer} config; prefer an environment reference so the key stays in the environment"
                    );
                }
                self.search.api_key = expand_env(&search.api_key, loader.lookup);
            }
        }
        self.mcp.extend(file.mcp.clone());
        if let Some(env) = &file.env {
            for name in &env.pass {
                if !self.pass_env.contains(name) {
                    self.pass_env.push(name.clone());
                }
            }
        }
        self.policy.base = self.policy.base.merge(&parse_policy(&file.policy)?);
        Ok(())
    }
    pub fn apply_limits(&mut self, value: &Limits) -> Result<()> {
        if value.max_depth < 0 || value.max_agents < 0 {
            bail!("limits: maxDepth and maxAgents must be positive");
        }
        if value.max_depth > 0 {
            self.limits.max_depth = value.max_depth;
        }
        if value.max_agents > 0 {
            self.limits.max_agents = value.max_agents;
        }
        if value.max_calls_per_turn != 0 {
            if value.max_calls_per_turn < 10 && value.max_calls_per_turn != -1 {
                bail!("limits.maxCallsPerTurn: at least 10, or -1 for no cap");
            }
            self.limits.max_calls_per_turn = value.max_calls_per_turn.max(0);
        }
        if value.max_channel_calls < -1
            || value.max_channel_tokens < -1
            || value.max_channel_cost_usd < 0. && value.max_channel_cost_usd != -1.
        {
            bail!("channel budgets: positive values, or -1 to disable");
        }
        if value.max_channel_calls != 0 {
            self.limits.max_channel_calls = value.max_channel_calls.max(0);
        }
        if value.max_channel_tokens != 0 {
            self.limits.max_channel_tokens = value.max_channel_tokens.max(0);
        }
        if value.max_channel_cost_usd != 0. {
            self.limits.max_channel_cost_usd = value.max_channel_cost_usd.max(0.);
        }
        if !value.max_channel_duration.is_empty() {
            if crate::duration::parse(&value.max_channel_duration)? < 0 {
                bail!("limits.maxChannelDuration must be a nonnegative duration");
            }
            self.limits.max_channel_duration = value.max_channel_duration.clone();
        }
        Ok(())
    }
    fn apply_compaction(&mut self, value: &Compaction) -> Result<()> {
        if value.threshold != 0. {
            if value.threshold <= 0. || value.threshold > 1. {
                bail!("compaction.threshold must be a fraction between 0 and 1");
            }
            self.compaction.threshold = value.threshold;
        }
        for (name, value, target) in [
            (
                "maxTokens",
                value.max_tokens,
                &mut self.compaction.max_tokens,
            ),
            (
                "clearTokens",
                value.clear_tokens,
                &mut self.compaction.clear_tokens,
            ),
        ] {
            if value != 0 {
                if value < 1000 && value != -1 {
                    bail!("compaction.{name}: at least 1000, or -1");
                }
                *target = value.max(0);
            }
        }
        if value.keep_tokens != 0 {
            if value.keep_tokens < 1000 {
                bail!("compaction.keepTokens: at least 1000");
            }
            self.compaction.keep_tokens = value.keep_tokens;
        }
        if !value.max_tool_output.is_empty() {
            self.compaction.max_tool_output = parse_size(&value.max_tool_output)?;
        }
        Ok(())
    }
    pub(super) fn load_roles(
        &mut self,
        source: &dyn Source,
        dir: &Path,
        layer: &str,
    ) -> Result<()> {
        for entry in optional_entries(source, dir)? {
            if entry.directory || !entry.name.ends_with(".md") {
                continue;
            }
            let mut role = super::frontmatter::read_role(source, &dir.join(entry.name))?;
            role.layer = layer.into();
            for rule in role.policy()?.rules() {
                let sample = rule.pattern.replace(['*', '?'], "x");
                let (base, _) = self.policy.decide(&rule.tool, &Subject::text(sample));
                if rule.verb < base {
                    bail!(
                        "{}: tools.{} loosens the policy; roles may only tighten",
                        role.source.display(),
                        rule.tool
                    );
                }
            }
            self.roles.insert(role.name.clone(), role);
        }
        Ok(())
    }
    pub(super) fn load_skills(&mut self, source: &dyn Source, dir: &Path) -> Result<()> {
        for entry in optional_entries(source, dir)? {
            if !entry.directory {
                continue;
            }
            let path = dir.join(&entry.name).join("SKILL.md");
            let Ok(bytes) = source.read_file(&path) else {
                continue;
            };
            let mut skill = super::frontmatter::skill(&String::from_utf8_lossy(&bytes))?;
            if skill.name.is_empty() {
                skill.name = entry.name.clone();
            }
            skill.dir = dir.join(entry.name);
            self.skills.insert(skill.name.clone(), skill);
        }
        Ok(())
    }
    pub(super) fn load_commands(&mut self, source: &dyn Source, dir: &Path) -> Result<()> {
        for entry in optional_entries(source, dir)? {
            if entry.directory || !entry.name.ends_with(".md") {
                continue;
            }
            let command = super::frontmatter::read_command(source, &dir.join(entry.name))?;
            self.commands.insert(command.name.clone(), command);
        }
        Ok(())
    }
}
fn optional_entries(source: &dyn Source, path: &Path) -> Result<Vec<super::snapshot::Entry>> {
    match source.read_dir(path) {
        Ok(entries) => Ok(entries),
        Err(error)
            if error
                .downcast_ref::<std::io::Error>()
                .is_some_and(|e| e.kind() == std::io::ErrorKind::NotFound) =>
        {
            Ok(Vec::new())
        }
        Err(error) => Err(error),
    }
}
pub fn parse_size(input: &str) -> Result<usize> {
    let lower = lower_go(input.trim());
    let (number, multiplier) = if let Some(s) = lower.strip_suffix("kb") {
        (s, 1024usize)
    } else if let Some(s) = lower.strip_suffix("mb") {
        (s, 1024 * 1024)
    } else {
        (lower.strip_suffix('b').unwrap_or(&lower), 1)
    };
    let number = number
        .trim()
        .parse::<usize>()
        .context("want a size such as 32kb or 1mb")?;
    if number == 0 {
        bail!("size must be positive");
    }
    number.checked_mul(multiplier).context("size overflow")
}
impl Discord {
    pub fn resolve(&self, home: &Path, lookup: EnvLookup<'_>) -> Result<Self> {
        let mut result = self.clone();
        result.token = result.token.trim().into();
        if result.token.contains("${") {
            if env_reference()
                .find(&result.token)
                .is_none_or(|m| m.as_str() != result.token)
            {
                bail!("discord.token must be a literal token or a complete environment reference");
            }
            result.token = expand_env(&result.token, lookup).trim().into();
        }
        if result.token.is_empty()
            || result.guild.is_empty()
            || result.category.is_empty()
            || result.approvers.is_empty()
            || result.dirs.is_empty()
        {
            bail!("discord requires a nonempty token, guild, category, approvers and dirs");
        }
        for id in std::iter::once(&result.guild).chain(&result.approvers) {
            if id.is_empty() || !id.bytes().all(|b| b.is_ascii_digit()) {
                bail!("discord IDs must be numeric");
            }
        }
        for directory in &mut result.dirs {
            if directory.is_empty() {
                bail!("discord.dirs contains an empty path");
            }
            let path = expand_path(directory, home, lookup);
            if !path.is_absolute() {
                bail!("discord directory must be absolute");
            }
            let path = path.canonicalize()?;
            if !path.is_dir() {
                bail!("discord directory is not a directory");
            }
            *directory = path.to_string_lossy().into_owned();
        }
        Ok(result)
    }
}
