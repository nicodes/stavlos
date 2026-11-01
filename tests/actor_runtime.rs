use serde_json::{Value, json};
use stavlos::{
    agent::{
        journal::{Core, Journal},
        pick::Models,
        runtime::{Channel, Environment, Spawn},
    },
    config,
    escalation::{self, Answer},
    event::{self, Event},
    model::{self, Model},
    paths::Paths,
    policy::{Layered, Rule, Set, Verb},
    protocol,
    sandbox::Level,
    wire::Time,
};
use std::{
    collections::BTreeMap,
    fs,
    path::Path,
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};
use tokio_util::sync::CancellationToken;
#[derive(Default)]
struct Memory {
    events: Mutex<Vec<Event>>,
    fail: Mutex<String>,
    changed: tokio::sync::Notify,
    streams: Mutex<Vec<protocol::StreamNotification>>,
}
impl Journal for Memory {
    fn append(&self, mut events: Vec<Event>, _: &CancellationToken) -> anyhow::Result<Vec<Event>> {
        anyhow::ensure!(
            !events
                .iter()
                .any(|event| event.kind == *self.fail.lock().unwrap()),
            "fixture write failure"
        );
        let mut all = self.events.lock().unwrap();
        for event in &mut events {
            event.seq = all.len() as i64 + 1;
            event.global = event.seq;
            event.numbered_time();
            all.push(event.clone());
        }
        self.changed.notify_one();
        Ok(events)
    }
    fn stream(&self, notification: protocol::StreamNotification) {
        self.streams.lock().unwrap().push(notification);
    }
}
impl Memory {
    async fn until(&self, predicate: impl Fn(&[Event]) -> bool) {
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                let changed = self.changed.notified();
                if predicate(&self.events.lock().unwrap()) {
                    return;
                }
                changed.await;
            }
        })
        .await
        .expect("actor did not produce its expected journal event");
    }
    fn count(&self, kind: &str) -> usize {
        self.events
            .lock()
            .unwrap()
            .iter()
            .filter(|event| event.kind == kind)
            .count()
    }
}
type Handler =
    Arc<dyn Fn(model::Request, CancellationToken) -> model::CompleteFuture<'static> + Send + Sync>;
