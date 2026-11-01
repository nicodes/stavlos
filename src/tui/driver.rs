//! One terminal task owns all view state; bounded RPC tasks return generation-tagged messages.
use super::{
    controller::{Controller, Effect, Reply, Tag},
    render::Renderer,
};
use crate::{
    client::{self, Client},
    paths::Paths,
    protocol::{self, methods as rpc},
    wire::Time,
};
use anyhow::{Context, Result, bail};
use crossterm::{
    event::{
        self, Event, EventStream, KeyCode, KeyEvent, KeyEventKind, KeyModifiers, MouseButton,
        MouseEventKind,
    },
    execute,
};
use futures_util::StreamExt;
use std::{
    collections::BTreeMap,
    io::{self, IsTerminal, Write},
    path::PathBuf,
    sync::Arc,
    time::Duration,
};
use tokio::{sync::mpsc, task::JoinSet};
use tokio_util::sync::CancellationToken;
pub struct Terminal {
    pub paths: Paths,
    pub home: PathBuf,
    pub cancel: CancellationToken,
}
impl crate::app::Terminal for Terminal {
    fn run(&self, client: Arc<Client>, channel: String) -> crate::tools::ServiceFuture<'_, ()> {
        Box::pin(async move {
            run(
                client,
                channel,
                self.paths.clone(),
                self.home.clone(),
                self.cancel.child_token(),
            )
            .await
        })
    }
}
struct Restore;
impl Drop for Restore {
    fn drop(&mut self) {
        let _ = execute!(
            io::stdout(),
            event::DisableMouseCapture,
            event::DisableBracketedPaste
        );
        ratatui::restore();
    }
}
struct Effects {
    client: Arc<Client>,
    cancel: CancellationToken,
    pending: JoinSet<()>,
    replies: mpsc::Sender<Reply>,
    logins: BTreeMap<String, CancellationToken>,
    paths: Paths,
}
impl Effects {
    fn apply(&mut self, effects: Vec<Effect>) -> Result<()> {
        for effect in effects {
            match effect {
                Effect::Cancel(id) => {
                    if let Some(cancel) = self.logins.remove(&id) {
                        cancel.cancel();
                    }
                }
                Effect::Copy(text) => copy(&text)?,
                Effect::OpenBrowser(url) => crate::app::open_browser(&url),
                Effect::Remember { channel, at } => {
                    let data = self.paths.data.clone();
                    self.pending.spawn(async move {
                        let _ = tokio::task::spawn_blocking(move || {
                            crate::navigation::remember(&data, &channel, at)
                        })
                        .await;
                    });
                }
                Effect::Request(request) => {
                    if self.pending.len() >= 256 {
                        bail!("too many outstanding terminal requests");
                    }
                    let client = self.client.clone();
                    let cancel = self.cancel.child_token();
                    if let Some(id) = request.cancellation
                        && let Some(old) = self.logins.insert(id, cancel.clone())
                    {
                        old.cancel();
                    }
                    let replies = self.replies.clone();
                    self.pending.spawn(async move{
                    let call=async{let value:serde_json::Value=client.call(request.method,&request.params).await?;Ok::<_,anyhow::Error>(value)};
                    let result=tokio::select!{biased;_=cancel.cancelled()=>return,result=async{if let Some(timeout)=request.timeout{tokio::time::timeout(timeout,call).await.context("owner request timed out")?}else{call.await}}=>result};
                    let _=replies.send(Reply{scope:request.scope,tag:request.tag,result}).await;
                });
                }
                Effect::Answer { scope, reply } => {
                    if self.pending.len() >= 256 {
                        bail!("too many outstanding terminal requests");
                    }
                    let client = self.client.clone();
                    let cancel = self.cancel.child_token();
                    let replies = self.replies.clone();
                    let id = reply.id.clone();
                    self.pending.spawn(async move{let result=tokio::select!{biased;_=cancel.cancelled()=>return,result=tokio::time::timeout(Duration::from_secs(15),async{client.do_method(rpc::PROMPT_CLAIM,&protocol::PromptClaimParams{id:reply.id.clone()}).await?;client.do_method(rpc::PROMPT_REPLY,&reply).await?;Ok::<_,anyhow::Error>(serde_json::Value::Null)})=>result.context("prompt answer timed out").and_then(|result|result)};let _=replies.send(Reply{scope:Some(scope),tag:Tag::PromptAnswered(id),result}).await;});
                }
            }
        }
        Ok(())
    }
}
pub fn key_name(key: KeyEvent) -> (String, Option<String>) {
    let base = match key.code {
        KeyCode::Backspace => "backspace".into(),
        KeyCode::Enter => "enter".into(),
        KeyCode::Left => "left".into(),
        KeyCode::Right => "right".into(),
        KeyCode::Up => "up".into(),
        KeyCode::Down => "down".into(),
        KeyCode::Home => "home".into(),
        KeyCode::End => "end".into(),
        KeyCode::PageUp => "pageup".into(),
        KeyCode::PageDown => "pagedown".into(),
        KeyCode::Tab => "tab".into(),
        KeyCode::BackTab => "backtab".into(),
        KeyCode::Delete => "delete".into(),
        KeyCode::Insert => "insert".into(),
        KeyCode::F(number) => format!("f{number}"),
        KeyCode::Esc => "esc".into(),
        KeyCode::Char(ch) => ch.to_string(),
        _ => String::new(),
    };
    let control = key.modifiers.contains(KeyModifiers::CONTROL);
    let alt = key.modifiers.contains(KeyModifiers::ALT);
    let name = if control && base == " " {
        "ctrl+space".into()
    } else {
        format!(
            "{}{}{base}",
            if control { "ctrl+" } else { "" },
            if alt { "alt+" } else { "" }
        )
    };
    let text = if !control && !alt {
        match key.code {
            KeyCode::Char(ch) => Some(ch.to_string()),
            _ => None,
        }
    } else {
        None
    };
    (name, text)
}
fn copy(text: &str) -> io::Result<()> {
    use base64::Engine;
    let encoded = base64::engine::general_purpose::STANDARD.encode(text);
    let mut stdout = io::stdout().lock();
    write!(stdout, "\x1b]52;c;{encoded}\x07")?;
    stdout.flush()
}
pub async fn run(
    client: Arc<Client>,
    channel: String,
    paths: Paths,
    home: PathBuf,
    cancel: CancellationToken,
) -> Result<()> {
    if !io::stdin().is_terminal() || !io::stdout().is_terminal() {
        bail!("the terminal UI needs a terminal; use --no-tui for channel IDs");
    }
    let attach = client
        .do_method(
            rpc::ATTACH,
            &protocol::AttachParams {
                client: "tui".into(),
                tier: protocol::TIER_INTERACTIVE.into(),
            },
        )
        .await?;
    let mut controller =
        Controller::new(paths.clone(), home, attach.client_id, channel, Time::now());
    let mut terminal = ratatui::try_init()?;
    let _restore = Restore;
    execute!(
        io::stdout(),
        event::EnableMouseCapture,
        event::EnableBracketedPaste
    )?;
    let mut events = EventStream::new();
    let (sender, mut replies) = mpsc::channel(256);
    let mut effects = Effects {
        client: client.clone(),
        cancel: cancel.clone(),
        pending: JoinSet::new(),
        replies: sender,
        logins: BTreeMap::new(),
        paths,
    };
    let mut renderer = Renderer::default();
    effects.apply(controller.initial())?;
    let mut tick = tokio::time::interval(Duration::from_millis(100));
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let mut ticks = 0_u64;
    let mut drag = false;
    let result:Result<()>=async{
        loop {
            if controller.dirty {terminal.draw(|frame|renderer.draw(frame,&mut controller))?;controller.dirty=false;}
            if controller.quit {if controller.status_error&&controller.quit_armed.is_none(){bail!("{}",controller.status);}break;}
            tokio::select! {
                _=cancel.cancelled()=>break,
                error=client.closed()=>return Err(error.into()),
                Some(reply)=replies.recv()=>{let next=controller.reply(reply);effects.apply(next)?;},
                notification=client.notification()=>{
                    let Some(first)=notification else{bail!("the daemon closed the connection")};let mut notification=Some(first);
                    for index in 0..128 {let Some(frame)=notification.take()else{break};if let Ok(message)=client::decode_notification(&frame){let next=controller.notification(message);effects.apply(next)?;}
                    if index==127{break;}
                    notification=client.try_notification();}
                },
                event=events.next()=>{match event {Some(Ok(Event::Key(key)))if key.kind!=KeyEventKind::Release=>{controller.now=Time::now();let (name,text)=key_name(key);let next=controller.key(&name,text.as_deref());effects.apply(next)?;},Some(Ok(Event::Paste(text)))=>{if let Some(config)=controller.config.as_mut(){config.input.insert(&text);config.dirty=true;}else if let Some(popup)=controller.popup.as_mut(){popup.input.insert(&text);}else{controller.channel.input.insert(&text);}controller.dirty=true;},Some(Ok(Event::Resize(_, _)))=>controller.dirty=true,Some(Ok(Event::Mouse(mouse)))=>{
                    use super::controller::Focus;let point=(mouse.column,mouse.row);let position=ratatui::layout::Position::new(mouse.column,mouse.row);let hit=&renderer.hit;
                    match mouse.kind {
                        MouseEventKind::ScrollUp=>controller.scroll(-3),MouseEventKind::ScrollDown=>controller.scroll(3),
                        MouseEventKind::Down(MouseButton::Left)if controller.popup.is_none()&&controller.config.is_none()=>{
                            if hit.sidebar.contains(position){let index=hit.sidebar_top+usize::from(mouse.row-hit.sidebar.y);let next=controller.sidebar_select(index);effects.apply(next)?;}
                            else if hit.chat.contains(position){renderer.selection=Some((point,point));drag=false;controller.focus=Focus::Chat;}
                            else if hit.input.contains(position){controller.focus=Focus::Input;}
                            else if hit.tabs.contains(position){let column=usize::from(mouse.column-hit.tabs.x);let mut at=0;let mut index=0;for (i,label)in ["Async","Todo","MCP","Questions","Permissions","Dirs"].iter().enumerate(){at+=label.len()+2;if column<at{index=i;break;}}controller.focus=Focus::Tabs;controller.tab_cursor=index;let next=controller.key("enter",None);effects.apply(next)?;}
                            else if hit.meta.contains(position){controller.focus=Focus::Meta;let next=controller.key("enter",None);effects.apply(next)?;}controller.dirty=true;
                        },
                        MouseEventKind::Drag(MouseButton::Left)=>if let Some((start,_))=renderer.selection{renderer.selection=Some((start,point));drag|=start!=point;controller.dirty=true;},
                        MouseEventKind::Up(MouseButton::Left)=>if let Some((start,_))=renderer.selection {renderer.selection=Some((start,point));if drag{let text=renderer.selected_text();if !text.is_empty(){copy(&text)?;controller.status("copied selection",false);}}else if renderer.hit.chat.contains(position){let row=usize::from(mouse.row-renderer.hit.chat.y);if let Some(line)=renderer.hit.rows.get(row){controller.toggle_item(line.item);}}renderer.selection=None;controller.dirty=true;},_=>{},
                    }
                },Some(Err(error))=>return Err(error.into()),None=>break,_=>{}}},
                _=tick.tick()=>{ticks=ticks.wrapping_add(1);let next=controller.tick(Time::now(),ticks.is_multiple_of(30),ticks.is_multiple_of(300));effects.apply(next)?;},
                Some(_)=effects.pending.join_next(),if !effects.pending.is_empty()=>{},
            }
        }Ok(())
    }.await;
    cancel.cancel();
    effects.pending.abort_all();
    while effects.pending.join_next().await.is_some() {}
    let _ = tokio::time::timeout(
        Duration::from_secs(2),
        client.do_method(
            rpc::UNSUBSCRIBE,
            &protocol::SubscribeParams {
                channel: controller.scope.channel,
                ..Default::default()
            },
        ),
    )
    .await;
    result
}
