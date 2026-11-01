use super::owner::{Daemon, escalation_config, lookup};
use crate::{config, protocol};
use anyhow::{Result, ensure};
use std::{
    path::{Path, PathBuf},
    sync::Arc,
};
fn convert<A: serde::Serialize, B: serde::de::DeserializeOwned>(value: A) -> Result<B> {
    Ok(serde_json::from_value(serde_json::to_value(value)?)?)
}
impl Daemon {
    pub fn editor_root(
        &self,
        scope: &str,
        channel: &str,
        expected: &str,
    ) -> Result<(PathBuf, Option<String>)> {
        let (root, directory) = match scope {
            "system" => (self.paths.config.clone(), None),
            "project" => {
                let dir = self.channel(channel)?.info().dir;
                (Paths::project(Path::new(&dir)), Some(dir))
            }
            _ => anyhow::bail!("config scope must be project or system"),
        };
        let root = crate::pathx::absolute(&root)?;
        let root = match std::fs::canonicalize(&root) {
            Ok(root) => root,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => root,
            Err(error) => return Err(error.into()),
        };
        ensure!(
            expected.is_empty() || expected == root.to_string_lossy(),
            "configuration directory changed; reopen settings"
        );
        Ok((root, directory))
    }
    pub fn config_list(&self, p: protocol::ConfigScope) -> Result<protocol::ConfigTree> {
        let (root, _) = self.editor_root(&p.scope, &p.channel, "")?;
        convert(config::editor::tree(&root)?)
    }
    pub fn config_read(&self, p: protocol::ConfigFileParams) -> Result<protocol::ConfigDocument> {
        let (root, _) = self.editor_root(&p.scope, &p.channel, &p.root)?;
        convert(config::editor::read(&root, &p.path)?)
    }
    pub fn config_edit(&self, p: protocol::ConfigEditParams) -> Result<protocol::ConfigEditResult> {
        let _editor = self.editor.lock().unwrap();
        ensure!(
            !p.root.is_empty(),
            "open the config directory before editing"
        );
        let (root, directory) = self.editor_root(&p.scope, &p.channel, &p.root)?;
        let trusted = if let Some(dir) = &directory {
            let status = self.trust_status(dir)?;
            status.hash.is_empty() || !status.pending
        } else {
            false
        };
        let edit = config::editor::ConfigEdit {
            path: p.path.clone(),
            revision: p.revision,
            action: p.action.clone(),
            content: p.content,
            destination: p.destination.clone(),
            field_path: p.field_path.to_vec(),
            field_value: p.field_value,
        };
        let tree = config::editor::apply(
            &root,
            p.scope == "system",
            &edit,
            &config::Loader {
                paths: &self.paths,
                home: &self.home,
                lookup: &lookup,
            },
        )?;
        let mut result = protocol::ConfigEditResult {
            tree: convert(tree)?,
            notice: "Saved to disk. ".into(),
            ..Default::default()
        };
        if let Some(directory) = &directory
            && trusted
        {
            let update = (|| -> Result<()> {
                let status = self.trust_status(directory)?;
                if !status.hash.is_empty() {
                    self.log
                        .set_trust(status.dir.clone(), status.hash.clone())?;
                    self.trust.write().unwrap().insert(status.dir, status.hash);
                }
                Ok(())
            })();
            if let Err(error) = update {
                result
                    .notice
                    .push_str(&format!("Trust update failed: {error:#}. "));
            }
        }
        result
            .notice
            .push_str(&self.reload_configuration(&p.scope, directory.as_deref()));
        let path = if p.action == "rename" {
            &p.destination
        } else {
            &p.path
        };
        if p.action != "delete" {
            result.document = config::editor::read(&root, path)
                .ok()
                .and_then(|value| convert(value).ok());
        }
        Ok(result)
    }
    pub fn reload_configuration(&self, scope: &str, directory: Option<&str>) -> String {
        let mut problems = Vec::new();
        if scope == "system" {
            match self.global() {
                Ok(config) => self.prompts.set_config(escalation_config(&config)),
                Err(error) => problems.push(error.to_string()),
            }
        }
        for channel in self.channels() {
            let info = channel.info();
            if scope == "project" && directory != Some(info.dir.as_str()) {
                continue;
            }
            match self.configuration(Path::new(&info.dir)) {
                Ok(config) => {
                    let pending = config.trust_pending;
                    channel.set_configuration(config);
                    let _ = self.maybe_trust_prompt(&channel);
                    if pending {
                        problems.push(format!("{}: project trust required", info.name));
                    }
                }
                Err(error) => problems.push(format!("{}: {error:#}", info.name)),
            }
        }
        if !problems.is_empty() {
            format!("Reload: {}", problems.join("; "))
        } else {
            "Reloaded. Policies/roles/commands: next step. Model/mode/root defaults: new channels. Escalation: new prompts. Discord: reconnect. Plugins: restart.".into()
        }
    }
    pub fn sandbox_status(&self) -> Result<protocol::SandboxStatus> {
        let config = self.global()?;
        let report = crate::sandbox::probe();
        Ok(protocol::SandboxStatus {
            enabled: config.sandbox.enabled,
            level: match report.level {
                crate::sandbox::Level::Landlock => "limited".into(),
                other => other.to_string(),
            },
            missing: report.missing.into(),
            why: report.why,
            fix: report.fix,
        })
    }
    pub fn set_sandbox(&self, enabled: bool) -> Result<protocol::SandboxStatus> {
        let _editor = self.editor.lock().unwrap();
        config::editor::set_global_field(
            &self.paths.config,
            &["sandbox".into(), "enabled".into()],
            if enabled { b"true" } else { b"false" },
        )?;
        self.reload_configuration("system", None);
        self.sandbox_status()
    }
    pub fn remember_model(&self, channel: &crate::agent::runtime::Channel, model: &str) {
        if model.is_empty() || !channel.core.configuration().model.is_empty() {
            return;
        }
        let _editor = self.editor.lock().unwrap();
        if self.global().is_ok_and(|config| config.model.is_empty()) {
            let _ = config::editor::set_global_field(
                &self.paths.config,
                &["model".into()],
                &serde_json::to_vec(model).unwrap(),
            );
        }
    }
    pub fn command_configuration(
        &self,
        id: &str,
    ) -> Result<(Arc<crate::agent::runtime::Channel>, Arc<config::Effective>)> {
        let channel = self.channel(id)?;
        let directory = channel.info().dir;
        let cfg = self.configuration(Path::new(&directory))?;
        ensure!(
            channel.info().dir == directory,
            "channel directory changed; retry the command"
        );
        channel.set_configuration(cfg.clone());
        self.maybe_trust_prompt(&channel)?;
        Ok((channel, cfg))
    }
    pub fn commands(&self, id: &str) -> Result<protocol::CommandListResult> {
        let (_, config) = self.command_configuration(id)?;
        Ok(protocol::CommandListResult {
            commands: config
                .commands
                .values()
                .map(|command| protocol::CommandInfo {
                    name: command.name.clone(),
                    description: command.description.clone(),
                })
                .collect(),
        })
    }
    pub fn run_command(
        &self,
        p: protocol::CommandRunParams,
        source: &str,
        cancel: &tokio_util::sync::CancellationToken,
    ) -> Result<()> {
        let (channel, config) = self.command_configuration(&p.channel)?;
        let command = config.commands.get(&p.name).ok_or_else(|| {
            anyhow::anyhow!(if config.trust_pending {
                "project configuration needs trust before project commands are available".into()
            } else {
                format!("unknown command /{}", p.name)
            })
        })?;
        channel.run_command(
            &config.dir.to_string_lossy(),
            &p.agent,
            &command.body,
            source,
            cancel,
        )
    }
}
use crate::paths::Paths;
