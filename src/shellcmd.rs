//! Permission classification only. These routines never execute shell input.
use std::collections::{BTreeMap, HashSet};

#[derive(Clone, Copy, PartialEq, Eq, Default)]
enum Quote {
    #[default]
    None,
    Single,
    Double,
}

/// Split one simple command, retaining literal variables but refusing execution syntax.
pub fn words(command: &str) -> Option<Vec<String>> {
    let chars: Vec<char> = command.chars().collect();
    let (mut i, mut quote, mut active) = (0, Quote::None, false);
    let (mut current, mut out) = (String::new(), Vec::new());
    while i < chars.len() {
        let ch = chars[i];
        match quote {
            Quote::Single => {
                if ch == '\'' {
                    quote = Quote::None;
                } else {
                    current.push(ch);
                }
            }
            Quote::Double => match ch {
                '"' => quote = Quote::None,
                '\\' if chars.get(i + 1).is_some_and(|c| "$\"\\`\n".contains(*c)) => {
                    i += 1;
                    if chars[i] != '\n' {
                        current.push(chars[i]);
                    }
                }
                '`' => return None,
                '$' if chars.get(i + 1) == Some(&'(') => return None,
                _ => current.push(ch),
            },
            Quote::None => {
                match ch {
                    ' ' | '\t' => {
                        flush(&mut current, &mut out, &mut active);
                        i += 1;
                        continue;
                    }
                    '\n' | '\r' | ';' | '|' | '&' | '<' | '>' | '(' | ')' | '`' => return None,
                    '\'' => quote = Quote::Single,
                    '"' => quote = Quote::Double,
                    '\\' => {
                        if i + 1 == chars.len() {
                            i += 1;
                            continue;
                        }
                        i += 1;
                        if chars[i] == '\n' {
                            flush(&mut current, &mut out, &mut active);
                            i += 1;
                            continue;
                        }
                        current.push(chars[i]);
                    }
                    '$' if chars.get(i + 1) == Some(&'(') => return None,
                    '{' | '}' if !active && chars.get(i + 1).is_none_or(|c| " \t".contains(*c)) => {
                        return None;
                    }
                    _ => current.push(ch),
                }
                active = true;
            }
        }
        i += 1;
    }
    if quote != Quote::None {
        return None;
    }
    flush(&mut current, &mut out, &mut active);
    Some(out)
}

fn flush(current: &mut String, out: &mut Vec<String>, active: &mut bool) {
    if *active {
        out.push(std::mem::take(current));
        *active = false;
    }
}

pub fn simple(command: &str) -> bool {
    words(command).is_some_and(|w| !w.is_empty())
}

fn two_word(program: &str) -> Option<&'static [&'static str]> {
    match program {
        "git" | "go" | "cargo" | "make" | "just" | "gh" | "pip" | "pip3" | "dotnet" | "mvn"
        | "gradle" | "terraform" | "helm" => Some(&[]),
        "npm" => Some(&["exec", "x"]),
        "pnpm" => Some(&["exec", "dlx", "x"]),
        "yarn" => Some(&["exec", "dlx"]),
        "bun" => Some(&["x", "exec"]),
        "uv" => Some(&["run", "tool"]),
        "poetry" => Some(&["run"]),
        "docker" => Some(&["run", "exec", "compose"]),
        "kubectl" => Some(&["exec", "run", "debug"]),
        _ => None,
    }
}
fn one_word(program: &str) -> bool {
    matches!(
        program,
        "ls" | "cat"
            | "head"
            | "tail"
            | "wc"
            | "echo"
            | "printf"
            | "grep"
            | "rg"
            | "sort"
            | "uniq"
            | "cut"
            | "diff"
            | "cmp"
            | "tree"
            | "file"
            | "stat"
            | "du"
            | "df"
            | "pwd"
            | "which"
            | "mkdir"
            | "touch"
            | "cp"
            | "mv"
            | "rm"
            | "ln"
            | "chmod"
            | "jq"
            | "yq"
            | "curl"
            | "wget"
            | "tsc"
            | "eslint"
            | "prettier"
            | "pytest"
            | "jest"
            | "vitest"
            | "mypy"
            | "ruff"
            | "black"
            | "gofmt"
            | "goimports"
            | "golangci-lint"
            | "staticcheck"
            | "rustfmt"
            | "clang-format"
            | "cmake"
            | "ninja"
    )
}
fn plain_word(word: &str) -> bool {
    !word.is_empty()
        && word
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "._/+-=@:,%".contains(c))
}

