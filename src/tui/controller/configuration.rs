use super::*;
use crate::protocol::methods as rpc;
impl Configuration {
    pub fn entries(&self) -> Vec<protocol::ConfigEntry> {
        if self.section == 0 {
            let mut out = vec![protocol::ConfigEntry {
                path: "stavlos.json".into(),
                directory: false,
            }];
            if self.scope.scope == "project" {
                out.push(protocol::ConfigEntry {
                    path: "stavlos.local.json".into(),
                    directory: false,
                });
            }
            return out;
        }
        let prefix = ["", "agents/", "commands/", "skills/", ""][self.section.min(4)];
        let mut out = self
            .tree
            .files
            .iter()
            .filter(|file| {
                file.path.starts_with(prefix)
                    && !self
                        .collapsed
                        .iter()
                        .any(|directory| file.path.starts_with(&format!("{directory}/")))
            })
            .cloned()
            .collect::<Vec<_>>();
        if let Some(document) = self.document.as_ref().filter(|document| {
            !document.exists
                && document.path.starts_with(prefix)
                && !out.iter().any(|file| file.path == document.path)
        }) {
            out.push(protocol::ConfigEntry {
                path: document.path.clone(),
                directory: false,
            });
        }
        out
    }
}
impl Controller {
    pub fn open_configuration(&mut self, system: bool) -> Vec<Effect> {
        let mut effects = self.close_popup();
        self.config_epoch = self.config_epoch.wrapping_add(1);
        let scope = protocol::ConfigScope {
            scope: if system { "system" } else { "project" }.into(),
            channel: if system {
                String::new()
            } else {
                self.scope.channel.clone()
            },
        };
        self.config = Some(Configuration {
            epoch: self.config_epoch,
            scope: scope.clone(),
            tree: Default::default(),
            document: None,
            file_cursor: 0,
            field_cursor: 0,
            section: 0,
            pane: 0,
            collapsed: BTreeSet::new(),
            raw: false,
            editing: false,
            input: Editor::default(),
            field: vec![],
            action: String::new(),
            pending: String::new(),
            dirty: false,
            busy: true,
        });
        effects.push(self.global(rpc::CONFIG_LIST, scope, Tag::ConfigTree(self.config_epoch)));
        effects
    }
    pub fn read_selected_config(&mut self) -> Vec<Effect> {
        let Some(config) = self.config.as_ref() else {
            return vec![];
        };
        let files = config.entries();
        let path = files
            .get(config.file_cursor)
            .map(|file| file.path.clone())
            .unwrap_or_else(|| "stavlos.json".into());
        self.read_config(path)
    }
    pub fn read_config(&mut self, path: String) -> Vec<Effect> {
        let Some(config) = self.config.as_mut() else {
            return vec![];
        };
        config.busy = true;
        config.epoch = config.epoch.wrapping_add(1);
        config.field_cursor = 0;
        let params = protocol::ConfigFileParams {
            scope: config.scope.scope.clone(),
            channel: config.scope.channel.clone(),
            root: config.tree.root.clone(),
            path,
        };
        let epoch = config.epoch;
        vec![self.global(rpc::CONFIG_READ, params, Tag::ConfigRead(epoch))]
    }
    pub fn edit_config(
        &mut self,
        action: &str,
        path: String,
        destination: String,
        content: String,
        field_path: Vec<String>,
        field_value: Option<Box<serde_json::value::RawValue>>,
    ) -> Vec<Effect> {
        let Some(config) = self.config.as_mut().filter(|config| !config.busy) else {
            return vec![];
        };
        config.busy = true;
        config.epoch = config.epoch.wrapping_add(1);
        let params = protocol::ConfigEditParams {
            scope: config.scope.scope.clone(),
            channel: config.scope.channel.clone(),
            root: config.tree.root.clone(),
            revision: config.tree.revision.clone(),
            path,
            action: action.into(),
            destination,
            content,
            field_path: field_path.into(),
            field_value,
        };
        let epoch = config.epoch;
        vec![self.global(rpc::CONFIG_EDIT, params, Tag::ConfigEdited(epoch))]
    }
    pub fn save_config(&mut self) -> Vec<Effect> {
        let Some(config) = self.config.as_ref() else {
            return vec![];
        };
        let Some(document) = &config.document else {
            return vec![];
        };
        let path = document.path.clone();
        if config.raw || !document.exists && !config.editing {
            return self.edit_config(
                "write",
                path,
                String::new(),
                config.input.text.clone(),
                vec![],
                None,
            );
        }
        if !config.editing {
            return vec![];
        }
        let Some(field) = document.fields.get(config.field_cursor) else {
            return vec![];
        };
        let value = if matches!(field.kind.as_str(), "string" | "body") {
            serde_json::to_value(&config.input.text).unwrap()
        } else {
            match serde_json::from_str(&config.input.text) {
                Ok(value) => value,
                Err(error) => {
                    self.status(format!("Enter a valid JSON value: {error}"), true);
                    return vec![];
                }
            }
        };
        let field = field.path.clone().into_vec();
        self.edit_config(
            "write",
            path,
            String::new(),
            String::new(),
            field,
            Some(serde_json::value::to_raw_value(&value).unwrap()),
        )
    }
    pub fn config_key(&mut self, key: &str, text: Option<&str>) -> Vec<Effect> {
        let Some(config) = self.config.as_mut() else {
            return vec![];
        };
        if config.busy {
            return vec![];
        }
        self.dirty = true;
        if config.action == "discard" {
            match key {
                "enter" => {
                    self.config = None;
                    self.config_epoch = self.config_epoch.wrapping_add(1);
                }
                "esc" => {
                    config.action = std::mem::take(&mut config.pending);
                }
                _ => {}
            }
            return vec![];
        }
        if matches!(key, "esc" | "ctrl+c") {
            if config.dirty {
                config.pending = std::mem::replace(&mut config.action, "discard".into());
                self.status(
                    "Discard unsaved changes and close? Enter discards; Esc keeps editing.",
                    false,
                );
            } else if config.editing || !config.action.is_empty() {
                config.editing = false;
                config.action.clear();
                config.input.set(
                    config
                        .document
                        .as_ref()
                        .map(|document| document.content.clone())
                        .unwrap_or_default(),
                );
            } else {
                self.config = None;
                self.config_epoch = self.config_epoch.wrapping_add(1);
            }
            return vec![];
        }
        if key == "ctrl+s" {
            return self.save_config();
        }
        if matches!(config.action.as_str(), "new" | "rename" | "delete") {
            if key != "enter" {
                if let Some(text) = text {
                    config.input.insert(text);
                } else {
                    config.input.key(key);
                }
                return vec![];
            }
            let action = std::mem::take(&mut config.action);
            let input = config.input.text.trim().to_owned();
            let files = config.entries();
            let path = files
                .get(config.file_cursor)
                .map(|file| file.path.clone())
                .unwrap_or_default();
            return match action.as_str() {
                "new" => self.read_config(input),
                "rename" => self.edit_config("rename", path, input, String::new(), vec![], None),
                "delete" => {
                    self.edit_config("delete", path, String::new(), String::new(), vec![], None)
                }
                _ => vec![],
            };
        }
        if key == "f4" {
            if config.dirty {
                self.status("Save with Ctrl+S before switching editors.", false);
                return vec![];
            }
            config.raw = !config.raw;
            config.editing = config.raw;
            config.pane = 1;
            config.field.clear();
            config.input.set(
                config
                    .document
                    .as_ref()
                    .map(|document| document.content.clone())
                    .unwrap_or_default(),
            );
            return vec![];
        }
        if key == "tab" {
            config.pane = 1 - config.pane;
            return vec![];
        }
        if key == "ctrl+l" {
            if config.dirty {
                self.status(
                    "Save with Ctrl+S or discard with Esc before reloading.",
                    false,
                );
                return vec![];
            }
            config.busy = true;
            config.epoch = config.epoch.wrapping_add(1);
            let scope = config.scope.clone();
            let epoch = config.epoch;
            return vec![self.global(rpc::CONFIG_LIST, scope, Tag::ConfigTree(epoch))];
        }
        if config.pane == 0 {
            if config.dirty {
                self.status(
                    "Save with Ctrl+S or discard with Esc before choosing another file.",
                    false,
                );
                return vec![];
            }
            let files = config.entries();
            match key {
                "left" => {
                    config.section = (config.section + 4) % 5;
                    config.file_cursor = 0;
                }
                "right" => {
                    config.section = (config.section + 1) % 5;
                    config.file_cursor = 0;
                }
                "up" | "k" => config.file_cursor = config.file_cursor.saturating_sub(1),
                "down" | "j" => {
                    config.file_cursor = (config.file_cursor + 1).min(files.len().saturating_sub(1))
                }
                "ctrl+n" => {
                    config.action = "new".into();
                    config.input.set(
                        [
                            "stavlos.local.json",
                            "agents/new-agent.md",
                            "commands/new-command.md",
                            "skills/new-skill/SKILL.md",
                            "new-file.md",
                        ][config.section.min(4)],
                    );
                }
                "f2" | "ctrl+d" => {
                    if let Some(file) = files.get(config.file_cursor) {
                        config.action = if key == "f2" { "rename" } else { "delete" }.into();
                        config.input.set(file.path.clone());
                    }
                }
                "enter" => {
                    if let Some(file) = files.get(config.file_cursor) {
                        if file.directory {
                            if !config.collapsed.insert(file.path.clone()) {
                                config.collapsed.remove(&file.path);
                            }
                        } else {
                            return self.read_config(file.path.clone());
                        }
                    }
                }
                _ => {}
            }
            return vec![];
        }
        if config.editing || config.raw {
            if key == "enter"
                && !config.raw
                && config
                    .document
                    .as_ref()
                    .and_then(|document| document.fields.get(config.field_cursor))
                    .is_none_or(|field| !matches!(field.kind.as_str(), "body" | "json"))
            {
                return self.save_config();
            }
            let before = config.input.text.clone();
            if key == "enter" {
                config.input.insert("\n");
            } else if let Some(text) = text {
                config.input.insert(text);
            } else {
                config.input.key(key);
            }
            config.dirty |= before != config.input.text;
            return vec![];
        }
        let Some(document) = &config.document else {
            return vec![];
        };
        match key {
            "up" | "k" => config.field_cursor = config.field_cursor.saturating_sub(1),
            "down" | "j" => {
                config.field_cursor =
                    (config.field_cursor + 1).min(document.fields.len().saturating_sub(1))
            }
            "enter" | " " => {
                if let Some(field) = document.fields.get(config.field_cursor) {
                    let path = document.path.clone();
                    let field_path = field.path.clone().into_vec();
                    let value = if field.kind == "boolean" {
                        Some(Value::Bool(field.value != "true"))
                    } else if !field.choices.is_empty() {
                        let current =
                            serde_json::from_str::<String>(&field.value).unwrap_or_default();
                        let index = field
                            .choices
                            .iter()
                            .position(|choice| choice == &current)
                            .map(|index| (index + 1) % field.choices.len())
                            .unwrap_or_default();
                        Some(Value::String(field.choices[index].clone()))
                    } else {
                        None
                    };
                    if let Some(value) = value {
                        return self.edit_config(
                            "write",
                            path,
                            String::new(),
                            String::new(),
                            field_path,
                            Some(serde_json::value::to_raw_value(&value).unwrap()),
                        );
                    }
                    let value = if matches!(field.kind.as_str(), "string" | "body") {
                        serde_json::from_str::<String>(&field.value).unwrap_or_default()
                    } else if !field.set {
                        "null".into()
                    } else {
                        field.value.clone()
                    };
                    config.input.set(value);
                    config.field = field_path;
                    config.editing = true;
                    config.action = "field".into();
                }
            }
            _ => {}
        }
        vec![]
    }
}
