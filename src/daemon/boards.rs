use super::owner::Daemon;
use crate::{
    agent::{
        names,
        routing::Routing,
        runtime::{BoardHost, Channel},
    },
    event, protocol, tools,
    wire::Time,
};
use anyhow::{Result, ensure};
use std::sync::{Arc, Weak};
use tokio_util::sync::CancellationToken;
pub struct OwnerBoards {
    owner: Weak<Daemon>,
}
impl OwnerBoards {
    pub fn new(owner: Weak<Daemon>) -> Self {
        Self { owner }
    }
}
struct BoardTools {
    owner: Weak<Daemon>,
    source: String,
}
impl BoardTools {
    fn owner(&self) -> Result<Arc<Daemon>> {
        self.owner
            .upgrade()
            .ok_or_else(|| anyhow::anyhow!("daemon is stopped"))
    }
}
impl BoardHost for OwnerBoards {
    fn tools(&self, source: &str) -> Arc<dyn tools::Boards> {
        Arc::new(BoardTools {
            owner: self.owner.clone(),
            source: source.into(),
        })
    }
    fn report<'a>(
        &'a self,
        source: &'a str,
        caller: &'a str,
        board: &'a str,
        targets: Vec<String>,
        text: String,
        reply: Vec<String>,
    ) -> tools::ServiceFuture<'a, ()> {
        Box::pin(async move {
            let owner = self
                .owner
                .upgrade()
                .ok_or_else(|| anyhow::anyhow!("daemon is stopped"))?;
            owner.board_message(
                source,
                caller,
                tools::Delivery {
                    channel: board.into(),
                    to: targets,
                    text,
                    kind: "response".into(),
                    reply_to: reply,
                },
                true,
                &owner.cancel,
            )?;
            Ok(())
        })
    }
}
impl tools::Boards for BoardTools {
    fn create<'a>(
        &'a self,
        caller: &'a str,
        name: &'a str,
        members: Vec<String>,
        cancel: CancellationToken,
    ) -> tools::ServiceFuture<'a, String> {
        Box::pin(async move {
            self.owner()?
                .board_create(&self.source, caller, name, &members, &cancel)
        })
    }
    fn list<'a>(
        &'a self,
        caller: &'a str,
        _cancel: CancellationToken,
    ) -> tools::ServiceFuture<'a, String> {
        Box::pin(async move {
            let owner = self.owner()?;
            let mut lines = Vec::new();
            for channel in owner.channels() {
                let info = channel.info();
                if !info.archived
                    && info.board.is_some_and(|board| {
                        board.source == self.source && board.members.iter().any(|m| m == caller)
                    })
                {
                    lines.push(format!("#{} ({})", info.name, info.id));
                }
            }
            lines.sort();
            Ok(if lines.is_empty() {
                "no shared channels".into()
            } else {
                lines.join("\n")
            })
        })
    }
    fn read<'a>(
        &'a self,
        caller: &'a str,
        reference: &'a str,
        from: i64,
        limit: usize,
        _cancel: CancellationToken,
    ) -> tools::ServiceFuture<'a, String> {
        Box::pin(async move {
            let owner = self.owner()?;
            let board = owner.board_for(&self.source, caller, reference)?;
            let info = board.info();
            let from = from.max(1);
            let limit = if limit == 0 { 100 } else { limit.min(500) };
            let mut lines = vec![format!("#{} ({})", info.name, info.id)];
            let mut next = from;
            for event in owner.log.read(&info.id, from, limit as i64)? {
                next = event.seq + 1;
                if !matches!(
                    event.kind.as_str(),
                    event::CHAT_POSTED | event::CHAT_MESSAGE
                ) {
                    continue;
                }
                let chat: event::ChatPayload = event.decode()?;
                let mut meta = String::new();
                if !chat.request_id.is_empty() {
                    meta.push_str(&format!(" request_id={}", chat.request_id));
                }
                if !chat.reply_to.is_empty() {
                    meta.push_str(&format!(" reply_to={}", chat.reply_to.join(",")));
                }
                lines.push(format!(
                    "[{}] {} -> {}{}: {}",
                    event.seq,
                    chat.from,
                    chat.to.join(", "),
                    meta,
                    chat.text
                ));
            }
            lines.push(format!("next: {next}"));
            Ok(lines.join("\n"))
        })
    }
    fn message<'a>(
        &'a self,
        caller: &'a str,
        message: tools::Delivery,
        cancel: CancellationToken,
    ) -> tools::ServiceFuture<'a, String> {
        Box::pin(async move {
            self.owner()?
                .board_message(&self.source, caller, message, false, &cancel)
        })
    }
}
impl Daemon {
    pub fn board_create(
        &self,
        source: &str,
        caller: &str,
        want: &str,
        references: &[String],
        cancel: &CancellationToken,
    ) -> Result<String> {
        let home = self.channel(source)?;
        let members = Routing {
            core: home.core.clone(),
        }
        .board_members(caller, references)?;
        let home_info = home.info();
        let _names = self.names.lock().unwrap();
        ensure!(!want.trim().is_empty(), "a channel name is required");
        let name = self.choose_name(want, "", "")?;
        let board = self.new_channel(
            names::new_id("c")?,
            home_info.dir.clone(),
            Time::now(),
            home.core.configuration(),
            String::new(),
            String::new(),
        );
        board.core.commit(
            vec![board.core.event(
                "",
                event::CHANNEL_CREATED,
                event::ChannelCreatedPayload {
                    name: name.clone(),
                    dir: home_info.dir,
                    board: Some(event::BoardInfo {
                        source: source.into(),
                        owner: caller.into(),
                        members: members.clone().into(),
                    }),
                    ..Default::default()
                },
            )?],
            cancel,
        )?;
        self.channels
            .write()
            .unwrap()
            .insert(board.core.id.clone(), board.clone());
        let labels = members
            .iter()
            .filter_map(|id| home.agent(id).map(|agent| agent.name()))
            .collect::<Vec<_>>();
        Ok(format!(
            "created #{name}; channel: {}; members: {}. The configured Discord bridge mirrors this channel automatically.",
            board.core.id,
            labels.join(", ")
        ))
    }
    pub fn board_for(&self, source: &str, caller: &str, reference: &str) -> Result<Arc<Channel>> {
        let reference = reference.trim().trim_start_matches('#');
        for channel in self.channels() {
            let info = channel.info();
            if info.id != reference && info.name != reference {
                continue;
            }
            ensure!(
                info.board.is_some_and(
                    |board| board.source == source && board.members.iter().any(|m| m == caller)
                ),
                "you are not a member of channel {reference:?}"
            );
            ensure!(!info.archived, "channel {reference:?} is archived");
            return Ok(channel);
        }
        anyhow::bail!("channel {reference:?} not found")
    }
    pub fn board_message(
        &self,
        source: &str,
        caller: &str,
        message: tools::Delivery,
        system: bool,
        cancel: &CancellationToken,
    ) -> Result<String> {
        let board = self.board_for(source, caller, &message.channel)?;
        let home = self.channel(source)?;
        Routing {
            core: home.core.clone(),
        }
        .deliver_board(&board.core, caller, message, "", system, cancel)
    }
    pub fn channel_tree(&self, channel: &Channel) -> Vec<protocol::AgentInfo> {
        if let Some(board) = channel.info().board {
            return self
                .channel(&board.source)
                .map(|home| {
                    home.tree()
                        .into_iter()
                        .filter(|agent| board.members.iter().any(|m| m == &agent.id))
                        .collect()
                })
                .unwrap_or_default();
        }
        channel.tree()
    }
    pub fn post(
        &self,
        channel: &Channel,
        text: &str,
        source: &str,
        cancel: &CancellationToken,
    ) -> Result<Vec<String>> {
        if let Some(board) = channel.info().board {
            let (references, body) = protocol::addressees(text);
            ensure!(!body.trim().is_empty(), "empty message");
            let home = self.channel(&board.source)?;
            Routing {
                core: home.core.clone(),
            }
            .deliver_board(
                &channel.core,
                "",
                tools::Delivery {
                    channel: channel.core.id.clone(),
                    to: references.clone(),
                    text: body,
                    kind: if references.is_empty() {
                        "info"
                    } else {
                        "request"
                    }
                    .into(),
                    reply_to: Vec::new(),
                },
                source,
                false,
                cancel,
            )?;
            return Ok(references);
        }
        channel.post(text, source, cancel)
    }
}