/// Return only a reviewed, unambiguous command prefix suitable for a permission prompt.
pub fn prefix(command: &str) -> String {
    let Some(words) = words(command) else {
        return String::new();
    };
    let Some(first) = words.first() else {
        return String::new();
    };
    if !plain_word(first) {
        return String::new();
    }
    if let Some(refused) = two_word(first) {
        return match words.get(1) {
            Some(second)
                if !second.starts_with('-')
                    && plain_word(second)
                    && !refused.contains(&second.as_str()) =>
            {
                format!("{first} {second}")
            }
            _ => String::new(),
        };
    }
    if one_word(first) || first.starts_with("./") && first.len() > 2 && !first[2..].contains("/..")
    {
        first.clone()
    } else {
        String::new()
    }
}
pub fn covers(prefix: &str, command: &str) -> bool {
    match (words(prefix), words(command)) {
        (Some(p), Some(c)) => !p.is_empty() && c.starts_with(&p),
        _ => false,
    }
}

fn segments(command: &str) -> Vec<String> {
    let chars: Vec<char> = command.chars().collect();
    let (mut i, mut quote) = (0, Quote::None);
    let (mut current, mut out) = (String::new(), Vec::new());
    while i < chars.len() {
        let ch = chars[i];
        let substitution = ch == '`' || ch == '$' && chars.get(i + 1) == Some(&'(');
        match quote {
            Quote::Single => {
                current.push(ch);
                if ch == '\'' {
                    quote = Quote::None;
                }
            }
            Quote::Double => match ch {
                '"' => {
                    quote = Quote::None;
                    current.push(ch);
                }
                '\\' => {
                    current.push(ch);
                    if i + 1 < chars.len() {
                        i += 1;
                        current.push(chars[i]);
                    }
                }
                _ if substitution => {
                    if ch == '$' {
                        i += 1;
                    }
                    cut(&mut current, &mut out);
                    quote = Quote::None;
                }
                _ => current.push(ch),
            },
            Quote::None => match ch {
                '\'' => {
                    quote = Quote::Single;
                    current.push(ch);
                }
                '"' => {
                    quote = Quote::Double;
                    current.push(ch);
                }
                '\\' => {
                    current.push(ch);
                    if i + 1 < chars.len() {
                        i += 1;
                        current.push(chars[i]);
                    }
                }
                _ if substitution => {
                    if ch == '$' {
                        i += 1;
                    }
                    cut(&mut current, &mut out);
                }
                _ if ";&|\n\r()".contains(ch)
                    || "{}".contains(ch)
                        && (i == 0 || " \t".contains(chars[i - 1]))
                        && chars.get(i + 1).is_none_or(|c| " \t;".contains(*c)) =>
                {
                    cut(&mut current, &mut out)
                }
                _ => current.push(ch),
            },
        }
        i += 1;
    }
    cut(&mut current, &mut out);
    out
}
fn cut(current: &mut String, out: &mut Vec<String>) {
    if !current.trim().is_empty() {
        out.push(current.trim().to_owned());
    }
    current.clear();
}
fn basename(word: &str) -> &str {
    if word.is_empty() {
        return ".";
    }
    let trimmed = word.trim_end_matches('/');
    if trimmed.is_empty() {
        "/"
    } else {
        trimmed.rsplit('/').next().unwrap_or(trimmed)
    }
}
fn assignment(word: &str) -> Option<(&str, &str)> {
    let (name, value) = word.split_once('=')?;
    if !name.is_empty()
        && name
            .chars()
            .enumerate()
            .all(|(i, c)| c == '_' || c.is_ascii_alphabetic() || i > 0 && c.is_ascii_digit())
    {
        Some((name, value))
    } else {
        None
    }
}
fn keyword(word: &str) -> bool {
    matches!(
        word,
        "if" | "then"
            | "else"
            | "elif"
            | "fi"
            | "while"
            | "until"
            | "do"
            | "done"
            | "!"
            | "{"
            | "}"
            | "time"
            | "coproc"
    )
}
fn launcher(word: &str) -> Option<(usize, &'static str)> {
    Some(match word {
        "sudo" => (0, "ughpCDRTUr"),
        "doas" => (0, "uC"),
        "env" => (0, "uCS"),
        "nice" => (0, "n"),
        "ionice" => (0, "cnpPu"),
        "timeout" => (1, "ks"),
        "chroot" => (1, ""),
        "stdbuf" => (0, "ioe"),
        "xargs" => (0, "IndPLEsa"),
        "exec" => (0, "a"),
        "nohup" | "command" | "builtin" | "setsid" | "chronic" | "unbuffer" => (0, ""),
        _ => return None,
    })
}
fn launched(mut words: &[String]) -> &[String] {
    while let Some(first) = words.first() {
        if keyword(first) || assignment(first).is_some() {
            words = &words[1..];
            continue;
        }
        let Some((own_args, flags)) = launcher(basename(first)) else {
            return words;
        };
        words = &words[1..];
        while let Some(word) = words.first() {
            if assignment(word).is_none() && (word.len() < 2 || !word.starts_with('-')) {
                break;
            }
            words = &words[1..];
            if word.len() == 2
                && word != "--"
                && flags.contains(char::from(word.as_bytes()[1]))
                && !words.is_empty()
            {
                words = &words[1..];
            }
        }
        words = &words[own_args.min(words.len())..];
    }
    words
}
fn bind(variables: &mut BTreeMap<String, String>, words: &[String]) {
    for word in words {
        let Some((name, value)) = assignment(word) else {
            break;
        };
        if value.contains(['$', '`']) {
            variables.remove(name);
        } else {
            variables.insert(name.to_owned(), value.to_owned());
        }
    }
}
fn expand_bare(variables: &BTreeMap<String, String>, word: &str) -> String {
    let mut out = String::new();
    let mut i = 0;
    while i < word.len() {
        if word.as_bytes()[i] == b'$' {
            let mut end = i + 1;
            while end < word.len() {
                let c = word.as_bytes()[end];
                if c == b'_' || c.is_ascii_alphabetic() || end > i + 1 && c.is_ascii_digit() {
                    end += 1;
                } else {
                    break;
                }
            }
            if let Some(value) = variables.get(&word[i + 1..end]) {
                out.push_str(value);
                i = end;
                continue;
            }
        }
        let c = word[i..].chars().next().expect("character boundary");
        out.push(c);
        i += c.len_utf8();
    }
    out
}
fn expand(variables: &BTreeMap<String, String>, words: Vec<String>) -> Vec<String> {
    if variables.is_empty() {
        return words;
    }
    let mut out = Vec::new();
    for mut word in words {
        if !word.contains('$') {
            out.push(word);
            continue;
        }
        for (name, value) in variables {
            word = word.replace(&format!("${{{name}}}"), value);
        }
        out.extend(
            expand_bare(variables, &word)
                .split_whitespace()
                .map(str::to_owned),
        );
    }
    out
}
fn scripts(words: &[String]) -> Vec<String> {
    let Some(first) = words.first() else {
        return Vec::new();
    };
    match basename(first) {
        "eval" => return vec![words[1..].join(" ")],
        "sh" | "bash" | "zsh" | "dash" | "ksh" | "fish" => {}
        _ => return Vec::new(),
    }
    let mut out = Vec::new();
    for i in 1..words.len().saturating_sub(1) {
        let word = &words[i];
        if word == "<<<" {
            out.push(
                words[i + 1..]
                    .join(" ")
                    .trim_matches(['\'', '"'])
                    .to_owned(),
            );
        } else if word.starts_with('-') && !word.starts_with("--") && word.contains('c') {
            out.push(words[i + 1].clone());
        }
    }
    out
}

