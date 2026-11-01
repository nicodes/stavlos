use serde_json::{Value, json};
use stavlos::{
    agent::{
        journal::{Core, Journal},
        routing::{self, PreparedPost, Routing},
    },
    config,
    event::{self, Event},
    protocol, tools,
    wire::Time,
};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
};
use tokio_util::sync::CancellationToken;
const MAIN: &str = "a_main111111";
const SCOUT: &str = "a_scout222222";
const PEER: &str = "a_peer333333";
#[test]
fn original_go_reply_validation_and_delivery_text_match() {
    let rows: Vec<Value> =
        serde_json::from_str(include_str!("data/go-reply-validation.json")).unwrap();
    assert_eq!(rows.len(), 1152);
    for row in rows {
        let mut state = stavlos::agent::state::ChannelState::new("", "");
        state.apply(
            &Event::new(
                "fixture",
                "",
                event::AGENT_SPAWNED,
                json!({"id":"scout","name":"scout"}),
            )
            .unwrap(),
        );
        state.apply(
            &Event::new(
                "fixture",
                "scout",
                event::INPUT_QUEUED,
                event::Input {
                    id: "i_fixture".into(),
                    request_id: "r_fixture".into(),
                    channel: row["original"].as_str().unwrap().into(),
                    kind: event::INPUT_REQUEST.into(),
                    from: "main".into(),
                    from_name: "main".into(),
                    text: "fixture request".into(),
                    ..Default::default()
                },
            )
            .unwrap(),
        );
        if row["taken"].as_bool().unwrap() {
            state.apply(
                &Event::new(
                    "fixture",
                    "scout",
                    event::INPUT_TAKEN,
                    event::InputTakenPayload {
                        ids: vec!["i_fixture".into()].into(),
                        ..Default::default()
                    },
                )
                .unwrap(),
            );
        }
        let targets: Vec<String> = serde_json::from_value(row["targets"].clone()).unwrap();
        let ids: Vec<String> = serde_json::from_value(row["ids"].clone()).unwrap();
        let result = routing::validate_reply(
            &state,
            "scout",
            &targets,
            row["kind"].as_str().unwrap(),
            &ids,
            row["channel"].as_str().unwrap(),
            row["system"].as_bool().unwrap(),
        );
        assert_eq!(
            result
                .err()
                .map(|error| error.to_string())
                .unwrap_or_default(),
            row["error"].as_str().unwrap(),
            "{row}"
        );
    }
    let rows: Vec<Value> =
        serde_json::from_str(include_str!("data/go-message-results.json")).unwrap();
    assert_eq!(rows.len(), 30);
    for row in rows {
        let names: Vec<String> = serde_json::from_value(row["names"].clone()).unwrap();
        assert_eq!(
            routing::message_result(&names, row["kind"].as_str().unwrap()),
            row["result"].as_str().unwrap()
        );
    }
}
#[derive(Default)]
struct Memory {
    events: Mutex<Vec<Event>>,
    fail: Mutex<bool>,
    batches: Mutex<Vec<Vec<Event>>>,
}
impl Journal for Memory {
    fn append(&self, mut events: Vec<Event>, _: &CancellationToken) -> anyhow::Result<Vec<Event>> {
        anyhow::ensure!(!*self.fail.lock().unwrap(), "fixture failure");
        let mut persisted = self.events.lock().unwrap();
        let mut sequences = BTreeMap::new();
        for event in persisted.iter() {
            sequences.insert(event.channel.clone(), event.seq);
        }
        for event in &mut events {
            let seq = sequences.entry(event.channel.clone()).or_insert(0);
            *seq += 1;
            event.seq = *seq;
            event.global = persisted.len() as i64 + 1;
            event.numbered_time();
            persisted.push(event.clone());
        }
        self.batches.lock().unwrap().push(events.clone());
        Ok(events)
    }
    fn stream(&self, _: protocol::StreamNotification) {}
}
fn setup() -> (Routing, Arc<Memory>) {
    let journal = Arc::new(Memory::default());
    let mut cfg = config::Effective::default();
    cfg.reminders = true;
    cfg.roles.insert(
        "general".into(),
        config::frontmatter::Role {
            name: "general".into(),
            spawn: vec!["general".into()],
            ..Default::default()
        },
    );
    let core = Arc::new(Core::new(
        "source".into(),
        "/work".into(),
        Time::now(),
        Arc::new(cfg),
        String::new(),
        String::new(),
        journal.clone(),
    ));
    let mut events = Vec::new();
    for (id, parent, name) in [
        (MAIN, "", "main"),
        (SCOUT, MAIN, "scout"),
        (PEER, MAIN, "peer"),
    ] {
        events.push(
            core.event(
                "",
                event::AGENT_SPAWNED,
                json!({"id":id,"parent":parent,"name":name,"role":"general"}),
            )
            .unwrap(),
        );
    }
    core.commit(events, &CancellationToken::new()).unwrap();
    (Routing { core }, journal)
}
fn delivery(to: &[&str], text: &str, kind: &str, reply_to: &[&str]) -> tools::Delivery {
    tools::Delivery {
        channel: String::new(),
        to: to.iter().map(|value| (*value).into()).collect(),
        text: text.into(),
        kind: kind.into(),
        reply_to: reply_to.iter().map(|value| (*value).into()).collect(),
    }
}
fn take(core: &Core, agent: &str) {
    let ids = core.inspect(|state, _| {
        state.agents[agent]
            .inbox
            .iter()
            .map(|input| input.id.clone())
            .collect::<Vec<_>>()
    });
    core.commit(
        vec![
            core.event(
                agent,
                event::INPUT_TAKEN,
                event::InputTakenPayload {
                    turn: 1,
                    ids: ids.into(),
                },
            )
            .unwrap(),
        ],
        &CancellationToken::new(),
    )
    .unwrap();
}
fn board(source: &Routing, journal: Arc<Memory>) -> Core {
    let core = Core::new(
        "board".into(),
        "/work".into(),
        Time::now(),
        source.core.configuration(),
        String::new(),
        String::new(),
        journal,
    );
    core.commit(
        vec![
            core.event(
                "",
                event::CHANNEL_CREATED,
                event::ChannelCreatedPayload {
                    name: "research".into(),
                    board: Some(event::BoardInfo {
                        source: source.core.id.clone(),
                        owner: MAIN.into(),
                        members: vec![MAIN.into(), SCOUT.into(), PEER.into()].into(),
                    }),
                    ..Default::default()
                },
            )
            .unwrap(),
        ],
        &CancellationToken::new(),
    )
    .unwrap();
    core
}
#[test]
fn names_aliases_and_unique_id_prefixes_resolve_without_partial_delivery() {
    let (router, journal) = setup();
    let count = journal.events.lock().unwrap().len();
    assert!(
        router
            .message(
                MAIN,
                delivery(&["scout", "missing"], "task", "request", &[])
            )
            .is_err()
    );
    assert_eq!(journal.events.lock().unwrap().len(), count);
    assert!(
        router
            .message(MAIN, delivery(&["main"], "task", "request", &[]))
            .is_err()
    );
    let result = router
        .message(
            MAIN,
            delivery(&["@SCOUT", SCOUT, "a_scout"], "task", "request", &[]),
        )
        .unwrap();
    assert!(result.starts_with("request delivered to scout;"));
    router.core.inspect(|state, _| {
        assert_eq!(state.agents[SCOUT].inbox.len(), 1);
        assert_eq!(state.awaiting_replies(MAIN).len(), 1);
        assert!(state.pending_replies(SCOUT).is_empty());
        assert!(routing::resolve(state, "a_").is_none());
        assert_eq!(routing::preorder(state), [MAIN, SCOUT, PEER]);
    });
    let status = router.status(MAIN, "@scout").unwrap();
    assert_eq!(status.len(), 1);
    assert!(!status[0].you);
}
#[test]
fn broadcasts_and_response_requests_settle_only_matching_taken_debts() {
    let (router, _) = setup();
    router
        .message(MAIN, delivery(&["scout", "peer"], "task", "request", &[]))
        .unwrap();
    let request = router
        .core
        .inspect(|state, _| state.agents[SCOUT].inbox[0].request_id.clone());
    assert!(
        router
            .message(
                SCOUT,
                delivery(&["main"], "premature", "response", &[&request])
            )
            .is_err()
    );
    take(&router.core, SCOUT);
    assert!(
        router
            .message(
                SCOUT,
                delivery(
                    &["main", "peer"],
                    "wrong extra target",
                    "response",
                    &[&request]
                )
            )
            .is_err()
    );
    assert!(
        router
            .message(
                SCOUT,
                delivery(&["main"], "duplicate", "response", &[&request, &request])
            )
            .is_err()
    );
    router
        .message(SCOUT, delivery(&["main"], "update", "info", &[]))
        .unwrap();
    assert_eq!(
        router
            .core
            .inspect(|state, _| state.pending_replies(SCOUT).len()),
        1
    );
    let answer = router
        .message(
            SCOUT,
            delivery(
                &["main"],
                "answer and follow-up",
                "response_request",
                &[&request],
            ),
        )
        .unwrap();
    assert!(answer.contains("request_id: "));
    router.core.inspect(|state, _| {
        assert!(state.pending_replies(SCOUT).is_empty());
        assert_eq!(state.awaiting_replies(MAIN).len(), 1);
        assert_eq!(state.awaiting_replies(MAIN)[0].from, PEER);
        assert_eq!(state.awaiting_replies(SCOUT).len(), 1);
    });
    assert!(
        router
            .message(
                SCOUT,
                delivery(&["main"], "already answered", "response", &[&request])
            )
            .is_err()
    );
}
#[test]
fn prepared_human_posts_recheck_identities_and_only_explicit_answers_settle_them() {
    let (router, journal) = setup();
    let post = PreparedPost {
        id: "post_fixture".into(),
        request_id: "human_request".into(),
        text: "Please report".into(),
        kind: "request".into(),
        targets: vec![MAIN.into()],
        names: vec!["main".into()],
    };
    let mut stale = post.clone();
    stale.names[0] = "old-name".into();
    assert!(
        router
            .accept_prepared(stale, &CancellationToken::new())
            .is_err()
    );
    let mut duplicate = post.clone();
    duplicate.targets.push(MAIN.into());
    duplicate.names.push("main".into());
    assert!(
        router
            .accept_prepared(duplicate, &CancellationToken::new())
            .is_err()
    );
    router
        .accept_prepared(post, &CancellationToken::new())
        .unwrap();
    take(&router.core, MAIN);
    router
        .message(MAIN, delivery(&["human"], "progress", "info", &[]))
        .unwrap();
    assert_eq!(
        router
            .core
            .inspect(|state, _| state.pending_replies(MAIN).len()),
        1
    );
    router
        .message(
            MAIN,
            delivery(&["@USER"], "done", "response", &["human_request"]),
        )
        .unwrap();
    assert!(
        router
            .core
            .inspect(|state, _| state.pending_replies(MAIN).is_empty())
    );
    let events = journal.events.lock().unwrap();
    let reply: event::ChatPayload = events.last().unwrap().decode().unwrap();
    assert_eq!(reply.posts.as_ref(), ["post_fixture"]);
    assert_eq!(reply.post, "post_fixture");
}
#[test]
fn shared_board_delivery_is_one_transaction_observers_are_passive_and_replies_stay_in_context() {
    let (router, journal) = setup();
    let board = board(&router, journal.clone());
    assert_eq!(
        router
            .board_members(MAIN, &["scout".into(), "peer".into(), "self".into()])
            .unwrap(),
        [MAIN, SCOUT, PEER]
    );
    assert!(router.board_members(SCOUT, &["peer".into()]).is_err());
    let result = router
        .deliver_board(
            &board,
            MAIN,
            delivery(&["scout"], "public task", "request", &[]),
            "",
            false,
            &CancellationToken::new(),
        )
        .unwrap();
    assert!(result.starts_with("posted in #research; request_id: "));
    let batch = journal.batches.lock().unwrap().last().unwrap().clone();
    assert_eq!(batch.len(), 3);
    assert_eq!(batch[0].channel, "board");
    assert!(batch[1..].iter().all(|event| event.channel == "source"));
    router.core.inspect(|state, _| {
        assert!(state.agents[SCOUT].starts_turn());
        assert!(!state.agents[PEER].starts_turn());
        assert_eq!(state.agents[PEER].inbox[0].kind, event::INPUT_INFO);
        assert_eq!(state.agents[PEER].inbox[0].channel, "board");
    });
    let request = router
        .core
        .inspect(|state, _| state.agents[SCOUT].inbox[0].request_id.clone());
    take(&router.core, SCOUT);
    assert!(
        router
            .message(
                SCOUT,
                delivery(&["main"], "wrong context", "response", &[&request])
            )
            .unwrap_err()
            .to_string()
            .contains("original channel")
    );
    router
        .deliver_board(
            &board,
            SCOUT,
            delivery(&["main"], "public answer", "response", &[&request]),
            "",
            false,
            &CancellationToken::new(),
        )
        .unwrap();
    assert!(
        router
            .core
            .inspect(|state, _| state.awaiting_replies(MAIN).is_empty())
    );
    let source_before = router
        .core
        .inspect(|state, _| state.agents[PEER].inbox.len());
    let count = journal.events.lock().unwrap().len();
    *journal.fail.lock().unwrap() = true;
    assert!(
        router
            .deliver_board(
                &board,
                MAIN,
                delivery(&[], "uncommitted", "info", &[]),
                "",
                false,
                &CancellationToken::new()
            )
            .is_err()
    );
    assert_eq!(journal.events.lock().unwrap().len(), count);
    assert_eq!(
        router
            .core
            .inspect(|state, _| state.agents[PEER].inbox.len()),
        source_before
    );
}
#[test]
fn prepared_shared_posts_allow_passive_broadcasts_and_require_live_explicit_request_targets() {
    let (router, journal) = setup();
    let board = board(&router, journal);
    let post = PreparedPost {
        id: "post_fixture".into(),
        request_id: String::new(),
        text: "announcement".into(),
        kind: "info".into(),
        targets: Vec::new(),
        names: Vec::new(),
    };
    let mut request = post.clone();
    request.kind = "request".into();
    assert!(
        router
            .accept_prepared_board(&board, request, &CancellationToken::new())
            .is_err()
    );
    router
        .accept_prepared_board(&board, post, &CancellationToken::new())
        .unwrap();
    assert!(
        router
            .core
            .inspect(|state, _| state.agents.values().all(|agent| !agent.starts_turn()))
    );
    let post = PreparedPost {
        id: "post_targeted".into(),
        request_id: "web_board_request".into(),
        text: "Please check".into(),
        kind: "request".into(),
        targets: vec![SCOUT.into()],
        names: vec!["scout".into()],
    };
    router
        .accept_prepared_board(&board, post, &CancellationToken::new())
        .unwrap();
    router.core.inspect(|state, _| {
        assert!(state.agents[SCOUT].starts_turn());
        assert!(!state.agents[PEER].starts_turn());
        assert_eq!(
            state.agents[SCOUT].inbox.last().unwrap().post,
            "post_targeted"
        );
    });
}
#[test]
fn recap_requires_new_work_and_never_queues_a_duplicate_unanswered_request() {
    let (router, journal) = setup();
    let cancel = CancellationToken::new();
    assert!(router.set_recap(-1, &cancel).is_err());
    assert!(router.set_recap(1441, &cancel).is_err());
    router.set_recap(1, &cancel).unwrap();
    let now = Time(router.core.created.0 + chrono::Duration::minutes(2));
    let count = journal.events.lock().unwrap().len();
    router.maybe_recap(now, &cancel).unwrap();
    assert_eq!(journal.events.lock().unwrap().len(), count);
    router
        .core
        .commit(
            vec![
                router
                    .core
                    .event(
                        MAIN,
                        event::ASSISTANT_MESSAGE,
                        event::AssistantMessagePayload {
                            turn: 1,
                            ..Default::default()
                        },
                    )
                    .unwrap(),
            ],
            &cancel,
        )
        .unwrap();
    router.maybe_recap(now, &cancel).unwrap();
    let first_count = journal.events.lock().unwrap().len();
    router.maybe_recap(now, &cancel).unwrap();
    assert_eq!(journal.events.lock().unwrap().len(), first_count);
    router.core.inspect(|state, _| {
        assert!(state.recap_open);
        assert!(state.agents[MAIN].inbox.last().unwrap().recap);
        assert!(
            state.agents[MAIN]
                .inbox
                .last()
                .unwrap()
                .text
                .contains("channel's recap timer")
        );
    });
}
#[test]
fn reminders_stop_after_three_empty_turns_and_do_not_follow_cancellation() {
    let (router, journal) = setup();
    let cancel = CancellationToken::new();
    router
        .accept_prepared(
            PreparedPost {
                id: "post_task".into(),
                request_id: "human_task".into(),
                text: "task".into(),
                kind: "request".into(),
                targets: vec![MAIN.into()],
                names: vec!["main".into()],
            },
            &cancel,
        )
        .unwrap();
    take(&router.core, MAIN);
    router.end_replies(MAIN, event::REASON_CANCELLED).unwrap();
    assert!(
        router
            .core
            .inspect(|state, _| state.agents[MAIN].inbox.is_empty())
    );
    for turn in 1..=3 {
        router.end_replies(MAIN, event::REASON_END_TURN).unwrap();
        router
            .core
            .commit(
                vec![
                    router
                        .core
                        .event(MAIN, event::TURN_STARTED, event::TurnPayload { turn })
                        .unwrap(),
                ],
                &cancel,
            )
            .unwrap();
        take(&router.core, MAIN);
        router
            .core
            .commit(
                vec![
                    router
                        .core
                        .event(
                            MAIN,
                            event::TURN_ENDED,
                            event::TurnEndedPayload {
                                turn,
                                reason: event::REASON_END_TURN.into(),
                                ..Default::default()
                            },
                        )
                        .unwrap(),
                ],
                &cancel,
            )
            .unwrap();
    }
    router.end_replies(MAIN, event::REASON_END_TURN).unwrap();
    assert!(
        router
            .core
            .inspect(|state, _| state.agents[MAIN].inbox.is_empty())
    );
    assert_eq!(
        journal
            .events
            .lock()
            .unwrap()
            .iter()
            .filter(|event| event.kind == event::INPUT_QUEUED
                && event.decode::<event::Input>().unwrap().kind == event::INPUT_REMINDER)
            .count(),
        3
    );
}
