use super::*;
use crate::discord::{
    api::{self, ChannelCreate, Missing},
    permission,
    store::BridgeLock,
};
use serde_json::json;
#[derive(Clone, Default)]
struct Post {
    id: String,
    agent: String,
    body: String,
    controls: Vec<Value>,
    webhook: String,
    terminal: String,
}
#[derive(Default)]
struct ApiState {
    channels: Vec<Channel>,
    posts: BTreeMap<String, Post>,
    sent: Vec<Post>,
    responses: Vec<Value>,
    feedback: Vec<String>,
    target: Option<Message>,
    fail_send: bool,
    block: bool,
}
#[derive(Default)]
struct FakeApi {
    state: Mutex<ApiState>,
    changed: tokio::sync::Notify,
    release: tokio::sync::Notify,
}
impl FakeApi {
    async fn until(&self, predicate: impl Fn() -> bool) {
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                let changed = self.changed.notified();
                tokio::pin!(changed);
                changed.as_mut().enable();
                if predicate() {
                    break;
                }
                changed.await;
            }
        })
        .await
        .expect("bridge state did not settle");
    }
    fn record(
        &self,
        agent: String,
        body: String,
        controls: Vec<Value>,
        webhook: String,
        terminal: String,
    ) -> Result<String> {
        let mut state = self.state.lock().unwrap();
        if std::mem::take(&mut state.fail_send) {
            bail!("fixture send failed");
        }
        let id = format!("m{}", state.sent.len() + 1);
        let post = Post {
            id: id.clone(),
            agent,
            body,
            controls,
            webhook,
            terminal,
        };
        state.sent.push(post.clone());
        state.posts.insert(id.clone(), post);
        self.changed.notify_one();
        Ok(id)
    }
    fn update(&self, id: &str, body: String, controls: Vec<Value>) -> Result<()> {
        let mut state = self.state.lock().unwrap();
        let post = state.posts.get_mut(id).ok_or(Missing)?;
        post.body = body;
        post.controls = controls;
        self.changed.notify_one();
        Ok(())
    }
    fn sent(&self) -> Vec<Post> {
        self.state.lock().unwrap().sent.clone()
    }
}
impl Api for FakeApi {
    fn channels(&self, _: String, _: CancellationToken) -> ServiceFuture<'_, Vec<Channel>> {
        Box::pin(async move { Ok(self.state.lock().unwrap().channels.clone()) })
    }
    fn create(
        &self,
        guild: String,
        data: ChannelCreate,
        _: CancellationToken,
    ) -> ServiceFuture<'_, Channel> {
        Box::pin(async move {
            let mut state = self.state.lock().unwrap();
            let channel = Channel {
                id: format!("d{}", state.channels.len() + 1),
                guild_id: guild,
                name: data.name,
                kind: data.kind,
                parent_id: (!data.parent_id.is_empty()).then_some(data.parent_id),
                topic: (!data.topic.is_empty()).then_some(data.topic),
            };
            state.channels.push(channel.clone());
            Ok(channel)
        })
    }
    fn rename(&self, id: String, name: String, _: CancellationToken) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            self.state
                .lock()
                .unwrap()
                .channels
                .iter_mut()
                .find(|channel| channel.id == id)
                .unwrap()
                .name = name;
            Ok(())
        })
    }
    fn send(
        &self,
        _: String,
        agent: String,
        body: String,
        controls: Vec<Value>,
        _: CancellationToken,
    ) -> ServiceFuture<'_, String> {
        Box::pin(async move {
            let blocked = self.state.lock().unwrap().block;
            if blocked {
                self.changed.notify_one();
                self.release.notified().await;
            }
            self.record(agent, body, controls, String::new(), String::new())
        })
    }
    fn send_question(
        &self,
        _: String,
        agent: String,
        body: String,
        controls: Vec<Value>,
        _: CancellationToken,
    ) -> ServiceFuture<'_, (String, String)> {
        Box::pin(async move {
            Ok((
                self.record(agent, body, controls, "owned-hook".into(), String::new())?,
                "owned-hook".into(),
            ))
        })
    }
    fn send_user(
        &self,
        _: String,
        _: String,
        user: String,
        body: String,
        _: CancellationToken,
    ) -> ServiceFuture<'_, String> {
        Box::pin(
            async move { self.record("Operator".into(), body, vec![], "human-hook".into(), user) },
        )
    }
    fn edit(
        &self,
        _: String,
        message: String,
        body: String,
        controls: Vec<Value>,
        _: CancellationToken,
    ) -> ServiceFuture<'_, ()> {
        Box::pin(async move { self.update(&message, body, controls) })
    }
    fn edit_webhook(
        &self,
        webhook: String,
        message: String,
        body: String,
        controls: Vec<Value>,
        _: CancellationToken,
    ) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            assert_eq!(webhook, "owned-hook");
            self.update(&message, body, controls)
        })
    }
    fn message(&self, _: String, _: String, _: CancellationToken) -> ServiceFuture<'_, Message> {
        Box::pin(async move {
            self.state
                .lock()
                .unwrap()
                .target
                .clone()
                .ok_or_else(|| Missing.into())
        })
    }
    fn own_webhook(
        &self,
        _: String,
        webhook: String,
        _: CancellationToken,
    ) -> ServiceFuture<'_, bool> {
        Box::pin(async move { Ok(webhook == "owned-hook") })
    }
    fn typing(&self, _: String, _: CancellationToken) -> ServiceFuture<'_, ()> {
        Box::pin(async { Ok(()) })
    }
    fn respond(&self, _: Value, response: Value, _: CancellationToken) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            self.state.lock().unwrap().responses.push(response);
            Ok(())
        })
    }
    fn response_edit(
        &self,
        _: Value,
        body: String,
        _: Vec<Value>,
        _: CancellationToken,
    ) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            self.state.lock().unwrap().feedback.push(body);
            Ok(())
        })
    }
}
struct RpcState {
    channels: Vec<protocol::ChannelInfo>,
    agents: Vec<protocol::AgentInfo>,
    prompts: Vec<protocol::PromptInfo>,
    calls: Vec<(String, Value)>,
    fail_post: bool,
    seq: i64,
}
struct FakeRpc(Mutex<RpcState>);
impl Rpc for FakeRpc {
    fn call(&self, method: &str, params: Value) -> ServiceFuture<'_, Value> {
        let method = method.to_owned();
        Box::pin(async move {
            let mut state = self.0.lock().unwrap();
            state.calls.push((method.clone(), params.clone()));
            Ok(match method.as_str() {
                protocol::MCHANNEL_LIST => serde_json::to_value(protocol::ChannelListResult {
                    channels: state.channels.clone().into(),
                })?,
                protocol::MCHANNEL_RESUME => serde_json::to_value(
                    state
                        .channels
                        .iter()
                        .find(|channel| params["channel"] == channel.id)
                        .unwrap(),
                )?,
                protocol::MRECONCILE => serde_json::to_value(protocol::ReconcileResult {
                    channel: state
                        .channels
                        .iter()
                        .find(|channel| params["channel"] == channel.id)
                        .unwrap()
                        .clone(),
                    seq: state.seq,
                    agents: state.agents.clone().into(),
                    ..Default::default()
                })?,
                protocol::MAGENT_TREE => serde_json::to_value(protocol::AgentTreeResult {
                    agents: state.agents.clone().into(),
                })?,
                protocol::MPROMPT_LIST => serde_json::to_value(protocol::PromptListResult {
                    prompts: state.prompts.clone().into(),
                })?,
                protocol::MPROMPT_CLAIM => {
                    let prompt = state
                        .prompts
                        .iter_mut()
                        .find(|p| params["id"] == p.id)
                        .unwrap();
                    if !prompt.claimed_by.is_empty() && prompt.claimed_by != "bridge" {
                        return Err(crate::client::Error::Protocol(protocol::Error::new(
                            protocol::ERR_CONFLICT,
                            "fixture claimed",
                        ))
                        .into());
                    }
                    prompt.claimed_by = "bridge".into();
                    json!({})
                }
                protocol::MPROMPT_REPLY => {
                    state.prompts.retain(|p| params["id"] != p.id);
                    json!({})
                }
                protocol::MCHANNEL_POST => {
                    if state.fail_post {
                        bail!("fixture connection lost after send");
                    }
                    json!({"to":["main"]})
                }
                _ => json!({}),
            })
        })
    }
}
struct Fixture {
    root: tempfile::TempDir,
    cfg: config::Discord,
    api: Arc<FakeApi>,
    rpc: Arc<FakeRpc>,
    bridge: Arc<Bridge>,
    link: Arc<Link>,
}
impl Fixture {
    fn new(prompts: Vec<protocol::PromptInfo>, board: bool) -> Self {
        let root = tempfile::tempdir().unwrap();
        let project = root.path().join("project");
        std::fs::create_dir(&project).unwrap();
        let cfg = config::Discord {
            guild: "guild".into(),
            category: "stavlos".into(),
            approvers: vec!["operator".into()],
            dirs: vec![project.to_str().unwrap().into()],
            ..Default::default()
        };
        let info = protocol::ChannelInfo {
            id: "channel".into(),
            name: "work".into(),
            dir: project.to_str().unwrap().into(),
            board: board.then(|| event::BoardInfo {
                source: "source".into(),
                owner: "root".into(),
                ..Default::default()
            }),
            ..Default::default()
        };
        let api = Arc::new(FakeApi::default());
        let rpc = Arc::new(FakeRpc(Mutex::new(RpcState {
            channels: vec![info],
            prompts,
            agents: vec![protocol::AgentInfo {
                id: "root".into(),
                name: "main".into(),
                channel: "channel".into(),
                state: protocol::AGENT_IDLE.into(),
                ..Default::default()
            }],
            calls: vec![],
            fail_post: false,
            seq: 100,
        })));
        let lock = BridgeLock::acquire(root.path(), "guild", "stavlos").unwrap();
        let bridge = Bridge::new(
            cfg.clone(),
            api.clone(),
            "bot".into(),
            lock.prompts,
            lock.boards,
            Arc::new(|_| {}),
            CancellationToken::new(),
        )
        .unwrap();
        let link = bridge.attach(rpc.clone(), "bridge".into());
        Self {
            root,
            cfg,
            api,
            rpc,
            bridge,
            link,
        }
    }
    async fn ready(&self) {
        self.bridge.discover(self.link.clone()).await.unwrap();
    }
    fn discord(&self) -> String {
        self.bridge.routing.lock().unwrap().workers["channel"]
            .discord
            .clone()
    }
    async fn flush(&self) {
        let (done, received) = oneshot::channel();
        assert!(self.bridge.enqueue(
            "channel",
            Work {
                link: self.link.clone(),
                payload: Payload::Tick,
                done: Some(done)
            }
        ));
        tokio::time::timeout(Duration::from_secs(2), received)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
    }
    fn event(&self, seq: i64, kind: &str, payload: impl Serialize) {
        let mut event = event::Event::new("channel", "root", kind, payload).unwrap();
        event.seq = seq;
        assert!(self.bridge.enqueue(
            "channel",
            Work {
                link: self.link.clone(),
                payload: Payload::Event(Box::new(event)),
                done: None
            }
        ));
    }
    fn interaction(&self, prompt: &str, action: &str, arg: &str) -> Value {
        json!({"guild_id":"guild","channel_id":self.discord(),"member":{"user":{"id":"operator"}},"type":3,"data":{"custom_id":permission::component_id(prompt,action,arg)}})
    }
    fn message(&self, id: &str, body: &str) -> Message {
        Message {
            id: id.into(),
            guild_id: "guild".into(),
            channel_id: self.discord(),
            content: body.into(),
            author: Some(api::User {
                id: "operator".into(),
                ..Default::default()
            }),
            ..Default::default()
        }
    }
    fn calls(&self, name: &str) -> Vec<Value> {
        self.rpc
            .0
            .lock()
            .unwrap()
            .calls
            .iter()
            .filter(|(method, _)| method == name)
            .map(|(_, params)| params.clone())
            .collect()
    }
}
fn question() -> protocol::PromptInfo {
    protocol::PromptInfo {
        id: "question".into(),
        channel: "channel".into(),
        agent: "root".into(),
        from: "main".into(),
        kind: protocol::PROMPT_QUESTION.into(),
        escalated: true,
        questions: vec![protocol::Question {
            question: "Pick a path".into(),
            options: vec![
                protocol::QuestionOption {
                    label: "A, with comma".into(),
                    ..Default::default()
                },
                protocol::QuestionOption {
                    label: "B".into(),
                    ..Default::default()
                },
            ]
            .into(),
        }]
        .into(),
        ..Default::default()
    }
}
fn permission_prompt() -> protocol::PromptInfo {
    protocol::PromptInfo {
        id: "permission".into(),
        channel: "channel".into(),
        agent: "root".into(),
        from: "main".into(),
        kind: protocol::PROMPT_PERMISSION.into(),
        tool: "shell".into(),
        input: Some(serde_json::value::to_raw_value(&json!({"command":"cargo test"})).unwrap()),
        prefix: "cargo test".into(),
        escalated: true,
        ..Default::default()
    }
}
#[tokio::test]
async fn operator_and_loop_filtering_duplicate_suppression_and_reply_identity_are_preserved() {
    let f = Fixture::new(vec![], false);
    f.ready().await;
    for mutation in 0..5 {
        let mut message = f.message(&format!("bad{mutation}"), "task");
        match mutation {
            0 => message.author.as_mut().unwrap().bot = true,
            1 => message.author.as_mut().unwrap().id = "intruder".into(),
            2 => message.guild_id = "other".into(),
            3 => message.webhook_id = Some("owned-hook".into()),
            _ => message.channel_id = "other".into(),
        };
        f.bridge.message(message).await;
    }
    f.bridge
        .message(f.message("once", "<@bot> @main task"))
        .await;
    f.bridge
        .message(f.message("once", "<@bot> @main task"))
        .await;
    f.flush().await;
    assert_eq!(
        f.calls(protocol::MCHANNEL_POST),
        vec![json!({"channel":"channel","text":"@main task"})]
    );
    for (id, webhook, expected) in [
        ("agent-reply", "owned-hook", "@scout reply"),
        ("human-reply", "human-hook", "reply"),
    ] {
        f.api.state.lock().unwrap().target = Some(Message {
            webhook_id: Some(webhook.into()),
            author: Some(api::User {
                username: "scout".into(),
                ..Default::default()
            }),
            ..Default::default()
        });
        let mut message = f.message(id, "reply");
        message.message_reference = Some(api::Reference {
            message_id: "target".into(),
            ..Default::default()
        });
        f.bridge.message(message).await;
        f.flush().await;
        assert_eq!(
            f.calls(protocol::MCHANNEL_POST).last().unwrap()["text"],
            expected
        );
    }
    let mut message = f.message("foreign-ref", "reply");
    message.message_reference = Some(api::Reference {
        message_id: "target".into(),
        guild_id: "other".into(),
        ..Default::default()
    });
    f.bridge.message(message).await;
    f.flush().await;
    assert_eq!(f.calls(protocol::MCHANNEL_POST).len(), 3);
    assert!(f.api.sent().last().unwrap().body.contains("Reply targets"));
    f.bridge.close_workers().await;
}
#[tokio::test]
async fn permission_actions_claim_then_reply_and_edit_the_original_agent_card() {
    let f = Fixture::new(vec![permission_prompt()], false);
    f.ready().await;
    let original = f.api.sent()[0].clone();
    assert_eq!(original.agent, "main");
    assert_eq!(original.webhook, "owned-hook");
    f.bridge
        .interaction(f.interaction("permission", "prefix", ""))
        .await;
    f.flush().await;
    assert_eq!(
        f.calls(protocol::MPROMPT_REPLY)[0]["answer"],
        protocol::ANSWER_ALLOW_PREFIX
    );
    let methods = f
        .rpc
        .0
        .lock()
        .unwrap()
        .calls
        .iter()
        .filter(|(name, _)| {
            matches!(
                name.as_str(),
                protocol::MPROMPT_CLAIM | protocol::MPROMPT_REPLY
            )
        })
        .map(|(name, _)| name.clone())
        .collect::<Vec<_>>();
    assert_eq!(
        methods,
        vec![protocol::MPROMPT_CLAIM, protocol::MPROMPT_REPLY]
    );
    {
        let state = f.api.state.lock().unwrap();
        assert_eq!(state.responses[0]["type"], 6);
        let resolved = &state.posts[&original.id];
        assert!(resolved.controls.is_empty());
        assert!(resolved.body.contains("cargo test"));
        assert!(resolved.body.contains("Allowed prefix"));
    }
    assert!(f.bridge.store.snapshot().is_empty());
    f.bridge.close_workers().await;
}
#[tokio::test]
async fn shared_question_draft_combines_choices_and_custom_text_and_rejects_stale_or_claimed_edits()
{
    let f = Fixture::new(vec![question()], false);
    f.ready().await;
    let original = f.api.sent()[0].id.clone();
    f.bridge
        .interaction(f.interaction("question", "toggle", "0.0"))
        .await;
    f.flush().await;
    let mut modal = f.interaction("question", "text-submit", "0");
    modal["type"] = json!(5);
    modal["data"]["components"] =
        json!([{"type":1,"components":[{"type":4,"custom_id":"value","value":"custom"}]}]);
    f.bridge.interaction(modal).await;
    f.flush().await;
    f.bridge
        .interaction(f.interaction("question", "toggle", "9.1"))
        .await;
    f.flush().await;
    assert!(
        f.api.state.lock().unwrap().posts[&original]
            .body
            .contains("view changed")
    );
    f.rpc.0.lock().unwrap().prompts[0].claimed_by = "other-client".into();
    f.bridge
        .interaction(f.interaction("question", "toggle", "0.1"))
        .await;
    f.flush().await;
    assert!(f.calls(protocol::MPROMPT_REPLY).is_empty());
    f.rpc.0.lock().unwrap().prompts[0].claimed_by.clear();
    f.bridge
        .interaction(f.interaction("question", "submit", "0"))
        .await;
    f.flush().await;
    let replies = f.calls(protocol::MPROMPT_REPLY);
    assert_eq!(replies[0]["answers"], json!(["A, with comma, custom"]));
    assert_eq!(
        replies[0]["details"],
        json!([{"selected":[0],"custom":"custom"}])
    );
    {
        let state = f.api.state.lock().unwrap();
        assert_eq!(state.sent.len(), 1);
        let resolved = &state.posts[&original];
        assert!(resolved.controls.is_empty());
        assert!(resolved.body.contains("A, with comma"));
        assert!(resolved.body.contains("custom"));
    }
    f.bridge.close_workers().await;
}
#[tokio::test]
async fn external_answer_notification_retains_destination_until_durable_result_and_restart_never_reposts_chat()
 {
    let f = Fixture::new(vec![question()], false);
    f.ready().await;
    let message = f.api.sent()[0].id.clone();
    f.rpc.0.lock().unwrap().prompts.clear();
    f.flush().await;
    assert!(
        f.api.state.lock().unwrap().posts[&message]
            .body
            .contains("Updating result")
    );
    assert_eq!(f.bridge.store.snapshot().len(), 1);
    f.bridge.close_workers().await;
    let lock = BridgeLock::acquire(f.root.path(), "guild", "stavlos").unwrap();
    let restarted = Bridge::new(
        f.cfg.clone(),
        f.api.clone(),
        "bot".into(),
        lock.prompts,
        lock.boards,
        Arc::new(|_| {}),
        CancellationToken::new(),
    )
    .unwrap();
    let link = restarted.attach(f.rpc.clone(), "bridge".into());
    restarted.discover(link.clone()).await.unwrap();
    assert_eq!(f.calls(protocol::MSUBSCRIBE).last().unwrap()["from"], 101);
    // Stored cards originally appeared at seq100; replay starts at101 even though the current snapshot moves ahead.
    let mut old = event::Event::new(
        "channel",
        "root",
        event::CHAT_MESSAGE,
        event::ChatPayload {
            from: "main".into(),
            text: "old chat".into(),
            ..Default::default()
        },
    )
    .unwrap();
    old.seq = 99;
    let mut outcome = event::Event::new(
        "channel",
        "root",
        event::ASK_RESOLVED,
        event::AskResolvedPayload {
            id: "question".into(),
            outcome: event::ASK_ANSWERED.into(),
            answers: vec!["terminal answer".into()].into(),
            ..Default::default()
        },
    )
    .unwrap();
    outcome.seq = 100;
    for event in [old, outcome] {
        assert!(restarted.enqueue(
            "channel",
            Work {
                link: link.clone(),
                payload: Payload::Event(Box::new(event)),
                done: None
            }
        ));
    }
    let (done, received) = oneshot::channel();
    restarted.enqueue(
        "channel",
        Work {
            link,
            payload: Payload::Tick,
            done: Some(done),
        },
    );
    received.await.unwrap().unwrap();
    assert!(restarted.store.snapshot().is_empty());
    {
        let state = f.api.state.lock().unwrap();
        assert_eq!(state.sent.len(), 1);
        assert!(state.posts[&message].body.contains("terminal answer"));
    }
    restarted.close_workers().await;
}
#[tokio::test]
async fn shared_board_cursor_advances_only_after_successful_mirror_and_execution_chat_uses_snapshot_cutoff()
 {
    let f = Fixture::new(vec![], true);
    f.ready().await;
    assert_eq!(f.calls(protocol::MSUBSCRIBE)[0]["from"], 1);
    f.event(
        1,
        event::CHAT_MESSAGE,
        event::ChatPayload {
            from: "main".into(),
            text: "board initial".into(),
            ..Default::default()
        },
    );
    f.flush().await;
    assert_eq!(f.bridge.boards.get(&f.discord()), 1);
    f.api.state.lock().unwrap().fail_send = true;
    f.event(
        2,
        event::CHAT_MESSAGE,
        event::ChatPayload {
            from: "main".into(),
            text: "board unsent".into(),
            ..Default::default()
        },
    );
    tokio::time::timeout(Duration::from_secs(2), f.link.cancel.cancelled())
        .await
        .unwrap();
    assert_eq!(f.bridge.boards.get(&f.discord()), 1);
    f.bridge.close_workers().await;
    let ordinary = Fixture::new(vec![], false);
    ordinary.ready().await;
    ordinary.event(
        1,
        event::CHAT_MESSAGE,
        event::ChatPayload {
            from: "main".into(),
            text: "old".into(),
            ..Default::default()
        },
    );
    ordinary.event(
        101,
        event::CHAT_MESSAGE,
        event::ChatPayload {
            from: "main".into(),
            text: "new".into(),
            to: vec!["user".into(), "scout".into()].into(),
            ..Default::default()
        },
    );
    ordinary.flush().await;
    assert_eq!(ordinary.api.sent().len(), 1);
    assert_eq!(ordinary.api.sent()[0].body, "@scout new");
    ordinary.bridge.close_workers().await;
}
#[tokio::test]
async fn directory_grants_archive_and_duplicate_markers_limit_routing() {
    let f = Fixture::new(vec![], false);
    let child = Path::new(&f.cfg.dirs[0]).join("child");
    std::fs::create_dir(&child).unwrap();
    let mut disallowed = f.rpc.0.lock().unwrap().channels[0].clone();
    disallowed.id = "child".into();
    disallowed.dir = child.to_str().unwrap().into();
    f.rpc.0.lock().unwrap().channels.push(disallowed);
    f.ready().await;
    assert_eq!(f.bridge.routing.lock().unwrap().workers.len(), 1);
    f.rpc.0.lock().unwrap().channels[0].archived = true;
    f.ready().await;
    assert!(f.bridge.routing.lock().unwrap().workers.is_empty());
    assert_eq!(f.calls(protocol::MUNSUBSCRIBE)[0]["channel"], "channel");
    f.bridge.close_workers().await;
    let duplicate = Fixture::new(vec![], false);
    duplicate.ready().await;
    {
        let mut state = duplicate.api.state.lock().unwrap();
        let mut channel = state
            .channels
            .iter()
            .find(|channel| channel.kind == 0)
            .unwrap()
            .clone();
        channel.id = "duplicate".into();
        state.channels.push(channel);
    }
    assert!(
        duplicate
            .bridge
            .discover(duplicate.link.clone())
            .await
            .is_err()
    );
    duplicate.bridge.close_workers().await;
}
#[tokio::test]
async fn uncertain_posts_are_not_retried_and_queue_overload_cancels_generation() {
    let f = Fixture::new(vec![], false);
    f.ready().await;
    f.rpc.0.lock().unwrap().fail_post = true;
    f.bridge.message(f.message("uncertain", "task")).await;
    f.flush().await;
    assert_eq!(f.calls(protocol::MCHANNEL_POST).len(), 1);
    assert!(f.api.sent().last().unwrap().body.contains("uncertain"));
    f.api.state.lock().unwrap().block = true;
    f.event(
        101,
        event::CHAT_MESSAGE,
        event::ChatPayload {
            from: "main".into(),
            text: "blocked".into(),
            ..Default::default()
        },
    );
    for _ in 0..258 {
        if !f.bridge.enqueue(
            "channel",
            Work {
                link: f.link.clone(),
                payload: Payload::Tick,
                done: None,
            },
        ) {
            break;
        }
    }
    assert!(f.link.cancel.is_cancelled());
    f.bridge.close_workers().await;
}
#[tokio::test]
async fn terminal_addressing_and_agent_parent_identity_stay_current_between_polls() {
    let f = Fixture::new(vec![], false);
    f.ready().await;
    f.event(
        101,
        event::CHAT_POSTED,
        event::ChatPayload {
            from: "human:tui".into(),
            text: "hello".into(),
            to: vec!["main".into(), "user".into()].into(),
            ..Default::default()
        },
    );
    f.event(
        102,
        event::CHAT_POSTED,
        event::ChatPayload {
            from: "human:discord".into(),
            text: "no loop".into(),
            ..Default::default()
        },
    );
    f.event(
        103,
        event::AGENT_SPAWNED,
        event::AgentSpawnedPayload {
            id: "child".into(),
            parent: "root".into(),
            name: "scout".into(),
            role: "research".into(),
            ..Default::default()
        },
    );
    f.flush().await;
    let sent = f.api.sent();
    assert_eq!(sent.len(), 2);
    assert_eq!(sent[0].body, "@main @user hello");
    assert_eq!(sent[0].terminal, "operator");
    assert_eq!(sent[1].agent, "main");
    assert_eq!(sent[1].body, "scout joined (research).");
    f.bridge.close_workers().await;
}

