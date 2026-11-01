use super::*;
use crate::{protocol, wire_struct};
use anyhow::{Context, bail, ensure};
wire_struct!(SkillInput {name:"name":String=>required});
wire_struct!(TodoInput {add:"add":Slice<TodoAdd> =>required,update:"update":Slice<TodoUpdate> =>required});
wire_struct!(AskInput {questions:"questions":Slice<event::Question> =>required});
wire_struct!(SheetInput {action:"action":String=>required,id:"id":String=>required,title:"title":String=>required,html:"html":String=>required});
wire_struct!(ChannelInput {action:"action":String=>required,name:"name":String=>required,members:"members":Slice<String> =>required,channel:"channel":String=>required,from:"from":i64=>required,limit:"limit":i64=>required});
wire_struct!(AgentInput {action:"action":String=>required,archetype:"archetype":String=>required,label:"label":String=>required,task:"task":String=>required,id:"id":String=>required,directories:"directories":Slice<String> =>required});
wire_struct!(MessageInput {channel:"channel":String=>required,to:"to":protocol::Recipients=>required,text:"text":String=>required,expect_response:"expect_response":Option<bool> =>required,reply_to:"reply_to":Slice<String> =>required,kind:"kind":String=>required});
pub(super) fn subject(name: &str, input: Option<&RawValue>) -> policy::Subject {
    match name {
        "skill" => policy::Subject::text(decode::<SkillInput>(input).unwrap_or_default().name),
        "todo" => {
            let a = decode::<TodoInput>(input).unwrap_or_default();
            policy::Subject::text(
                a.add
                    .iter()
                    .map(|i| i.text.clone())
                    .chain(a.update.iter().map(|i| i.id.clone()))
                    .collect::<Vec<_>>()
                    .join("\n"),
            )
        }
        "ask" => policy::Subject::text(
            parse_questions(input)
                .unwrap_or_default()
                .into_iter()
                .map(|q| q.question)
                .collect::<Vec<_>>()
                .join(" | "),
        ),
        "sheet" => {
            let a = decode::<SheetInput>(input).unwrap_or_default();
            policy::Subject::text(format!("{} {} {}", a.action, a.id, a.title).trim())
        }
        "channel" => {
            let a = decode::<ChannelInput>(input).unwrap_or_default();
            policy::Subject::text(format!(
                "{} {}",
                a.action,
                if a.action == "create" {
                    a.name
                } else {
                    a.channel
                }
            ))
        }
        "agent" => {
            let a = decode::<AgentInput>(input).unwrap_or_default();
            if crate::toolname::operation_raw(name, input) == crate::toolname::AGENT_CREATE {
                policy::Subject::text(a.archetype)
            } else {
                policy::Subject::id(a.id)
            }
        }
        "message" => {
            let a = decode::<MessageInput>(input).unwrap_or_default();
            let mut values = a.to.normalized();
            if !a.channel.is_empty() {
                values.push(format!("#{}", a.channel));
            }
            policy::Subject::new(policy::Kind::Id, values)
        }
        _ => policy::Subject::text(""),
    }
}
pub(super) async fn run(
    name: &str,
    input: Option<&RawValue>,
    env: &Env,
    cancel: CancellationToken,
) -> Result<Output> {
    let text = match name {
        "skill" => {
            let a = decode::<SkillInput>(input).context("bad input")?;
            let skill = env.skills.get(&a.name).with_context(|| {
                format!(
                    "unknown skill {}; available: {}",
                    quote(&a.name),
                    env.skills.keys().cloned().collect::<Vec<_>>().join(", ")
                )
            })?;
            format!(
                "# Skill: {}\nDirectory: {}\n\n{}",
                skill.name,
                skill.directory.display(),
                skill.body
            )
        }
        "todo" => todo(input, env)?,
        "ask" => {
            let asker = env
                .ask
                .as_ref()
                .context("asking the human is not available here")?;
            let questions = parse_questions(input)?;
            let outcome = asker.ask(questions.clone(), cancel).await;
            let result = format_answers(&questions, &outcome.answers);
            if let Some(error) = outcome.error {
                return Ok(Output::error(if outcome.answers.is_empty() {
                    error
                } else {
                    format!(
                        "{}\n\n{error}",
                        format_answers(
                            &questions[..questions.len().min(outcome.answers.len())],
                            &outcome.answers
                        )
                    )
                }));
            }
            result
        }
        "sheet" => sheet(input, env)?,
        "channel" => channel(input, env, cancel).await?,
        "agent" => agent(input, env, cancel).await?,
        "message" => message(input, env, cancel).await?,
        _ => bail!("unknown tool {}", quote(name)),
    };
    Ok(Output::ok(text))
}
fn quote(text: &str) -> String {
    serde_json::to_string(text).expect("string encoding")
}
pub fn parse_questions(input: Option<&RawValue>) -> Result<Vec<event::Question>> {
    let mut questions = decode::<AskInput>(input)
        .context("bad input")?
        .questions
        .into_vec();
    ensure!(!questions.is_empty(), "at least one question is required");
    ensure!(questions.len() <= 4, "at most 4 questions per call");
    for (i, q) in questions.iter_mut().enumerate() {
        q.question = q.question.trim().into();
        let number = i + 1;
        ensure!(!q.question.is_empty(), "question {number} needs its text");
        ensure!(
            !q.options.is_empty(),
            "question {number} needs at least one option (the human can always type something else)"
        );
        ensure!(q.options.len() <= 4, "question {number}: at most 4 options");
        ensure!(
            q.options.iter().all(|o| !o.label.trim().is_empty()),
            "question {number}: every option needs a label"
        );
    }
    Ok(questions)
}
pub fn format_answers(questions: &[event::Question], answers: &[String]) -> String {
    questions
        .iter()
        .enumerate()
        .map(|(i, q)| {
            format!(
                "{} → {}",
                q.question,
                answers
                    .get(i)
                    .map(|s| s.trim())
                    .filter(|s| !s.is_empty())
                    .unwrap_or("(no answer)")
            )
        })
        .collect::<Vec<_>>()
        .join("\n")
}
fn todo(input: Option<&RawValue>, env: &Env) -> Result<String> {
    let todo = env
        .todo
        .as_ref()
        .context("the todo list is not available to this agent")?;
    let a = decode::<TodoInput>(input)?;
    let (mut add, mut updates) = (a.add.into_vec(), a.update.into_vec());
    ensure!(
        !add.is_empty() || !updates.is_empty(),
        "nothing to do: give add, update, or both"
    );
    let valid =
        |s: &str| s.is_empty() || matches!(s, "pending" | "in_progress" | "done" | "cancelled");
    for it in &mut add {
        it.text = it.text.trim().into();
        ensure!(!it.text.is_empty(), "add: a step needs text");
        ensure!(
            valid(&it.status),
            "add {}: status must be one of pending, in_progress, done, cancelled",
            quote(&it.text)
        );
    }
    for u in &mut updates {
        ensure!(!u.id.is_empty(), "update: every change needs the item's id");
        ensure!(
            valid(&u.status),
            "update {}: status must be one of pending, in_progress, done, cancelled",
            u.id
        );
        ensure!(
            !u.status.is_empty() || !u.text.trim().is_empty(),
            "update {}: nothing to change: give a status, a text, or both",
            u.id
        );
        u.text = u.text.trim().into();
    }
    let added = add.len();
    let items = todo.edit(add, updates.clone())?;
    let mut output = String::new();
    use std::fmt::Write as _;
    for it in &items[items.len().saturating_sub(added)..] {
        writeln!(output, "added {} [{}] {}", it.id, it.status, it.text)?;
    }
    for u in updates {
        for it in &items {
            if it.id == u.id {
                writeln!(output, "{} → {}", it.id, it.status)?;
            }
        }
    }
    write!(
        output,
        "{} of {} done",
        items.iter().filter(|i| i.status == "done").count(),
        items.len()
    )?;
    Ok(output)
}
fn sheet(input: Option<&RawValue>, env: &Env) -> Result<String> {
    let sheets = env
        .sheets
        .as_ref()
        .context("sheets are not available to this agent")?;
    let a = decode::<SheetInput>(input)?;
    Ok(match a.action.as_str() {
        "write" => {
            ensure!(!a.html.trim().is_empty(), "write needs html");
            ensure!(
                !a.id.is_empty() || !a.title.trim().is_empty(),
                "a new sheet needs a title"
            );
            let r = sheets.write(&a.id, a.title.trim(), &a.html)?;
            format!(
                "Sheet {} {}: {} ({} bytes), shown as a tab in the web UI.\nFile: {} (edit it with read and patch)",
                r.id,
                if a.id.is_empty() {
                    "created"
                } else {
                    "replaced"
                },
                quote(&r.title),
                r.size,
                r.path
            )
        }
        "list" => {
            let refs = sheets.list();
            if refs.is_empty() {
                "This channel has no sheets.".into()
            } else {
                refs.iter()
                    .map(|r| {
                        format!(
                            "{}  {}  by {}  {} bytes  {}",
                            r.id,
                            quote(&r.title),
                            r.author,
                            r.size,
                            r.path
                        )
                    })
                    .collect::<Vec<_>>()
                    .join("\n")
            }
        }
        "delete" => {
            ensure!(!a.id.is_empty(), "delete needs an id");
            sheets.delete(&a.id)?;
            format!("Sheet {} deleted.", a.id)
        }
        _ => bail!("action must be write, list or delete"),
    })
}
async fn channel(input: Option<&RawValue>, env: &Env, cancel: CancellationToken) -> Result<String> {
    let a = decode::<ChannelInput>(input).context("bad input")?;
    let boards = env
        .boards
        .as_ref()
        .context("shared channels are not available")?;
    match a.action.as_str() {
        "create" => {
            ensure!(!a.name.trim().is_empty(), "create requires name");
            boards
                .create(&env.agent, &a.name, a.members.into_vec(), cancel)
                .await
        }
        "list" => boards.list(&env.agent, cancel).await,
        "read" => {
            ensure!(!a.channel.trim().is_empty(), "read requires channel");
            boards
                .read(
                    &env.agent,
                    &a.channel,
                    a.from.max(1),
                    if a.limit <= 0 {
                        100
                    } else {
                        a.limit.min(500) as usize
                    },
                    cancel,
                )
                .await
        }
        _ => bail!("action must be create, list or read"),
    }
}
async fn agent(input: Option<&RawValue>, env: &Env, cancel: CancellationToken) -> Result<String> {
    let a = decode::<AgentInput>(input).context("bad input")?;
    // Action validation has the same precedence as the Go owner.
    if a.action == "create" {
        ensure!(
            !a.archetype.trim().is_empty() && !a.task.trim().is_empty(),
            "create requires archetype, label and task"
        );
    }
    if a.action == "cancel" {
        ensure!(!a.id.trim().is_empty(), "cancel requires id");
    }
    ensure!(
        matches!(a.action.as_str(), "create" | "cancel" | "status"),
        "action must be create, cancel or status"
    );
    let orch = env
        .orchestrator
        .as_ref()
        .context("orchestration tools are not available to this agent")?;
    match a.action.as_str() {
        "create" => {
            ensure!(!a.label.trim().is_empty(), "label is required");
            let (ok, why) = orch.can_spawn(&env.agent);
            ensure!(ok, "cannot spawn: {why}");
            ensure!(
                a.directories.is_empty() || orch.supports_scopes(),
                "this runtime does not support task directory scopes"
            );
            let archetype = a.archetype.clone();
            let (id, name) = orch
                .spawn(
                    Spawn {
                        parent: env.agent.clone(),
                        archetype: a.archetype,
                        label: a.label,
                        task: a.task,
                        directories: a.directories.into_vec(),
                    },
                    cancel,
                )
                .await?;
            Ok(format!(
                "created {name} ({archetype}), id {id}; address it by its name"
            ))
        }
        "cancel" => {
            ensure!(
                !orch.archetypes(&env.agent).is_empty(),
                "this role cannot cancel agents"
            );
            orch.cancel(&env.agent, &a.id)?;
            Ok("cancelled".into())
        }
        _ => Ok(status_lines(&orch.status(&env.agent, &a.id)?)),
    }
}
pub fn status_lines(statuses: &[ChildStatus]) -> String {
    use std::fmt::Write as _;
    statuses
        .iter()
        .map(|s| {
            let mut line = format!(
                "{}{} [{}] {} · turn {} · ${:.2}",
                s.name,
                if s.you { " (you)" } else { "" },
                s.id,
                s.state,
                s.turn,
                s.cost_usd
            );
            if !s.pending_replies.is_empty() {
                let _ = write!(
                    line,
                    " · owes {} repl{}",
                    s.pending_replies.len(),
                    if s.pending_replies.len() > 1 {
                        "ies"
                    } else {
                        "y"
                    }
                );
                for r in s.pending_replies.iter() {
                    let mut text = r.text.split_whitespace().collect::<Vec<_>>().join(" ");
                    if text.chars().count() > 80 {
                        text = text.chars().take(79).collect::<String>() + "…";
                    }
                    let _ = write!(line, " ({} from {}: {})", r.id, r.from_name, text);
                }
            }
            if !s.awaiting_replies.is_empty() {
                let _ = write!(
                    line,
                    " · awaits {}",
                    s.awaiting_replies
                        .iter()
                        .map(|r| r.from_name.as_str())
                        .collect::<Vec<_>>()
                        .join(", ")
                );
            }
            line
        })
        .collect::<Vec<_>>()
        .join("\n")
}
pub fn message_kind(a: &MessageInput, recipients: &[String]) -> Result<String> {
    if !a.kind.is_empty() {
        ensure!(
            a.expect_response.is_none(),
            "use expect_response instead of kind, not both"
        );
        let mut kind = a.kind.trim().to_lowercase();
        if kind == "no_reply" {
            kind = "info".into();
        }
        ensure!(
            matches!(kind.as_str(), "request" | "response" | "info"),
            "kind {}: use request, response, info or no_reply",
            quote(&a.kind)
        );
        ensure!(
            kind != "response" || !a.reply_to.is_empty(),
            "responses require reply_to request IDs"
        );
        ensure!(
            kind == "response" || a.reply_to.is_empty(),
            "reply_to is only valid for kind response"
        );
        return Ok(kind);
    }
    let expect = a.expect_response.unwrap_or(
        !(recipients.is_empty() || recipients.len() == 1 && recipients[0] == "user")
            && a.reply_to.is_empty(),
    );
    ensure!(
        !expect || !recipients.is_empty(),
        "expect_response requires explicit recipients"
    );
    Ok(if !a.reply_to.is_empty() {
        if expect {
            "response_request"
        } else {
            "response"
        }
    } else if expect {
        "request"
    } else if !recipients.is_empty() {
        "steer"
    } else {
        "info"
    }
    .into())
}
async fn message(input: Option<&RawValue>, env: &Env, cancel: CancellationToken) -> Result<String> {
    let a = decode::<MessageInput>(input).context("bad input")?;
    ensure!(!a.text.trim().is_empty(), "text is required");
    let recipients = a.to.normalized();
    let kind = message_kind(&a, &recipients)?;
    ensure!(
        !recipients.is_empty() || !a.channel.is_empty(),
        "at least one recipient is required"
    );
    ensure!(
        recipients.iter().all(|s| !s.is_empty()),
        "recipient must not be empty"
    );
    let delivery = Delivery {
        channel: a.channel,
        to: recipients,
        text: a.text,
        kind,
        reply_to: a.reply_to.into_vec(),
    };
    if !delivery.channel.is_empty() {
        env.boards
            .as_ref()
            .context("shared channels are not available")?
            .message(&env.agent, delivery, cancel)
            .await
    } else {
        env.orchestrator
            .as_ref()
            .context("orchestration tools are not available to this agent")?
            .message(&env.agent, delivery)
    }
}
