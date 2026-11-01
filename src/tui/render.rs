//! Ratatui draws only visible cached transcript rows; raw data never becomes terminal commands.
use super::{
    controller::{Controller, Dialog, Focus},
    editor::Editor,
    transcript::{self, Kind, Tone, Visibility},
};
use ratatui::{
    Frame,
    layout::{Constraint, Direction, Layout, Rect},
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Clear, Paragraph, Sparkline},
};
use std::collections::BTreeMap;
use unicode_segmentation::UnicodeSegmentation;
use unicode_width::UnicodeWidthStr;
const ACCENT: Color = Color::Rgb(91, 141, 239);
const MUTED: Color = Color::Rgb(138, 143, 152);
const GOOD: Color = Color::Rgb(61, 214, 140);
const WARN: Color = Color::Rgb(245, 165, 36);
const BAD: Color = Color::Rgb(242, 109, 109);
#[derive(Clone)]
pub struct Row {
    pub text: String,
    pub style: Style,
    pub item: i64,
}
struct Cached {
    revision: u64,
    width: u16,
    expanded: bool,
    minute: i64,
    rows: Vec<Row>,
}
#[derive(Default)]
pub struct Renderer {
    cache: BTreeMap<(String, String), Vec<Cached>>,
    pub hit: HitMap,
    pub selection: Option<((u16, u16), (u16, u16))>,
}
#[derive(Default)]
pub struct HitMap {
    pub sidebar: Rect,
    pub sidebar_top: usize,
    pub chat: Rect,
    pub input: Rect,
    pub tabs: Rect,
    pub meta: Rect,
    pub popup: Rect,
    pub rows: Vec<Row>,
    pub chat_offset: usize,
    pub total: usize,
}
fn safe(text: &str) -> String {
    crate::textsafe::visible(&crate::textsafe::clean(text))
}
fn focused(focus: bool) -> Style {
    Style::default().fg(if focus { ACCENT } else { MUTED })
}
fn block(title: impl Into<String>, active: bool) -> Block<'static> {
    Block::default()
        .borders(Borders::ALL)
        .border_style(focused(active))
        .title(title.into())
}
pub fn wrap(text: &str, width: u16) -> Vec<String> {
    let width = usize::from(width.max(1));
    let mut rows = vec![];
    for line in text.split('\n') {
        let mut row = String::new();
        let mut used = 0;
        for grapheme in line.graphemes(true) {
            let cols = UnicodeWidthStr::width(grapheme);
            if used + cols > width && !row.is_empty() {
                rows.push(std::mem::take(&mut row));
                used = 0;
            }
            if cols > width {
                row.push('�');
                used += 1;
            } else {
                row.push_str(grapheme);
                used += cols;
            }
        }
        rows.push(row);
    }
    rows
}
fn line_rows(
    lines: &[transcript::Line],
    width: u16,
    expanded: bool,
    now: crate::wire::Time,
) -> Vec<Row> {
    let mut out = vec![];
    for line in lines {
        if matches!(line.vis, Visibility::Collapsed) && expanded
            || matches!(line.vis, Visibility::Expanded) && !expanded
        {
            continue;
        }
        let mut style = Style::default();
        style = match line.kind {
            Kind::Heading | Kind::Label => style.add_modifier(Modifier::BOLD),
            Kind::Dim
            | Kind::Tool
            | Kind::ToolOut
            | Kind::Notice
            | Kind::Think
            | Kind::ToolNote
            | Kind::Model => style.fg(MUTED),
            Kind::Finished => style.fg(GOOD),
            Kind::Error => style.fg(BAD),
            Kind::Code => style.fg(Color::Cyan),
            _ => style,
        };
        if line.error || line.tone == Tone::Error {
            style = style.fg(BAD);
        } else if line.running || line.tone == Tone::Working {
            style = style.fg(WARN);
        }
        let mut prefix = if line.lead {
            match line.block {
                transcript::Block::User => "› ",
                transcript::Block::Steer => "↪ ",
                transcript::Block::Child => "⑂ ",
                _ => "",
            }
            .to_owned()
        } else {
            String::new()
        };
        prefix.push_str(&" ".repeat((line.indent.max(0) as usize).min(30)));
        if !line.glyph.is_empty() {
            prefix.push_str(&safe(&line.glyph));
            prefix.push(' ');
        }
        if !line.agent.is_empty() {
            prefix.push_str(&safe(&line.agent));
            prefix.push_str(": ");
        }
        let mut text = format!("{prefix}{}", safe(&line.text));
        if !line.suffix.is_empty() {
            text.push_str("  ");
            text.push_str(&safe(&line.suffix));
        }
        if line.lead && line.at.0.timestamp() > 0 {
            text.push_str("  ");
            text.push_str(&super::format::ago(line.at.0, now.0));
        }
        if line.kind == Kind::Rule {
            text = "─".repeat(width as usize);
        }
        for text in wrap(&text, width) {
            out.push(Row {
                text,
                style,
                item: line.item,
            });
        }
    }
    out
}
impl Renderer {
    pub fn draw(&mut self, frame: &mut Frame, controller: &mut Controller) {
        let size = frame.area();
        controller.width = size.width;
        controller.height = size.height;
        self.hit = HitMap::default();
        if size.width < 12 || size.height < 6 {
            frame.render_widget(Paragraph::new("Enlarge the terminal"), size);
            return;
        }
        let columns = Layout::default()
            .direction(Direction::Horizontal)
            .constraints(if controller.show_sidebar && size.width >= 80 {
                vec![Constraint::Length(29), Constraint::Min(1)]
            } else {
                vec![Constraint::Length(0), Constraint::Min(1)]
            })
            .split(size);
        if columns[0].width > 0 {
            self.sidebar(frame, controller, columns[0]);
        }
        let input_height = (wrap(
            &controller.channel.input.display(),
            columns[1].width.saturating_sub(9),
        )
        .len() as u16
            + 2)
        .clamp(3, (size.height / 2).clamp(3, 20));
        let chunks = Layout::vertical([
            Constraint::Length(1),
            Constraint::Min(1),
            Constraint::Length(1),
            Constraint::Length(input_height),
            Constraint::Length(1),
            Constraint::Length(if controller.hide_keys { 0 } else { 1 }),
            Constraint::Length(1),
        ])
        .split(columns[1]);
        let title = if controller.channel.chat {
            format!("#{} · channel chat", controller.channel.info.name)
        } else {
            controller
                .channel
                .selected_agent()
                .map(|agent| {
                    format!(
                        "#{} / {} · {}",
                        controller.channel.info.name, agent.name, agent.state
                    )
                })
                .unwrap_or_else(|| "Agent chat".into())
        };
        frame.render_widget(
            Paragraph::new(safe(&title))
                .style(Style::default().fg(ACCENT).add_modifier(Modifier::BOLD)),
            chunks[0],
        );
        self.chat(frame, controller, chunks[1]);
        self.hit.tabs = chunks[2];
        self.hit.input = chunks[3];
        self.hit.meta = chunks[4];
        let tabs = ["Async", "Todo", "MCP", "Questions", "Permissions", "Dirs"]
            .iter()
            .enumerate()
            .map(|(index, name)| {
                Span::styled(
                    format!(" {name} "),
                    if controller.focus == Focus::Tabs && controller.tab_cursor == index {
                        Style::default().fg(ACCENT).add_modifier(Modifier::REVERSED)
                    } else {
                        Style::default().fg(MUTED)
                    },
                )
            })
            .collect::<Vec<_>>();
        frame.render_widget(Paragraph::new(Line::from(tabs)), chunks[2]);
        let input_block = block(
            format!(
                "{} · {}",
                controller.channel.info.mode.to_uppercase(),
                if controller.channel.chat {
                    "Message the channel · @name addresses an agent"
                } else {
                    "Enter steers · /queue waits for this turn"
                }
            ),
            controller.focus == Focus::Input
                && controller.popup.is_none()
                && controller.config.is_none(),
        );
        let inner = input_block.inner(chunks[3]);
        frame.render_widget(input_block, chunks[3]);
        self.editor(
            frame,
            &controller.channel.input,
            inner,
            controller.focus == Focus::Input
                && controller.popup.is_none()
                && controller.config.is_none(),
        );
        let agent = controller.channel.selected_agent();
        let meta = [
            agent
                .map(|agent| agent.role.clone())
                .unwrap_or_else(|| "role".into()),
            agent
                .map(|agent| agent.model.clone())
                .unwrap_or_else(|| controller.channel.info.model.clone()),
            agent
                .map(|agent| {
                    if agent.variant.is_empty() {
                        "default".into()
                    } else {
                        agent.variant.clone()
                    }
                })
                .unwrap_or_else(|| "variant".into()),
            controller.channel.info.mode.clone(),
        ];
        frame.render_widget(
            Paragraph::new(Line::from(
                meta.iter()
                    .enumerate()
                    .map(|(index, label)| {
                        Span::styled(
                            format!(" {} ", safe(label)),
                            if controller.focus == Focus::Meta && controller.meta_cursor == index {
                                Style::default().fg(ACCENT).add_modifier(Modifier::REVERSED)
                            } else {
                                Style::default().fg(MUTED)
                            },
                        )
                    })
                    .collect::<Vec<_>>(),
            )),
            chunks[4],
        );
        if !controller.hide_keys {
            frame.render_widget(Paragraph::new("Tab focus · Ctrl+B tree · Ctrl+N/P agent · Enter send · Alt+Enter newline · Esc clear · Ctrl+C twice quit").style(Style::default().fg(MUTED)),chunks[5]);
        }
        let status = if controller.status.is_empty() {
            format!(
                "{} tokens · {} · {} cached",
                super::format::tokens(controller.channel.info.tokens),
                super::format::cost(controller.channel.info.cost_usd),
                super::format::tokens(controller.cache.cached)
            )
        } else {
            safe(&controller.status)
        };
        frame.render_widget(
            Paragraph::new(status).style(Style::default().fg(if controller.status_error {
                BAD
            } else {
                MUTED
            })),
            chunks[6],
        );
        if let Some(config) = &controller.config {
            self.configuration(frame, config, size);
        } else if controller.popup.is_some() {
            self.popup(frame, controller, size);
        } else if controller.focus == Focus::Input {
            self.completions(frame, controller, chunks[3]);
        }
    }
    fn sidebar(&mut self, frame: &mut Frame, controller: &Controller, area: Rect) {
        let border = block("stavlos", controller.focus == Focus::Sidebar);
        let inner = border.inner(area);
        frame.render_widget(border, area);
        self.hit.sidebar = inner;
        let rows = controller.sidebar_rows();
        let top = controller
            .sidebar_cursor
            .saturating_sub(inner.height.saturating_sub(1) as usize);
        self.hit.sidebar_top = top;
        let lines = rows
            .iter()
            .enumerate()
            .skip(top)
            .take(inner.height as usize)
            .map(|(index, row)| {
                let active = row.channel == controller.scope.channel
                    && (row.agent == controller.channel.selected && !controller.channel.chat
                        || row.agent.is_empty() && controller.channel.chat);
                let marker = if active {
                    "●"
                } else if row.state == crate::protocol::AGENT_RUNNING {
                    "◌"
                } else {
                    " "
                };
                let style =
                    if controller.focus == Focus::Sidebar && index == controller.sidebar_cursor {
                        Style::default().fg(ACCENT).add_modifier(Modifier::REVERSED)
                    } else if active {
                        Style::default().fg(ACCENT)
                    } else if row.state == crate::protocol::AGENT_KILLED {
                        Style::default().fg(MUTED)
                    } else {
                        Style::default()
                    };
                Line::styled(
                    format!(
                        "{marker} {}{}",
                        "  ".repeat(row.depth.min(6)),
                        safe(&row.label)
                    ),
                    style,
                )
            })
            .collect::<Vec<_>>();
        frame.render_widget(Paragraph::new(lines), inner);
    }
    fn chat(&mut self, frame: &mut Frame, controller: &mut Controller, area: Rect) {
        self.hit.chat = area;
        let key = (
            controller.scope.channel.clone(),
            controller.channel.view_id().to_owned(),
        );
        if !self.cache.contains_key(&key)
            && self.cache.len() >= 32
            && let Some(old) = self.cache.keys().next().cloned()
        {
            self.cache.remove(&old);
        }
        let Some(view) = controller.channel.view() else {
            frame.render_widget(
                Paragraph::new(if controller.channel.loading {
                    "Loading channel history…"
                } else {
                    "Message the channel to begin."
                })
                .style(Style::default().fg(MUTED)),
                area,
            );
            return;
        };
        let expanded = controller
            .channel
            .expanded
            .get(controller.channel.view_id());
        let minute = controller.now.0.timestamp() / 60;
        let entries = self.cache.entry(key).or_default();
        entries.truncate(view.committed());
        for index in 0..view.committed() {
            let expanded = expanded.is_some_and(|items| items.contains(&(index as i64)));
            let revision = view.revision(index);
            let update = entries.get(index).is_none_or(|cached| {
                cached.revision != revision
                    || cached.width != area.width
                    || cached.expanded != expanded
                    || cached.minute != minute
            });
            if update {
                let cached = Cached {
                    revision,
                    width: area.width,
                    expanded,
                    minute,
                    rows: line_rows(view.item(index), area.width, expanded, controller.now),
                };
                if index < entries.len() {
                    entries[index] = cached;
                } else {
                    entries.push(cached);
                }
            }
        }
        let tail = line_rows(&view.tail(), area.width, false, controller.now);
        let working = view.in_turn();
        let working_text = if working {
            format!(
                "{}… {}",
                view.turn_verb(),
                super::format::elapsed(view.turn_stats(controller.now).0)
            )
        } else {
            String::new()
        };
        let live_item = view.item_count() as i64;
        let total = entries
            .iter()
            .map(|cached| cached.rows.len())
            .sum::<usize>()
            + tail.len()
            + usize::from(working);
        let max_scroll = total.saturating_sub(area.height as usize);
        controller.channel.scroll = controller.channel.scroll.min(max_scroll);
        let start = total
            .saturating_sub(area.height as usize)
            .saturating_sub(controller.channel.scroll);
        self.hit.chat_offset = start;
        self.hit.total = total;
        let mut offset = 0;
        let end = start + area.height as usize;
        let mut visible = vec![];
        for rows in entries
            .iter()
            .map(|cached| &cached.rows)
            .chain(std::iter::once(&tail))
        {
            let next = offset + rows.len();
            if next > start && offset < end {
                visible.extend(
                    rows.iter()
                        .skip(start.saturating_sub(offset))
                        .take(end.saturating_sub(offset.max(start)))
                        .cloned(),
                );
            }
            offset = next;
            if offset >= end {
                break;
            }
        }
        if working && offset < end && total > start {
            visible.push(Row {
                text: working_text,
                style: Style::default().fg(WARN),
                item: live_item,
            });
        }
        self.hit.rows = visible;
        let lines = self
            .hit
            .rows
            .iter()
            .enumerate()
            .map(|(row_index, row)| {
                let mut style = row.style;
                if controller.focus == Focus::Chat && row.item == controller.channel.chat_cursor {
                    style = style.bg(Color::Rgb(42, 47, 58));
                }
                if let Some((start, end)) = self.selection {
                    let y = area.y + row_index as u16;
                    if y >= start.1.min(end.1) && y <= start.1.max(end.1) {
                        style = style.add_modifier(Modifier::REVERSED);
                    }
                }
                Line::styled(row.text.clone(), style)
            })
            .collect::<Vec<_>>();
        frame.render_widget(Paragraph::new(lines), area);
    }
    fn editor(&self, frame: &mut Frame, editor: &Editor, area: Rect, active: bool) {
        if area.is_empty() {
            return;
        }
        let rows = wrap(&editor.display(), area.width);
        let before = wrap(&editor.display_before_cursor(), area.width);
        let cursor_row = before.len().saturating_sub(1);
        let top = cursor_row.saturating_sub(area.height.saturating_sub(1) as usize);
        let lines = rows
            .into_iter()
            .skip(top)
            .take(area.height as usize)
            .map(Line::raw)
            .collect::<Vec<_>>();
        frame.render_widget(Paragraph::new(lines), area);
        if active {
            let column = before
                .last()
                .map(|row| UnicodeWidthStr::width(row.as_str()))
                .unwrap_or_default()
                .min(area.width.saturating_sub(1) as usize);
            frame.set_cursor_position((area.x + column as u16, area.y + (cursor_row - top) as u16));
        }
    }
    fn completions(&self, frame: &mut Frame, controller: &Controller, input: Rect) {
        let mentions = controller.mentions();
        let choices = controller.completions();
        let rows = if !mentions.is_empty() {
            mentions
                .into_iter()
                .enumerate()
                .map(|(index, name)| (index, format!("@{name}")))
                .collect::<Vec<_>>()
        } else {
            choices
                .iter()
                .enumerate()
                .map(|(index, choice)| {
                    (
                        index,
                        format!("{} {} · {}", choice.name, choice.args, choice.description),
                    )
                })
                .collect()
        };
        if rows.is_empty() {
            return;
        }
        let cursor = if choices.is_empty() {
            controller.mention_cursor
        } else {
            controller.palette_cursor
        };
        let height = (rows.len() as u16).min(8).min(input.y);
        let area = Rect::new(input.x, input.y - height, input.width, height);
        frame.render_widget(Clear, area);
        let top = cursor.saturating_sub(height.saturating_sub(1) as usize);
        let lines = rows
            .into_iter()
            .skip(top)
            .take(height as usize)
            .map(|(index, text)| {
                Line::styled(
                    safe(&text),
                    if index == cursor {
                        Style::default().fg(ACCENT).add_modifier(Modifier::REVERSED)
                    } else {
                        Style::default().fg(MUTED)
                    },
                )
            })
            .collect::<Vec<_>>();
        frame.render_widget(Paragraph::new(lines), area);
    }
    fn popup(&mut self, frame: &mut Frame, controller: &Controller, size: Rect) {
        let popup = controller.popup.as_ref().unwrap();
        let area = center(size, 85, 80);
        self.hit.popup = area;
        frame.render_widget(Clear, area);
        let border = block(safe(&popup.title), true);
        let inner = border.inner(area);
        frame.render_widget(border, area);
        if matches!(popup.kind, Dialog::Usage | Dialog::Plan) {
            let mut data = vec![];
            if popup.kind == Dialog::Plan {
                if let Some(series) = &controller.plan_series {
                    data = series
                        .used_percent
                        .iter()
                        .map(|value| (value.max(0.) * 1000.) as u64)
                        .collect();
                }
            } else if let Some(series) = &controller.usage {
                data = if popup.title.starts_with("cost") {
                    series
                        .cost_usd
                        .iter()
                        .map(|value| (value.max(0.) * 1_000_000.) as u64)
                        .collect()
                } else {
                    series
                        .tokens
                        .iter()
                        .map(|value| (*value).max(0) as u64)
                        .collect()
                };
            }
            let chunks = Layout::vertical([
                Constraint::Length(2),
                Constraint::Min(1),
                Constraint::Length(2),
            ])
            .split(inner);
            frame.render_widget(
                Paragraph::new(format!(
                    "{} · {} · ←/→ range",
                    safe(&popup.context),
                    ["1 hour", "24 hours", "7 days", "30 days"][popup.cursor.min(3)]
                )),
                chunks[0],
            );
            frame.render_widget(
                Sparkline::default()
                    .data(&data)
                    .style(Style::default().fg(ACCENT)),
                chunks[1],
            );
            frame.render_widget(
                Paragraph::new(format!(
                    "{}  ·  Esc closes",
                    if popup.kind == Dialog::Plan {
                        format!(
                            "latest {:.1}%",
                            data.last().copied().unwrap_or_default() as f64 / 1000.
                        )
                    } else if popup.title.starts_with("cost") {
                        format!("total ${:.4}", data.iter().sum::<u64>() as f64 / 1_000_000.)
                    } else {
                        format!("total {} tokens", data.iter().sum::<u64>())
                    }
                )),
                chunks[2],
            );
            return;
        }
        if popup.kind == Dialog::Questions {
            let Some(prompt) = controller.prompts.get(&controller.active_prompt) else {
                return;
            };
            let Some(draft) = controller.questions.get(&controller.active_prompt) else {
                return;
            };
            let Some(question) = prompt.questions.get(draft.index) else {
                return;
            };
            let mut lines = vec![
                Line::styled(
                    format!("Question {}/{}", draft.index + 1, prompt.questions.len()),
                    Style::default().fg(ACCENT),
                ),
                Line::raw(safe(&question.question)),
                Line::raw(""),
            ];
            for (index, option) in question.options.iter().enumerate() {
                let selected = draft.selected[draft.index].contains(&index);
                lines.push(Line::styled(
                    format!(
                        "{} [{}] {}  {}",
                        if draft.cursor == index { "›" } else { " " },
                        if selected { "×" } else { " " },
                        safe(&option.label),
                        safe(&option.description)
                    ),
                    if draft.cursor == index {
                        Style::default().fg(ACCENT)
                    } else {
                        Style::default()
                    },
                ));
            }
            lines.push(Line::styled(
                format!(
                    "{} Custom: {}",
                    if draft.cursor == question.options.len() {
                        "›"
                    } else {
                        " "
                    },
                    safe(&draft.custom[draft.index])
                ),
                Style::default().fg(MUTED),
            ));
            lines.push(Line::styled(
                format!(
                    "{} Submit answer",
                    if draft.cursor == question.options.len() + 1 {
                        "›"
                    } else {
                        " "
                    }
                ),
                Style::default().fg(ACCENT),
            ));
            lines.push(Line::raw(
                "Space toggles · Enter confirms · ←/→ question · Esc keeps draft",
            ));
            frame.render_widget(Paragraph::new(lines), inner);
            return;
        }
        let input = matches!(
            popup.kind,
            Dialog::NewName
                | Dialog::NewDirectory
                | Dialog::AddDirectory
                | Dialog::SetDirectory
                | Dialog::EditDirectory
                | Dialog::DenyReason
                | Dialog::PermissionDirectory
                | Dialog::QuestionText
                | Dialog::LoginKey
        );
        if input {
            let chunks = Layout::vertical([
                Constraint::Length(2),
                Constraint::Min(1),
                Constraint::Length(1),
            ])
            .split(inner);
            frame.render_widget(Paragraph::new(safe(&popup.detail)), chunks[0]);
            self.editor(frame, &popup.input, chunks[1], true);
            frame.render_widget(
                Paragraph::new("Enter continues · Esc keeps the pending request")
                    .style(Style::default().fg(MUTED)),
                chunks[2],
            );
            return;
        }
        if popup.kind == Dialog::Login {
            frame.render_widget(Paragraph::new(format!("{}\n\n{}\n\n{}\n\nWaiting for sign-in…\no opens browser · c copies URL · Esc cancels",safe(&popup.detail),safe(&controller.login.url),safe(&controller.login.code))),inner);
            return;
        }
        if popup.kind == Dialog::ConfirmLogout {
            frame.render_widget(
                Paragraph::new(format!(
                    "Sign out of {}?\nEnter confirms · Esc keeps signed in",
                    safe(&popup.context)
                )),
                inner,
            );
            return;
        }
        let detail = wrap(&safe(&popup.detail), inner.width);
        let detail_height = (detail.len() as u16).min(inner.height / 3);
        let chunks = Layout::vertical([
            Constraint::Length(detail_height),
            Constraint::Length(1),
            Constraint::Min(1),
            Constraint::Length(1),
        ])
        .split(inner);
        frame.render_widget(
            Paragraph::new(
                detail
                    .into_iter()
                    .take(detail_height as usize)
                    .map(Line::raw)
                    .collect::<Vec<_>>(),
            ),
            chunks[0],
        );
        frame.render_widget(
            Paragraph::new(format!("Filter: {}", popup.input.display()))
                .style(Style::default().fg(MUTED)),
            chunks[1],
        );
        let indices = controller.filtered_choices();
        let cursor = popup.cursor.min(indices.len().saturating_sub(1));
        let top = cursor.saturating_sub(chunks[2].height.saturating_sub(1) as usize);
        let lines = indices
            .iter()
            .enumerate()
            .skip(top)
            .take(chunks[2].height as usize)
            .map(|(position, index)| {
                let choice = &popup.choices[*index];
                Line::styled(
                    format!(
                        "{} {}  {}",
                        if position == cursor { "›" } else { " " },
                        safe(&choice.label),
                        safe(&choice.description)
                    ),
                    if position == cursor {
                        Style::default().fg(ACCENT)
                    } else {
                        Style::default()
                    },
                )
            })
            .collect::<Vec<_>>();
        frame.render_widget(Paragraph::new(lines), chunks[2]);
        frame.render_widget(
            Paragraph::new("↑/↓ choose · Enter selects · Esc closes")
                .style(Style::default().fg(MUTED)),
            chunks[3],
        );
    }
    fn configuration(
        &mut self,
        frame: &mut Frame,
        config: &super::controller::Configuration,
        size: Rect,
    ) {
        let area = center(size, 95, 95);
        self.hit.popup = area;
        frame.render_widget(Clear, area);
        let border = block(
            format!(
                "{} settings · {}",
                config.scope.scope,
                safe(&config.tree.root)
            ),
            true,
        );
        let inner = border.inner(area);
        frame.render_widget(border, area);
        let vertical = Layout::vertical([
            Constraint::Length(1),
            Constraint::Min(1),
            Constraint::Length(2),
        ])
        .split(inner);
        let sections = ["Settings", "Agents", "Commands", "Skills", "Files"];
        frame.render_widget(
            Paragraph::new(Line::from(
                sections
                    .iter()
                    .enumerate()
                    .map(|(index, label)| {
                        Span::styled(
                            format!(" {label} "),
                            if index == config.section {
                                Style::default().fg(ACCENT).add_modifier(Modifier::BOLD)
                            } else {
                                Style::default().fg(MUTED)
                            },
                        )
                    })
                    .collect::<Vec<_>>(),
            )),
            vertical[0],
        );
        let panes = Layout::horizontal([Constraint::Percentage(30), Constraint::Percentage(70)])
            .split(vertical[1]);
        let entries = config.entries();
        let top = config
            .file_cursor
            .saturating_sub(panes[0].height.saturating_sub(1) as usize);
        frame.render_widget(
            Paragraph::new(
                entries
                    .iter()
                    .enumerate()
                    .skip(top)
                    .take(panes[0].height as usize)
                    .map(|(index, file)| {
                        Line::styled(
                            format!(
                                "{}{}",
                                if file.directory { "▸ " } else { "  " },
                                safe(&file.path)
                            ),
                            if config.pane == 0 && index == config.file_cursor {
                                Style::default().fg(ACCENT).add_modifier(Modifier::REVERSED)
                            } else {
                                Style::default()
                            },
                        )
                    })
                    .collect::<Vec<_>>(),
            ),
            panes[0],
        );
        if config.action == "discard" {
            frame.render_widget(
                Paragraph::new(
                    "Discard unsaved changes and close?\nEnter discards · Esc keeps editing",
                )
                .style(Style::default().fg(WARN)),
                panes[1],
            );
        } else if config.action == "delete" {
            frame.render_widget(
                Paragraph::new(format!(
                    "Delete {}?\nEnter confirms · Esc cancels",
                    safe(&config.input.text)
                ))
                .style(Style::default().fg(WARN)),
                panes[1],
            );
        } else if config.editing || config.raw || matches!(config.action.as_str(), "new" | "rename")
        {
            self.editor(
                frame,
                &config.input,
                panes[1],
                config.pane == 1 || !config.action.is_empty(),
            );
        } else if let Some(document) = &config.document {
            let top = config
                .field_cursor
                .saturating_sub(panes[1].height.saturating_sub(1) as usize);
            frame.render_widget(
                Paragraph::new(
                    document
                        .fields
                        .iter()
                        .enumerate()
                        .skip(top)
                        .take(panes[1].height as usize)
                        .map(|(index, field)| {
                            Line::styled(
                                format!("{}: {}", field.path.join("."), safe(&field.value)),
                                if index == config.field_cursor && config.pane == 1 {
                                    Style::default().fg(ACCENT)
                                } else {
                                    Style::default()
                                },
                            )
                        })
                        .collect::<Vec<_>>(),
                ),
                panes[1],
            );
        }
        frame.render_widget(Paragraph::new(if config.busy{"Working…"}else{"Tab pane · ←/→ section · Enter edits · F4 raw · Ctrl+S saves\nCtrl+N new · F2 rename · Ctrl+D delete · Ctrl+L reload · Esc closes"}).style(Style::default().fg(MUTED)),vertical[2]);
    }
    pub fn selected_text(&self) -> String {
        let Some((a, b)) = self.selection else {
            return String::new();
        };
        let (start, end) = if (a.1, a.0) <= (b.1, b.0) {
            (a, b)
        } else {
            (b, a)
        };
        let mut out = vec![];
        for (index, row) in self.hit.rows.iter().enumerate() {
            let y = self.hit.chat.y + index as u16;
            if y < start.1 || y > end.1 {
                continue;
            }
            let left = if y == start.1 {
                start.0.saturating_sub(self.hit.chat.x) as usize
            } else {
                0
            };
            let right = if y == end.1 {
                end.0.saturating_sub(self.hit.chat.x) as usize
            } else {
                usize::MAX
            };
            let mut column = 0;
            let mut text = String::new();
            for grapheme in row.text.graphemes(true) {
                let next = column + UnicodeWidthStr::width(grapheme);
                if next > left && column <= right {
                    text.push_str(grapheme);
                }
                column = next;
            }
            out.push(text);
        }
        out.join("\n")
    }
}
fn center(area: Rect, width: u16, height: u16) -> Rect {
    let w = (area.width as u32 * width as u32 / 100).max(1) as u16;
    let h = (area.height as u32 * height as u32 / 100).max(1) as u16;
    Rect::new(
        area.x + (area.width - w) / 2,
        area.y + (area.height - h) / 2,
        w,
        h,
    )
}