struct Backend {
    own: Weak<Backend>,
    handler: Mutex<Handler>,
    requests: Mutex<Vec<model::Request>>,
    usage: Mutex<BTreeMap<String, model::PlanUsage>>,
}
impl Backend {
    fn new() -> Arc<Self> {
        Arc::new_cyclic(|own| Self {
            own: own.clone(),
            handler: Mutex::new(Arc::new(|_, _| Box::pin(async { Ok(response(vec![])) }))),
            requests: Mutex::new(Vec::new()),
            usage: Mutex::new(BTreeMap::new()),
        })
    }
    fn set(
        &self,
        handler: impl Fn(model::Request, CancellationToken) -> model::CompleteFuture<'static>
        + Send
        + Sync
        + 'static,
    ) {
        *self.handler.lock().unwrap() = Arc::new(handler);
    }
}
impl Model for Backend {
    fn complete(
        &self,
        request: model::Request,
        on_delta: Option<model::DeltaSink>,
        cancel: CancellationToken,
    ) -> model::CompleteFuture<'_> {
        self.requests.lock().unwrap().push(request.clone());
        if let Some(sink) = on_delta {
            sink(model::Delta {
                text: "owned fixture delta".into(),
                ..Default::default()
            });
        }
        let handler = self.handler.lock().unwrap().clone();
        handler(request, cancel)
    }
}
impl Models for Backend {
    fn resolve(&self, id: &str) -> anyhow::Result<(Arc<dyn Model>, model::Info)> {
        self.check(id)?;
        Ok((
            self.own.upgrade().unwrap(),
            model::Info {
                context_window: 100_000,
                max_output: 5000,
                ..Default::default()
            },
        ))
    }
    fn check(&self, id: &str) -> anyhow::Result<()> {
        anyhow::ensure!(model::split(id).is_ok(), "invalid fixture model");
        Ok(())
    }
    fn variants(&self, _: &str) -> Vec<String> {
        vec!["medium".into()]
    }
    fn plan_usage(&self) -> BTreeMap<String, model::PlanUsage> {
        self.usage.lock().unwrap().clone()
    }
    fn mark_limited(&self, provider: &str, until: Time) {
        self.usage
            .lock()
            .unwrap()
            .entry(provider.into())
            .or_default()
            .limited_until = until;
    }
}
struct Fixture {
    directory: tempfile::TempDir,
    channel: Arc<Channel>,
    journal: Arc<Memory>,
    backend: Arc<Backend>,
    prompts: Arc<escalation::Manager>,
}
impl Fixture {
    fn new() -> Self {
        let directory = tempfile::tempdir().unwrap();
        let root = directory.path();
        let work = root.join("work");
        fs::create_dir(&work).unwrap();
        let paths = Paths {
            config: root.join("config"),
            data: root.join("data"),
            cache: root.join("cache"),
            socket: root.join("socket"),
            shared_skills: root.join("skills"),
        };
        let mut cfg = config::Effective::default();
        cfg.dir = work.clone();
        cfg.model = "first/m1".into();
        cfg.mode = "yolo".into();
        cfg.models = vec!["first/m1".into(), "second/m2".into()];
        cfg.limits.max_depth = 4;
        cfg.limits.max_agents = 16;
        cfg.limits.max_calls_per_turn = 8;
        cfg.compaction.threshold = 0.9;
        cfg.compaction.max_tokens = 90_000;
        cfg.compaction.keep_tokens = 1000;
        cfg.compaction.clear_tokens = 50_000;
        cfg.compaction.max_tool_output = 32_768;
        cfg.resume_after_limit = true;
        let mut role = config::frontmatter::builtin_general();
        role.tool_rules.clear();
        role.spawn = vec!["general".into()];
        cfg.roles.insert("general".into(), role);
        cfg.root_agent = "general".into();
        cfg.policy = Layered::new(
            Set::new([Rule {
                tool: "*".into(),
                pattern: "*".into(),
                verb: Verb::Allow,
            }]),
            [],
        );
        let journal = Arc::new(Memory::default());
        let backend = Backend::new();
        let prompts = escalation::Manager::new(
            escalation::Config {
                answer_timeout: Duration::from_secs(60),
                ..Default::default()
            },
            Arc::new(|_, _| {}),
        );
        let core = Arc::new(Core::new(
            "channel".into(),
            work.to_string_lossy().into_owned(),
            Time::now(),
            Arc::new(cfg),
            "first/m1".into(),
            String::new(),
            journal.clone(),
        ));
        let channel = Channel::new(
            core,
            Arc::new(Environment {
                models: backend.clone(),
                prompts: prompts.clone(),
                project_changed: Arc::new(|_| {}),
                paths,
                home: root.join("home"),
                runtime: None,
                sandbox_level: Level::None,
                boards: None,
            }),
        );
        channel.start("fixture", &CancellationToken::new()).unwrap();
        Self {
            directory,
            channel,
            journal,
            backend,
            prompts,
        }
    }
    fn post(&self) {
        self.channel
            .post("owned task", "fixture", &CancellationToken::new())
            .unwrap();
    }
    async fn ended(&self) {
        self.journal
            .until(|events| events.iter().any(|event| event.kind == event::TURN_ENDED))
            .await;
    }
    fn work(&self) -> std::path::PathBuf {
        self.directory.path().join("work")
    }
}
fn response(blocks: Vec<model::Block>) -> model::Response {
    model::Response {
        blocks,
        stop_reason: model::STOP_END_TURN.into(),
        ..Default::default()
    }
}
fn call(name: &str, input: Value) -> model::Block {
    serde_json::from_value(
        json!({"type":"tool_use","id":format!("call_{name}"),"name":name,"input":input}),
    )
    .unwrap()
}
#[tokio::test]
async fn the_actor_runs_native_tools_streams_and_explicitly_answers_the_human_in_one_turn() {
    let fixture = Fixture::new();
    let counter = Arc::new(AtomicUsize::new(0));
    let steps = counter.clone();
    let core = fixture.channel.core.clone();
    fixture.backend.set(move |_,_|{let index=steps.fetch_add(1,Ordering::SeqCst);let replies=core.inspect(|state,_|state.pending_replies(&state.order[0]).iter().map(|request|request.id.clone()).collect::<Vec<_>>());Box::pin(async move {Ok(if index==0 {response(vec![call("patch",json!({"patch":"*** Begin Patch\n*** Add File: note.txt\n+Hello\n*** End Patch"})),call("read",json!({"path":"note.txt"})),call("message",json!({"to":"user","reply_to":replies,"text":"owned result"}))])}else{response(vec![model::Block::text("done")])})})});
    fixture.post();
    fixture.ended().await;
    assert_eq!(
        fs::read_to_string(fixture.work().join("note.txt")).unwrap(),
        "Hello\n"
    );
    assert_eq!(counter.load(Ordering::SeqCst), 2);
    assert_eq!(fixture.journal.count(event::TOOL_STARTED), 3);
    assert_eq!(fixture.journal.count(event::TOOL_FINISHED), 3);
    fixture.channel.core.inspect(|state, _| {
        assert!(!state.agents[&state.order[0]].in_turn);
        assert!(state.pending_replies(&state.order[0]).is_empty());
        assert_eq!(state.model_calls, 2);
    });
    assert_eq!(fixture.journal.streams.lock().unwrap().len(), 2);
    assert!(fixture.channel.tree()[0].context > 0);
    fixture.channel.stop().await;
}
#[tokio::test]
async fn a_control_edit_stays_blocked_until_the_human_answers_and_a_denial_writes_nothing() {
    let fixture = Fixture::new();
    let steps = Arc::new(AtomicUsize::new(0));
    fixture.backend.set(move |_,_|{let index=steps.fetch_add(1,Ordering::SeqCst);Box::pin(async move {Ok(if index==0{response(vec![call("patch",json!({"patch":"*** Begin Patch\n*** Add File: AGENTS.md\n+change\n*** End Patch"}))])}else{response(vec![])})})});
    fixture.post();
    fixture
        .journal
        .until(|events| {
            events
                .iter()
                .any(|event| event.kind == event::ASK_REQUESTED)
        })
        .await;
    let pending = fixture.prompts.pending("channel");
    assert_eq!(pending.len(), 1);
    assert!(pending[0].sticky);
    assert!(!fixture.work().join("AGENTS.md").exists());
    fixture
        .prompts
        .reply(
            &pending[0].id,
            "fixture-owner",
            Answer {
                value: "deny".into(),
                reason: "keep instructions".into(),
                ..Default::default()
            },
        )
        .unwrap();
    fixture.ended().await;
    assert!(!fixture.work().join("AGENTS.md").exists());
    let result: event::ToolFinishedPayload = fixture
        .journal
        .events
        .lock()
        .unwrap()
        .iter()
        .find(|event| event.kind == event::TOOL_FINISHED)
        .unwrap()
        .decode()
        .unwrap();
    assert!(result.denied);
    assert!(result.output.contains("keep instructions"));
    fixture.channel.stop().await;
}
#[tokio::test]
async fn a_tool_start_that_cannot_be_logged_never_runs_and_ends_the_turn_before_another_call() {
    let fixture = Fixture::new();
    *fixture.journal.fail.lock().unwrap() = event::TOOL_STARTED.into();
    fixture.backend.set(|_, _| {
        Box::pin(async {
            Ok(response(vec![call(
                "patch",
                json!({"patch":"*** Begin Patch\n*** Add File: forbidden.txt\n+x\n*** End Patch"}),
            )]))
        })
    });
    fixture.post();
    fixture.ended().await;
    assert!(!fixture.work().join("forbidden.txt").exists());
    assert_eq!(fixture.backend.requests.lock().unwrap().len(), 1);
    let ended: event::TurnEndedPayload = fixture
        .journal
        .events
        .lock()
        .unwrap()
        .iter()
        .find(|event| event.kind == event::TURN_ENDED)
        .unwrap()
        .decode()
        .unwrap();
    assert_eq!(ended.reason, event::REASON_ERROR);
    assert!(ended.error.contains("event log"));
    fixture.channel.stop().await;
}
#[tokio::test]
async fn cancelling_an_active_call_keeps_partial_output_and_usage_then_releases_the_turn() {
    let fixture = Fixture::new();
    let entered = Arc::new(tokio::sync::Notify::new());
    let ready = entered.clone();
    fixture.backend.set(move |_, cancel| {
        let ready = ready.clone();
        Box::pin(async move {
            ready.notify_one();
            cancel.cancelled().await;
            Err(model::Failure {
                response: model::Response {
                    blocks: vec![model::Block::text("partial")],
                    usage: model::Usage {
                        input_tokens: 5,
                        ..Default::default()
                    },
                    ..Default::default()
                },
                error: model::CallError::Cancelled,
            })
        })
    });
    fixture.post();
    tokio::time::timeout(Duration::from_secs(3), entered.notified())
        .await
        .unwrap();
    let root = fixture.channel.root().unwrap();
    fixture.channel.cancel(&root.id).unwrap();
    fixture.ended().await;
    fixture.channel.core.inspect(|state, _| {
        assert_eq!(state.budget_tokens, 5);
        assert!(!state.agents[&state.order[0]].in_turn);
    });
    let ended: event::TurnEndedPayload = fixture
        .journal
        .events
        .lock()
        .unwrap()
        .iter()
        .find(|event| event.kind == event::TURN_ENDED)
        .unwrap()
        .decode()
        .unwrap();
    assert_eq!(ended.reason, event::REASON_CANCELLED);
    let partial: event::AssistantMessagePayload = fixture
        .journal
        .events
        .lock()
        .unwrap()
        .iter()
        .find(|event| event.kind == event::ASSISTANT_MESSAGE)
        .unwrap()
        .decode()
        .unwrap();
    assert_eq!(partial.stop_reason, "cancelled");
    assert_eq!(partial.blocks[0].text.as_ref(), "partial");
    fixture.channel.stop().await;
}
#[tokio::test]
async fn a_provider_limit_moves_to_an_allowed_provider_and_accounts_for_both_calls() {
    let fixture = Fixture::new();
    fixture.backend.set(|request, _| {
        Box::pin(async move {
            if request.model == "m1" {
                Err(model::Failure {
                    response: model::Response {
                        usage: model::Usage {
                            input_tokens: 2,
                            ..Default::default()
                        },
                        ..Default::default()
                    },
                    error: model::CallError::Limit {
                        message: "fixture limit".into(),
                        retry_after: Duration::from_secs(60),
                    },
                })
            } else {
                Ok(model::Response {
                    usage: model::Usage {
                        input_tokens: 3,
                        ..Default::default()
                    },
                    ..response(vec![])
                })
            }
        })
    });
    fixture.post();
    fixture.ended().await;
    assert_eq!(fixture.channel.root().unwrap().model_id(), "second/m2");
    fixture.channel.core.inspect(|state, _| {
        assert_eq!(state.model_calls, 2);
        assert_eq!(state.budget_tokens, 5);
    });
    assert_eq!(fixture.backend.requests.lock().unwrap().len(), 2);
    assert_eq!(fixture.journal.count(event::AGENT_UPDATED), 1);
    fixture.channel.stop().await;
}
async fn published_pid(path: &Path) -> i32 {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            if let Ok(text) = fs::read_to_string(path)
                && let Ok(pid) = text.trim().parse::<i32>()
            {
                return pid;
            }
            tokio::time::sleep(Duration::from_millis(2)).await;
        }
    })
    .await
    .unwrap()
}
fn not_running(pid: i32) -> bool {
    match fs::read_to_string(format!("/proc/{pid}/stat")) {
        Err(_) => true,
        Ok(stat) => stat
            .rsplit_once(')')
            .unwrap()
            .1
            .trim_start()
            .starts_with('Z'),
    }
}
#[tokio::test]
async fn owner_cancellation_stops_adopted_commands_and_prevents_a_late_job_completion() {
    let fixture = Fixture::new();
    let path = fixture.work().join("owned.pid");
    let command = format!("printf '%s' $$ > '{}'; sleep 30", path.display());
    let steps = Arc::new(AtomicUsize::new(0));
    let ready = Arc::new(tokio::sync::Notify::new());
    let entered = ready.clone();
    fixture.backend.set(move |_, cancel| {
        let index = steps.fetch_add(1, Ordering::SeqCst);
        let command = command.clone();
        let ready = ready.clone();
        Box::pin(async move {
            if index == 0 {
                Ok(response(vec![call(
                    "shell",
                    json!({"command":command,"background":true}),
                )]))
            } else {
                ready.notify_one();
                cancel.cancelled().await;
                Err(model::Failure {
                    response: model::Response::default(),
                    error: model::CallError::Cancelled,
                })
            }
        })
    });
    fixture.post();
    tokio::time::timeout(Duration::from_secs(3), entered.notified())
        .await
        .unwrap();
    let pid = published_pid(&path).await;
    assert!(!not_running(pid));
    let root = fixture.channel.root().unwrap();
    assert_eq!(root.jobs.infos().len(), 1);
    fixture.channel.cancel(&root.id).unwrap();
    fixture.ended().await;
    fixture.channel.stop().await;
    assert!(not_running(pid));
    assert!(root.jobs.infos().is_empty());
    assert_eq!(fixture.journal.count(event::JOB_STOPPED), 1);
    assert_eq!(fixture.journal.count(event::JOB_FINISHED), 0);
}
#[tokio::test]
async fn directory_changes_wait_for_idle_and_publish_the_new_environment_atomically() {
    let fixture = Fixture::new();
    let entered = Arc::new(tokio::sync::Notify::new());
    let ready = entered.clone();
    fixture.backend.set(move |_, cancel| {
        let ready = ready.clone();
        Box::pin(async move {
            ready.notify_one();
            cancel.cancelled().await;
            Err(model::Failure {
                response: model::Response::default(),
                error: model::CallError::Cancelled,
            })
        })
    });
    fixture.post();
    tokio::time::timeout(Duration::from_secs(3), entered.notified())
        .await
        .unwrap();
    let next = fixture.directory.path().join("next");
    fs::create_dir(&next).unwrap();
    let calls = AtomicUsize::new(0);
    assert!(
        fixture
            .channel
            .set_directory(
                &next.to_string_lossy(),
                |_| {
                    calls.fetch_add(1, Ordering::SeqCst);
                    panic!("busy transition called loader")
                },
                &CancellationToken::new()
            )
            .await
            .is_err()
    );
    assert_eq!(calls.load(Ordering::SeqCst), 0);
    fixture
        .channel
        .cancel(&fixture.channel.root().unwrap().id)
        .unwrap();
    fixture.ended().await;
    let before = fixture.channel.core.configuration();
    let expected = next.clone();
    fixture
        .channel
        .set_directory(
            &next.to_string_lossy(),
            |path| {
                let mut cfg = (*before).clone();
                cfg.dir = path.to_owned();
                Ok(Arc::new(cfg))
            },
            &CancellationToken::new(),
        )
        .await
        .unwrap();
    fixture.channel.core.inspect(|state, config| {
        assert_eq!(Path::new(&state.dir), expected);
        assert_eq!(config.dir, expected);
        assert_eq!(state.mode, "ask");
        assert!(state.permits.calls.is_empty());
        assert!(state.agents[&state.order[0]].inbox.iter().any(|input| {
            input
                .text
                .contains("Earlier relative paths refer to the old directory")
        }));
    });
    fixture.channel.stop().await;
}
#[tokio::test]
async fn a_transient_failure_resumes_with_harness_input_and_preserves_the_users_request() {
    let fixture = Fixture::new();
    let steps = Arc::new(AtomicUsize::new(0));
    fixture.backend.set(move |_, _| {
        let index = steps.fetch_add(1, Ordering::SeqCst);
        Box::pin(async move {
            if index == 0 {
                Err(model::Failure {
                    response: model::Response::default(),
                    error: model::CallError::Transient("fixture connection dropped".into()),
                })
            } else {
                Ok(response(vec![]))
            }
        })
    });
    fixture.post();
    fixture.ended().await;
    let root = fixture.channel.root().unwrap();
    let before = fixture
        .channel
        .core
        .inspect(|state, _| state.pending_replies(&root.id));
    assert_eq!(before.len(), 1);
    assert!(
        !fixture
            .channel
            .core
            .inspect(|state, _| state.agents[&root.id].resume_at.is_zero())
    );
    fixture
        .channel
        .maybe_resume(
            Time(chrono::Local::now().fixed_offset() + chrono::Duration::minutes(1)),
            &CancellationToken::new(),
        )
        .unwrap();
    fixture
        .journal
        .until(|events| {
            events
                .iter()
                .filter(|event| event.kind == event::TURN_ENDED)
                .count()
                == 2
        })
        .await;
    assert_eq!(fixture.backend.requests.lock().unwrap().len(), 2);
    fixture
        .channel
        .core
        .inspect(|state, _| assert_eq!(state.pending_replies(&root.id)[0].id, before[0].id));
    let input: event::Input = fixture
        .journal
        .events
        .lock()
        .unwrap()
        .iter()
        .rev()
        .find(|event| event.kind == event::INPUT_QUEUED)
        .unwrap()
        .decode()
        .unwrap();
    assert_eq!(input.kind, event::INPUT_RESUME);
    assert!(input.from.is_empty());
    assert!(input.text.contains("nobody has written to you since"));
    fixture.channel.stop().await;
}
#[tokio::test]
async fn shutdown_leaves_an_open_turn_for_recovery_without_replaying_its_model_call() {
    let fixture = Fixture::new();
    let entered = Arc::new(tokio::sync::Notify::new());
    let ready = entered.clone();
    fixture.backend.set(move |_, cancel| {
        let ready = ready.clone();
        Box::pin(async move {
            ready.notify_one();
            cancel.cancelled().await;
            Err(model::Failure {
                response: model::Response::default(),
                error: model::CallError::Cancelled,
            })
        })
    });
    fixture.post();
    tokio::time::timeout(Duration::from_secs(3), entered.notified())
        .await
        .unwrap();
    fixture.channel.stop().await;
    assert_eq!(fixture.journal.count(event::TURN_ENDED), 0);
    let core = Arc::new(Core::new(
        "channel".into(),
        fixture.work().to_string_lossy().into_owned(),
        fixture.channel.core.created,
        fixture.channel.core.configuration(),
        String::new(),
        String::new(),
        fixture.journal.clone(),
    ));
    let events = fixture.journal.events.lock().unwrap().clone();
    core.restore_page(&events).unwrap();
    let recovered = Channel::new(core, fixture.channel.environment.clone());
    let calls = fixture.backend.requests.lock().unwrap().len();
    recovered.restore().unwrap();
    assert_eq!(fixture.journal.count(event::TURN_ABORTED), 1);
    assert_eq!(fixture.backend.requests.lock().unwrap().len(), calls);
    assert!(!recovered.info().archived);
    fixture
        .backend
        .set(|_, _| Box::pin(async { Ok(response(vec![])) }));
    recovered
        .post("a new owned task", "fixture", &CancellationToken::new())
        .unwrap();
    fixture
        .journal
        .until(|events| events.iter().any(|event| event.kind == event::TURN_ENDED))
        .await;
    assert_eq!(fixture.backend.requests.lock().unwrap().len(), calls + 1);
    assert_eq!(recovered.tree()[0].turn, 2);
    recovered.stop().await;
}
#[tokio::test]
async fn delegated_actors_keep_their_directory_scope_and_answer_through_the_native_orchestrator() {
    let fixture = Fixture::new();
    fs::create_dir(fixture.work().join("sub")).unwrap();
    let root = fixture.channel.root().unwrap().id.clone();
    let parent = root.clone();
    let core = fixture.channel.core.clone();
    let calls = Arc::new(Mutex::new(BTreeMap::<String, usize>::new()));
    fixture.backend.set(move |request,_|{let index={let mut calls=calls.lock().unwrap();let next=calls.entry(request.cache_key.clone()).or_default();let index=*next;*next+=1;index};let is_child=request.cache_key!=parent;let replies=core.inspect(|state,_|state.pending_replies(&request.cache_key).iter().map(|request|request.id.clone()).collect::<Vec<_>>());Box::pin(async move {Ok(if is_child && index==0{response(vec![call("patch",json!({"patch":"*** Begin Patch\n*** Add File: ../outside.txt\n+forbidden\n*** End Patch"})),call("message",json!({"to":"main","reply_to":replies,"text":"scoped result"}))])}else{response(vec![])})})});
    let child = fixture
        .channel
        .spawn(
            Spawn {
                parent: root.clone(),
                role: "general".into(),
                label: "scout".into(),
                task: "owned scoped task".into(),
                model: String::new(),
                directories: vec!["sub".into()],
            },
            &CancellationToken::new(),
        )
        .unwrap();
    let id = child.id.clone();
    fixture
        .journal
        .until(|events| {
            events
                .iter()
                .any(|event| event.kind == event::TURN_ENDED && event.agent == id)
                && events
                    .iter()
                    .any(|event| event.kind == event::TURN_ENDED && event.agent == root)
        })
        .await;
    assert!(!fixture.work().join("outside.txt").exists());
    assert_eq!(
        child.info().directories.as_ref(),
        [fixture.work().join("sub").to_string_lossy().into_owned()]
    );
    let denied: event::ToolFinishedPayload = fixture
        .journal
        .events
        .lock()
        .unwrap()
        .iter()
        .find(|event| {
            event.kind == event::TOOL_FINISHED
                && event.agent == id
                && event.decode::<event::ToolFinishedPayload>().unwrap().name == "patch"
        })
        .unwrap()
        .decode()
        .unwrap();
    assert!(denied.denied);
    assert!(
        denied
            .output
            .contains("outside the agent's task directories")
    );
    assert!(fixture.prompts.pending("").is_empty());
    fixture.channel.core.inspect(|state, _| {
        assert!(state.awaiting_replies(&root).is_empty());
        assert!(state.pending_replies(&id).is_empty());
    });
    fixture.channel.stop().await;
}
#[tokio::test]
async fn busy_agent_limits_are_atomic_and_a_kill_remains_true_when_the_journal_refuses_it() {
    let fixture = Fixture::new();
    let mut cfg = (*fixture.channel.core.configuration()).clone();
    cfg.limits.max_agents = 1;
    fixture.channel.set_configuration(Arc::new(cfg));
    let entered = Arc::new(tokio::sync::Notify::new());
    let ready = entered.clone();
    fixture.backend.set(move |_, cancel| {
        let ready = ready.clone();
        Box::pin(async move {
            ready.notify_one();
            cancel.cancelled().await;
            Err(model::Failure {
                response: model::Response::default(),
                error: model::CallError::Cancelled,
            })
        })
    });
    let root = fixture.channel.root().unwrap().id.clone();
    let child = fixture
        .channel
        .spawn(
            Spawn {
                parent: root.clone(),
                role: "general".into(),
                label: "scout".into(),
                task: "hold this owned task".into(),
                model: String::new(),
                directories: Vec::new(),
            },
            &CancellationToken::new(),
        )
        .unwrap();
    tokio::time::timeout(Duration::from_secs(3), entered.notified())
        .await
        .unwrap();
    let second = fixture.channel.spawn(
        Spawn {
            parent: root,
            role: "general".into(),
            label: "second".into(),
            task: "would exceed fan-out".into(),
            model: String::new(),
            directories: Vec::new(),
        },
        &CancellationToken::new(),
    );
    assert!(
        second
            .err()
            .unwrap()
            .to_string()
            .contains("max busy agents")
    );
    assert_eq!(fixture.journal.count(event::AGENT_SPAWNED), 2);
    *fixture.journal.fail.lock().unwrap() = event::AGENT_KILLED.into();
    assert!(fixture.channel.kill(&child.id).await.is_err());
    assert!(!child.alive());
    assert_eq!(fixture.journal.count(event::AGENT_KILLED), 0);
    fixture
        .channel
        .core
        .inspect(|state, _| assert!(state.agents[&child.id].killed));
    fixture.channel.stop().await;
}
#[tokio::test]
async fn read_only_parents_cannot_delegate_to_a_writable_role() {
    let fixture = Fixture::new();
    let mut cfg = (*fixture.channel.core.configuration()).clone();
    let mut writer = cfg.roles["general"].clone();
    writer.name = "writer".into();
    writer.read_only = false;
    cfg.roles.insert("writer".into(), writer);
    let root_role = cfg.roles.get_mut("general").unwrap();
    root_role.read_only = true;
    root_role.spawn = vec!["writer".into()];
    fixture.channel.set_configuration(Arc::new(cfg));
    let result = fixture.channel.spawn(
        Spawn {
            parent: fixture.channel.root().unwrap().id.clone(),
            role: "writer".into(),
            label: "writer".into(),
            task: "would widen authority".into(),
            model: String::new(),
            directories: Vec::new(),
        },
        &CancellationToken::new(),
    );
    assert!(
        result
            .err()
            .unwrap()
            .to_string()
            .contains("read-only agent cannot delegate")
    );
    assert_eq!(fixture.journal.count(event::AGENT_SPAWNED), 1);
    assert!(fixture.backend.requests.lock().unwrap().is_empty());
    fixture.channel.stop().await;
}
#[tokio::test]
async fn role_mcp_servers_are_started_called_and_reaped_by_the_actor() {
    let fixture = Fixture::new();
    let server = fixture.directory.path().join("mcp");
    fs::create_dir(&server).unwrap();
    let mut cfg = (*fixture.channel.core.configuration()).clone();
    cfg.roles.get_mut("general").unwrap().mcp = vec!["fixture".into()];
    cfg.mcp.insert(
        "fixture".into(),
        config::Mcp {
            command: "/usr/bin/python3".into(),
            args: vec![
                format!(
                    "{}/tests/data/mcp-stdio-fixture.py",
                    env!("CARGO_MANIFEST_DIR")
                ),
                server.to_string_lossy().into_owned(),
                "ordinary".into(),
            ],
            ..Default::default()
        },
    );
    fixture.channel.set_configuration(Arc::new(cfg));
    let calls = Arc::new(AtomicUsize::new(0));
    fixture.backend.set(move |request, _| {
        let index = calls.fetch_add(1, Ordering::SeqCst);
        assert!(
            request
                .tools
                .iter()
                .any(|tool| tool.name == "mcp__fixture__echo")
        );
        if index > 0 {
            assert!(
                request
                    .messages
                    .iter()
                    .flat_map(|message| message.blocks.iter())
                    .any(|block| block.kind == model::BLOCK_TOOL_RESULT
                        && block.content.contains("owned echo"))
            );
        }
        Box::pin(async move {
            Ok(if index == 0 {
                response(vec![call(
                    "mcp__fixture__echo",
                    json!({"value":"owned echo"}),
                )])
            } else {
                response(vec![])
            })
        })
    });
    fixture.post();
    fixture.ended().await;
    let pid = published_pid(&server.join("server.pid")).await;
    assert!(!not_running(pid));
    assert_eq!(fixture.journal.count(event::MCPSTARTED), 1);
    assert!(
        fixture
            .channel
            .root()
            .unwrap()
            .info()
            .mcp
            .iter()
            .any(|server| server.state == protocol::MCP_CONNECTED)
    );
    fixture.channel.stop().await;
    assert!(not_running(pid));
}
#[tokio::test]
async fn idle_manual_compaction_uses_the_runtime_model_and_updates_its_context_gauge() {
    let fixture = Fixture::new();
    fixture.backend.set(|_, _| {
        Box::pin(async {
            Ok(response(vec![model::Block::text(
                "owned context ".repeat(1000),
            )]))
        })
    });
    fixture.post();
    fixture.ended().await;
    fixture.backend.set(|request, _| {
        assert!(request.cache_key.ends_with(":compact"));
        assert_eq!(request.max_tokens, 4000);
        Box::pin(async {
            Ok(response(vec![model::Block::text(
                "retained native summary",
            )]))
        })
    });
    let root = fixture.channel.root().unwrap();
    assert_eq!(
        root.compact(CancellationToken::new()).await.unwrap(),
        "compacted"
    );
    assert!(root.info().context > 0);
    assert_eq!(fixture.backend.requests.lock().unwrap().len(), 2);
    assert_eq!(fixture.journal.count(event::COMPACTION_DONE), 1);
    assert_eq!(fixture.journal.count(event::TURN_STARTED), 1);
    fixture.channel.stop().await;
}
#[tokio::test]
async fn role_model_and_variant_edits_are_applied_without_running_a_model() {
    let fixture = Fixture::new();
    let root = fixture.channel.root().unwrap();
    root.set_variant("medium", &CancellationToken::new())
        .unwrap();
    assert_eq!(root.info().variant, "medium");
    assert!(
        root.set_variant("invented", &CancellationToken::new())
            .is_err()
    );
    let mut cfg = (*fixture.channel.core.configuration()).clone();
    let mut role = cfg.roles["general"].clone();
    role.name = "worker".into();
    role.models = vec![config::frontmatter::ModelSpec {
        id: "second/m2".into(),
        variants: Vec::new(),
    }];
    cfg.roles.insert("worker".into(), role);
    fixture.channel.set_configuration(Arc::new(cfg));
    root.set_role("worker", &CancellationToken::new()).unwrap();
    assert_eq!(root.info().role, "worker");
    assert_eq!(root.model_id(), "second/m2");
    assert_eq!(root.name(), "main");
    assert!(
        root.set_model("first/m1", &CancellationToken::new())
            .is_err()
    );
    assert!(fixture.backend.requests.lock().unwrap().is_empty());
    fixture.channel.stop().await;
}
#[tokio::test]
async fn an_agent_without_a_model_reports_the_setup_path_without_calling_a_provider() {
    let fixture = Fixture::new();
    let id = fixture.channel.root().unwrap().id.clone();
    fixture
        .channel
        .core
        .commit(
            vec![
                fixture
                    .channel
                    .core
                    .event(
                        &id,
                        event::AGENT_UPDATED,
                        event::AgentUpdatedPayload {
                            model: Some(String::new()),
                            ..Default::default()
                        },
                    )
                    .unwrap(),
            ],
            &CancellationToken::new(),
        )
        .unwrap();
    fixture.post();
    fixture.ended().await;
    assert!(fixture.backend.requests.lock().unwrap().is_empty());
    assert_eq!(fixture.journal.count(event::MODEL_CALL_STARTED), 0);
    assert!(
        fixture
            .channel
            .root()
            .unwrap()
            .info()
            .last_error
            .contains("/providers first")
    );
    fixture.channel.stop().await;
}
