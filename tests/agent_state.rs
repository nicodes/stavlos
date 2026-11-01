use serde_json::{Value, json};
use stavlos::{agent::state::ChannelState, event};
use std::collections::BTreeMap;
fn equivalent(actual: &Value, expected: &Value, context: &str) {
    match (actual, expected) {
        (Value::Number(a), Value::Number(b)) => assert_eq!(a.as_f64(), b.as_f64(), "{context}"),
        (Value::Array(a), Value::Array(b)) => {
            assert_eq!(a.len(), b.len(), "{context}");
            for (index, (a, b)) in a.iter().zip(b).enumerate() {
                equivalent(a, b, &format!("{context}[{index}]"));
            }
        }
        (Value::Object(a), Value::Object(b)) => {
            assert_eq!(
                a.keys().collect::<Vec<_>>(),
                b.keys().collect::<Vec<_>>(),
                "{context}"
            );
            for (key, value) in a {
                equivalent(value, &b[key], &format!("{context}.{key}"));
            }
        }
        _ => assert_eq!(actual, expected, "{context}"),
    }
}
fn snapshot(state: &mut ChannelState) -> Value {
    let ids: Vec<_> = state.agents.keys().cloned().collect();
    let mut agents = BTreeMap::new();
    for id in ids {
        let status = state.status(&id);
        let waiting = state.waiting(&id);
        let pending = state.pending_replies(&id);
        let due = state.due(&id);
        let awaiting = state.awaiting_replies(&id);
        let awaiting_ids = state.awaiting_ids(&id);
        let a = state.agents.get_mut(&id).unwrap();
        let history = a.history.history();
        agents.insert(id,json!({"id":a.id,"parent":a.parent,"role":a.role,"name":a.name,"model":a.model,"variant":a.variant,"depth":a.depth,"children":a.children,"directories":a.directories,"killed":a.killed,"instructed":a.instructed,"turn":a.turn,"in_turn":a.in_turn,"last_error":a.last_error,"inbox":a.inbox,"nudges":a.nudges,"last_post":a.last_post,"jobs":a.jobs,"asks":a.asks,"resume_at":a.resume_at,"resume_why":a.resume_why,"resumes":a.resumes,"turn_reminder_only":a.turn_reminder_only,"turn_had_tools":a.turn_had_tools,"turn_settled":a.turn_settled,"todos":a.todos,"todo_seq":a.todo_seq,"compacting":a.compacting,"tokens":a.tokens,"last_context":a.last_context,"cost":a.cost,"history":history,"status":status,"starts_turn":a.starts_turn(),"busy":a.busy(),"waiting":waiting,"pending":pending,"due":due,"awaiting":awaiting,"awaiting_ids":awaiting_ids}));
    }
    let requests:BTreeMap<_,_>=state.requests.iter().map(|(id,r)|(id,json!({"request":r.request,"seq":r.seq,"legacy":r.legacy,"open":r.open,"held":r.held}))).collect();
    let sheets:BTreeMap<_,_>=state.sheets.iter().map(|(id,sheet)|(id,json!({"id":sheet.id,"title":sheet.title,"author":sheet.author,"hash":sheet.hash,"size":sheet.size,"updated":sheet.updated}))).collect();
    let directories: Vec<_> = state
        .dirs
        .iter()
        .map(|dir| json!({"path":dir.path,"source":dir.source}))
        .collect();
    json!({"model_calls":state.model_calls,"budget_tokens":state.budget_tokens,"budget_cost":state.budget_cost,"board":state.board,"name":state.name,"model":state.model,"role":state.role,"mode":state.mode,"dir":state.dir,"archived":state.archived,"recap":state.recap,"last_heard":state.last_heard,"last_work":state.last_work,"last_recap":state.last_recap,"recap_open":state.recap_open,"dirs":directories,"permits":{"calls":state.permits.calls,"prefixes":state.permits.prefixes},"agents":agents,"order":state.order,"names":state.names,"requests":requests,"request_seq":state.request_seq,"sheets":sheets,"sheet_seq":state.sheet_seq})
}
#[test]
fn every_original_go_channel_state_and_wakeup_matches_during_live_fold_and_replay() {
    let rows: Vec<Value> = serde_json::from_str(include_str!("data/go-agent-state.json")).unwrap();
    assert_eq!(rows.len(), 61);
    for pass in 0..2 {
        let mut state = ChannelState::new("fake/default", "general");
        for (index, row) in rows.iter().enumerate() {
            let event: event::Event = serde_json::from_value(row["event"].clone()).unwrap();
            let wake = state.apply(&event);
            assert_eq!(json!(wake), row["wake"], "pass {pass} event {index}");
            equivalent(
                &snapshot(&mut state),
                &row["state"],
                &format!("pass {pass} event {index} {}", event.kind),
            );
        }
        assert_eq!(
            state
                .sorted_sheets()
                .iter()
                .map(|s| s.id.as_str())
                .collect::<Vec<_>>(),
            ["s3"]
        );
        assert_eq!(state.names["scout"], "a1");
        assert_eq!(state.names["lookout"], "a1");
        assert_eq!(state.status("a1"), "killed");
        assert!(state.permits.calls.is_empty());
        assert!(state.permits.prefixes.is_empty());
    }
}
#[test]
fn cancellation_forgets_both_sides_of_broadcast_obligations_without_releasing_names() {
    let mut state = ChannelState::new("fake/model", "general");
    for (id, name, parent) in [
        ("a0", "root", ""),
        ("a1", "scout", "a0"),
        ("a2", "reviewer", "a0"),
    ] {
        state.apply(
            &event::Event::new(
                "c",
                "",
                event::AGENT_SPAWNED,
                event::AgentSpawnedPayload {
                    id: id.into(),
                    name: name.into(),
                    parent: parent.into(),
                    ..Default::default()
                },
            )
            .unwrap(),
        );
    }
    for id in ["a1", "a2"] {
        let input = event::Input {
            id: format!("input_{id}"),
            kind: event::INPUT_REQUEST.into(),
            from: "a0".into(),
            from_name: "root".into(),
            request_id: "broadcast".into(),
            text: "owned request".into(),
            ..Default::default()
        };
        state.apply(&event::Event::new("c", id, event::INPUT_QUEUED, input).unwrap());
        state.apply(
            &event::Event::new(
                "c",
                id,
                event::INPUT_TAKEN,
                event::InputTakenPayload {
                    ids: vec![format!("input_{id}")].into(),
                    ..Default::default()
                },
            )
            .unwrap(),
        );
    }
    assert_eq!(state.awaiting_ids("a0"), ["a1", "a2"]);
    assert_eq!(state.pending_replies("a1").len(), 1);
    assert_eq!(state.pending_replies("a2").len(), 1);
    state.apply(&event::Event::new("c", "a1", event::AGENT_CANCELLED, Value::Null).unwrap());
    assert_eq!(state.awaiting_ids("a0"), ["a2"]);
    assert!(state.pending_replies("a1").is_empty());
    assert_eq!(state.names["scout"], "a1");
    state.apply(&event::Event::new("c", "a0", event::AGENT_CANCELLED, Value::Null).unwrap());
    assert!(state.requests.is_empty());
    assert!(state.pending_replies("a2").is_empty());
}
