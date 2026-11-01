use serde::Deserialize;
use std::sync::OnceLock;
#[derive(Clone, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct Command {
    pub name: String,
    pub args: String,
    pub desc: String,
    pub aliases: Option<Vec<String>>,
    pub direct: bool,
}
pub fn all() -> &'static [Command] {
    static ALL: OnceLock<Vec<Command>> = OnceLock::new();
    ALL.get_or_init(|| {
        serde_json::from_str(include_str!("commands.json")).expect("built-in command registry")
    })
}
pub fn active(input: &str) -> bool {
    input.starts_with('/') && !input.contains(' ')
}
pub fn matches(input: &str) -> Vec<&'static Command> {
    if !active(input) {
        return vec![];
    }
    let query = input.strip_prefix('/').unwrap().to_lowercase();
    let exact = format!("/{query}");
    let mut out = all()
        .iter()
        .filter(|command| {
            !query.is_empty()
                && (command.name == exact
                    || command
                        .aliases
                        .as_ref()
                        .is_some_and(|aliases| aliases.contains(&exact)))
        })
        .collect::<Vec<_>>();
    let first = out.first().map(|command| command.name.as_str());
    for command in all() {
        if first == Some(&command.name) {
            continue;
        }
        if command.name.trim_start_matches('/').starts_with(&query)
            || !query.is_empty()
                && command.aliases.as_ref().is_some_and(|aliases| {
                    aliases
                        .iter()
                        .any(|alias| alias.trim_start_matches('/').starts_with(&query))
                })
        {
            out.push(command);
        }
    }
    out
}
pub fn find(name: &str) -> Option<&'static Command> {
    all().iter().find(|command| {
        command.name == name
            || command
                .aliases
                .as_ref()
                .is_some_and(|aliases| aliases.iter().any(|alias| alias == name))
    })
}
