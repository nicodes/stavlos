use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use ratatui::{Terminal, backend::TestBackend};
use serde_json::{json, to_value};
use stavlos::{
    event,
    paths::Paths,
    protocol,
    tui::{
        controller::{Controller, Dialog, Effect, Focus, Reply, Tag},
        driver::key_name,
        editor::Editor,
        render::{Renderer, wrap},
    },
    wire::Time,
};
fn new_ui() -> Controller {
    let temp = std::path::PathBuf::from("/tmp/stavlos-terminal-fixture");
    let paths = Paths {
        config: temp.join("config"),
        data: temp.join("data"),
        cache: temp.join("cache"),
        socket: temp.join("socket"),
        shared_skills: temp.join("skills"),
    };
    let mut ui = Controller::new(
        paths,
        temp,
        "terminal-client".into(),
        "channel".into(),
        Time::now(),
    );
    ui.on_reconcile(protocol::ReconcileResult {
        channel: protocol::ChannelInfo {
            id: "channel".into(),
            name: "stable".into(),
            dir: "/tmp".into(),
            mode: "ask".into(),
            ..Default::default()
        },
        agents: vec![protocol::AgentInfo {
            id: "root".into(),
            name: "main".into(),
            role: "worker".into(),
            model: "test/model".into(),
            state: "idle".into(),
            ..Default::default()
        }]
        .into(),
        ..Default::default()
    });
    ui
}
fn reply(
    ui: &mut Controller,
    scope: Option<stavlos::tui::controller::Scope>,
    tag: Tag,
    value: serde_json::Value,
) -> Vec<Effect> {
    ui.reply(Reply {
        scope,
        tag,
        result: Ok(value),
    })
}
fn event(ui: &mut Controller, seq: i64, kind: &str, payload: serde_json::Value) {
    let mut event = event::Event::new("channel", "root", kind, payload).unwrap();
    event.seq = seq;
    ui.apply_event(event);
}
#[test]
fn editor_preserves_graphemes_and_display_columns_and_masks_secrets() {
    let mut editor = Editor::new("a👩‍💻e\u{301}");
    assert!(editor.key("backspace"));
    assert_eq!(editor.text, "a👩‍💻");
    editor.key("left");
    assert_eq!(editor.cursor, 1);
    editor.insert("\r\n界");
    assert_eq!(editor.text, "a\n界👩‍💻");
    editor.key("up");
    assert_eq!(editor.cursor, 1);
    editor.secret = true;
    assert_eq!(editor.display(), "••••");
    assert!(!editor.display().contains('界'));
    let rows = wrap("界abc\ne\u{301}👩‍💻", 3);
    assert_eq!(rows, vec!["界a", "bc", "e\u{301}👩‍💻"]);
    assert_eq!(
        key_name(KeyEvent::new(KeyCode::Char(' '), KeyModifiers::CONTROL)).0,
        "ctrl+space"
    );
    assert_eq!(
        key_name(KeyEvent::new(KeyCode::Char('É'), KeyModifiers::SHIFT))
            .1
            .as_deref(),
        Some("É")
    );
}
#[test]
fn stale_channel_replies_cannot_replace_current_view_and_recent_history_restarts_at_one() {
    let mut ui = new_ui();
    let old = ui.scope.clone();
    ui.bind_channel(
        protocol::ChannelInfo {
            id: "other".into(),
            name: "other".into(),
            ..Default::default()
        },
        String::new(),
    );
    let effects = reply(
        &mut ui,
        Some(old),
        Tag::Reconcile,
        to_value(protocol::ReconcileResult {
            channel: protocol::ChannelInfo {
                id: "channel".into(),
                ..Default::default()
            },
            seq: 100,
            ..Default::default()
        })
        .unwrap(),
    );
    assert!(effects.is_empty());
    assert_eq!(ui.scope.channel, "other");
    assert_eq!(ui.channel.info.id, "other");
    ui.bind_channel(
        protocol::ChannelInfo {
            id: "channel".into(),
            name: "stable".into(),
            ..Default::default()
        },
        String::new(),
    );
    ui.channel.history_from = 90;
    let generation = ui.scope.generation;
    assert_eq!(ui.load_history().len(), 1);
    assert!(ui.scope.generation > generation);
    event(
        &mut ui,
        99,
        event::CHAT_POSTED,
        json!({"text":"stale delivery"}),
    );
    assert_eq!(ui.channel.seq, 0);
    event(
        &mut ui,
        1,
        event::CHAT_POSTED,
        json!({"text":"first replay"}),
    );
    assert_eq!(ui.channel.seq, 1);
    assert!(!ui.channel.await_first);
}
#[test]
fn completion_commands_and_channel_agent_sends_keep_their_distinct_contracts() {
    let mut ui = new_ui();
    ui.channel.input.set("/mod");
    ui.key("tab", None);
    assert_eq!(ui.channel.input.text, "/models");
    ui.channel.input.set("hello");
    let effects = ui.key("enter", None);
    let Effect::Request(request) = &effects[0] else {
        panic!()
    };
    assert_eq!(request.method, protocol::MCHANNEL_POST);
    assert_eq!(request.params, json!({"channel":"channel","text":"hello"}));
    ui.move_agent(1);
    ui.channel.input.set("next boundary");
    let effects = ui.key("enter", None);
    let Effect::Request(request) = &effects[0] else {
        panic!()
    };
    assert_eq!(
        request.params,
        json!({"agent":"root","kind":"steer","text":"next boundary"})
    );
    let effects = ui.command("/queue after this turn");
    let Effect::Request(request) = &effects[0] else {
        panic!()
    };
    assert_eq!(request.params["kind"], "prompt");
    ui.custom_commands.push(protocol::CommandInfo {
        name: "model".into(),
        description: "cannot override builtin alias".into(),
    });
    assert_eq!(ui.command("/model").len(), 1);
    assert_eq!(ui.popup.as_ref().unwrap().kind as u8, Dialog::Models as u8);
}
#[test]
fn history_preserves_draft_and_two_step_quit_and_cancel_do_not_fire_early() {
    let mut ui = new_ui();
    ui.channel.push_history("previous".into());
    ui.channel.input.set("draft");
    ui.key("up", None);
    assert_eq!(ui.channel.input.text, "previous");
    ui.key("down", None);
    assert_eq!(ui.channel.input.text, "draft");
    ui.key("ctrl+c", None);
    assert!(!ui.quit);
    ui.key("x", Some("x"));
    ui.key("ctrl+c", None);
    assert!(!ui.quit);
    ui.key("ctrl+c", None);
    assert!(ui.quit);
    let mut ui = super_ui_running();
    assert!(ui.key("esc", None).is_empty());
    ui.key("x", Some("x"));
    ui.key("esc", None);
    assert!(ui.key("esc", None).is_empty());
    let effects = ui.key("esc", None);
    let Effect::Request(request) = &effects[0] else {
        panic!()
    };
    assert_eq!(request.params["kind"], "cancel");
}
fn super_ui_running() -> Controller {
    let mut ui = new_ui();
    ui.channel.agents[0].state = "running".into();
    ui
}
#[test]
fn showing_questions_never_claims_and_drafts_survive_until_explicit_final_submission() {
    let mut ui = new_ui();
    ui.upsert_prompt(protocol::PromptInfo {
        id: "question".into(),
        channel: "channel".into(),
        agent: "root".into(),
        kind: "question".into(),
        questions: vec![protocol::Question {
            question: "Pick".into(),
            options: vec![
                protocol::QuestionOption {
                    label: "One".into(),
                    ..Default::default()
                },
                protocol::QuestionOption {
                    label: "Two".into(),
                    ..Default::default()
                },
            ]
            .into(),
        }]
        .into(),
        ..Default::default()
    });
    ui.open_prompts(true, false);
    assert!(!ui.prompt_busy.contains("question"));
    assert!(ui.key(" ", Some(" ")).is_empty());
    ui.key("esc", None);
    ui.open_prompts(true, false);
    assert!(ui.questions["question"].selected[0].contains(&0));
    ui.key("down", None);
    ui.key("down", None);
    ui.key("enter", None);
    assert!(matches!(
        ui.popup.as_ref().unwrap().kind,
        Dialog::QuestionText
    ));
    ui.key("n", Some("note"));
    assert!(ui.key("enter", None).is_empty());
    assert!(!ui.prompt_busy.contains("question"));
    let effects = ui.key("enter", None);
    let Effect::Answer { reply, .. } = &effects[0] else {
        panic!()
    };
    assert_eq!(reply.id, "question");
    assert_eq!(reply.details[0].selected.as_ref(), &[0]);
    assert_eq!(reply.details[0].custom, "note");
    assert!(ui.key("enter", None).is_empty());
    assert!(ui.prompt_busy.contains("question"));
}
#[test]
fn other_clients_claims_are_respected_and_stale_login_results_cannot_reopen_overlays() {
    let mut ui = new_ui();
    ui.upsert_prompt(protocol::PromptInfo {
        id: "permission".into(),
        channel: "channel".into(),
        kind: "permission".into(),
        claimed_by: "another-client".into(),
        ..Default::default()
    });
    ui.open_prompts(false, true);
    assert!(ui.permission_choice("allow").is_empty());
    assert!(ui.status.contains("another client"));
    ui.command("/providers");
    let epoch = ui.popup_epoch;
    ui.close_popup();
    reply(
        &mut ui,
        None,
        Tag::LoginStart {
            provider: "test".into(),
            epoch,
        },
        to_value(protocol::LoginStartResult {
            id: "stale".into(),
            ..Default::default()
        })
        .unwrap(),
    );
    assert!(ui.popup.is_none());
    assert!(ui.login_id.is_empty());
}
#[test]
fn a_completed_turn_rejects_late_streams_and_future_turns_until_started() {
    let mut ui = new_ui();
    event(&mut ui, 1, event::TURN_STARTED, json!({"turn":1}));
    ui.apply_stream(protocol::StreamNotification {
        channel: "channel".into(),
        agent: "root".into(),
        turn: 1,
        text: "partial".into(),
        ..Default::default()
    });
    assert!(ui.channel.transcripts["root"].streaming());
    event(
        &mut ui,
        2,
        event::TURN_ENDED,
        json!({"turn":1,"reason":"end_turn"}),
    );
    ui.apply_stream(protocol::StreamNotification {
        channel: "channel".into(),
        agent: "root".into(),
        turn: 1,
        text: "late".into(),
        ..Default::default()
    });
    assert!(!ui.channel.transcripts["root"].streaming());
    ui.apply_stream(protocol::StreamNotification {
        channel: "channel".into(),
        agent: "root".into(),
        turn: 2,
        text: "premature".into(),
        ..Default::default()
    });
    assert!(!ui.channel.transcripts["root"].streaming());
    event(&mut ui, 3, event::TURN_STARTED, json!({"turn":2}));
    ui.apply_stream(protocol::StreamNotification {
        channel: "channel".into(),
        agent: "root".into(),
        turn: 2,
        text: "current".into(),
        ..Default::default()
    });
    assert!(ui.channel.transcripts["root"].streaming());
}
#[test]
fn configuration_edits_carry_server_root_revision_and_scope_and_keep_stale_reads_out() {
    let mut ui = new_ui();
    ui.open_configuration(false);
    let epoch = ui.config.as_ref().unwrap().epoch;
    let effects = reply(
        &mut ui,
        None,
        Tag::ConfigTree(epoch),
        to_value(protocol::ConfigTree {
            root: "/tmp/project/.stavlos".into(),
            revision: "original".into(),
            ..Default::default()
        })
        .unwrap(),
    );
    assert_eq!(effects.len(), 1);
    let epoch = ui.config.as_ref().unwrap().epoch;
    reply(
        &mut ui,
        None,
        Tag::ConfigRead(epoch),
        to_value(protocol::ConfigDocument {
            path: "stavlos.json".into(),
            content: "{}".into(),
            exists: true,
            ..Default::default()
        })
        .unwrap(),
    );
    ui.config_key("x", Some("x"));
    assert!(ui.config.as_ref().unwrap().dirty);
    let effects = ui.config_key("ctrl+s", None);
    let Effect::Request(request) = &effects[0] else {
        panic!()
    };
    assert_eq!(request.method, protocol::MCONFIG_EDIT);
    assert_eq!(request.params["root"], "/tmp/project/.stavlos");
    assert_eq!(request.params["revision"], "original");
    assert_eq!(request.params["scope"], "project");
    assert_eq!(request.params["channel"], "channel");
    assert_eq!(request.params["content"], "{}x");
    reply(
        &mut ui,
        None,
        Tag::ConfigRead(epoch),
        to_value(protocol::ConfigDocument {
            path: "stale".into(),
            ..Default::default()
        })
        .unwrap(),
    );
    assert_eq!(
        ui.config.as_ref().unwrap().document.as_ref().unwrap().path,
        "stavlos.json"
    );
}
#[test]
fn ratatui_draws_real_views_at_small_and_large_sizes_without_exposing_secret_inputs() {
    let mut ui = new_ui();
    event(
        &mut ui,
        1,
        event::CHAT_POSTED,
        json!({"text":"hello terminal"}),
    );
    let mut renderer = Renderer::default();
    for (width, height) in [(120, 40), (45, 15), (12, 6)] {
        let mut terminal = Terminal::new(TestBackend::new(width, height)).unwrap();
        terminal
            .draw(|frame| renderer.draw(frame, &mut ui))
            .unwrap();
    }
    ui.popup(Dialog::LoginKey, "Secret").input = Editor::new("never-display-this-key");
    ui.popup.as_mut().unwrap().input.secret = true;
    let mut terminal = Terminal::new(TestBackend::new(120, 40)).unwrap();
    terminal
        .draw(|frame| renderer.draw(frame, &mut ui))
        .unwrap();
    let buffer = terminal.backend().buffer();
    let text = buffer
        .content
        .iter()
        .map(|cell| cell.symbol())
        .collect::<String>();
    assert!(!text.contains("never-display"));
    assert!(text.contains('•'));
    ui.close_popup();
    ui.focus = Focus::Input;
}
#[test]
fn permission_reason_drafts_and_directory_grants_survive_navigation_without_early_removal() {
    let mut ui = new_ui();
    ui.upsert_prompt(protocol::PromptInfo {
        id: "permission".into(),
        channel: "channel".into(),
        kind: "permission".into(),
        ..Default::default()
    });
    ui.open_prompts(false, true);
    ui.permission_choice("deny");
    ui.key("r", Some("please use the other directory"));
    ui.key("esc", None);
    ui.permission_choice("deny");
    assert_eq!(
        ui.popup.as_ref().unwrap().input.text,
        "please use the other directory"
    );
    ui.close_popup();
    ui.channel.info.dirs.push(protocol::DirInfo {
        path: "/old".into(),
        source: "permission".into(),
    });
    ui.open_tab(Dialog::Dirs);
    ui.popup.as_mut().unwrap().cursor = 1;
    ui.popup_submit(false);
    assert!(matches!(
        ui.popup.as_ref().unwrap().kind,
        Dialog::EditDirectory
    ));
    ui.popup.as_mut().unwrap().input.set("/new");
    let effects = ui.popup_submit(false);
    let Effect::Request(request) = &effects[0] else {
        panic!()
    };
    assert_eq!(request.method, protocol::MCHANNEL_ADD_DIR);
    assert_eq!(request.params["dir"], "/new");
    let scope = ui.scope.clone();
    let effects = reply(
        &mut ui,
        Some(scope),
        Tag::DirectoryAdded("/old".into()),
        json!({}),
    );
    let Effect::Request(request) = &effects[0] else {
        panic!()
    };
    assert_eq!(request.method, protocol::MCHANNEL_REMOVE_DIR);
    assert_eq!(request.params["dir"], "/old");
}
#[test]
fn history_waits_for_the_first_visual_row_and_pending_cards_wait_for_replay_positions() {
    let mut ui = new_ui();
    ui.width = 40;
    ui.show_sidebar = false;
    ui.channel.push_history("previous".into());
    ui.channel.input.set("a".repeat(70));
    ui.key("up", None);
    assert_eq!(ui.channel.input.text, "a".repeat(70));
    assert!(ui.channel.input.cursor < 38);
    ui.key("up", None);
    assert_eq!(ui.channel.input.text, "previous");
    let mut ui = new_ui();
    ui.channel.loading = true;
    ui.channel.replay_to = 2;
    ui.upsert_prompt(protocol::PromptInfo {
        id: "waiting".into(),
        channel: "channel".into(),
        agent: "root".into(),
        kind: "permission".into(),
        tool: "shell".into(),
        ..Default::default()
    });
    assert!(ui.channel.transcripts.is_empty());
    event(
        &mut ui,
        1,
        event::CHAT_POSTED,
        json!({"text":"before the pending prompt"}),
    );
    event(
        &mut ui,
        2,
        event::ASK_REQUESTED,
        json!({"id":"waiting","kind":"permission","tool":"shell"}),
    );
    assert!(!ui.channel.loading);
    assert!(
        ui.channel.transcripts[stavlos::tui::controller::CHAT]
            .permission_item("waiting")
            .unwrap()
            > 0
    );
}
