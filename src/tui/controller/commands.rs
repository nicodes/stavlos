use super::*;
use crate::protocol::methods as rpc;
impl Controller {
    pub fn send(&self, kind: &str, text: String) -> Vec<Effect> {
        if self.channel.selected.is_empty() {
            return vec![];
        }
        vec![self.scoped(
            rpc::AGENT_SEND,
            protocol::AgentSendParams {
                agent: self.channel.selected.clone(),
                kind: kind.into(),
                text,
            },
            Tag::Changed(String::new()),
        )]
    }
    pub fn submit(&mut self) -> Vec<Effect> {
        let text = self.channel.input.text.trim().to_owned();
        if text.is_empty() {
            return vec![];
        }
        self.channel.input.clear();
        self.channel.push_history(text.clone());
        self.channel.follow = true;
        self.channel.scroll = 0;
        self.dirty = true;
        if text.starts_with('/') {
            return self.command(&text);
        }
        if self.channel.chat {
            vec![self.scoped(
                rpc::CHANNEL_POST,
                protocol::ChannelPostParams {
                    channel: self.scope.channel.clone(),
                    text,
                },
                Tag::Changed(String::new()),
            )]
        } else {
            self.send(protocol::KIND_STEER, text)
        }
    }
    pub fn command(&mut self, text: &str) -> Vec<Effect> {
        let mut words = text.splitn(2, char::is_whitespace);
        let name = words.next().unwrap_or_default().to_lowercase();
        let rest = words.next().unwrap_or_default().trim();
        let builtin = super::super::commands::find(&name);
        let Some(builtin) = builtin else {
            if !rest.is_empty() {
                self.status("custom commands do not take arguments", true);
                return vec![];
            }
            return vec![self.scoped(
                rpc::COMMAND_RUN,
                protocol::CommandRunParams {
                    channel: self.scope.channel.clone(),
                    name: name.trim_start_matches('/').into(),
                    agent: if self.channel.chat {
                        String::new()
                    } else {
                        self.channel.selected.clone()
                    },
                },
                Tag::Changed(String::new()),
            )];
        };
        if matches!(
            builtin.name.as_str(),
            "/queue" | "/variants" | "/roles" | "/compact"
        ) && self.channel.selected.is_empty()
        {
            self.status("no agent selected", true);
            return vec![];
        }
        match builtin.name.as_str() {
            "/queue" => {
                if rest.is_empty() {
                    self.status("usage: /queue <text>", true);
                    vec![]
                } else {
                    self.send(protocol::KIND_PROMPT, rest.into())
                }
            }
            "/providers" => {
                self.popup(
                    Dialog::Providers,
                    "Providers · enter connects · ctrl+d signs out",
                );
                vec![self.global(
                    rpc::PROVIDER_LIST,
                    protocol::None {},
                    Tag::Providers {
                        open: true,
                        jump: rest.to_lowercase(),
                        epoch: self.popup_epoch,
                    },
                )]
            }
            "/models" => {
                self.popup(
                    Dialog::Models,
                    "Models · enter sets agent · ctrl+s sets channel default",
                );
                vec![self.scoped(
                    rpc::MODEL_LIST,
                    protocol::ModelListParams::default(),
                    Tag::Models(self.popup_epoch),
                )]
            }
            "/variants" => {
                if !rest.is_empty() {
                    let variant = if rest == "default" { "" } else { rest };
                    return vec![self.scoped(
                        rpc::AGENT_SET_VARIANT,
                        protocol::AgentSetVariantParams {
                            agent: self.channel.selected.clone(),
                            variant: variant.into(),
                        },
                        Tag::Changed("variant updated".into()),
                    )];
                }
                let model = self
                    .channel
                    .selected_agent()
                    .map(|agent| agent.model.clone())
                    .unwrap_or_default();
                let agent = self.channel.selected.clone();
                let popup = self.popup(Dialog::Variants, "Variant");
                popup.agent = agent;
                vec![self.scoped(
                    rpc::VARIANTS,
                    protocol::VariantsParams { model },
                    Tag::Variants(self.popup_epoch),
                )]
            }
            "/roles" => {
                if rest.is_empty() {
                    vec![self.scoped(
                        rpc::ROLES,
                        protocol::RolesParams {
                            channel: self.scope.channel.clone(),
                        },
                        Tag::Roles(false),
                    )]
                } else {
                    vec![self.scoped(
                        rpc::AGENT_SET_ROLE,
                        protocol::AgentSetRoleParams {
                            agent: self.channel.selected.clone(),
                            role: rest.to_lowercase(),
                        },
                        Tag::Changed("role updated".into()),
                    )]
                }
            }
            "/mode" => {
                let popup = self.popup(Dialog::Mode, "Permission mode");
                popup.choices = [protocol::MODE_ASK, protocol::MODE_AUTO, protocol::MODE_YOLO]
                    .iter()
                    .map(|mode| Choice {
                        id: (*mode).into(),
                        label: (*mode).into(),
                        description: protocol::mode_summary(mode).into(),
                    })
                    .collect();
                vec![]
            }
            "/auto" | "/yolo" => {
                let mode = builtin.name.trim_start_matches('/');
                let enabled = match rest {
                    "" => self.channel.info.mode != mode,
                    "on" => true,
                    "off" => false,
                    _ => {
                        self.status(format!("usage: {} [on|off]", builtin.name), true);
                        return vec![];
                    }
                };
                vec![self.scoped(
                    rpc::CHANNEL_SET_MODE,
                    protocol::ChannelSetModeParams {
                        channel: self.scope.channel.clone(),
                        mode: if enabled { mode } else { protocol::MODE_ASK }.into(),
                    },
                    Tag::Changed("permission mode updated".into()),
                )]
            }
            "/channels" => {
                let choices = self
                    .catalog
                    .iter()
                    .filter(|info| info.id != self.scope.channel)
                    .map(|info| Choice {
                        id: info.id.clone(),
                        label: format!("#{}", info.name),
                        description: info.dir.clone(),
                    })
                    .collect();
                self.popup(Dialog::Channels, "Channels").choices = choices;
                vec![self.catalog_request()]
            }
            "/dir" => {
                if rest.is_empty() {
                    self.open_tab(Dialog::Dirs);
                    vec![]
                } else {
                    self.set_directory(rest)
                }
            }
            "/sandbox" => {
                self.popup(Dialog::Sandbox, "Sandbox · checking…");
                self.sandbox_epoch = self.sandbox_epoch.wrapping_add(1);
                let tag = Tag::Sandbox(true, self.sandbox_epoch);
                match rest {
                    "" => vec![self.global(rpc::SANDBOX_STATUS_METHOD, protocol::None {}, tag)],
                    "on" | "off" => vec![self.global(
                        rpc::SANDBOX_SET,
                        protocol::SandboxSetParams {
                            enabled: rest == "on",
                        },
                        tag,
                    )],
                    _ => {
                        self.status("usage: /sandbox [on|off]", true);
                        vec![]
                    }
                }
            }
            "/web" => {
                self.web_epoch = self.web_epoch.wrapping_add(1);
                let method = match rest {
                    "" | "status" => rpc::WEB_STATUS_METHOD,
                    "on" | "enable" => rpc::WEB_ENABLE,
                    "off" | "disable" => rpc::WEB_DISABLE,
                    "open" => rpc::WEB_OPEN,
                    _ => {
                        self.status("usage: /web [on|off|open]", true);
                        return vec![];
                    }
                };
                if rest != "open" {
                    self.popup(Dialog::Web, "Web UI · checking…");
                }
                vec![self.global(
                    method,
                    protocol::None {},
                    Tag::Web(rest != "open", self.web_epoch),
                )]
            }
            "/discord" => {
                self.popup(Dialog::Discord, "Discord · checking…");
                self.discord_epoch = self.discord_epoch.wrapping_add(1);
                let method = match rest {
                    "" | "status" => rpc::DISCORD_STATUS_METHOD,
                    "on" | "connect" => rpc::DISCORD_CONNECT,
                    "off" | "disconnect" => rpc::DISCORD_DISCONNECT,
                    _ => {
                        self.status("usage: /discord [connect|disconnect]", true);
                        return vec![];
                    }
                };
                vec![self.global(
                    method,
                    protocol::None {},
                    Tag::Discord(true, self.discord_epoch),
                )]
            }
            "/rename" => {
                if rest.is_empty() {
                    self.status("usage: /rename <name>", true);
                    vec![]
                } else {
                    vec![self.scoped(
                        rpc::CHANNEL_RENAME,
                        protocol::ChannelRenameParams {
                            channel: self.scope.channel.clone(),
                            name: rest.trim_start_matches('#').into(),
                        },
                        Tag::Changed("channel renamed".into()),
                    )]
                }
            }
            "/tree" => {
                self.show_sidebar = !self.show_sidebar;
                self.focus = if self.show_sidebar {
                    Focus::Sidebar
                } else {
                    Focus::Input
                };
                vec![]
            }
            "/chat" => {
                self.channel.chat = true;
                self.focus = Focus::Input;
                self.channel.follow = true;
                vec![]
            }
            "/tokens" | "/cost" => {
                if !rest.is_empty() && rest != "system" {
                    self.status(format!("usage: {} [system]", builtin.name), true);
                    return vec![];
                }
                let context = if rest == "system" {
                    String::new()
                } else {
                    self.scope.channel.clone()
                };
                let agent = if rest == "system" || self.channel.chat {
                    String::new()
                } else {
                    self.channel.selected.clone()
                };
                self.usage = None;
                let popup = self.popup(
                    Dialog::Usage,
                    format!("{} over time", builtin.name.trim_start_matches('/')),
                );
                popup.context = context;
                popup.agent = agent;
                popup.cursor = 1;
                vec![self.usage_request()]
            }
            "/recap" => {
                if rest.is_empty() {
                    self.status(
                        format!("recap: {} minutes (0 is off)", self.channel.info.recap),
                        false,
                    );
                    return vec![];
                }
                let minutes = if rest == "off" {
                    0
                } else {
                    match rest.parse::<i64>() {
                        Ok(n) if n >= 0 => n,
                        _ => {
                            self.status("usage: /recap [minutes|off]", true);
                            return vec![];
                        }
                    }
                };
                vec![self.scoped(
                    rpc::CHANNEL_SET_RECAP,
                    protocol::ChannelSetRecapParams {
                        channel: self.scope.channel.clone(),
                        minutes,
                    },
                    Tag::Changed("recap updated".into()),
                )]
            }
            "/plan" => {
                let provider = if rest.is_empty() {
                    self.plans
                        .first()
                        .map(|plan| plan.provider.clone())
                        .unwrap_or_default()
                } else {
                    rest.into()
                };
                if provider.is_empty() {
                    self.status("no plan usage has been reported", false);
                    return vec![];
                }
                self.plan_series = None;
                let popup = self.popup(Dialog::Plan, "Plan usage over time");
                popup.context = provider;
                popup.cursor = 1;
                vec![self.usage_request()]
            }
            "/history" => self.load_history(),
            "/compact" => vec![self.scoped(
                rpc::AGENT_COMPACT,
                protocol::AgentCompactParams {
                    agent: self.channel.selected.clone(),
                },
                Tag::Changed(String::new()),
            )],
            "/help" => {
                self.hide_keys = !self.hide_keys;
                vec![]
            }
            "/settings" => {
                if !matches!(rest, "" | "project" | "system") {
                    self.status("usage: /settings [project|system]", true);
                    vec![]
                } else {
                    self.open_configuration(rest == "system")
                }
            }
            _ => unreachable!("built-in registry and command dispatch agree"),
        }
    }
    pub fn set_directory(&mut self, dir: &str) -> Vec<Effect> {
        match crate::config::effective::working_directory(
            std::path::Path::new(&self.channel.info.dir),
            dir,
            &self.home,
            &|key| std::env::var(key).ok(),
        ) {
            Ok(dir) => vec![self.scoped(
                rpc::CHANNEL_SET_DIR,
                protocol::ChannelDirParams {
                    channel: self.scope.channel.clone(),
                    dir: dir.to_string_lossy().into_owned(),
                },
                Tag::DirectoryChanged,
            )],
            Err(error) => {
                self.status(error.to_string(), true);
                vec![]
            }
        }
    }
    pub fn usage_request(&self) -> Effect {
        let popup = self.popup.as_ref().unwrap();
        let hours = [1, 24, 168, 720][popup.cursor.min(3)];
        let from = Time(self.now.0 - chrono::TimeDelta::hours(hours));
        if popup.kind == Dialog::Plan {
            self.global(
                rpc::PLAN_SERIES,
                protocol::PlanSeriesParams {
                    provider: popup.context.clone(),
                    from,
                    to: self.now,
                    buckets: 80,
                },
                Tag::PlanSeries(self.popup_epoch),
            )
        } else {
            self.scoped(
                rpc::USAGE_SERIES,
                protocol::UsageSeriesParams {
                    channel: popup.context.clone(),
                    agent: popup.agent.clone(),
                    from,
                    to: self.now,
                    buckets: 80,
                },
                Tag::Usage(self.popup_epoch),
            )
        }
    }
}