/// List normalized embedded commands for allow/ask/deny classification; never a shell parser.
pub fn commands(command: &str) -> Vec<String> {
    fn visit(
        command: &str,
        depth: u8,
        variables: &mut BTreeMap<String, String>,
        seen: &mut HashSet<String>,
        out: &mut Vec<String>,
    ) {
        let segments = if words(command).is_some() {
            vec![command.to_owned()]
        } else {
            segments(command)
        };
        for segment in segments {
            let words = words(&segment)
                .unwrap_or_else(|| segment.split_whitespace().map(str::to_owned).collect());
            bind(variables, &words);
            let expanded = expand(variables, words);
            let words = launched(&expanded);
            if words.is_empty() {
                continue;
            }
            let normalized = words.join(" ");
            if !normalized.is_empty() && seen.insert(normalized.clone()) {
                out.push(normalized);
            }
            let base = basename(&words[0]);
            if base != words[0] {
                let normalized = std::iter::once(base)
                    .chain(words[1..].iter().map(String::as_str))
                    .collect::<Vec<_>>()
                    .join(" ");
                if seen.insert(normalized.clone()) {
                    out.push(normalized);
                }
            }
            if depth < 4 {
                for script in scripts(words) {
                    visit(&script, depth + 1, variables, seen, out);
                }
            }
        }
    }
    let mut out = Vec::new();
    visit(
        command,
        0,
        &mut BTreeMap::new(),
        &mut HashSet::new(),
        &mut out,
    );
    out
}
