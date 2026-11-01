use serde_json::{Value, json};
use stavlos::{
    agent::{
        journal::{Core, Journal},
        permission::{Admission, BoundaryEnvironment, Scope},
        prompt::RoleView,
        state::Permits,
    },
    config,
    escalation::Answer,
    event::{self, Event},
    model,
    paths::Paths,
    policy::{Layered, Rule, Set, Verb},
    protocol,
    sandbox::Level,
    wire::Time,
};
use std::{fs, path::Path, sync::Arc};
use tokio_util::sync::CancellationToken;
struct Memory;
impl Journal for Memory {
    fn append(&self, mut events: Vec<Event>, _: &CancellationToken) -> anyhow::Result<Vec<Event>> {
        for event in &mut events {
            event.numbered_time();
        }
        Ok(events)
    }
    fn stream(&self, _: protocol::StreamNotification) {}
}
fn fixture(
    root: &Path,
) -> (
    BoundaryEnvironment,
    Scope,
    RoleView,
    config::Effective,
    Core,
) {
    let paths = Paths {
        config: root.join("harness/config"),
        data: root.join("harness/data"),
        cache: root.join("harness/cache"),
        socket: root.join("harness/socket"),
        shared_skills: root.join("skills"),
    };
    let work = root.join("work");
    fs::create_dir_all(&work).unwrap();
    let mut cfg = config::Effective::default();
    cfg.dir = work.clone();
    cfg.sandbox.enabled = true;
    cfg.policy = Layered::new(
        Set::new([Rule {
            tool: "*".into(),
            pattern: "*".into(),
            verb: Verb::Allow,
        }]),
        [],
    );
    let core = Core::new(
        "channel".into(),
        work.to_string_lossy().into_owned(),
        Time::now(),
        Arc::new(cfg.clone()),
        String::new(),
        String::new(),
        Arc::new(Memory),
    );
    core.commit(
        vec![
            core.event("", event::AGENT_SPAWNED, json!({"id":"a1","name":"main"}))
                .unwrap(),
        ],
        &CancellationToken::new(),
    )
    .unwrap();
    let mut scope = Scope::capture(&core, "a1", &paths).unwrap();
    scope.mode = "yolo".into();
    let role = RoleView {
        definition: config::frontmatter::Role::default(),
        missing: false,
        role: "general".into(),
        name: "main".into(),
        parent_name: String::new(),
        turn: 1,
    };
    (
        BoundaryEnvironment {
            paths,
            home: root.join("home"),
            runtime: None,
            uid: 123,
            level: Level::Full,
        },
        scope,
        role,
        cfg,
        core,
    )
}
fn call(tool: &str, input: Value) -> model::Block {
    serde_json::from_value(json!({"type":"tool_use","id":"call1","name":tool,"input":input}))
        .unwrap()
}
#[test]
fn commands_ask_in_every_mode_when_the_requested_sandbox_is_incomplete() {
    let temp = tempfile::tempdir().unwrap();
    let (mut boundary, mut scope, role, mut cfg, core) = fixture(temp.path());
    let call = call("shell", json!({"command":"printf hello"}));
    assert_eq!(
        boundary.decide(&scope, &call, &role, &cfg).unwrap().verb,
        Verb::Allow
    );
    for level in [Level::Landlock, Level::None] {
        boundary.level = level;
        for mode in ["ask", "auto", "yolo"] {
            scope.mode = mode.into();
            let decision = boundary.decide(&scope, &call, &role, &cfg).unwrap();
            assert_eq!(decision.verb, Verb::Ask);
            assert!(decision.bare);
            let prompt = decision.prompt(&core, "a1", &call, &role).unwrap();
            assert!(prompt.sticky);
            assert!(prompt.prefix.is_empty());
        }
    }
    cfg.sandbox.enabled = false;
    scope.mode = "yolo".into();
    assert_eq!(
        boundary.decide(&scope, &call, &role, &cfg).unwrap().verb,
        Verb::Allow
    );
}
#[test]
fn child_scopes_cannot_be_widened_by_mode_permits_or_a_boundary_prompt() {
    let temp = tempfile::tempdir().unwrap();
    let (boundary, mut scope, role, cfg, _) = fixture(temp.path());
    scope.parent = "parent".into();
    let outside = temp.path().join("outside/file");
    let read = call("read", json!({"path":outside}));
    scope.permits.apply(&event::PermitPayload {
        tool: "read".into(),
        call: outside.to_string_lossy().into_owned(),
        ..Default::default()
    });
    let decision = boundary.decide(&scope, &read, &role, &cfg).unwrap();
    assert_eq!(decision.verb, Verb::Deny);
    assert!(
        decision
            .why
            .contains("outside the agent's task directories")
    );
    scope.directories.clear();
    let status = call("agent", json!({"action":"status"}));
    assert_eq!(
        boundary.decide(&scope, &status, &role, &cfg).unwrap().verb,
        Verb::Deny
    );
}
#[test]
fn credential_paths_stay_hidden_and_control_edits_stay_sticky_while_owned_sheets_are_readable() {
    let temp = tempfile::tempdir().unwrap();
    let (boundary, mut scope, role, cfg, core) = fixture(temp.path());
    for path in [
        boundary.home.join(".ssh/id_rsa"),
        boundary.paths.data.join("auth.json"),
        temp.path().join(".secrets/token"),
    ] {
        let read = call("read", json!({"path":path}));
        let decision = boundary.decide(&scope, &read, &role, &cfg).unwrap();
        assert_eq!(decision.verb, Verb::Deny);
        assert!(decision.why.contains("hidden from agents in every mode"));
    }
    let patch = call(
        "patch",
        json!({"patch":"*** Begin Patch\n*** Add File: AGENTS.md\n+untrusted change\n*** End Patch"}),
    );
    scope.permits.apply(&event::PermitPayload {
        tool: "patch".into(),
        call: "AGENTS.md".into(),
        ..Default::default()
    });
    let decision = boundary.decide(&scope, &patch, &role, &cfg).unwrap();
    assert_eq!(decision.verb, Verb::Ask);
    assert!(decision.prompt(&core, "a1", &patch, &role).unwrap().sticky);
    let read = call("read", json!({"path":scope.sheets.join("s1.html")}));
    assert_eq!(
        boundary.decide(&scope, &read, &role, &cfg).unwrap().verb,
        Verb::Allow
    );
}
#[test]
fn read_only_roles_require_full_shell_enforcement_and_refuse_all_native_page_and_file_changes() {
    let temp = tempfile::tempdir().unwrap();
    let (mut boundary, scope, mut role, cfg, _) = fixture(temp.path());
    role.definition.read_only = true;
    for change in [
        call(
            "patch",
            json!({"patch":"*** Begin Patch\n*** Add File: file\n+x\n*** End Patch"}),
        ),
        call("sheet", json!({"action":"write","html":"x"})),
    ] {
        let decision = boundary.decide(&scope, &change, &role, &cfg).unwrap();
        assert_eq!(decision.verb, Verb::Deny);
        assert!(decision.why.contains("read-only"));
    }
    let shell = call("shell", json!({"command":"printf hello"}));
    assert_eq!(
        boundary.decide(&scope, &shell, &role, &cfg).unwrap().verb,
        Verb::Allow
    );
    boundary.level = Level::Landlock;
    let decision = boundary.decide(&scope, &shell, &role, &cfg).unwrap();
    assert_eq!(decision.verb, Verb::Deny);
    assert!(decision.why.contains("full OS sandbox"));
}
#[test]
fn standing_grants_cover_every_subject_and_merged_actions_keep_separate_authority() {
    let temp = tempfile::tempdir().unwrap();
    let (boundary, mut scope, role, mut cfg, _) = fixture(temp.path());
    cfg.policy = Layered::new(Set::default(), []);
    scope.mode = "ask".into();
    let patch = call(
        "patch",
        json!({"patch":"*** Begin Patch\n*** Add File: one.rs\n+x\n*** Add File: two.rs\n+y\n*** End Patch"}),
    );
    scope.permits.apply(&event::PermitPayload {
        tool: "patch".into(),
        call: "one.rs".into(),
        ..Default::default()
    });
    assert_eq!(
        boundary.decide(&scope, &patch, &role, &cfg).unwrap().verb,
        Verb::Ask
    );
    scope.permits.apply(&event::PermitPayload {
        tool: "patch".into(),
        call: "two.rs".into(),
        ..Default::default()
    });
    assert_eq!(
        boundary.decide(&scope, &patch, &role, &cfg).unwrap().verb,
        Verb::Allow
    );
    scope.permits = Permits::default();
    scope.permits.apply(&event::PermitPayload {
        tool: "agent_status".into(),
        call: "worker".into(),
        ..Default::default()
    });
    let cancel = call("agent", json!({"action":"cancel","id":"worker"}));
    assert_eq!(
        boundary.decide(&scope, &cancel, &role, &cfg).unwrap().verb,
        Verb::Ask
    );
}
#[test]
fn permission_answers_only_create_the_grants_and_directories_the_human_selected() {
    let temp = tempfile::tempdir().unwrap();
    let (boundary, mut scope, role, cfg, _) = fixture(temp.path());
    scope.mode = "ask".into();
    let path = temp.path().join("outside/file");
    let read = call("read", json!({"path":path}));
    let decision = boundary.decide(&scope, &read, &role, &cfg).unwrap();
    assert_eq!(decision.verb, Verb::Ask);
    match decision
        .answer(
            &read,
            &scope,
            &Answer {
                value: "allow".into(),
                ..Default::default()
            },
        )
        .unwrap()
    {
        Admission::Allowed { grants, directory } => {
            assert!(grants.is_empty());
            assert!(directory.is_none());
        }
        _ => panic!("allow refused"),
    }
    match decision
        .answer(
            &read,
            &scope,
            &Answer {
                value: "allow_always".into(),
                dir: "../chosen".into(),
                ..Default::default()
            },
        )
        .unwrap()
    {
        Admission::Allowed { grants, directory } => {
            assert_eq!(grants.len(), 1);
            assert_eq!(grants[0].tool, "read");
            assert_eq!(directory.unwrap(), temp.path().join("chosen"));
        }
        _ => panic!("allow refused"),
    }
    assert!(matches!(
        decision
            .answer(
                &read,
                &scope,
                &Answer {
                    withdrawn: true,
                    ..Default::default()
                }
            )
            .unwrap(),
        Admission::Withdrawn
    ));
    match decision
        .answer(
            &read,
            &scope,
            &Answer {
                value: "deny".into(),
                reason: "  keep private  ".into(),
                ..Default::default()
            },
        )
        .unwrap()
    {
        Admission::Denied(text) => assert_eq!(text, "Permission denied by the user: keep private"),
        _ => panic!("deny allowed"),
    }
}
#[test]
fn auto_mode_keeps_egress_asking_except_for_a_listed_host_or_explicit_permit() {
    let temp = tempfile::tempdir().unwrap();
    let (boundary, mut scope, role, mut cfg, _) = fixture(temp.path());
    cfg.policy = Layered::new(Set::default(), []);
    scope.mode = "auto".into();
    let fetch = call(
        "web",
        json!({"action":"fetch","url":"https://example.com/page"}),
    );
    let decision = boundary.decide(&scope, &fetch, &role, &cfg).unwrap();
    assert_eq!(decision.verb, Verb::Ask);
    assert!(decision.egress);
    cfg.hosts = vec!["example.com".into()];
    assert_eq!(
        boundary.decide(&scope, &fetch, &role, &cfg).unwrap().verb,
        Verb::Allow
    );
    let other = call(
        "web",
        json!({"action":"fetch","url":"https://evil.example.com/page"}),
    );
    assert_eq!(
        boundary.decide(&scope, &other, &role, &cfg).unwrap().verb,
        Verb::Ask
    );
    let search = call("web", json!({"action":"search","query":"fixture"}));
    assert_eq!(
        boundary.decide(&scope, &search, &role, &cfg).unwrap().verb,
        Verb::Ask
    );
}
