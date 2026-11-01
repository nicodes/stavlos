use super::*;
impl Controller {
    pub fn upsert_prompt(&mut self, mut prompt: protocol::PromptInfo) {
        if prompt.kind == protocol::PROMPT_QUESTION && prompt.questions.is_empty() {
            prompt.questions.push(protocol::Question {
                question: prompt.question.clone(),
                options: prompt
                    .options
                    .iter()
                    .map(|label| protocol::QuestionOption {
                        label: label.clone(),
                        ..Default::default()
                    })
                    .collect(),
            });
        }
        if prompt.kind == protocol::PROMPT_QUESTION {
            self.questions
                .entry(prompt.id.clone())
                .or_insert_with(|| QuestionDraft::new(&prompt));
        }
        if prompt.channel == self.scope.channel && !self.channel.loading {
            let id = if prompt.agent.is_empty() {
                CHAT
            } else {
                &prompt.agent
            };
            let transcript = self.channel.transcript(id);
            if prompt.kind == protocol::PROMPT_QUESTION {
                transcript.ensure_question(prompt.clone());
            } else {
                transcript.ensure_permission(prompt.clone());
            }
            if !prompt.agent.is_empty() {
                let chat = self.channel.transcript(CHAT);
                if prompt.kind == protocol::PROMPT_QUESTION {
                    chat.ensure_question(prompt.clone());
                } else {
                    chat.ensure_permission(prompt.clone());
                }
            }
        }
        self.prompts.insert(prompt.id.clone(), prompt);
        self.dirty = true;
    }
    pub fn materialize_prompts(&mut self) {
        let prompts = self
            .prompts
            .values()
            .filter(|prompt| prompt.channel == self.scope.channel)
            .cloned()
            .collect::<Vec<_>>();
        for prompt in prompts {
            self.upsert_prompt(prompt);
        }
    }
    pub fn resolve_prompt(&mut self, id: &str) {
        self.prompts.remove(id);
        self.questions.remove(id);
        self.permission_cursor.remove(id);
        self.permission_drafts.remove(id);
        self.prompt_busy.remove(id);
        if self.active_prompt == id {
            self.active_prompt.clear();
            if self.popup.as_ref().is_some_and(|popup| {
                matches!(
                    popup.kind,
                    Dialog::Questions
                        | Dialog::Permissions
                        | Dialog::QuestionText
                        | Dialog::PermissionDirectory
                        | Dialog::DenyReason
                )
            }) {
                self.popup = None;
                self.focus = Focus::Input;
            }
        }
        self.dirty = true;
    }
    pub fn prompt_notification(&mut self, notification: protocol::PromptNotification) {
        let open = self.prompts.is_empty()
            && notification.prompt.kind == protocol::PROMPT_TRUST
            && (notification.prompt.channel.is_empty()
                || notification.prompt.channel == self.scope.channel)
            && self.focus == Focus::Input
            && self.popup.is_none()
            && self.config.is_none()
            && self.channel.input.text.trim().is_empty();
        match notification.action.as_str() {
            protocol::ACTION_REQUESTED | protocol::ACTION_ESCALATED | protocol::ACTION_CLAIMED => {
                self.upsert_prompt(notification.prompt)
            }
            _ => self.resolve_prompt(&notification.prompt.id),
        }
        if open && !self.prompts.is_empty() {
            self.open_prompts(false, true);
        }
    }
    pub fn pending(&self, questions: bool, all: bool) -> Vec<&protocol::PromptInfo> {
        let mut prompts = self
            .prompts
            .values()
            .filter(|prompt| {
                (prompt.kind == protocol::PROMPT_QUESTION) == questions
                    && (all || prompt.channel.is_empty() || prompt.channel == self.scope.channel)
                    && (all
                        || self.channel.chat
                        || prompt.agent == self.channel.selected
                        || prompt.kind == protocol::PROMPT_TRUST)
            })
            .collect::<Vec<_>>();
        prompts.sort_by(|left, right| (&left.created, &left.id).cmp(&(&right.created, &right.id)));
        prompts
    }
    pub fn open_prompts(&mut self, questions: bool, all: bool) {
        let prompt = self.pending(questions, all).first().copied().cloned();
        let Some(prompt) = prompt else {
            self.status(
                if questions {
                    "no questions waiting"
                } else {
                    "no permissions waiting"
                },
                false,
            );
            return;
        };
        self.active_prompt = prompt.id.clone();
        self.focus = Focus::Prompt;
        if questions {
            self.questions
                .entry(prompt.id.clone())
                .or_insert_with(|| QuestionDraft::new(&prompt));
            let popup = self.popup(Dialog::Questions, "Questions");
            popup.context = prompt.id;
        } else {
            let choices = permission_choices(&prompt);
            let cursor = self
                .permission_cursor
                .get(&prompt.id)
                .copied()
                .unwrap_or_default();
            let popup = self.popup(
                Dialog::Permissions,
                if prompt.kind == protocol::PROMPT_TRUST {
                    "Project trust"
                } else {
                    "Permission"
                },
            );
            popup.context = prompt.id;
            popup.detail = prompt.question;
            popup.choices = choices;
            popup.cursor = cursor;
        }
    }
    pub fn answer(&mut self, reply: protocol::PromptReplyParams) -> Vec<Effect> {
        let Some(prompt) = self.prompts.get(&reply.id) else {
            return vec![];
        };
        if self.prompt_busy.contains(&reply.id) {
            self.status("answer in flight…", false);
            return vec![];
        }
        if !prompt.claimed_by.is_empty() && prompt.claimed_by != self.client_id {
            self.status("claimed by another client", true);
            return vec![];
        }
        self.prompt_busy.insert(reply.id.clone());
        self.dirty = true;
        vec![Effect::Answer {
            scope: self.scope.clone(),
            reply,
        }]
    }
    pub fn permission_choice(&mut self, choice: &str) -> Vec<Effect> {
        let Some(prompt) = self.prompts.get(&self.active_prompt).cloned() else {
            return self.close_popup();
        };
        match choice {
            "deny" => {
                let draft = self
                    .permission_drafts
                    .get(&prompt.id)
                    .filter(|(kind, _)| kind == "deny")
                    .map(|(_, text)| text.clone())
                    .unwrap_or_default();
                let popup = self.popup(Dialog::DenyReason, "Why not? (optional) · enter denies");
                popup.context = prompt.id;
                popup.input.set(draft);
                vec![]
            }
            "other" => {
                let draft = self
                    .permission_drafts
                    .get(&prompt.id)
                    .filter(|(kind, _)| kind == "directory")
                    .map(|(_, text)| text.clone())
                    .unwrap_or(prompt.dir.clone());
                let popup = self.popup(
                    Dialog::PermissionDirectory,
                    "Allow and add another directory",
                );
                popup.context = prompt.id;
                popup.input.set(draft);
                vec![]
            }
            _ => {
                let answer = match choice {
                    "allow" => protocol::ANSWER_ALLOW,
                    "always" | "add" => protocol::ANSWER_ALLOW_ALWAYS,
                    "prefix" => protocol::ANSWER_ALLOW_PREFIX,
                    _ => protocol::ANSWER_DENY,
                };
                self.answer(protocol::PromptReplyParams {
                    id: prompt.id,
                    answer: answer.into(),
                    ..Default::default()
                })
            }
        }
    }
    pub fn question_key(&mut self, key: &str, text: Option<&str>) -> Vec<Effect> {
        let id = self.active_prompt.clone();
        let Some(prompt) = self.prompts.get(&id) else {
            return self.close_popup();
        };
        if self.prompt_busy.contains(&id)
            || !prompt.claimed_by.is_empty() && prompt.claimed_by != self.client_id
        {
            return vec![];
        }
        let Some(draft) = self.questions.get_mut(&id) else {
            return vec![];
        };
        let Some(question) = prompt.questions.get(draft.index) else {
            return vec![];
        };
        let count = question.options.len();
        match key {
            "up" | "k" => {
                draft.cursor =
                    super::super::wrap_index(draft.cursor as i64 - 1, (count + 2) as i64) as usize
            }
            "down" | "j" => draft.cursor = (draft.cursor + 1) % (count + 2),
            "left" => {
                draft.index = draft.index.saturating_sub(1);
                draft.cursor = 0;
            }
            "right" => {
                draft.index = (draft.index + 1).min(prompt.questions.len() - 1);
                draft.cursor = 0;
            }
            " " | "enter" => {
                if draft.cursor == count {
                    let value = draft.custom[draft.index].clone();
                    let popup = self.popup(Dialog::QuestionText, "Custom answer · enter saves");
                    popup.context = id.clone();
                    popup.input.set(value);
                    return vec![];
                }
                if draft.cursor < count {
                    if !draft.selected[draft.index].insert(draft.cursor) {
                        draft.selected[draft.index].remove(&draft.cursor);
                    }
                    if key == " " {
                        return vec![];
                    }
                }
                if draft.selected[draft.index].is_empty()
                    && draft.custom[draft.index].trim().is_empty()
                {
                    return vec![];
                }
                if draft.index + 1 < prompt.questions.len() {
                    draft.index += 1;
                    draft.cursor = 0;
                    return vec![];
                }
                if draft
                    .selected
                    .iter()
                    .zip(&draft.custom)
                    .any(|(selected, custom)| selected.is_empty() && custom.trim().is_empty())
                {
                    self.status("answer every question before submitting", true);
                    return vec![];
                }
                let details = draft
                    .selected
                    .iter()
                    .zip(&draft.custom)
                    .map(|(selected, custom)| protocol::QuestionAnswer {
                        selected: selected.iter().map(|index| *index as i64).collect(),
                        custom: custom.trim().into(),
                    })
                    .collect::<Vec<_>>();
                let answers = details
                    .iter()
                    .zip(prompt.questions.iter())
                    .map(|(detail, question)| {
                        let mut labels = detail
                            .selected
                            .iter()
                            .filter_map(|index| question.options.get(*index as usize))
                            .map(|option| option.label.clone())
                            .collect::<Vec<_>>();
                        if !detail.custom.is_empty() {
                            labels.push(detail.custom.clone());
                        }
                        labels.join(", ")
                    })
                    .collect::<Vec<_>>();
                return self.answer(protocol::PromptReplyParams {
                    id,
                    answer: protocol::ANSWER_ANSWERED.into(),
                    answers: answers.into(),
                    details: details.into(),
                    ..Default::default()
                });
            }
            _ => {
                if let Some(text) = text {
                    let popup = self.popup(Dialog::QuestionText, "Custom answer · enter saves");
                    popup.context = id;
                    popup.input.insert(text);
                }
            }
        }
        self.dirty = true;
        vec![]
    }
}
pub fn permission_choices(prompt: &protocol::PromptInfo) -> Vec<Choice> {
    let choice = |id: &str, label: String, description: &str| Choice {
        id: id.into(),
        label,
        description: description.into(),
    };
    if prompt.kind == protocol::PROMPT_TRUST {
        return vec![
            choice("allow", "Trust this project's config".into(), ""),
            choice("skip", "Not now".into(), ""),
        ];
    }
    let mut out = vec![choice("allow", "Allow once".into(), "")];
    if !prompt.dir.is_empty() {
        out.push(choice(
            "add",
            "Allow and add this directory".into(),
            &prompt.dir,
        ));
        out.push(choice(
            "other",
            "Allow and add another directory".into(),
            "",
        ));
    } else if !prompt.sticky {
        out.push(choice(
            "always",
            "Allow for this channel".into(),
            "this exact call",
        ));
        if !prompt.prefix.is_empty() {
            out.push(choice(
                "prefix",
                format!("Allow prefix {}", prompt.prefix),
                "every matching command",
            ));
        }
    }
    out.push(choice("deny", "Deny".into(), "with an optional reason"));
    out
}
