use super::*;
use crate::protocol::methods as rpc;
use serde::de::DeserializeOwned;
fn decode<T: DeserializeOwned>(value: Value) -> anyhow::Result<T> {
    Ok(serde_json::from_value(value)?)
}
impl Controller {
    fn tag_current(&self, tag: &Tag) -> bool {
        match tag {
            Tag::Models(epoch)
            | Tag::Variants(epoch)
            | Tag::Providers { epoch, .. }
            | Tag::LoginStart { epoch, .. }
            | Tag::Usage(epoch)
            | Tag::PlanSeries(epoch) => *epoch == self.popup_epoch,
            Tag::LoginDone(id) => id == &self.login_id,
            Tag::Discord(_, epoch) => *epoch == self.discord_epoch,
            Tag::Web(_, epoch) => *epoch == self.web_epoch,
            Tag::Sandbox(_, epoch) => *epoch == self.sandbox_epoch,
            Tag::ConfigTree(epoch) | Tag::ConfigRead(epoch) | Tag::ConfigEdited(epoch) => self
                .config
                .as_ref()
                .is_some_and(|config| config.epoch == *epoch),
            _ => true,
        }
    }
    pub fn reply(&mut self, reply: Reply) -> Vec<Effect> {
        // Prompt answers remain global even when the human changes channel while they complete.
        if let Tag::PromptAnswered(id) = &reply.tag {
            self.prompt_busy.remove(id);
            match reply.result {
                Ok(_) => self.resolve_prompt(id),
                Err(error) => self.status(error.to_string(), true),
            }
            return vec![];
        }
        if !self.accepts(&reply.scope) || !self.tag_current(&reply.tag) {
            return vec![];
        }
        let value = match reply.result {
            Ok(value) => value,
            Err(error) => {
                if matches!(reply.tag, Tag::Reconcile | Tag::Subscribed) {
                    self.quit = true;
                }
                if let Some(config) = &mut self.config {
                    config.busy = false;
                }
                self.status(error.to_string(), true);
                if let Some(popup) = &mut self.popup {
                    popup.detail = error.to_string();
                }
                return vec![];
            }
        };
        let result = self.apply_reply(reply.tag, value);
        self.dirty = true;
        match result {
            Ok(effects) => effects,
            Err(error) => {
                self.status(error.to_string(), true);
                vec![]
            }
        }
    }
    fn apply_reply(&mut self, tag: Tag, value: Value) -> anyhow::Result<Vec<Effect>> {
        match tag {
            Tag::Reconcile => return Ok(self.on_reconcile(decode(value)?)),
            Tag::Subscribed => {
                let result: protocol::SubscribeResult = decode(value)?;
                self.channel.history_from = result.first;
                if result.first > 1 {
                    self.status("showing recent history · /history loads all of it", false);
                }
            }
            Tag::Catalog => {
                let result: protocol::ChannelListResult = decode(value)?;
                self.catalog = result.channels.into_vec();
                self.catalog
                    .sort_by(|left, right| (&left.name, &left.id).cmp(&(&right.name, &right.id)));
                let ids = self
                    .catalog
                    .iter()
                    .map(|info| info.id.clone())
                    .collect::<BTreeSet<_>>();
                self.trees
                    .retain(|id, _| ids.contains(id) || id == &self.scope.channel);
                if let Some(popup) = self
                    .popup
                    .as_mut()
                    .filter(|popup| popup.kind == Dialog::Channels)
                {
                    popup.choices = self
                        .catalog
                        .iter()
                        .filter(|info| info.id != self.scope.channel)
                        .map(|info| Choice {
                            id: info.id.clone(),
                            label: format!("#{}", info.name),
                            description: info.dir.clone(),
                        })
                        .collect();
                }
            }
            Tag::Tree(id) => {
                let result: protocol::AgentTreeResult = decode(value)?;
                if id == self.scope.channel {
                    self.set_agents(result.agents.into_vec());
                } else {
                    self.trees.insert(id, result.agents.into_vec());
                }
            }
            Tag::Prompts => {
                let result: protocol::PromptListResult = decode(value)?;
                for prompt in result.prompts.into_vec() {
                    self.upsert_prompt(prompt);
                }
            }
            Tag::Commands => {
                let result: protocol::CommandListResult = decode(value)?;
                self.custom_commands = result.commands.into_vec();
            }
            Tag::Roles(quiet) => {
                let result: protocol::RolesResult = decode(value)?;
                self.roles = result.roles.into_vec();
                if !quiet {
                    let choices = self
                        .roles
                        .iter()
                        .map(|role| Choice {
                            id: role.name.clone(),
                            label: role.name.clone(),
                            description: role.description.clone(),
                        })
                        .collect();
                    let agent = self.channel.selected.clone();
                    let popup = self.popup(Dialog::Roles, "Role");
                    popup.agent = agent;
                    popup.choices = choices;
                }
            }
            Tag::Models(_) => {
                let result: protocol::ModelListResult = decode(value)?;
                self.models = result.models.into_vec();
                let role = self
                    .channel
                    .selected_agent()
                    .and_then(|agent| self.roles.iter().find(|role| role.name == agent.role));
                let choices = self
                    .models
                    .iter()
                    .filter(|model| {
                        role.is_none_or(|role| {
                            role.models.is_empty()
                                || role.models.iter().any(|spec| spec.id == model.id)
                        })
                    })
                    .map(|model| Choice {
                        id: model.id.clone(),
                        label: model.name.clone(),
                        description: format!("{} · {}", model.provider, model.id),
                    })
                    .collect();
                let agent = self.channel.selected.clone();
                let popup = self.popup.as_mut().unwrap();
                popup.agent = agent;
                popup.choices = choices;
                popup.detail.clear();
            }
            Tag::Variants(_) => {
                let result: protocol::VariantsResult = decode(value)?;
                self.variants = result.variants.into_vec();
                let choices = std::iter::once(Choice {
                    id: String::new(),
                    label: "Provider default".into(),
                    description: String::new(),
                })
                .chain(self.variants.iter().map(|variant| Choice {
                    id: variant.clone(),
                    label: variant.clone(),
                    description: String::new(),
                }))
                .collect();
                let popup = self.popup.as_mut().unwrap();
                popup.choices = choices;
                popup.detail.clear();
            }
            Tag::Providers { open, jump, .. } => {
                let result: protocol::ProviderListResult = decode(value)?;
                self.providers = result.providers.into_vec();
                if !jump.is_empty() {
                    return Ok(self.choose_provider(&jump));
                }
                if open {
                    let choices = self
                        .providers
                        .iter()
                        .map(|provider| Choice {
                            id: provider.id.clone(),
                            label: provider.name.clone(),
                            description: if provider.connected {
                                format!("connected · {}", provider.account)
                            } else {
                                "not connected".into()
                            },
                        })
                        .collect();
                    if !self
                        .popup
                        .as_ref()
                        .is_some_and(|popup| popup.kind == Dialog::Providers)
                    {
                        self.popup(
                            Dialog::Providers,
                            "Providers · enter connects · ctrl+d signs out",
                        );
                    }
                    self.popup.as_mut().unwrap().choices = choices;
                }
            }
            Tag::LoginStart { provider, .. } => {
                let result: protocol::LoginStartResult = decode(value)?;
                self.login_id = result.id.clone();
                self.login_provider = provider;
                self.login_method = result.method.clone();
                self.login = result;
                let key = self.login.method == crate::oauth::METHOD_API_KEY;
                let popup = self.popup.as_mut().unwrap();
                popup.kind = if key { Dialog::LoginKey } else { Dialog::Login };
                popup.input.secret = key;
                popup.detail = self.login.instructions.clone();
                let mut wait = self.global(
                    rpc::PROVIDER_LOGIN_WAIT,
                    protocol::LoginWaitParams {
                        id: self.login_id.clone(),
                    },
                    Tag::LoginDone(self.login_id.clone()),
                );
                if let Effect::Request(request) = &mut wait {
                    request.timeout = None;
                    request.cancellation = Some(self.login_id.clone());
                }
                let mut effects = vec![wait];
                if !key && !self.login.url.is_empty() {
                    effects.push(Effect::OpenBrowser(self.login.url.clone()));
                }
                return Ok(effects);
            }
            Tag::LoginDone(_) => {
                let result: protocol::ProviderInfo = decode(value)?;
                self.login_id.clear();
                let mut effects = self.close_popup();
                self.status(format!("connected to {}", result.name), false);
                effects.push(self.global(
                    rpc::PROVIDER_LIST,
                    protocol::None {},
                    Tag::Providers {
                        open: false,
                        jump: String::new(),
                        epoch: self.popup_epoch,
                    },
                ));
                return Ok(effects);
            }
            Tag::Changed(message) => {
                if message == "@providers" {
                    return Ok(vec![self.global(
                        rpc::PROVIDER_LIST,
                        protocol::None {},
                        Tag::Providers {
                            open: true,
                            jump: String::new(),
                            epoch: self.popup_epoch,
                        },
                    )]);
                }
                if !message.is_empty() {
                    self.status(message, false);
                }
                self.tree_dirty = true;
            }
            Tag::ChannelOpened { agent } => return Ok(self.bind_channel(decode(value)?, agent)),
            Tag::DirectoryChanged => {
                let result: protocol::ChannelInfo = decode(value)?;
                self.channel.info = result;
                self.scope.generation = self.scope.generation.wrapping_add(1);
                self.status("default directory updated", false);
                return Ok(vec![self.reconcile()]);
            }
            Tag::DirectoryAdded(old) => {
                return Ok(vec![self.scoped(
                    rpc::CHANNEL_REMOVE_DIR,
                    protocol::ChannelDirParams {
                        channel: self.scope.channel.clone(),
                        dir: old,
                    },
                    Tag::Changed("directory replaced".into()),
                )]);
            }
            Tag::PromptAnswered(_) => unreachable!(),
            Tag::Discord(open, _) => {
                self.discord = decode(value)?;
                if open
                    && self
                        .popup
                        .as_ref()
                        .is_some_and(|popup| popup.kind == Dialog::Discord)
                {
                    self.show_discord();
                }
            }
            Tag::Web(open, _) => {
                self.web = decode(value)?;
                let browser = std::mem::take(&mut self.web.open_url);
                if open
                    && self
                        .popup
                        .as_ref()
                        .is_some_and(|popup| popup.kind == Dialog::Web)
                {
                    self.show_web();
                }
                if !browser.is_empty() {
                    return Ok(vec![Effect::OpenBrowser(browser)]);
                }
            }
            Tag::Sandbox(open, _) => {
                self.sandbox = decode(value)?;
                if open
                    && self
                        .popup
                        .as_ref()
                        .is_some_and(|popup| popup.kind == Dialog::Sandbox)
                {
                    self.show_sandbox();
                }
            }
            Tag::Plans => {
                let result: protocol::PlanUsageResult = decode(value)?;
                self.plans = result.plans.into_vec();
            }
            Tag::Cache => self.cache = decode(value)?,
            Tag::Usage(_) => self.usage = Some(decode(value)?),
            Tag::PlanSeries(_) => self.plan_series = Some(decode(value)?),
            Tag::ConfigTree(epoch) => {
                let tree: protocol::ConfigTree = decode(value)?;
                let config = self.config.as_mut().unwrap();
                config.tree = tree;
                config.busy = false;
                config.epoch = epoch;
                return Ok(self.read_selected_config());
            }
            Tag::ConfigRead(_) => {
                let document: protocol::ConfigDocument = decode(value)?;
                let config = self.config.as_mut().unwrap();
                config.raw = document.fields.is_empty();
                config.editing = config.raw;
                config.pane = 1;
                config.action.clear();
                config.input.set(document.content.clone());
                config.document = Some(document);
                config.busy = false;
                config.dirty = false;
            }
            Tag::ConfigEdited(_) => {
                let result: protocol::ConfigEditResult = decode(value)?;
                let config = self.config.as_mut().unwrap();
                config.tree = result.tree;
                config.document = result.document;
                config.input.set(
                    config
                        .document
                        .as_ref()
                        .map(|document| document.content.clone())
                        .unwrap_or_default(),
                );
                config.busy = false;
                config.dirty = false;
                config.editing = false;
                config.action.clear();
                self.status(result.notice, false);
            }
        }
        Ok(vec![])
    }
}
