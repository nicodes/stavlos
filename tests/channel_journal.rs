use serde_json::{Value, json};
use stavlos::{
    agent::{
        journal::{BudgetExhausted, Core, Journal, LogJournal, Stopped},
        names,
    },
    config,
    event::{self, Event},
    model, protocol,
    wire::Time,
};
use std::{
    collections::BTreeMap,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};
use tokio_util::sync::CancellationToken;
#[derive(Default)]
struct Memory {
    events: Mutex<Vec<Event>>,
    fail: Mutex<String>,
}
impl Journal for Memory {
    fn append(
        &self,
        mut events: Vec<Event>,
        cancel: &CancellationToken,
    ) -> anyhow::Result<Vec<Event>> {
        anyhow::ensure!(!cancel.is_cancelled(), "context canceled");
        let fail = self.fail.lock().unwrap().clone();
        anyhow::ensure!(
            !events.iter().any(|e| e.kind == fail),
            "fixture append failure"
        );
        let mut persisted = self.events.lock().unwrap();
        for event in &mut events {
            event.seq = persisted.len() as i64 + 1;
            event.global = event.seq;
            event.numbered_time();
            persisted.push(event.clone());
        }
        Ok(events)
    }
    fn stream(&self, _: protocol::StreamNotification) {}
}
fn make_core(config: config::Effective, journal: Arc<dyn Journal>) -> Core {
    Core::new(
        "channel".into(),
        "/work".into(),
        Time::now(),
        Arc::new(config),
        String::new(),
        String::new(),
        journal,
    )
}
fn seed(core: &Core) {
    core.commit(
        vec![
            core.event(
                "",
                event::CHANNEL_CREATED,
                json!({"name":"initial","dir":"/work","role":"main","mode":"ask"}),
            )
            .unwrap(),
            core.event(
                "a1",
                event::AGENT_SPAWNED,
                json!({"id":"a1","role":"main","name":"main"}),
            )
            .unwrap(),
        ],
        &CancellationToken::new(),
    )
    .unwrap();
}
#[test]
fn all_original_name_rules_and_budget_priorities_match() {
    let cases: Vec<Value> = serde_json::from_str(include_str!("data/go-agent-names.json")).unwrap();
    assert_eq!(cases.len(), 96);
    for c in cases {
        let want = c["want"].as_str().unwrap();
        let fallback = c["fallback"].as_str().unwrap();
        let id = c["id"].as_str().unwrap();
        let taken: BTreeMap<String, String> = serde_json::from_value(c["taken"].clone()).unwrap();
        assert_eq!(names::normalize(want), c["normalized"].as_str().unwrap());
        let result = names::claim(want, fallback, id, &taken);
        if c["error"].as_str().unwrap().is_empty() {
            assert_eq!(result.unwrap(), c["name"].as_str().unwrap());
        } else {
            assert_eq!(
                result.unwrap_err().to_string(),
                c["error"].as_str().unwrap()
            );
        }
    }
    for _ in 0..20 {
        let id = names::new_id("a").unwrap();
        assert_eq!(id.len(), 11);
        assert!(
            id.starts_with('a')
                && id[1..]
                    .bytes()
                    .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
        );
    }
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-channel-budget-reasons.json")).unwrap();
    assert_eq!(cases.len(), 223);
    let now = chrono::DateTime::parse_from_rfc3339("2026-11-01T12:00:00Z").unwrap();
    for c in cases {
        let mut config = config::Effective::default();
        config.limits = serde_json::from_value(c["limits"].clone()).unwrap();
        let core = Core::new(
            "channel".into(),
            "/work".into(),
            Time(now - chrono::Duration::minutes(2)),
            Arc::new(config),
            String::new(),
            String::new(),
            Arc::new(Memory::default()),
        );
        let mut events = Vec::new();
        for _ in 0..c["calls"].as_u64().unwrap() {
            events.push(
                core.event("a1", event::MODEL_CALL_STARTED, json!({"purpose":"turn"}))
                    .unwrap(),
            );
        }
        events.push(
            core.event(
                "a1",
                event::MODEL_CALL_COMPLETED,
                json!({"purpose":"turn","usage":{"input_tokens":c["tokens"]},"cost_usd":c["cost"]}),
            )
            .unwrap(),
        );
        core.restore_page(&events).unwrap();
        assert_eq!(
            core.budget_reason(now),
            c["reason"].as_str().unwrap(),
            "{c}"
        );
    }
}
#[tokio::test]
async fn commands_wake_only_after_commit_and_failed_observed_facts_still_follow_the_world() {
    let journal = Arc::new(Memory::default());
    let core = make_core(config::Effective::default(), journal.clone());
    seed(&core);
    let wake = Arc::new(tokio::sync::Notify::new());
    core.register_wake("a1".into(), wake.clone());
    *journal.fail.lock().unwrap() = event::INPUT_QUEUED.into();
    let queued = core
        .event(
            "a1",
            event::INPUT_QUEUED,
            event::Input {
                id: "i1".into(),
                kind: event::INPUT_PROMPT.into(),
                text: "hello".into(),
                ..Default::default()
            },
        )
        .unwrap();
    assert!(
        core.commit(vec![queued.clone()], &CancellationToken::new())
            .is_err()
    );
    assert!(core.inspect(|state, _| state.agents["a1"].inbox.is_empty()));
    assert!(
        tokio::time::timeout(Duration::from_millis(10), wake.notified())
            .await
            .is_err()
    );
    journal.fail.lock().unwrap().clear();
    core.commit(vec![queued], &CancellationToken::new())
        .unwrap();
    tokio::time::timeout(Duration::from_secs(1), wake.notified())
        .await
        .unwrap();
    assert_eq!(core.inspect(|state, _| state.agents["a1"].inbox.len()), 1);
    core.commit(
        vec![
            core.event("a1", event::TURN_STARTED, json!({"turn":1}))
                .unwrap(),
        ],
        &CancellationToken::new(),
    )
    .unwrap();
    assert!(core.inspect(|state, _| state.agents["a1"].in_turn));
    *journal.fail.lock().unwrap() = event::TURN_ENDED.into();
    assert!(
        core.fact(
            vec![
                core.event("a1", event::TURN_ENDED, json!({"turn":1,"reason":"done"}))
                    .unwrap()
            ],
            &CancellationToken::new()
        )
        .is_err()
    );
    assert!(!core.inspect(|state, _| state.agents["a1"].in_turn));
    core.stop();
    let before = core.inspect(|state, _| state.model_calls);
    assert!(
        core.fact(
            vec![
                core.event("a1", event::MODEL_CALL_STARTED, json!({"purpose":"turn"}))
                    .unwrap()
            ],
            &CancellationToken::new()
        )
        .unwrap_err()
        .is::<Stopped>()
    );
    assert_eq!(core.inspect(|state, _| state.model_calls), before);
}
#[test]
fn concurrent_reservations_are_atomic_and_failed_reservations_spend_nothing() {
    let journal = Arc::new(Memory::default());
    let mut config = config::Effective::default();
    config.limits.max_channel_calls = 7;
    let core = Arc::new(make_core(config, journal.clone()));
    seed(&core);
    *journal.fail.lock().unwrap() = event::MODEL_CALL_STARTED.into();
    assert!(
        core.reserve_call("a1", "turn", &CancellationToken::new())
            .is_err()
    );
    assert_eq!(core.inspect(|s, _| s.model_calls), 0);
    journal.fail.lock().unwrap().clear();
    let accepted = AtomicUsize::new(0);
    std::thread::scope(|scope| {
        for _ in 0..30 {
            let core = &core;
            let accepted = &accepted;
            scope.spawn(
                move || match core.reserve_call("a1", "turn", &CancellationToken::new()) {
                    Ok(_) => {
                        accepted.fetch_add(1, Ordering::SeqCst);
                    }
                    Err(e) => assert!(e.is::<BudgetExhausted>(), "{e:#}"),
                },
            );
        }
    });
    assert_eq!(accepted.load(Ordering::SeqCst), 7);
    assert_eq!(core.inspect(|s, _| s.model_calls), 7);
    let replay = make_core(config::Effective::default(), Arc::new(Memory::default()));
    replay
        .restore_page(&journal.events.lock().unwrap())
        .unwrap();
    assert_eq!(replay.inspect(|s, _| s.model_calls), 7);
}
#[test]
fn real_sqlite_acknowledgements_and_failed_usage_facts_keep_live_and_durable_accounting_distinct() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("events.db");
    let log = stavlos::eventlog::Log::open(path.clone(), false, None).unwrap();
    let streams = Arc::new(AtomicUsize::new(0));
    let copy = streams.clone();
    let journal = Arc::new(LogJournal {
        log: log.clone(),
        stream: Arc::new(move |_| {
            copy.fetch_add(1, Ordering::SeqCst);
        }),
    });
    let mut cfg = config::Effective::default();
    cfg.limits.max_channel_cost_usd = 1.;
    let core = make_core(cfg, journal);
    seed(&core);
    core.reserve_call("a1", "compaction", &CancellationToken::new())
        .unwrap();
    core.stream(protocol::StreamNotification::default());
    assert_eq!(streams.load(Ordering::SeqCst), 1);
    let persisted = log.read("channel", 1, 100).unwrap();
    assert_eq!(persisted.len(), 3);
    log.close().unwrap();
    let usage = model::Usage {
        input_tokens: 10,
        output_tokens: 20,
        cache_read_tokens: 30,
        cache_write_tokens: 40,
    };
    assert!(core.finish_call("a1", "compaction", usage, 1.25).is_err());
    assert_eq!(
        core.inspect(|s, _| (s.model_calls, s.budget_tokens, s.budget_cost)),
        (1, 100, 1.25)
    );
    assert!(
        core.reserve_call("a1", "turn", &CancellationToken::new())
            .unwrap_err()
            .is::<BudgetExhausted>()
    );
    let reopened = stavlos::eventlog::Log::open(path, false, None).unwrap();
    let durable = make_core(config::Effective::default(), Arc::new(Memory::default()));
    durable
        .restore_page(&reopened.read("channel", 1, 100).unwrap())
        .unwrap();
    assert_eq!(
        durable.inspect(|s, _| (s.model_calls, s.budget_tokens, s.budget_cost)),
        (1, 0, 0.)
    );
    reopened.close().unwrap();
}
#[tokio::test]
async fn channel_deadlines_and_parent_cancellation_end_only_the_call_scope() {
    let mut cfg = config::Effective::default();
    cfg.limits.max_channel_duration = "80ms".into();
    let core = make_core(cfg, Arc::new(Memory::default()));
    let parent = CancellationToken::new();
    let scope = core.begin_call("a1", "turn", &parent).await.unwrap();
    let token = scope.cancel.clone();
    tokio::time::timeout(Duration::from_secs(1), token.cancelled())
        .await
        .unwrap();
    assert!(!parent.is_cancelled());
    assert_eq!(
        core.budget_reason(chrono::Utc::now().fixed_offset()),
        "limits.maxChannelDuration"
    );
    drop(scope);
    let core = make_core(config::Effective::default(), Arc::new(Memory::default()));
    let scope = core.begin_call("a1", "turn", &parent).await.unwrap();
    let token = scope.cancel.clone();
    drop(scope);
    assert!(token.is_cancelled() && !parent.is_cancelled());
    let scope = core.begin_call("a1", "turn", &parent).await.unwrap();
    parent.cancel();
    assert!(scope.cancel.is_cancelled());
}
