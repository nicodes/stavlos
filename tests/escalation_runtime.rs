use stavlos::{
    escalation::{Answer, Config, Manager},
    protocol::{self, PromptInfo, PromptNotification},
};
use std::{
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio_util::sync::CancellationToken;

fn manager(
    config: Config,
) -> (
    Arc<Manager>,
    tokio::sync::mpsc::UnboundedReceiver<PromptNotification>,
) {
    let (sender, receiver) = tokio::sync::mpsc::unbounded_channel();
    (
        Manager::new(
            config,
            Arc::new(move |notification, _| {
                let _ = sender.send(notification);
            }),
        ),
        receiver,
    )
}
fn prompt(id: &str, kind: &str) -> PromptInfo {
    PromptInfo {
        id: id.into(),
        channel: "channel".into(),
        kind: kind.into(),
        ..Default::default()
    }
}
async fn next(
    receiver: &mut tokio::sync::mpsc::UnboundedReceiver<PromptNotification>,
    action: &str,
) -> PromptInfo {
    let notification = tokio::time::timeout(Duration::from_secs(2), receiver.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(notification.action, action);
    notification.prompt
}
fn start(
    manager: &Arc<Manager>,
    info: PromptInfo,
    cancel: CancellationToken,
) -> tokio::task::JoinHandle<Answer> {
    let manager = manager.clone();
    tokio::spawn(async move { manager.request(info, cancel, None).await })
}
#[tokio::test]
async fn claims_are_exclusive_disconnect_releases_and_late_answers_are_refused() {
    let (manager, mut received) = manager(Config {
        answer_timeout: Duration::from_secs(60),
        ..Default::default()
    });
    let task = start(
        &manager,
        prompt("p1", protocol::PROMPT_PERMISSION),
        CancellationToken::new(),
    );
    assert!(
        next(&mut received, protocol::ACTION_REQUESTED)
            .await
            .escalated
    );
    manager.claim("p1", "browser").unwrap();
    next(&mut received, protocol::ACTION_CLAIMED).await;
    assert!(manager.claim("p1", "tui").is_err());
    assert!(
        manager
            .reply(
                "p1",
                "tui",
                Answer {
                    value: "allow".into(),
                    ..Default::default()
                }
            )
            .is_err()
    );
    manager.release_client("browser");
    assert!(
        next(&mut received, protocol::ACTION_REQUESTED)
            .await
            .claimed_by
            .is_empty()
    );
    manager
        .reply(
            "p1",
            "tui",
            Answer {
                value: "deny".into(),
                reason: "keep private".into(),
                ..Default::default()
            },
        )
        .unwrap();
    next(&mut received, protocol::ACTION_ANSWERED).await;
    let answer = task.await.unwrap();
    assert_eq!(answer.client, "tui");
    assert_eq!(answer.reason, "keep private");
    assert!(manager.reply("p1", "browser", Answer::default()).is_err());
    assert!(manager.pending("").is_empty());
}
#[tokio::test]
async fn accepted_answer_wins_cancellation_and_opened_runs_before_waiter_returns() {
    let (manager, _) = manager(Config::default());
    let cancel = CancellationToken::new();
    let events = Arc::new(Mutex::new(Vec::new()));
    let opened_events = events.clone();
    let resolver = manager.clone();
    let token = cancel.clone();
    let answer = manager
        .request(
            prompt("p1", protocol::PROMPT_QUESTION),
            cancel,
            Some(Box::new(move || {
                assert_eq!(resolver.pending("").len(), 1);
                opened_events.lock().unwrap().push("opened");
                resolver
                    .reply(
                        "p1",
                        "owner",
                        Answer {
                            answers: vec!["reply".into()],
                            ..Default::default()
                        },
                    )
                    .unwrap();
                token.cancel();
            })),
        )
        .await;
    events.lock().unwrap().push("returned");
    assert_eq!(*events.lock().unwrap(), ["opened", "returned"]);
    assert_eq!(answer.answers, ["reply"]);
    assert!(!answer.withdrawn);
}
#[tokio::test]
async fn only_permissions_default_questions_and_trust_wait_and_dropped_futures_withdraw() {
    let (manager, mut received) = manager(Config::default());
    let permission = start(
        &manager,
        prompt("permission", protocol::PROMPT_PERMISSION),
        CancellationToken::new(),
    );
    next(&mut received, protocol::ACTION_REQUESTED).await;
    next(&mut received, protocol::ACTION_DEFAULTED).await;
    let answer = permission.await.unwrap();
    assert!(answer.defaulted);
    assert_eq!(answer.value, "deny");
    let cancel = CancellationToken::new();
    let question = start(
        &manager,
        prompt("question", protocol::PROMPT_QUESTION),
        cancel.clone(),
    );
    next(&mut received, protocol::ACTION_REQUESTED).await;
    let trust = start(
        &manager,
        prompt("trust", protocol::PROMPT_TRUST),
        cancel.clone(),
    );
    next(&mut received, protocol::ACTION_REQUESTED).await;
    tokio::time::sleep(Duration::from_millis(20)).await;
    assert_eq!(manager.pending("channel").len(), 2);
    assert!(!question.is_finished());
    assert!(!trust.is_finished());
    question.abort();
    assert!(question.await.unwrap_err().is_cancelled());
    next(&mut received, protocol::ACTION_WITHDRAWN).await;
    cancel.cancel();
    assert!(trust.await.unwrap().withdrawn);
    next(&mut received, protocol::ACTION_WITHDRAWN).await;
    assert!(manager.pending("").is_empty());
}
#[tokio::test]
async fn independent_question_answers_keep_claims_and_reject_empty_or_multiple_answers() {
    let (manager, mut received) = manager(Config::default());
    let mut info = prompt("p1", protocol::PROMPT_QUESTION);
    info.question_number = 1;
    info.question_total = 2;
    let task = start(&manager, info, CancellationToken::new());
    next(&mut received, protocol::ACTION_REQUESTED).await;
    for answers in [vec![], vec![" ".into()], vec!["a".into(), "b".into()]] {
        assert!(
            manager
                .reply(
                    "p1",
                    "owner",
                    Answer {
                        answers,
                        ..Default::default()
                    }
                )
                .is_err()
        );
    }
    manager
        .reply(
            "p1",
            "owner",
            Answer {
                answers: vec!["chosen".into()],
                ..Default::default()
            },
        )
        .unwrap();
    assert_eq!(task.await.unwrap().answers, ["chosen"]);
}
#[tokio::test]
async fn claim_expiry_and_mode_changes_apply_only_to_matching_prompts() {
    let (manager, mut received) = manager(Config {
        answer_timeout: Duration::from_secs(60),
        ..Default::default()
    });
    let mut sticky = prompt("p1", protocol::PROMPT_PERMISSION);
    sticky.sticky = true;
    let first = start(&manager, sticky, CancellationToken::new());
    next(&mut received, protocol::ACTION_REQUESTED).await;
    let second = start(
        &manager,
        prompt("p2", protocol::PROMPT_PERMISSION),
        CancellationToken::new(),
    );
    next(&mut received, protocol::ACTION_REQUESTED).await;
    manager.claim("p2", "browser").unwrap();
    next(&mut received, protocol::ACTION_CLAIMED).await;
    manager.expire_claims(
        tokio::time::Instant::now() + Duration::from_secs(121),
        Duration::from_secs(120),
    );
    assert!(
        next(&mut received, protocol::ACTION_REQUESTED)
            .await
            .claimed_by
            .is_empty()
    );
    assert_eq!(
        manager.answer_where(
            "other",
            protocol::PROMPT_PERMISSION,
            "allow",
            "mode",
            |_| true
        ),
        0
    );
    assert_eq!(
        manager.answer_where(
            "channel",
            protocol::PROMPT_PERMISSION,
            "allow",
            "mode",
            |info| !info.sticky
        ),
        1
    );
    assert_eq!(second.await.unwrap().value, "allow");
    assert_eq!(manager.pending("")[0].id, "p1");
    manager
        .resolve(
            "p1",
            "trust-owner",
            Answer {
                value: "deny".into(),
                ..Default::default()
            },
        )
        .unwrap();
    assert_eq!(first.await.unwrap().client, "trust-owner");
}
