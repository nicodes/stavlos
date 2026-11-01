//! The same binding registry drives keyboard handling and visible legends.
use serde::Deserialize;
use std::sync::OnceLock;
#[derive(Deserialize)]
pub struct Binding {
    pub name: String,
    pub keys: Vec<String>,
    pub help: String,
}
pub fn all() -> &'static [Binding] {
    static ALL: OnceLock<Vec<Binding>> = OnceLock::new();
    ALL.get_or_init(|| {
        serde_json::from_str(include_str!("keys.json")).expect("built-in keyboard registry")
    })
}
pub fn binding(name: &str) -> &'static Binding {
    all()
        .iter()
        .find(|binding| binding.name == name)
        .expect("declared keyboard action")
}
pub fn matches(name: &str, key: &str) -> bool {
    binding(name).keys.iter().any(|candidate| candidate == key)
}
pub fn label(names: &[&str]) -> String {
    let mut output = String::new();
    for (index, name) in names.iter().enumerate() {
        let help = &binding(name).help;
        if index == 0 {
            output.push_str(help);
            continue;
        }
        output.push('/');
        let common = output.rfind('+').map(|at| &output[..at + 1]);
        let help = common
            .and_then(|prefix| help.strip_prefix(prefix))
            .unwrap_or(help);
        output.push_str(help);
    }
    output
}
