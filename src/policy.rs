//! Declarative rules with explicit, tightening-only project and role overlays.
use regex::Regex;
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Verb {
    Allow,
    #[default]
    Ask,
    Deny,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    Text,
    Command,
    Path,
    Url,
    Id,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Subject {
    pub kind: Kind,
    pub values: Vec<String>,
}
impl Subject {
    pub fn new(kind: Kind, values: impl IntoIterator<Item = String>) -> Self {
        Self {
            kind,
            values: values.into_iter().collect(),
        }
    }
    pub fn text(value: impl Into<String>) -> Self {
        Self::new(Kind::Text, [value.into()])
    }
    pub fn command(value: impl Into<String>) -> Self {
        Self::new(Kind::Command, [value.into()])
    }
    pub fn paths(values: impl IntoIterator<Item = String>) -> Self {
        Self::new(Kind::Path, values)
    }
    pub fn url(value: impl Into<String>) -> Self {
        Self::new(Kind::Url, [value.into()])
    }
    pub fn id(value: impl Into<String>) -> Self {
        Self::new(Kind::Id, [value.into()])
    }
    pub fn primary(&self) -> &str {
        self.values.first().map_or("", String::as_str)
    }
    fn texts(&self) -> Vec<String> {
        let mut out = self.values.clone();
        if self.kind == Kind::Command {
            for value in &self.values {
                out.extend(
                    crate::shellcmd::commands(value)
                        .into_iter()
                        .filter(|c| c != value),
                );
            }
        }
        if out.is_empty() {
            out.push(String::new());
        }
        out
    }
}
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Rule {
    pub tool: String,
    pub pattern: String,
    pub verb: Verb,
}
#[derive(Clone, Debug)]
struct Compiled {
    rule: Rule,
    tool: Regex,
    flat: Regex,
    path: Regex,
    literal: usize,
    tool_literal: usize,
}
#[derive(Clone, Debug, Default)]
pub struct Set {
    rules: Vec<Compiled>,
}

/// Compile every non-wildcard character literally. All input patterns are valid.
pub fn glob(pattern: &str, path: bool) -> Regex {
    let mut pattern_chars = pattern.chars().peekable();
    let mut expression = String::from("(?s)\\A");
    while let Some(ch) = pattern_chars.next() {
        match ch {
            '*' if pattern_chars.peek() == Some(&'*') => {
                pattern_chars.next();
                expression.push_str(".*");
            }
            '*' => expression.push_str(if path { "[^/]*" } else { ".*" }),
            '?' => expression.push_str(if path { "[^/]" } else { "." }),
            _ => expression.push_str(&regex::escape(&ch.to_string())),
        }
    }
    expression.push_str("\\z");
    Regex::new(&expression).expect("quoted glob is a valid regular expression")
}
fn literal_prefix(pattern: &str) -> usize {
    pattern.find(['*', '?']).unwrap_or(pattern.len())
}
impl Set {
    pub fn new(rules: impl IntoIterator<Item = Rule>) -> Self {
        Self {
            rules: rules
                .into_iter()
                .map(|rule| Compiled {
                    tool: glob(&rule.tool, false),
                    flat: glob(&rule.pattern, false),
                    path: glob(&rule.pattern, rule.pattern != "*"),
                    literal: literal_prefix(&rule.pattern),
                    tool_literal: literal_prefix(&rule.tool),
                    rule,
                })
                .collect(),
        }
    }
    pub fn rules(&self) -> Vec<Rule> {
        self.rules.iter().map(|r| r.rule.clone()).collect()
    }
    pub fn sorted(&self) -> Vec<Rule> {
        let mut rules = self.rules();
        rules.sort_by(|a, b| (&a.tool, &a.pattern).cmp(&(&b.tool, &b.pattern)));
        rules
    }
    pub fn merge(&self, other: &Self) -> Self {
        let mut merged = self.clone();
        for rule in &other.rules {
            if let Some(index) = merged
                .rules
                .iter()
                .position(|r| r.rule.tool == rule.rule.tool && r.rule.pattern == rule.rule.pattern)
            {
                merged.rules[index] = rule.clone();
            } else {
                merged.rules.push(rule.clone());
            }
        }
        merged
    }
    pub fn lookup(&self, tool: &str, kind: Kind, text: &str) -> Option<Verb> {
        self.rules
            .iter()
            .filter(|r| {
                r.tool.is_match(tool)
                    && (if kind == Kind::Path { &r.path } else { &r.flat }).is_match(text)
            })
            .max_by_key(|r| (r.literal, r.tool_literal, r.rule.verb))
            .map(|r| r.rule.verb)
    }
    pub fn decide(&self, tool: &str, subject: &Subject) -> Verb {
        judge(subject, |text| {
            self.lookup(tool, subject.kind, text).unwrap_or_default()
        })
        .0
    }
}
#[derive(Clone, Debug)]
pub struct Layered {
    pub base: Set,
    pub overlays: Vec<Set>,
}
impl Layered {
    pub fn new(base: Set, overlays: impl IntoIterator<Item = Set>) -> Self {
        Self {
            base,
            overlays: overlays
                .into_iter()
                .filter(|s| !s.rules.is_empty())
                .collect(),
        }
    }
    pub fn with(&self, overlay: Set) -> Self {
        let mut out = self.clone();
        if !overlay.rules.is_empty() {
            out.overlays.push(overlay);
        }
        out
    }
    pub fn decide(&self, tool: &str, subject: &Subject) -> (Verb, String) {
        judge(subject, |text| {
            self.overlays
                .iter()
                .filter_map(|s| s.lookup(tool, subject.kind, text))
                .fold(
                    self.base
                        .lookup(tool, subject.kind, text)
                        .unwrap_or_default(),
                    std::cmp::max,
                )
        })
    }
}
fn judge(subject: &Subject, one: impl Fn(&str) -> Verb) -> (Verb, String) {
    let texts = subject.texts();
    let (mut verb, mut about) = (one(&texts[0]), texts[0].clone());
    for text in texts.iter().skip(1) {
        let decision = one(text);
        if decision > verb {
            verb = decision;
            about = text.clone();
        }
    }
    if subject.kind == Kind::Command && verb > Verb::Allow && about != subject.primary() {
        about = subject.primary().to_owned();
    }
    (verb, about)
}
