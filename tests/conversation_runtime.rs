use serde_json::{Value, json, value::RawValue};
use stavlos::{
    agent::{
        conversation::{self, Reads, SentNote},
        journal::{Core, Journal},
        prompt::{self, PrefixCache, Situation},
    },
    config, event, model,
    wire::{Slice, Time},
};
use std::{
    path::{Path, PathBuf},
    sync::Arc,
};
use tokio_util::sync::CancellationToken;
fn messages(value: &Value) -> Vec<model::Message> {
    serde_json::from_value::<Slice<model::Message>>(value.clone())
        .unwrap()
        .into_vec()
}
#[test]
fn original_go_note_positions_and_read_deduplication_keep_the_cached_prefix() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-conversation-notes.json")).unwrap();
    assert_eq!(cases.len(), 21);
    for (index, c) in cases.iter().enumerate() {
        let prior = serde_json::from_value::<Vec<SentNote>>(c["prior"].clone()).unwrap();
        let (history, kept) =
            conversation::with_notes(messages(&c["history"]), &prior, c["note"].as_str().unwrap());
        assert_eq!(
            serde_json::to_value(history).unwrap(),
            c["output"],
            "note case {index}"
        );
        assert_eq!(
            serde_json::to_value(kept).unwrap(),
            c["kept"],
            "note case {index}"
        );
    }
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-deduplicated-reads.json")).unwrap();
    let mut reads = Reads::default();
    assert_eq!(cases.len(), 7);
    for c in cases {
        reads.forget(&serde_json::from_value::<Vec<String>>(c["forget"].clone()).unwrap());
        let input: Box<RawValue> = serde_json::from_str(c["input"].as_str().unwrap()).unwrap();
        let call = model::Block {
            id: c["id"].as_str().unwrap().into(),
            input: Some(input),
            ..Default::default()
        };
        assert_eq!(
            reads.deduplicate(&call, c["output"].as_str().unwrap()),
            c["result"].as_str().unwrap()
        );
    }
}
#[test]
fn every_original_summary_request_and_output_reservation_remains_exact() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-summary-requests.json")).unwrap();
    assert_eq!(cases.len(), 4);
    for c in cases {
        let info = model::Info {
            capabilities: model::Capabilities {
                ignores_max_tokens: c["ignore_max"].as_bool().unwrap(),
            },
            ..Default::default()
        };
        let request = conversation::summary_request(
            "fixture-model",
            "user: task\nassistant: worked",
            c["previous"].as_str().unwrap(),
            &info,
        );
        assert_eq!(
            json!({"Model":request.model,"System":request.system,"Messages":request.messages,"Tools":null,"MaxTokens":request.max_tokens,"Variant":request.variant,"CacheKey":request.cache_key}),
            c["request"]
        );
    }
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-usable-windows.json")).unwrap();
    assert_eq!(cases.len(), 30);
    for c in cases {
        let info = model::Info {
            context_window: c["size"].as_i64().unwrap(),
            max_output: c["max"].as_i64().unwrap(),
            ..Default::default()
        };
        assert_eq!(
            conversation::usable_window(&info),
            c["usable"].as_i64().unwrap()
        );
    }
    assert_eq!(conversation::bare_id("provider/model/sub"), "model/sub");
    assert_eq!(conversation::bare_id("invalid"), "invalid");
}
#[test]
fn full_original_role_prompts_tools_and_state_notes_stay_unchanged() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-prompt-prefixes.json")).unwrap();
    assert_eq!(cases.len(), 4);
    for c in cases {
        let mut cfg = config::Effective::default();
        cfg.dir = "/fixture-stavlos-work".into();
        cfg.limits = serde_json::from_value(c["limits"].clone()).unwrap();
        cfg.roles = serde_json::from_value(c["roles"].clone()).unwrap();
        cfg.skills = serde_json::from_value(c["skills"].clone()).unwrap();
        cfg.instructions = vec![stavlos::instructions::File {
            path: "/fixture-stavlos-work/AGENTS.md".into(),
            text: "Fixture trusted instructions.".into(),
        }];
        let cfg = Arc::new(cfg);
        let mut state = stavlos::agent::state::ChannelState::new("", "lead");
        state.dir = "/fixture-stavlos-work".into();
        for value in c["events"].as_array().unwrap() {
            let event: event::Event = serde_json::from_value(value.clone()).unwrap();
            state.apply(&event);
        }
        let id = c["id"].as_str().unwrap();
        let view = prompt::role_view(&state, id, &cfg).unwrap();
        let directories: Vec<PathBuf> = serde_json::from_value(c["directories"].clone()).unwrap();
        let mcp = serde_json::from_value::<Slice<model::ToolDef>>(c["mcp"].clone()).unwrap();
        let situation = Situation {
            id,
            parent: c["parent"].as_str().unwrap(),
            directory: Path::new(c["directory"].as_str().unwrap()),
            directories: &directories,
            home: Path::new("/home/fixture"),
        };
        let mut cache = PrefixCache::default();
        let prefix = cache.build(&cfg, &view, &situation, &mcp);
        assert_eq!(
            prefix.system,
            c["system"].as_str().unwrap(),
            "prompt case {}",
            c["scenario"]
        );
        assert_eq!(
            serde_json::to_value(prefix.definitions).unwrap(),
            c["definitions"],
            "tool case {}",
            c["scenario"]
        );
        assert_eq!(
            prompt::state_note(&state, id, &view, &cfg),
            c["note"].as_str().unwrap(),
            "note case {}",
            c["scenario"]
        );
        assert_eq!(
            cache.build(&cfg, &view, &situation, &mcp).system,
            c["system"].as_str().unwrap()
        );
        let mut updated = cfg.as_ref().clone();
        updated.instructions[0].text = "Fresh trusted instructions.".into();
        assert!(
            cache
                .build(&Arc::new(updated), &view, &situation, &mcp)
                .system
                .contains("Fresh trusted instructions.")
        );
    }
}
struct NoAppend;
impl Journal for NoAppend {
    fn append(
        &self,
        _: Vec<event::Event>,
        _: &CancellationToken,
    ) -> anyhow::Result<Vec<event::Event>> {
        anyhow::bail!("recovery planning must not append by itself")
    }
    fn stream(&self, _: stavlos::protocol::StreamNotification) {}
}
#[test]
fn original_restart_repairs_abort_open_work_and_report_lost_jobs_without_running_them() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-recovery-repairs.json")).unwrap();
    assert_eq!(cases.len(), 4);
    for c in cases {
        let core = Core::new(
            "repair".into(),
            "/fixture".into(),
            Time::now(),
            Arc::new(config::Effective::default()),
            String::new(),
            String::new(),
            Arc::new(NoAppend),
        );
        let events = serde_json::from_value::<Vec<event::Event>>(c["events"].clone()).unwrap();
        core.restore_page(&events).unwrap();
        let repairs = core
            .inspect(|state, _| conversation::recovery_events(&core, state))
            .unwrap();
        let actual = repairs
            .into_iter()
            .map(|e| {
                let mut payload = e.value().unwrap();
                if e.kind == event::INPUT_QUEUED {
                    let id = payload["id"].as_str().unwrap();
                    assert!(id.starts_with('i') && id.len() == 11);
                    payload["id"] = "<id>".into();
                }
                json!({"agent":e.agent,"type":e.kind,"payload":payload})
            })
            .collect::<Vec<_>>();
        assert_eq!(json!(actual), c["repairs"]);
    }
}