#[tokio::test]
async fn event_mirroring_matches_eight_frozen_go_journeys() {
    let cases: Vec<Value> = serde_json::from_str(include_str!(
        "../../tests/data/go-discord-mirror-journeys.json"
    ))
    .unwrap();
    assert_eq!(cases.len(), 8);
    for case in cases {
        let mut f = Fixture::new(vec![], false);
        if case["operators"] == 2 {
            Arc::get_mut(&mut f.bridge)
                .unwrap()
                .cfg
                .approvers
                .push("second".into());
        }
        f.ready().await;
        for value in case["events"].as_array().unwrap() {
            let event: event::Event = serde_json::from_value(value.clone()).unwrap();
            assert!(f.bridge.enqueue(
                "channel",
                Work {
                    link: f.link.clone(),
                    payload: Payload::Event(Box::new(event)),
                    done: None
                }
            ));
        }
        f.flush().await;
        let actual = f
            .api
            .sent()
            .iter()
            .map(|post| json!({"agent":post.agent,"body":post.body,"terminal":post.terminal}))
            .collect::<Vec<_>>();
        assert_eq!(json!(actual), case["expected"], "{}", case["name"]);
        f.bridge.close_workers().await;
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn real_daemon_socket_delivers_a_question_roundtrip_with_fixed_bridge_authority() {
    use crate::{
        auth::Store,
        daemon::{
            Daemon,
            server::{Scope, Socket},
        },
        model::registry::Registry,
        paths::Paths,
    };
    let root = tempfile::tempdir().unwrap();
    let home = root.path().join("home");
    std::fs::create_dir(&home).unwrap();
    let project = root.path().join("project");
    std::fs::create_dir(&project).unwrap();
    let paths = Paths {
        config: home.join("config"),
        data: home.join("data"),
        cache: home.join("cache"),
        shared_skills: home.join("skills"),
        socket: root.path().join("owner.sock"),
    };
    let registry = Registry::new(
        None,
        Some(Arc::new(Store::open(paths.auth_file()))),
        BTreeMap::new(),
    )
    .unwrap();
    let daemon = Daemon::open(paths.clone(), home, registry).await.unwrap();
    let channel = daemon
        .create(
            project.to_str().unwrap(),
            "",
            "",
            "discord-fixture",
            &CancellationToken::new(),
        )
        .unwrap();
    daemon.prompts.set_config(crate::escalation::Config {
        answer_timeout: Duration::from_secs(3600),
        ..Default::default()
    });
    let socket = Socket::bind(&paths.socket).unwrap();
    let stopping = daemon.cancel.clone();
    let handler = daemon.clone();
    let hub = daemon.hub.clone();
    let serving =
        tokio::spawn(async move { socket.serve(handler, hub, stopping, Scope::Bridge).await });
    let api = Arc::new(FakeApi::default());
    let state = Arc::new(Mutex::new(Progress::default()));
    let progress = state.clone();
    let changed = api.clone();
    let publish: Publish = Arc::new(move |value| {
        *progress.lock().unwrap() = value;
        changed.changed.notify_one();
    });
    let cfg = config::Discord {
        guild: "guild".into(),
        category: "stavlos".into(),
        approvers: vec!["operator".into()],
        dirs: vec![project.to_str().unwrap().into()],
        ..Default::default()
    };
    let lock = BridgeLock::acquire(root.path(), "guild", "stavlos").unwrap();
    let stop = CancellationToken::new();
    let bridge = Bridge::new(
        cfg,
        api.clone(),
        "bot".into(),
        lock.prompts,
        lock.boards,
        publish,
        stop.clone(),
    )
    .unwrap();
    let running = bridge.clone();
    let path = paths.socket.clone();
    let task = tokio::spawn(async move { running.run(&path).await });
    api.until(|| state.lock().unwrap().channels == 1).await;
    let probe = Client::connect(&paths.socket).await.unwrap();
    let forbidden = probe
        .call::<_, Value>(
            protocol::MCHANNEL_SET_MODE,
            &json!({"channel":channel.core.id,"mode":"yolo"}),
        )
        .await
        .unwrap_err();
    assert!(
        matches!(forbidden,crate::client::Error::Protocol(error)if error.code==protocol::ERR_FORBIDDEN)
    );
    probe.close();
    let mut prompt = question();
    prompt.channel = channel.core.id.clone();
    prompt.agent = channel.root().unwrap().id.clone();
    let answer = daemon.prompts.begin(prompt, CancellationToken::new(), None);
    api.until(|| !api.sent().is_empty()).await;
    let discord = bridge.routing.lock().unwrap().workers[&channel.core.id]
        .discord
        .clone();
    let interaction = |action: &str, arg: &str| json!({"guild_id":"guild","channel_id":discord,"member":{"user":{"id":"operator"}},"type":3,"data":{"custom_id":permission::component_id("question",action,arg)}});
    bridge.interaction(interaction("toggle", "0.1")).await;
    bridge.interaction(interaction("submit", "0")).await;
    let answer = tokio::time::timeout(Duration::from_secs(5), answer)
        .await
        .unwrap();
    assert_eq!(answer.value, protocol::ANSWER_ANSWERED);
    assert_eq!(answer.answers, vec!["B"]);
    assert_eq!(answer.details[0].selected.to_vec(), vec![1]);
    api.until(|| {
        api.state
            .lock()
            .unwrap()
            .posts
            .values()
            .any(|post| post.controls.is_empty() && post.body.contains("B"))
    })
    .await;
    stop.cancel();
    tokio::time::timeout(Duration::from_secs(5), task)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    daemon.close().await.unwrap();
    serving.await.unwrap().unwrap();
}
