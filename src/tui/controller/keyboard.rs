use super::*;
use crate::protocol::methods as rpc;
pub struct SidebarRow {
    pub channel: String,
    pub agent: String,
    pub action: &'static str,
    pub label: String,
    pub description: String,
    pub depth: usize,
    pub state: String,
}
pub struct Completion {
    pub name: String,
    pub args: String,
    pub description: String,
    pub direct: bool,
    pub exact: bool,
}
impl Controller {
    pub fn completions(&self) -> Vec<Completion> {
        let input = &self.channel.input.text;
        if !super::super::commands::active(input) {
            return vec![];
        }
        let mut choices = super::super::commands::matches(input)
            .iter()
            .map(|command| Completion {
                name: command.name.clone(),
                args: command.args.clone(),
                description: command.desc.clone(),
                direct: command.direct,
                exact: command.name.eq_ignore_ascii_case(input)
                    || command.aliases.as_ref().is_some_and(|aliases| {
                        aliases
                            .iter()
                            .any(|alias| alias.eq_ignore_ascii_case(input))
                    }),
            })
            .collect::<Vec<_>>();
        let query = input.trim_start_matches('/').to_lowercase();
        for command in &self.custom_commands {
            let name = format!("/{}", command.name);
            if super::super::commands::find(&name).is_none()
                && command.name.to_lowercase().starts_with(&query)
            {
                choices.push(Completion {
                    exact: name.eq_ignore_ascii_case(input),
                    name,
                    args: String::new(),
                    description: command.description.clone(),
                    direct: true,
                });
            }
        }
        choices
    }
    pub fn mentions(&self) -> Vec<String> {
        if !self.channel.chat {
            return vec![];
        }
        let before = &self.channel.input.text[..self.channel.input.cursor];
        let word = before
            .rsplit(char::is_whitespace)
            .next()
            .unwrap_or_default();
        let Some(query) = word.strip_prefix('@') else {
            return vec![];
        };
        self.channel
            .agents
            .iter()
            .filter(|agent| {
                agent.state != protocol::AGENT_KILLED
                    && agent.name.to_lowercase().starts_with(&query.to_lowercase())
            })
            .map(|agent| agent.name.clone())
            .collect()
    }
    pub fn sidebar_rows(&self) -> Vec<SidebarRow> {
        let row = |action, label| SidebarRow {
            action,
            label,
            channel: String::new(),
            agent: String::new(),
            description: String::new(),
            depth: 0,
            state: String::new(),
        };
        let mut out = vec![row("new", "Channels  +".into())];
        let mut infos = self.catalog.iter().collect::<Vec<_>>();
        if !infos.iter().any(|info| info.id == self.scope.channel) {
            infos.push(&self.channel.info);
        }
        infos.sort_by(|left, right| (&left.name, &left.id).cmp(&(&right.name, &right.id)));
        for info in infos {
            out.push(SidebarRow {
                channel: info.id.clone(),
                agent: String::new(),
                action: "channel",
                label: format!("#{}", info.name),
                description: info.title.clone(),
                depth: 0,
                state: info.state.clone(),
            });
            if self.tree_closed.contains(&info.id) {
                continue;
            }
            let agents = if info.id == self.scope.channel {
                Some(&self.channel.agents)
            } else {
                self.trees.get(&info.id)
            };
            if let Some(agents) = agents {
                for agent in agents {
                    if agent.state == protocol::AGENT_KILLED && !self.tree_all.contains(&info.id) {
                        continue;
                    }
                    out.push(SidebarRow {
                        channel: info.id.clone(),
                        agent: agent.id.clone(),
                        action: "agent",
                        label: agent.name.clone(),
                        description: agent.role.clone(),
                        depth: agent.depth.max(0) as usize + 1,
                        state: agent.state.clone(),
                    });
                }
            }
        }
        out.push(row(
            "permissions",
            format!("Permissions ({})", self.pending(false, true).len()),
        ));
        out.push(row(
            "questions",
            format!("Questions ({})", self.pending(true, true).len()),
        ));
        out.push(row("providers", "Providers".into()));
        out.push(row(
            "web",
            format!("Web UI · {}", if self.web.enabled { "on" } else { "off" }),
        ));
        out.push(row("discord", format!("Discord · {}", self.discord.state)));
        out.push(row(
            "sandbox",
            format!(
                "Sandbox · {}",
                if self.sandbox.enabled { "on" } else { "off" }
            ),
        ));
        out.push(row("tokens", "Tokens".into()));
        out.push(row("cost", "Cost".into()));
        out.push(row("settings", "Settings".into()));
        out
    }
    pub fn sidebar_select(&mut self, index: usize) -> Vec<Effect> {
        let rows = self.sidebar_rows();
        let Some(row) = rows.get(index) else {
            return vec![];
        };
        self.sidebar_cursor = index;
        match row.action {
            "new" => {
                self.popup(Dialog::NewName, "New channel · name (optional)");
                vec![]
            }
            "channel" | "agent" => {
                if row.channel != self.scope.channel {
                    return vec![self.scoped(
                        rpc::CHANNEL_RESUME,
                        protocol::ChannelRef {
                            channel: row.channel.clone(),
                        },
                        Tag::ChannelOpened {
                            agent: row.agent.clone(),
                        },
                    )];
                }
                self.channel.chat = row.agent.is_empty();
                if !row.agent.is_empty() {
                    self.channel.selected = row.agent.clone();
                }
                self.focus = Focus::Input;
                self.channel.scroll = 0;
                self.channel.follow = true;
                self.dirty = true;
                vec![]
            }
            "permissions" => {
                self.open_prompts(false, true);
                vec![]
            }
            "questions" => {
                self.open_prompts(true, false);
                vec![]
            }
            "tokens" | "cost" => self.command(&format!("/{} system", row.action)),
            _ => self.command(&format!("/{}", row.action)),
        }
    }
    pub fn move_agent(&mut self, delta: i64) {
        let agents = &self.channel.agents;
        if agents.is_empty() {
            return;
        }
        let current = agents
            .iter()
            .position(|agent| agent.id == self.channel.selected)
            .unwrap_or_default();
        let index = super::super::wrap_index(current as i64 + delta, agents.len() as i64) as usize;
        self.channel.selected = agents[index].id.clone();
        self.channel.chat = false;
        self.channel.scroll = 0;
        self.channel.follow = true;
        self.dirty = true;
    }
    pub fn scroll(&mut self, delta: i64) {
        if delta < 0 {
            self.channel.scroll = self
                .channel
                .scroll
                .saturating_add(delta.unsigned_abs() as usize);
            self.channel.follow = false;
        } else {
            self.channel.scroll = self.channel.scroll.saturating_sub(delta as usize);
            self.channel.follow = self.channel.scroll == 0;
        }
        self.dirty = true;
    }
    pub fn toggle_item(&mut self, item: i64) {
        let view = self.channel.view_id().to_owned();
        let expanded = self.channel.expanded.entry(view).or_default();
        if !expanded.insert(item) {
            expanded.remove(&item);
        }
        self.channel.chat_cursor = item;
        self.dirty = true;
    }
    fn history_move(&mut self, delta: i64) {
        let count = self.channel.history.len();
        if count == 0 {
            return;
        }
        if self.channel.history_index == count {
            self.channel.history_draft = self.channel.input.text.clone();
        }
        let next = (self.channel.history_index as i64 + delta).clamp(0, count as i64) as usize;
        if next == self.channel.history_index {
            return;
        }
        self.channel.history_index = next;
        self.channel.input.set(if next == count {
            self.channel.history_draft.clone()
        } else {
            self.channel.history[next].clone()
        });
    }
    pub fn key(&mut self, key: &str, text: Option<&str>) -> Vec<Effect> {
        self.dirty = true;
        if self.config.is_some() {
            return self.config_key(key, text);
        }
        if self.popup.is_some() && key != "ctrl+c" && key != "ctrl+space" {
            return self.popup_key(key, text);
        }
        if key == "ctrl+c" {
            if self
                .quit_armed
                .is_some_and(|at| (self.now.0 - at.0).num_milliseconds() <= 3000)
            {
                self.quit = true;
                return vec![];
            }
            self.quit_armed = Some(self.now);
            self.cancel_armed = None;
            self.channel.input.clear();
            let effects = self.close_popup();
            self.status("press ctrl+c again to quit", true);
            return effects;
        }
        self.quit_armed = None;
        if key != "esc" {
            self.cancel_armed = None;
        }
        if matches!(key, "ctrl+space" | "ctrl+@") {
            self.focus = Focus::Input;
            return self.close_popup();
        }
        match key {
            "ctrl+b" => return self.command("/tree"),
            "ctrl+n" => {
                self.move_agent(1);
                return vec![];
            }
            "ctrl+p" => {
                self.move_agent(-1);
                return vec![];
            }
            _ => {}
        }
        let completion = self.focus == Focus::Input
            && (!self.completions().is_empty() || !self.mentions().is_empty());
        if key == "backtab" || key == "tab" && !completion {
            let mut focuses = vec![Focus::Input, Focus::Chat];
            if self.show_sidebar {
                focuses.push(Focus::Sidebar);
            }
            focuses.extend([Focus::Tabs, Focus::Meta]);
            let index = focuses
                .iter()
                .position(|focus| focus == &self.focus)
                .unwrap_or_default();
            self.focus = focuses[super::super::wrap_index(
                index as i64 + if key == "backtab" { -1 } else { 1 },
                focuses.len() as i64,
            ) as usize];
            return vec![];
        }
        match self.focus {
            Focus::Sidebar => {
                let count = self.sidebar_rows().len();
                match key {
                    "esc" => self.focus = Focus::Input,
                    "up" | "k" => {
                        self.sidebar_cursor =
                            super::super::wrap_index(self.sidebar_cursor as i64 - 1, count as i64)
                                as usize
                    }
                    "down" | "j" => self.sidebar_cursor = (self.sidebar_cursor + 1) % count,
                    "enter" | " " => return self.sidebar_select(self.sidebar_cursor),
                    "left" => {
                        if let Some(row) = self
                            .sidebar_rows()
                            .get(self.sidebar_cursor)
                            .filter(|row| !row.channel.is_empty())
                            && !self.tree_closed.insert(row.channel.clone())
                        {
                            self.tree_closed.remove(&row.channel);
                        }
                    }
                    "right" => {
                        if self.sidebar_cursor == 0 {
                            self.popup(Dialog::NewName, "New channel · name (optional)");
                        } else {
                            self.open_tab(Dialog::Dirs);
                        }
                    }
                    "a" => {
                        if let Some(row) = self
                            .sidebar_rows()
                            .get(self.sidebar_cursor)
                            .filter(|row| !row.channel.is_empty())
                            && !self.tree_all.insert(row.channel.clone())
                        {
                            self.tree_all.remove(&row.channel);
                        }
                    }
                    _ => {}
                }
                return vec![];
            }
            Focus::Tabs => {
                match key {
                    "left" | "h" => self.tab_cursor = (self.tab_cursor + 5) % 6,
                    "right" | "l" => self.tab_cursor = (self.tab_cursor + 1) % 6,
                    "enter" | " " => match self.tab_cursor {
                        0 => self.open_tab(Dialog::Async),
                        1 => self.open_tab(Dialog::Todo),
                        2 => self.open_tab(Dialog::Mcp),
                        3 => self.open_prompts(true, false),
                        4 => self.open_prompts(false, true),
                        _ => self.open_tab(Dialog::Dirs),
                    },
                    "esc" => self.focus = Focus::Input,
                    _ => {}
                }
                return vec![];
            }
            Focus::Meta => {
                match key {
                    "left" | "h" => self.meta_cursor = (self.meta_cursor + 3) % 4,
                    "right" | "l" => self.meta_cursor = (self.meta_cursor + 1) % 4,
                    "enter" | " " => {
                        return self.command(
                            ["/roles", "/models", "/variants", "/mode"][self.meta_cursor],
                        );
                    }
                    "esc" => self.focus = Focus::Input,
                    _ => {}
                }
                return vec![];
            }
            Focus::Chat => {
                match key {
                    "up" | "k" => {
                        self.channel.chat_cursor = self.channel.chat_cursor.saturating_sub(1)
                    }
                    "down" | "j" => {
                        let last = self
                            .channel
                            .view()
                            .map(|view| view.item_count().saturating_sub(1) as i64)
                            .unwrap_or_default();
                        self.channel.chat_cursor = (self.channel.chat_cursor + 1).min(last);
                    }
                    "enter" | " " => {
                        if let Some(prompt) = self.pending(true, false).first() {
                            self.active_prompt = prompt.id.clone();
                            self.open_prompts(true, false);
                        } else if !self.pending(false, false).is_empty() {
                            self.open_prompts(false, false);
                        } else {
                            self.toggle_item(self.channel.chat_cursor);
                        }
                    }
                    "pageup" => self.scroll(-(self.height as i64 / 2)),
                    "pagedown" => self.scroll(self.height as i64 / 2),
                    "home" | "ctrl+home" => {
                        self.channel.scroll = usize::MAX;
                        self.channel.follow = false;
                    }
                    "end" | "ctrl+end" => {
                        self.channel.scroll = 0;
                        self.channel.follow = true;
                    }
                    "esc" => self.focus = Focus::Input,
                    _ => {}
                }
                return vec![];
            }
            _ => {}
        }
        if key == "pageup" {
            self.scroll(-(self.height as i64 / 2));
            return vec![];
        }
        if key == "pagedown" {
            self.scroll(self.height as i64 / 2);
            return vec![];
        }
        let mentions = self.mentions();
        if !mentions.is_empty() && matches!(key, "up" | "down" | "tab" | "enter") {
            match key {
                "up" => {
                    self.mention_cursor = super::super::wrap_index(
                        self.mention_cursor as i64 - 1,
                        mentions.len() as i64,
                    ) as usize
                }
                "down" => self.mention_cursor = (self.mention_cursor + 1) % mentions.len(),
                _ => {
                    let before = &self.channel.input.text[..self.channel.input.cursor];
                    let at = before.rfind('@').unwrap();
                    let end = self.channel.input.cursor;
                    let replacement = format!(
                        "@{} ",
                        mentions[self.mention_cursor.min(mentions.len() - 1)]
                    );
                    self.channel.input.text.replace_range(at..end, &replacement);
                    self.channel.input.cursor = at + replacement.len();
                    self.mention_cursor = 0;
                }
            }
            return vec![];
        }
        let choices = self.completions();
        if !choices.is_empty() && matches!(key, "up" | "down" | "tab" | "enter") {
            match key {
                "up" => {
                    self.palette_cursor = super::super::wrap_index(
                        self.palette_cursor as i64 - 1,
                        choices.len() as i64,
                    ) as usize
                }
                "down" => self.palette_cursor = (self.palette_cursor + 1) % choices.len(),
                _ => {
                    let choice = &choices[self.palette_cursor.min(choices.len() - 1)];
                    if key == "enter" && choice.direct && (choice.exact || choice.args.is_empty()) {
                        let name = choice.name.clone();
                        self.channel.input.clear();
                        self.channel.push_history(name.clone());
                        self.palette_cursor = 0;
                        return self.command(&name);
                    }
                    self.channel.input.set(format!(
                        "{}{}",
                        choice.name,
                        if choice.args.is_empty() { "" } else { " " }
                    ));
                    self.palette_cursor = 0;
                }
            }
            return vec![];
        }
        match key {
            "enter" => return self.submit(),
            "up" | "down" => {
                let width = self
                    .width
                    .saturating_sub(if self.show_sidebar && self.width >= 80 {
                        31
                    } else {
                        2
                    })
                    .max(1) as usize;
                let delta = if key == "up" { -1 } else { 1 };
                if !self.channel.input.visual_vertical(delta, width) {
                    self.history_move(delta as i64);
                }
            }
            "esc" => {
                if !self.channel.input.text.is_empty() {
                    self.channel.input.clear();
                    self.palette_cursor = 0;
                    self.cancel_armed = None;
                } else if self
                    .channel
                    .selected_agent()
                    .is_some_and(|agent| protocol::agent_busy(&agent.state))
                {
                    if self
                        .cancel_armed
                        .is_some_and(|at| (self.now.0 - at.0).num_milliseconds() <= 3000)
                    {
                        self.cancel_armed = None;
                        return self.send(protocol::KIND_CANCEL, String::new());
                    }
                    self.cancel_armed = Some(self.now);
                    self.status("press esc again to cancel the selected agent's turn", true);
                }
            }
            _ => {
                if let Some(text) = text {
                    self.channel.input.insert(text);
                } else {
                    self.channel.input.key(key);
                }
            }
        }
        vec![]
    }
}
