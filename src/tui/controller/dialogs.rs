use super::*;
use crate::protocol::methods as rpc;
impl Controller {
    pub fn choose_provider(&mut self, id: &str) -> Vec<Effect> {
        let Some(provider) = self
            .providers
            .iter()
            .find(|provider| provider.id == id)
            .cloned()
        else {
            self.status("provider not found", true);
            return vec![];
        };
        if provider.methods.len() > 1 {
            let popup = self.popup(Dialog::Methods, format!("Sign in to {}", provider.name));
            popup.context = provider.id;
            popup.choices = provider
                .methods
                .iter()
                .map(|method| Choice {
                    id: method.id.clone(),
                    label: method.label.clone(),
                    description: String::new(),
                })
                .collect();
            return vec![];
        }
        let method = provider
            .methods
            .first()
            .map(|method| method.id.clone())
            .unwrap_or_default();
        self.start_login(provider.id, method)
    }
    pub fn start_login(&mut self, provider: String, method: String) -> Vec<Effect> {
        let mut effects = self.close_popup();
        self.login_provider = provider.clone();
        self.login_method = method.clone();
        self.popup(Dialog::Login, format!("Sign in to {provider}"));
        effects.push(self.global(
            rpc::PROVIDER_LOGIN_START,
            protocol::LoginStartParams {
                provider: provider.clone(),
                method,
            },
            Tag::LoginStart {
                provider,
                epoch: self.popup_epoch,
            },
        ));
        effects
    }
    pub fn show_discord(&mut self) {
        let status = self.discord.clone();
        let popup = self.popup(Dialog::Discord, format!("Discord · {}", status.state));
        popup.detail = format!(
            "{}\n{} · {} bridged channels\n{}",
            status.config_path, status.guild_name, status.channels, status.error
        );
        popup.choices = vec![
            Choice {
                id: if status.enabled {
                    "disconnect"
                } else {
                    "connect"
                }
                .into(),
                label: if status.enabled {
                    "Disconnect"
                } else {
                    "Connect"
                }
                .into(),
                description: String::new(),
            },
            Choice {
                id: "status".into(),
                label: "Refresh".into(),
                description: String::new(),
            },
            Choice {
                id: "config".into(),
                label: "Global settings".into(),
                description: status.config_path,
            },
        ];
    }
    pub fn show_web(&mut self) {
        let status = self.web.clone();
        let popup = self.popup(Dialog::Web, "Web UI");
        popup.detail = format!("{}\n{}", status.url, status.error);
        popup.choices = vec![Choice {
            id: if status.enabled { "off" } else { "on" }.into(),
            label: if status.enabled { "Disable" } else { "Enable" }.into(),
            description: String::new(),
        }];
        if status.enabled {
            popup.choices.push(Choice {
                id: "open".into(),
                label: "Open in browser".into(),
                description: String::new(),
            });
        }
    }
    pub fn show_sandbox(&mut self) {
        let status = self.sandbox.clone();
        let popup = self.popup(
            Dialog::Sandbox,
            format!(
                "Sandbox · {} · {}",
                if status.enabled { "on" } else { "off" },
                status.level
            ),
        );
        popup.detail = format!("{}\n{}", status.missing.join("\n"), status.why);
        popup.choices=vec![Choice{id:if status.enabled{"off"}else{"on"}.into(),label:if status.enabled{"Turn off"}else{"Turn on"}.into(),description:if status.enabled{"Commands run with your full access; nothing is hidden from them"}else{"Commands write only inside the channel's directories and cannot see your credentials"}.into()}];
        if !status.fix.is_empty() {
            popup.choices.push(Choice {
                id: "fix".into(),
                label: "Copy fix; then restart the daemon".into(),
                description: status.fix,
            });
        }
    }
    pub fn open_tab(&mut self, kind: Dialog) {
        let agent = self.channel.selected_agent();
        let title = match kind {
            Dialog::Async => "Async",
            Dialog::Todo => "Todo",
            Dialog::Mcp => "MCP",
            Dialog::Dirs => "Directories",
            _ => return,
        };
        let choices = match kind {
            Dialog::Async => agent
                .map(|agent| {
                    let mut choices = vec![];
                    let request = |request: &crate::event::ReplyRequest, waiting: bool| Choice {
                        id: format!("@agent:{}", request.from),
                        label: format!(
                            "{} {} @{}",
                            if waiting { "Waiting" } else { "Owes reply" },
                            request.id,
                            request.from_name
                        ),
                        description: request.text.clone(),
                    };
                    if !agent.awaiting_replies.is_empty() || !agent.pending_replies.is_empty() {
                        choices.extend(
                            agent
                                .awaiting_replies
                                .iter()
                                .map(|item| request(item, true)),
                        );
                    } else {
                        choices.extend(
                            self.channel
                                .agents
                                .iter()
                                .filter(|other| {
                                    agent.awaiting.iter().any(|id| id == &other.id)
                                        && other.state != protocol::AGENT_KILLED
                                })
                                .map(|other| Choice {
                                    id: format!("@agent:{}", other.id),
                                    label: format!("Waiting for {}", other.name),
                                    description: other.state.clone(),
                                }),
                        );
                    }
                    choices.extend(agent.jobs.iter().map(|job| Choice {
                        id: format!("@job:{}", job.id),
                        label: job.label.clone(),
                        description: format!("{} · {}", job.spec, job.progress),
                    }));
                    if !agent.awaiting_replies.is_empty() || !agent.pending_replies.is_empty() {
                        choices.extend(
                            agent
                                .pending_replies
                                .iter()
                                .map(|item| request(item, false)),
                        );
                    } else {
                        choices.extend(agent.due.iter().map(|id| Choice {
                            id: format!("@agent:{id}"),
                            label: format!(
                                    "Owes reply to {}",
                                    self.channel
                                        .agents
                                        .iter()
                                        .find(|other| &other.id == id)
                                        .map(|other| other.name.as_str())
                                        .unwrap_or(id)
                                ),
                            description: String::new(),
                        }));
                    }
                    choices
                })
                .unwrap_or_default(),
            Dialog::Todo => agent
                .map(|agent| {
                    agent
                        .todos
                        .iter()
                        .map(|todo| Choice {
                            id: todo.id.clone(),
                            label: format!("[{}] {}", todo.status, todo.text),
                            description: String::new(),
                        })
                        .collect()
                })
                .unwrap_or_default(),
            Dialog::Mcp => agent
                .map(|agent| {
                    agent
                        .mcp
                        .iter()
                        .map(|mcp| Choice {
                            id: mcp.name.clone(),
                            label: format!("{} · {}", mcp.name, mcp.state),
                            description: if self.mcp_open.contains(&mcp.name) {
                                format!("{} {}", mcp.error, mcp.tools.join(", "))
                            } else {
                                format!("{} · {} tools · Enter expands", mcp.error, mcp.tools.len())
                            },
                        })
                        .collect()
                })
                .unwrap_or_default(),
            Dialog::Dirs => {
                let mut dirs = vec![Choice {
                    id: self.channel.info.dir.clone(),
                    label: self.channel.info.dir.clone(),
                    description: "default directory · ctrl+s changes it".into(),
                }];
                dirs.extend(self.channel.info.dirs.iter().map(|dir| Choice {
                    id: dir.path.clone(),
                    label: dir.path.clone(),
                    description: format!("{} · ctrl+d removes", dir.source),
                }));
                dirs.push(Choice {
                    id: "@add".into(),
                    label: "Add directory…".into(),
                    description: String::new(),
                });
                dirs
            }
            _ => vec![],
        };
        self.popup(kind, title).choices = choices;
    }
    pub fn filtered_choices(&self) -> Vec<usize> {
        let Some(popup) = &self.popup else {
            return vec![];
        };
        let query = popup.input.text.to_lowercase();
        popup
            .choices
            .iter()
            .enumerate()
            .filter(|(_, choice)| {
                query.is_empty()
                    || format!("{} {} {}", choice.id, choice.label, choice.description)
                        .to_lowercase()
                        .contains(&query)
            })
            .map(|(index, _)| index)
            .collect()
    }
    pub fn popup_key(&mut self, key: &str, text: Option<&str>) -> Vec<Effect> {
        let Some(kind) = self.popup.as_ref().map(|popup| popup.kind) else {
            return vec![];
        };
        if key == "esc" {
            if matches!(kind, Dialog::DenyReason | Dialog::PermissionDirectory) {
                let popup = self.popup.as_ref().unwrap();
                self.permission_drafts.insert(
                    popup.context.clone(),
                    (
                        if kind == Dialog::DenyReason {
                            "deny"
                        } else {
                            "directory"
                        }
                        .into(),
                        popup.input.text.clone(),
                    ),
                );
                self.open_prompts(false, true);
                return vec![];
            }
            if kind == Dialog::QuestionText {
                self.save_custom_question();
                self.popup(Dialog::Questions, "Questions").context = self.active_prompt.clone();
                return vec![];
            }
            return self.close_popup();
        }
        if kind == Dialog::Questions {
            return self.question_key(key, text);
        }
        if kind == Dialog::Login {
            return match key {
                "o" => vec![Effect::OpenBrowser(self.login.url.clone())],
                "c" => vec![Effect::Copy(self.login.url.clone())],
                "enter" if self.status_error => {
                    self.start_login(self.login_provider.clone(), self.login_method.clone())
                }
                _ => vec![],
            };
        }
        if matches!(kind, Dialog::Usage | Dialog::Plan) {
            match key {
                "left" => {
                    self.popup.as_mut().unwrap().cursor =
                        self.popup.as_ref().unwrap().cursor.saturating_sub(1)
                }
                "right" => {
                    self.popup.as_mut().unwrap().cursor =
                        (self.popup.as_ref().unwrap().cursor + 1).min(3)
                }
                _ => return vec![],
            };
            self.popup_epoch = self.popup_epoch.wrapping_add(1);
            return vec![self.usage_request()];
        }
        let input = matches!(
            kind,
            Dialog::LoginKey
                | Dialog::NewName
                | Dialog::NewDirectory
                | Dialog::AddDirectory
                | Dialog::SetDirectory
                | Dialog::EditDirectory
                | Dialog::DenyReason
                | Dialog::PermissionDirectory
                | Dialog::QuestionText
        );
        if input {
            if key == "enter" {
                return self.popup_submit(false);
            }
            let popup = self.popup.as_mut().unwrap();
            if let Some(text) = text {
                popup.input.insert(text);
            } else {
                popup.input.key(key);
            }
            self.dirty = true;
            return vec![];
        }
        if kind == Dialog::Dirs && key == "a" {
            self.popup(Dialog::AddDirectory, "Add directory");
            return vec![];
        }
        if key == "ctrl+d" {
            let selected = self.selected_choice().map(|choice| choice.id.clone());
            if let Some(id) = selected {
                return match kind {
                    Dialog::Providers => {
                        self.popup(
                            Dialog::ConfirmLogout,
                            "Sign out of provider? · enter confirms",
                        )
                        .context = id;
                        vec![]
                    }
                    Dialog::Dirs if id != self.channel.info.dir && id != "@add" => {
                        if self
                            .channel
                            .info
                            .dirs
                            .iter()
                            .any(|dir| dir.path == id && dir.source == "config")
                        {
                            self.status("set by dirs in stavlos.json: remove it there", true);
                            return vec![];
                        }
                        vec![self.scoped(
                            rpc::CHANNEL_REMOVE_DIR,
                            protocol::ChannelDirParams {
                                channel: self.scope.channel.clone(),
                                dir: id,
                            },
                            Tag::Changed("directory removed".into()),
                        )]
                    }
                    _ => vec![],
                };
            }
        }
        if key == "ctrl+s" {
            if kind == Dialog::Dirs {
                let dir = self.channel.info.dir.clone();
                self.popup(Dialog::SetDirectory, "Default directory")
                    .input
                    .set(dir);
                return vec![];
            }
            return self.popup_submit(true);
        }
        if key == "enter" || key == " " && kind == Dialog::Permissions {
            return self.popup_submit(false);
        }
        let count = self.filtered_choices().len();
        let popup = self.popup.as_mut().unwrap();
        match key {
            "up" | "ctrl+p" | "k"
                if matches!(
                    kind,
                    Dialog::Dirs | Dialog::Async | Dialog::Todo | Dialog::Mcp | Dialog::Permissions
                ) || key != "k" =>
            {
                popup.cursor =
                    super::super::wrap_index(popup.cursor as i64 - 1, count as i64) as usize
            }
            "down" | "ctrl+n" | "j"
                if matches!(
                    kind,
                    Dialog::Dirs | Dialog::Async | Dialog::Todo | Dialog::Mcp | Dialog::Permissions
                ) || key != "j" =>
            {
                popup.cursor =
                    super::super::wrap_index(popup.cursor as i64 + 1, count as i64) as usize
            }
            "home" => popup.cursor = 0,
            "end" => popup.cursor = count.saturating_sub(1),
            "pageup" => popup.cursor = popup.cursor.saturating_sub(10),
            "pagedown" => popup.cursor = (popup.cursor + 10).min(count.saturating_sub(1)),
            _ => {
                if let Some(text) = text {
                    popup.input.insert(text);
                    popup.cursor = 0;
                } else {
                    popup.input.key(key);
                }
            }
        }
        self.dirty = true;
        vec![]
    }
    pub fn selected_choice(&self) -> Option<&Choice> {
        let popup = self.popup.as_ref()?;
        let indices = self.filtered_choices();
        popup
            .choices
            .get(*indices.get(popup.cursor.min(indices.len().saturating_sub(1)))?)
    }
    fn save_custom_question(&mut self) {
        if let Some(draft) = self.questions.get_mut(&self.active_prompt) {
            draft.custom[draft.index] = self.popup.as_ref().unwrap().input.text.clone();
            if let Some(prompt) = self.prompts.get(&draft.prompt) {
                draft.cursor = prompt.questions[draft.index].options.len() + 1;
            }
        }
    }
    pub fn popup_submit(&mut self, alternate: bool) -> Vec<Effect> {
        let popup = self.popup.as_ref().unwrap();
        let kind = popup.kind;
        let input = popup.input.text.trim().to_owned();
        let context = popup.context.clone();
        let agent = popup.agent.clone();
        let choice = self
            .selected_choice()
            .map(|choice| choice.id.clone())
            .unwrap_or_default();
        match kind {
            Dialog::Models => {
                if choice.is_empty() {
                    return vec![];
                }
                let effect = if alternate {
                    self.scoped(
                        rpc::CHANNEL_SET_MODEL,
                        protocol::ChannelSetModelParams {
                            channel: self.scope.channel.clone(),
                            model: choice,
                        },
                        Tag::Changed("channel default model updated".into()),
                    )
                } else {
                    if agent.is_empty() {
                        self.status("no agent selected", true);
                        return vec![];
                    }
                    self.scoped(
                        rpc::AGENT_SET_MODEL,
                        protocol::AgentSetModelParams {
                            agent,
                            model: choice,
                        },
                        Tag::Changed("model updated".into()),
                    )
                };
                let mut effects = self.close_popup();
                effects.push(effect);
                effects
            }
            Dialog::Variants => {
                let effect = self.scoped(
                    rpc::AGENT_SET_VARIANT,
                    protocol::AgentSetVariantParams {
                        agent,
                        variant: choice,
                    },
                    Tag::Changed("variant updated".into()),
                );
                let mut effects = self.close_popup();
                effects.push(effect);
                effects
            }
            Dialog::Roles => {
                let effect = self.scoped(
                    rpc::AGENT_SET_ROLE,
                    protocol::AgentSetRoleParams {
                        agent,
                        role: choice,
                    },
                    Tag::Changed("role updated".into()),
                );
                let mut effects = self.close_popup();
                effects.push(effect);
                effects
            }
            Dialog::Mode => {
                let effect = self.scoped(
                    rpc::CHANNEL_SET_MODE,
                    protocol::ChannelSetModeParams {
                        channel: self.scope.channel.clone(),
                        mode: choice,
                    },
                    Tag::Changed("permission mode updated".into()),
                );
                let mut effects = self.close_popup();
                effects.push(effect);
                effects
            }
            Dialog::Providers => self.choose_provider(&choice),
            Dialog::Methods => self.start_login(context, choice),
            Dialog::ConfirmLogout => {
                self.popup(Dialog::Providers, "Providers");
                vec![self.global(
                    rpc::PROVIDER_DISCONNECT,
                    protocol::ProviderRef { provider: context },
                    Tag::Changed("@providers".into()),
                )]
            }
            Dialog::Channels => {
                if choice.is_empty() {
                    return vec![];
                }
                vec![self.scoped(
                    rpc::CHANNEL_RESUME,
                    protocol::ChannelRef { channel: choice },
                    Tag::ChannelOpened {
                        agent: String::new(),
                    },
                )]
            }
            Dialog::Permissions => self.permission_choice(&choice),
            Dialog::DenyReason => self.answer(protocol::PromptReplyParams {
                id: context,
                answer: protocol::ANSWER_DENY.into(),
                reason: input,
                ..Default::default()
            }),
            Dialog::PermissionDirectory => {
                if input.is_empty() {
                    vec![]
                } else {
                    self.answer(protocol::PromptReplyParams {
                        id: context,
                        answer: protocol::ANSWER_ALLOW_ALWAYS.into(),
                        dir: input,
                        ..Default::default()
                    })
                }
            }
            Dialog::QuestionText => {
                self.save_custom_question();
                self.popup(Dialog::Questions, "Questions").context = context;
                vec![]
            }
            Dialog::LoginKey => {
                if input.is_empty() {
                    return vec![];
                }
                self.popup.as_mut().unwrap().input.clear();
                vec![self.global(
                    rpc::PROVIDER_LOGIN_KEY,
                    protocol::LoginKeyParams {
                        id: self.login_id.clone(),
                        key: input,
                    },
                    Tag::Changed(String::new()),
                )]
            }
            Dialog::Discord => {
                if choice == "config" {
                    self.open_configuration(true)
                } else {
                    self.command(&format!("/discord {choice}"))
                }
            }
            Dialog::Web => self.command(&format!("/web {choice}")),
            Dialog::Sandbox => {
                if choice == "fix" {
                    vec![Effect::Copy(self.sandbox.fix.clone())]
                } else {
                    self.command(&format!("/sandbox {choice}"))
                }
            }
            Dialog::Async => {
                let Some(id) = choice.strip_prefix("@agent:") else {
                    return vec![];
                };
                if id == "user" {
                    self.channel.chat = true;
                } else if self.channel.agents.iter().any(|agent| agent.id == id) {
                    self.channel.selected = id.into();
                    self.channel.chat = false;
                } else {
                    return vec![];
                }
                self.close_popup()
            }
            Dialog::Mcp => {
                if !self.mcp_open.insert(choice.clone()) {
                    self.mcp_open.remove(&choice);
                }
                self.open_tab(Dialog::Mcp);
                vec![]
            }
            Dialog::Dirs => {
                if choice == "@add" {
                    self.popup(Dialog::AddDirectory, "Add directory");
                } else if choice == self.channel.info.dir {
                    self.popup(Dialog::SetDirectory, "Default directory")
                        .input
                        .set(choice);
                } else if self
                    .channel
                    .info
                    .dirs
                    .iter()
                    .any(|dir| dir.path == choice && dir.source == "config")
                {
                    self.status("set by dirs in stavlos.json: change it there", true);
                } else if !choice.is_empty() {
                    let popup = self.popup(Dialog::EditDirectory, "Replace directory");
                    popup.context = choice.clone();
                    popup.input.set(choice);
                }
                vec![]
            }
            Dialog::EditDirectory => {
                if input.is_empty() {
                    return vec![];
                }
                let effect = self.scoped(
                    rpc::CHANNEL_ADD_DIR,
                    protocol::ChannelDirParams {
                        channel: self.scope.channel.clone(),
                        dir: input,
                    },
                    Tag::DirectoryAdded(context),
                );
                let mut effects = self.close_popup();
                effects.push(effect);
                effects
            }
            Dialog::SetDirectory => {
                let mut effects = self.close_popup();
                effects.extend(self.set_directory(&input));
                effects
            }
            Dialog::AddDirectory => {
                if input.is_empty() {
                    return vec![];
                }
                let effect = self.scoped(
                    rpc::CHANNEL_ADD_DIR,
                    protocol::ChannelDirParams {
                        channel: self.scope.channel.clone(),
                        dir: input,
                    },
                    Tag::Changed("directory added".into()),
                );
                let mut effects = self.close_popup();
                effects.push(effect);
                effects
            }
            Dialog::NewName => {
                let dir = self.channel.info.dir.clone();
                let popup = self.popup(Dialog::NewDirectory, "New channel · working directory");
                popup.context = input;
                popup.input.set(dir);
                vec![]
            }
            Dialog::NewDirectory => {
                let dir = match crate::config::effective::working_directory(
                    std::path::Path::new(&self.channel.info.dir),
                    &input,
                    &self.home,
                    &|key| std::env::var(key).ok(),
                ) {
                    Ok(dir) => dir,
                    Err(error) => {
                        self.status(error.to_string(), true);
                        return vec![];
                    }
                };
                vec![self.scoped(
                    rpc::CHANNEL_CREATE,
                    protocol::ChannelCreateParams {
                        name: context,
                        dir: dir.to_string_lossy().into_owned(),
                        ..Default::default()
                    },
                    Tag::ChannelOpened {
                        agent: String::new(),
                    },
                )]
            }
            _ => vec![],
        }
    }
}
