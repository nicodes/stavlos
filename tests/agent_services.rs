use serde_json::json;
use stavlos::{
    agent::{
        journal::{Core, Journal},
        services::{AskService, JobApi, JobRegistry, TodoService},
    },
    config,
    escalation::{self, Answer},
    event::{self, Event},
    proc, protocol,
    tools::{self, Asker, Jobs, Todos},
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
    changed: tokio::sync::Notify,
}
impl Journal for Memory {
    fn append(&self, mut events: Vec<Event>, _: &CancellationToken) -> anyhow::Result<Vec<Event>> {
        let fail = self.fail.lock().unwrap();
        anyhow::ensure!(
            !events.iter().any(|event| event.kind == *fail),
            "fixture append failure"
        );
        let mut persisted = self.events.lock().unwrap();
        for event in &mut events {
            event.seq = persisted.len() as i64 + 1;
            event.numbered_time();
            persisted.push(event.clone());
        }
        self.changed.notify_one();
        Ok(events)
    }
    fn stream(&self, _: protocol::StreamNotification) {}
}
fn setup() -> (Arc<Core>, Arc<Memory>) {
    let journal = Arc::new(Memory::default());
    let core = Arc::new(Core::new(
        "channel".into(),
        "/work".into(),
        Time::now(),
        Arc::new(config::Effective::default()),
        String::new(),
        String::new(),
        journal.clone(),
    ));
    core.commit(
        vec![
            core.event(
                "",
                event::AGENT_SPAWNED,
                json!({"id":"a1","name":"main","role":"general"}),
            )
            .unwrap(),
        ],
        &CancellationToken::new(),
    )
    .unwrap();
    (core, journal)
}
async fn wait_until(journal: &Memory, predicate: impl Fn(&[Event]) -> bool) {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let notified = journal.changed.notified();
            if predicate(&journal.events.lock().unwrap()) {
                return;
            }
            notified.await;
        }
    })
    .await
    .unwrap();
}
#[test]
fn todos_commit_atomically_count_past_old_ids_and_replay_exactly() {
    let (core, journal) = setup();
    let service = Arc::new(TodoService {
        core: core.clone(),
        agent: "a1".into(),
    });
    let mut threads = Vec::new();
    for index in 0..12 {
        let service = service.clone();
        threads.push(std::thread::spawn(move || {
            service
                .edit(
                    vec![tools::TodoAdd {
                        text: format!("step {index}"),
                        ..Default::default()
                    }],
                    vec![],
                )
                .unwrap();
        }));
    }
    for thread in threads {
        thread.join().unwrap();
    }
    let items = service.list();
    assert_eq!(items.len(), 12);
    assert_eq!(items[0].id, "t1");
    assert_eq!(items[11].id, "t12");
    assert!(
        service
            .edit(
                vec![],
                vec![
                    tools::TodoUpdate {
                        id: "t1".into(),
                        status: "done".into(),
                        ..Default::default()
                    },
                    tools::TodoUpdate {
                        id: "missing".into(),
                        status: "done".into(),
                        ..Default::default()
                    }
                ]
            )
            .is_err()
    );
    assert_eq!(service.list()[0].status, "pending");
    *journal.fail.lock().unwrap() = event::TODO_CHANGED.into();
    assert!(
        service
            .edit(
                vec![tools::TodoAdd {
                    text: "uncommitted".into(),
                    ..Default::default()
                }],
                vec![]
            )
            .is_err()
    );
    assert_eq!(service.list().len(), 12);
    journal.fail.lock().unwrap().clear();
    service
        .edit(
            vec![],
            vec![tools::TodoUpdate {
                id: "t1".into(),
                status: "done".into(),
                ..Default::default()
            }],
        )
        .unwrap();
    let mut replay = stavlos::agent::state::ChannelState::new("", "");
    for event in journal.events.lock().unwrap().iter() {
        replay.apply(event);
    }
    assert_eq!(json!(replay.agents["a1"].todos), json!(service.list()));
}
fn command(directory: &std::path::Path, command: &str) -> proc::Job {
    proc::Job::start(
        command,
        directory,
        &["PATH=/usr/bin:/bin".into()],
        None,
        None,
    )
    .unwrap()
}
#[tokio::test]
async fn background_completion_and_input_are_one_transaction_and_wake_after_ack() {
    let (core, journal) = setup();
    let temp = tempfile::tempdir().unwrap();
    let registry = JobRegistry::new(
        core.clone(),
        "a1".into(),
        CancellationToken::new(),
        temp.path().join("output"),
    );
    let api = JobApi {
        registry: registry.clone(),
        turn: CancellationToken::new(),
        after: Some(Arc::new(|| "owned control-change note".into())),
    };
    let wake = Arc::new(tokio::sync::Notify::new());
    core.register_wake("a1".into(), wake.clone());
    let job = command(temp.path(), "printf 'owned output\n'; exit 7");
    let id = api
        .adopt("fixture command", job, Duration::from_secs(10))
        .unwrap();
    tokio::time::timeout(Duration::from_secs(3), wake.notified())
        .await
        .unwrap();
    assert!(!api.has(&id));
    {
        let events = journal.events.lock().unwrap();
        let finished = events
            .iter()
            .position(|event| event.kind == event::JOB_FINISHED)
            .unwrap();
        assert_eq!(events[finished + 1].kind, event::INPUT_QUEUED);
        let result: event::JobFinishedPayload = events[finished].decode().unwrap();
        assert_eq!(result.exit_code, 7);
        assert!(result.is_error);
        assert!(result.output.contains("owned output"));
        assert!(result.output.contains("owned control-change note"));
    }
    assert!(core.inspect(|state, _| state.agents["a1"].starts_turn()));
    registry.shutdown().await;
}
#[tokio::test]
async fn failed_finished_write_follows_the_process_and_stopping_jobs_prevents_late_completion() {
    let (core, journal) = setup();
    let temp = tempfile::tempdir().unwrap();
    let registry = JobRegistry::new(
        core.clone(),
        "a1".into(),
        CancellationToken::new(),
        temp.path().join("output"),
    );
    let api = JobApi {
        registry: registry.clone(),
        turn: CancellationToken::new(),
        after: None,
    };
    let wake = Arc::new(tokio::sync::Notify::new());
    core.register_wake("a1".into(), wake.clone());
    *journal.fail.lock().unwrap() = event::JOB_FINISHED.into();
    let id = api
        .adopt(
            "true",
            command(temp.path(), "true"),
            Duration::from_secs(10),
        )
        .unwrap();
    tokio::time::timeout(Duration::from_secs(3), wake.notified())
        .await
        .unwrap();
    assert!(!api.has(&id));
    assert!(
        core.inspect(
            |state, _| state.agents["a1"].jobs.is_empty() && state.agents["a1"].starts_turn()
        )
    );
    assert!(
        !journal
            .events
            .lock()
            .unwrap()
            .iter()
            .any(|event| event.kind == event::JOB_FINISHED)
    );
    journal.fail.lock().unwrap().clear();
    let job = command(temp.path(), "sleep 60");
    let process = job.clone();
    let id = api.adopt("sleep 60", job, Duration::from_secs(60)).unwrap();
    api.stop(&id).unwrap();
    registry.shutdown().await;
    assert!(process.is_done());
    assert!(
        !journal
            .events
            .lock()
            .unwrap()
            .iter()
            .any(|event| event.kind == event::JOB_FINISHED)
    );
    assert!(
        journal
            .events
            .lock()
            .unwrap()
            .iter()
            .any(|event| event.kind == event::JOB_STOPPED)
    );
}
#[tokio::test]
async fn timeout_kills_the_owned_process_and_reports_the_original_deadline() {
    let (core, journal) = setup();
    let temp = tempfile::tempdir().unwrap();
    let registry = JobRegistry::new(
        core,
        "a1".into(),
        CancellationToken::new(),
        temp.path().join("output"),
    );
    let api = JobApi {
        registry: registry.clone(),
        turn: CancellationToken::new(),
        after: None,
    };
    let job = command(temp.path(), "sleep 60");
    let process = job.clone();
    api.adopt("sleep 60", job, Duration::from_millis(40))
        .unwrap();
    wait_until(&journal, |events| {
        events.iter().any(|event| event.kind == event::JOB_FINISHED)
    })
    .await;
    let result: event::JobFinishedPayload = journal
        .events
        .lock()
        .unwrap()
        .iter()
        .find(|event| event.kind == event::JOB_FINISHED)
        .unwrap()
        .decode()
        .unwrap();
    assert!(process.is_done());
    assert_eq!(result.exit_code, -1);
    assert_eq!(result.summary, "background command killed after 0s timeout");
    registry.shutdown().await;
}
#[tokio::test]
async fn questions_publish_in_order_finish_independently_and_keep_empty_cancelled_slots() {
    let (core, journal) = setup();
    let (sender, mut receiver) = tokio::sync::mpsc::unbounded_channel();
    let manager = escalation::Manager::new(
        escalation::Config::default(),
        Arc::new(move |notification, _| {
            let _ = sender.send(notification);
        }),
    );
    let service = AskService {
        core: core.clone(),
        agent: "a1".into(),
        manager: manager.clone(),
    };
    let token = CancellationToken::new();
    let cancel = token.clone();
    let task = tokio::spawn(async move {
        service
            .ask(
                (1..=3)
                    .map(|n| event::Question {
                        question: format!("Question {n}"),
                        ..Default::default()
                    })
                    .collect(),
                token,
            )
            .await
    });
    let mut prompts = Vec::new();
    while prompts.len() < 3 {
        let notification = tokio::time::timeout(Duration::from_secs(3), receiver.recv())
            .await
            .unwrap()
            .unwrap();
        if notification.action == protocol::ACTION_REQUESTED {
            prompts.push(notification.prompt);
        }
    }
    assert_eq!(
        prompts
            .iter()
            .map(|prompt| prompt.question_number)
            .collect::<Vec<_>>(),
        [1, 2, 3]
    );
    assert_eq!(
        journal
            .events
            .lock()
            .unwrap()
            .iter()
            .filter(|event| event.kind == event::ASK_REQUESTED)
            .count(),
        3
    );
    manager
        .reply(
            &prompts[2].id,
            "owner",
            Answer {
                answers: vec!["third".into()],
                ..Default::default()
            },
        )
        .unwrap();
    wait_until(&journal, |events| {
        events.iter().any(|event| {
            event.kind == event::ASK_RESOLVED
                && event.decode::<event::AskResolvedPayload>().unwrap().id == prompts[2].id
        })
    })
    .await;
    cancel.cancel();
    let result = task.await.unwrap();
    assert_eq!(result.answers, ["", "", "third"]);
    assert!(result.error.unwrap().contains("withdrawn"));
    assert!(manager.pending("").is_empty());
    assert!(core.inspect(|state, _| state.agents["a1"].asks.is_empty()));
    assert_eq!(
        journal
            .events
            .lock()
            .unwrap()
            .iter()
            .filter(|event| event.kind == event::ASK_RESOLVED)
            .count(),
        3
    );
}
#[tokio::test]
async fn a_failed_question_decision_stops_the_next_step_but_closed_prompts_still_fold() {
    let (core, journal) = setup();
    *journal.fail.lock().unwrap() = event::ASK_REQUESTED.into();
    let manager = escalation::Manager::new(escalation::Config::default(), Arc::new(|_, _| {}));
    let service = AskService {
        core: core.clone(),
        agent: "a1".into(),
        manager: manager.clone(),
    };
    let response = service
        .request(
            protocol::PromptInfo {
                id: "p1".into(),
                channel: "channel".into(),
                kind: protocol::PROMPT_PERMISSION.into(),
                ..Default::default()
            },
            String::new(),
            CancellationToken::new(),
            None,
        )
        .await;
    assert!(response.defaulted);
    assert!(core.log_error("a1").unwrap().contains("event log"));
    assert!(
        journal
            .events
            .lock()
            .unwrap()
            .iter()
            .any(|event| event.kind == event::ASK_RESOLVED)
    );
    assert!(core.inspect(|state, _| state.agents["a1"].asks.is_empty()));
    core.clear_log_error("a1");
    assert!(core.log_error("a1").is_none());
}
