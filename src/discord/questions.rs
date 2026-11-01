use super::{permission, text};
use crate::{event, protocol};
use anyhow::{Result, ensure};
use serde_json::{Value, json};
use std::collections::BTreeSet;
pub const PAGE_SIZE: usize = 4;
#[derive(Clone)]
pub struct Draft {
    pub prompt: String,
    pub index: usize,
    pub page: usize,
    pub selected: Vec<BTreeSet<usize>>,
    pub text: Vec<String>,
}
pub enum Submission {
    Ready(protocol::PromptReplyParams),
    Continue,
}
impl Draft {
    pub fn new(prompt: &protocol::PromptInfo) -> Self {
        Self {
            prompt: prompt.id.clone(),
            index: 0,
            page: 0,
            selected: vec![BTreeSet::new(); prompt.questions.len()],
            text: vec![String::new(); prompt.questions.len()],
        }
    }
    fn valid(&self, prompt: &protocol::PromptInfo) -> bool {
        self.prompt == prompt.id
            && self.index < prompt.questions.len()
            && self.selected.len() == prompt.questions.len()
            && self.text.len() == prompt.questions.len()
    }
    pub fn update(
        &mut self,
        prompt: &protocol::PromptInfo,
        action: &str,
        arg: &str,
        value: &str,
        values: Option<&[String]>,
    ) -> Result<()> {
        ensure!(
            self.valid(prompt),
            "this question view changed; use the current form"
        );
        if action != "questions" {
            ensure!(
                arg.split('.').next().and_then(|v| v.parse::<usize>().ok()) == Some(self.index),
                "this question view changed; use the current form"
            );
        }
        let question = &prompt.questions[self.index];
        match action {
            "questions" => {}
            "next" => {
                ensure!(
                    !draft_answer(question, &self.selected[self.index], &self.text[self.index])
                        .is_empty(),
                    "{}",
                    missing(self.index)
                );
                self.index = (self.index + 1).min(prompt.questions.len() - 1);
                self.page = 0;
            }
            "back" => {
                self.index = self.index.saturating_sub(1);
                self.page = 0;
            }
            "page-next" => {
                self.page =
                    (self.page + 1).min(question.options.len().saturating_sub(1) / PAGE_SIZE)
            }
            "page-back" => self.page = self.page.saturating_sub(1),
            "text-submit" => {
                ensure!(
                    !value.trim().is_empty(),
                    "enter a custom answer, or close the popup to leave it unchecked"
                );
                self.text[self.index] = value.trim().into();
            }
            "clear-text" => self.text[self.index].clear(),
            "toggle" => {
                let parts = arg.split('.').collect::<Vec<_>>();
                ensure!(parts.len() == 2, "invalid option");
                let number = parts[1].parse::<usize>().ok();
                let start = self.page * PAGE_SIZE;
                let end = ((self.page + 1) * PAGE_SIZE).min(question.options.len());
                ensure!(
                    number.is_some_and(|n| n >= start && n < end),
                    "this option page changed; use the current buttons"
                );
                let number = number.unwrap();
                if !self.selected[self.index].remove(&number) {
                    self.selected[self.index].insert(number);
                }
            }
            "pick" => {
                let values = values.ok_or_else(|| anyhow::anyhow!("invalid selection"))?;
                ensure!(
                    arg == format!("{}.{}", self.index, self.page),
                    "this option page is stale"
                );
                let start = self.page * 25;
                let end = ((self.page + 1) * 25).min(question.options.len());
                for n in start..end {
                    self.selected[self.index].remove(&n);
                }
                for value in values {
                    let n = value.parse::<usize>().ok();
                    ensure!(n.is_some_and(|n| n >= start && n < end), "invalid option");
                    self.selected[self.index].insert(n.unwrap());
                }
            }
            _ => anyhow::bail!("invalid question action"),
        }
        Ok(())
    }
    pub fn submit(&mut self, prompt: &protocol::PromptInfo) -> Result<Submission> {
        ensure!(self.valid(prompt), "empty question batch");
        let mut answers = Vec::new();
        let mut details = Vec::new();
        for (index, question) in prompt.questions.iter().enumerate() {
            let answer = draft_answer(question, &self.selected[index], &self.text[index]);
            if answer.is_empty() {
                let previous = self.index;
                self.index = index;
                self.page = 0;
                if index > previous {
                    return Ok(Submission::Continue);
                }
                anyhow::bail!("{}", missing(index));
            }
            details.push(protocol::QuestionAnswer {
                custom: self.text[index].trim().into(),
                selected: (0..question.options.len())
                    .filter(|n| self.selected[index].contains(n))
                    .map(|n| n as i64)
                    .collect(),
            });
            answers.push(answer);
        }
        Ok(Submission::Ready(protocol::PromptReplyParams {
            id: prompt.id.clone(),
            answer: protocol::ANSWER_ANSWERED.into(),
            answers: answers.into(),
            details: details.into(),
            ..Default::default()
        }))
    }
}
fn missing(index: usize) -> String {
    format!(
        "question {} still needs an answer: select options or enter a custom answer",
        index + 1
    )
}
pub fn draft_answer(
    question: &protocol::Question,
    selected: &BTreeSet<usize>,
    custom: &str,
) -> String {
    let mut parts = question
        .options
        .iter()
        .enumerate()
        .filter(|(n, _)| selected.contains(n))
        .map(|(_, option)| option.label.clone())
        .collect::<Vec<_>>();
    if !custom.trim().is_empty() {
        parts.push(custom.trim().into());
    }
    parts.join(", ").trim().into()
}
fn choice_button(id: String, label: &str, checked: bool) -> Value {
    json!({"type":2,"style":if checked {3}else{2},"label":text::clip(label,80),"custom_id":id,"emoji":{"name":if checked {"✅"}else{"⬜"}},"disabled":false})
}
pub fn view(prompt: &protocol::PromptInfo, draft: &Draft) -> (String, Vec<Value>) {
    if !draft.valid(prompt) {
        return ("No questions available.".into(), Vec::new());
    }
    let question = &prompt.questions[draft.index];
    let custom = draft.text[draft.index].trim();
    let footer = if custom.is_empty() {
        String::new()
    } else {
        format!("\nCustom answer: {}", text::clip(custom, 1000))
    };
    let body = text::question_heading(
        &question.question,
        2000usize.saturating_sub(text::units(&footer)),
    ) + &footer;
    let mut components = Vec::new();
    let start = draft.page * PAGE_SIZE;
    let end = ((draft.page + 1) * PAGE_SIZE).min(question.options.len());
    for n in start..end {
        let option = &question.options[n];
        let label = if option.description.is_empty() {
            option.label.clone()
        } else {
            format!("{} — {}", option.label, option.description)
        };
        components.push(permission::row(vec![choice_button(
            permission::component_id(&prompt.id, "toggle", &format!("{}.{n}", draft.index)),
            &label,
            draft.selected[draft.index].contains(&n),
        )]));
    }
    let label = if custom.is_empty() {
        "Custom answer".into()
    } else {
        custom.split_whitespace().collect::<Vec<_>>().join(" ")
    };
    components.push(permission::row(vec![choice_button(
        permission::component_id(
            &prompt.id,
            if custom.is_empty() {
                "text"
            } else {
                "clear-text"
            },
            &draft.index.to_string(),
        ),
        &label,
        !custom.is_empty(),
    )]));
    let mut actions = Vec::new();
    if draft.index > 0 {
        actions.push(("back", "Previous question"));
    }
    if draft.index + 1 < prompt.questions.len() {
        actions.push(("next", "Next question"));
    } else {
        actions.push((
            "submit",
            if prompt.questions.len() > 1 {
                "Submit answers"
            } else {
                "Submit answer"
            },
        ));
    }
    if question.options.len() > PAGE_SIZE {
        actions.extend([
            ("page-back", "Previous options"),
            ("page-next", "More options"),
        ]);
    }
    for (action, label) in actions {
        let mut button = permission::button(&prompt.id, action, label, false);
        button["custom_id"] = json!(permission::component_id(
            &prompt.id,
            action,
            &draft.index.to_string()
        ));
        if matches!(action, "next" | "submit") {
            button["style"] = json!(1);
        }
        components.push(permission::row(vec![button]));
    }
    (text::clip(&body, 2000), components)
}
pub fn prompt_view(
    prompt: &protocol::PromptInfo,
    draft: &Draft,
    client: &str,
) -> (String, Vec<Value>) {
    let (mut body, mut components) = view(prompt, draft);
    if !prompt.claimed_by.is_empty() && prompt.claimed_by != client {
        body = text::clip(&body, 1950) + "\nClaimed by another client.";
        for row in &mut components {
            if let Some(children) = row["components"].as_array_mut() {
                for child in children {
                    child["disabled"] = json!(true);
                }
            }
        }
    }
    (body, components)
}
fn answer_choice(mark: &str, body: &str) -> String {
    format!(
        "\u{a0}\u{a0}\u{a0}\u{a0}{mark} {}",
        body.replace('\n', "\n\u{a0}\u{a0}\u{a0}\u{a0}\u{a0}\u{a0}\u{a0}")
    )
}
pub fn answered(prompt: &protocol::PromptInfo, draft: Option<&Draft>) -> String {
    if prompt.questions.is_empty() {
        return "❓".into();
    }
    let mut lines = Vec::new();
    let mut headings = std::collections::BTreeMap::new();
    for (index, question) in prompt.questions.iter().enumerate() {
        if index > 0 {
            lines.push(String::new());
        }
        headings.insert(lines.len(), question.question.as_str());
        lines.push(text::question_heading(&question.question, 2000));
        let Some(draft) =
            draft.filter(|draft| draft.selected.len() > index && draft.text.len() > index)
        else {
            continue;
        };
        for (n, option) in question.options.iter().enumerate() {
            let label = if option.description.is_empty() {
                option.label.clone()
            } else {
                format!("{} — {}", option.label, option.description)
            };
            lines.push(answer_choice(
                if draft.selected[index].contains(&n) {
                    "✅"
                } else {
                    "⬜"
                },
                &label,
            ));
        }
        let custom = draft.text[index].trim();
        if !custom.is_empty() {
            lines.push(answer_choice("✅", custom));
        }
    }
    if text::units(&lines.join("\n")) > 2000 {
        let count = lines.iter().filter(|line| !line.is_empty()).count();
        let limit = (2001usize.saturating_sub(lines.len()) / count.max(1)).max(1);
        for (index, line) in lines.iter_mut().enumerate() {
            if let Some(heading) = headings.get(&index) {
                *line = text::question_heading(heading, limit);
            } else if text::units(line) > limit {
                *line = text::clip(line, limit.saturating_sub(1)) + "…";
            }
        }
    }
    lines.join("\n")
}
pub fn result_note(prompt: &protocol::PromptInfo, note: &str) -> String {
    let note = format!("\n\n{}", text::clip(note, 1000));
    text::clip(
        &answered(prompt, None),
        2000usize.saturating_sub(text::units(&note)),
    ) + &note
}
pub fn recorded_result(
    prompt: &protocol::PromptInfo,
    result: &event::AskResolvedPayload,
) -> String {
    match result.outcome.as_str() {
        "withdrawn" => result_note(prompt, "Question withdrawn."),
        "defaulted" => result_note(prompt, "Defaulted by the daemon."),
        "answered" => {
            if !prompt.questions.is_empty() && result.details.len() == prompt.questions.len() {
                let mut draft = Draft::new(prompt);
                for (index, answer) in result.details.iter().enumerate() {
                    draft.selected[index].extend(
                        answer
                            .selected
                            .iter()
                            .filter_map(|n| usize::try_from(*n).ok()),
                    );
                    draft.text[index] = answer.custom.clone();
                }
                return answered(prompt, Some(&draft));
            }
            result_note(
                prompt,
                &format!(
                    "Answer: {}",
                    if result.answers.is_empty() {
                        result.answer.clone()
                    } else {
                        result.answers.join("\n")
                    }
                ),
            )
        }
        _ => result_note(prompt, "Question resolved."),
    }
}
