use serde_json::json;
use stavlos::{
    agent::{
        context::Conversation,
        journal::{Core, Journal},
    },
    config,
    event::{self, Event},
    model::{self, Model},
    protocol,
    wire::Time,
};
use std::{
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio_util::sync::CancellationToken;
#[derive(Default)]
struct Memory {
    events: Mutex<Vec<Event>>,
    fail: Mutex<String>,
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
            event.numbered_time();
            all.push(event.clone());
        }
        Ok(events)
    }
    fn stream(&self, _: protocol::StreamNotification) {}
}
fn setup(turns: i64) -> (Arc<Core>, Arc<Conversation>, Arc<Memory>) {
    let journal = Arc::new(Memory::default());
    let mut cfg = config::Effective::default();
    cfg.compaction.threshold = 0.9;
    cfg.compaction.keep_tokens = 100;
    cfg.compaction.max_tokens = 100_000;
    let core = Arc::new(Core::new(
        "channel".into(),
        "/work".into(),
        Time::now(),
        Arc::new(cfg),
        String::new(),
        String::new(),
        journal.clone(),
    ));
    core.commit(
        vec![
            core.event(
                "",
                event::AGENT_SPAWNED,
                json!({"id":"a1","name":"main","model":"fixture/model"}),
            )
            .unwrap(),
        ],
        &CancellationToken::new(),
    )
    .unwrap();
    for turn in 1..=turns {
        let id = format!("i{turn}");
        let events = vec![
            core.event(
                "a1",
                event::INPUT_QUEUED,
                event::Input {
                    id: id.clone(),
                    kind: event::INPUT_PROMPT.into(),
                    text: format!("task {turn}: {}", "owned fixture ".repeat(20)),
                    ..Default::default()
                },
            )
            .unwrap(),
            core.event("a1", event::TURN_STARTED, event::TurnPayload { turn })
                .unwrap(),
            core.event(
                "a1",
                event::INPUT_TAKEN,
                event::InputTakenPayload {
                    turn,
                    ids: vec![id].into(),
                },
            )
            .unwrap(),
            core.event(
                "a1",
                event::ASSISTANT_MESSAGE,
                event::AssistantMessagePayload {
                    turn,
                    blocks: vec![model::Block::text(format!(
                        "reply {turn}: {}",
                        "owned result ".repeat(20)
                    ))]
                    .into(),
                    stop_reason: model::STOP_END_TURN.into(),
                    model: "fixture/model".into(),
                    ..Default::default()
                },
            )
            .unwrap(),
            core.event(
                "a1",
                event::TURN_ENDED,
                event::TurnEndedPayload {
                    turn,
                    reason: event::REASON_END_TURN.into(),
                    ..Default::default()
                },
            )
            .unwrap(),
        ];
        core.commit(events, &CancellationToken::new()).unwrap();
    }
    let context = Arc::new(Conversation::new(core.clone(), "a1".into()));
    (core, context, journal)
}
struct Immediate {
    requests: Mutex<Vec<model::Request>>,
    response: Mutex<Option<model::CompleteResult>>,
}
impl Immediate {
    fn summary(text: &str) -> Self {
        Self {
            requests: Mutex::new(Vec::new()),
            response: Mutex::new(Some(Ok(model::Response {
                blocks: vec![model::Block::text(text)],
                usage: model::Usage {
                    input_tokens: 20,
                    output_tokens: 5,
                    cache_read_tokens: 3,
                    cache_write_tokens: 4,
                },
                ..Default::default()
            }))),
        }
    }
}
impl Model for Immediate {
    fn complete(
        &self,
        request: model::Request,
        _: Option<model::DeltaSink>,
        _: CancellationToken,
    ) -> model::CompleteFuture<'_> {
        self.requests.lock().unwrap().push(request);
        Box::pin(async move {
            self.response
                .lock()
                .unwrap()
                .take()
                .expect("unexpected extra model call")
        })
    }
}
fn info() -> model::Info {
    model::Info {
        context_window: 10_000,
        max_output: 1000,
        input_price: 1_000_000.0,
        output_price: 2_000_000.0,
        cache_read_price: 500_000.0,
        cache_write_price: 1_500_000.0,
        ..Default::default()
    }
}
#[tokio::test]
async fn successful_compaction_records_accounting_summary_and_the_request_cache_identity() {
    let (core, context, journal) = setup(3);
    let model = Immediate::summary("Keep the task and constraints.");
    let read: model::Block = serde_json::from_value(
        json!({"type":"tool_use","id":"read1","name":"read","input":{"path":"file"}}),
    )
    .unwrap();
    assert_eq!(context.deduplicate_read(&read, "body"), "body");
    assert!(
        context
            .deduplicate_read(&read, "body")
            .contains("unchanged")
    );
    context
        .compact(CancellationToken::new(), &model, &info(), true)
        .await
        .unwrap();
    {
        let requests = model.requests.lock().unwrap();
        assert_eq!(requests.len(), 1);
        assert_eq!(requests[0].model, "model");
        assert_eq!(requests[0].cache_key, "a1:compact");
        assert_eq!(requests[0].max_tokens, 4000);
        assert!(requests[0].tools.is_empty());
    }
    core.inspect(|state, _| {
        assert!(!state.agents["a1"].compacting);
        assert_eq!(state.model_calls, 1);
        assert_eq!(state.budget_tokens, 32);
        assert_eq!(state.budget_cost, 37.5);
    });
    {
        let events = journal.events.lock().unwrap();
        let compact = events
            .iter()
            .position(|event| event.kind == event::COMPACTION_STARTED)
            .unwrap();
        assert_eq!(events[compact + 1].kind, event::MODEL_CALL_STARTED);
        assert_eq!(events[compact + 2].kind, event::MODEL_CALL_COMPLETED);
        assert_eq!(events[compact + 3].kind, event::COMPACTION_DONE);
        let payload: event::CompactionPayload = events[compact + 3].decode().unwrap();
        assert_eq!(payload.summary, "Keep the task and constraints.");
    }
    assert_eq!(context.deduplicate_read(&read, "body"), "body");
    assert!(
        context
            .compact(CancellationToken::new(), &model, &info(), true)
            .await
            .unwrap_err()
            .to_string()
            .contains("nothing to compact")
    );
}
#[tokio::test]
async fn a_summary_that_cannot_be_committed_is_not_used_and_clears_the_busy_flag() {
    let (core, context, journal) = setup(2);
    *journal.fail.lock().unwrap() = event::COMPACTION_DONE.into();
    let model = Immediate::summary("uncommitted summary");
    assert!(
        context
            .compact(CancellationToken::new(), &model, &info(), true)
            .await
            .is_err()
    );
    core.inspect(|state, _| {
        assert!(!state.agents["a1"].compacting);
        assert_eq!(state.model_calls, 1);
    });
    assert!(core.log_error("a1").is_some());
    assert!(
        !journal
            .events
            .lock()
            .unwrap()
            .iter()
            .any(|event| event.kind == event::COMPACTION_DONE)
    );
    assert_eq!(
        journal
            .events
            .lock()
            .unwrap()
            .iter()
            .filter(|event| event.kind == event::COMPACTION_FAILED)
            .count(),
        1
    );
}
#[tokio::test]
async fn an_empty_summary_still_accounts_for_usage_and_finishes_as_a_failed_fact() {
    let (core, context, journal) = setup(2);
    let model = Immediate::summary("");
    assert_eq!(
        context
            .compact(CancellationToken::new(), &model, &info(), true)
            .await
            .unwrap_err()
            .to_string(),
        "empty summary"
    );
    assert_eq!(core.inspect(|state, _| state.budget_tokens), 32);
    assert_eq!(
        journal.events.lock().unwrap().last().unwrap().kind,
        event::COMPACTION_FAILED
    );
}
#[tokio::test]
async fn a_failed_start_or_exhausted_budget_never_calls_the_summarizer() {
    let (core, context, journal) = setup(2);
    let model = Immediate::summary("must not run");
    *journal.fail.lock().unwrap() = event::COMPACTION_STARTED.into();
    assert!(
        context
            .compact(CancellationToken::new(), &model, &info(), true)
            .await
            .is_err()
    );
    assert!(model.requests.lock().unwrap().is_empty());
    assert!(!core.inspect(|state, _| state.agents["a1"].compacting));
    journal.fail.lock().unwrap().clear();
    let mut cfg = (*core.configuration()).clone();
    cfg.limits.max_channel_calls = 1;
    core.set_configuration(Arc::new(cfg));
    core.commit(
        vec![
            core.event(
                "a1",
                event::MODEL_CALL_STARTED,
                event::ModelCallPayload {
                    purpose: "turn".into(),
                    ..Default::default()
                },
            )
            .unwrap(),
        ],
        &CancellationToken::new(),
    )
    .unwrap();
    assert!(
        context
            .compact(CancellationToken::new(), &model, &info(), true)
            .await
            .is_err()
    );
    assert!(model.requests.lock().unwrap().is_empty());
    assert!(!core.inspect(|state, _| state.agents["a1"].compacting));
}
struct Blocking {
    entered: tokio::sync::Notify,
}
impl Model for Blocking {
    fn complete(
        &self,
        _: model::Request,
        _: Option<model::DeltaSink>,
        cancel: CancellationToken,
    ) -> model::CompleteFuture<'_> {
        Box::pin(async move {
            self.entered.notify_one();
            cancel.cancelled().await;
            Err(model::Failure {
                response: model::Response {
                    usage: model::Usage {
                        input_tokens: 3,
                        ..Default::default()
                    },
                    ..Default::default()
                },
                error: model::CallError::Cancelled,
            })
        })
    }
}
#[tokio::test]
async fn concurrent_compaction_is_refused_and_cancellation_joins_the_original_call() {
    let (core, context, journal) = setup(2);
    let model = Arc::new(Blocking {
        entered: tokio::sync::Notify::new(),
    });
    let token = CancellationToken::new();
    let cancel = token.clone();
    let task_context = context.clone();
    let task_model = model.clone();
    let task = tokio::spawn(async move {
        task_context
            .compact(token, task_model.as_ref(), &info(), true)
            .await
    });
    tokio::time::timeout(Duration::from_secs(2), model.entered.notified())
        .await
        .unwrap();
    assert!(
        context
            .compact(CancellationToken::new(), model.as_ref(), &info(), true)
            .await
            .unwrap_err()
            .to_string()
            .contains("already running")
    );
    cancel.cancel();
    assert!(task.await.unwrap().is_err());
    assert!(!core.inspect(|state, _| state.agents["a1"].compacting));
    assert_eq!(core.inspect(|state, _| state.budget_tokens), 3);
    assert_eq!(
        journal
            .events
            .lock()
            .unwrap()
            .iter()
            .filter(|event| event.kind == event::COMPACTION_FAILED)
            .count(),
        1
    );
}
#[tokio::test]
async fn dropping_a_compaction_future_closes_its_runtime_fact_without_using_a_summary() {
    let (core, context, journal) = setup(2);
    let model = Arc::new(Blocking {
        entered: tokio::sync::Notify::new(),
    });
    let task_context = context.clone();
    let task_model = model.clone();
    let task = tokio::spawn(async move {
        task_context
            .compact(CancellationToken::new(), task_model.as_ref(), &info(), true)
            .await
    });
    tokio::time::timeout(Duration::from_secs(2), model.entered.notified())
        .await
        .unwrap();
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    assert!(!core.inspect(|state, _| state.agents["a1"].compacting));
    assert_eq!(
        journal
            .events
            .lock()
            .unwrap()
            .iter()
            .filter(|event| event.kind == event::COMPACTION_FAILED)
            .count(),
        1
    );
}
#[tokio::test]
async fn requested_compaction_runs_at_preparation_and_keeps_a_context_gauge() {
    let (_, context, _) = setup(2);
    let model = Immediate::summary("retained summary");
    context.request_compaction();
    let history = context
        .prepare(CancellationToken::new(), &model, &info(), "system", &[])
        .await;
    assert_eq!(model.requests.lock().unwrap().len(), 1);
    assert!(
        history
            .iter()
            .flat_map(|message| message.blocks.iter())
            .any(|block| block.text.contains("retained summary"))
    );
    let (tokens, window) = context.gauge();
    assert!(tokens > 0);
    assert_eq!(window, 10_000);
    context
        .prepare(CancellationToken::new(), &model, &info(), "system", &[])
        .await;
    assert_eq!(model.requests.lock().unwrap().len(), 1);
}
