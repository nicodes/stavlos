use serde_json::{Value, json};
use stavlos::{
    agent::{scope, security, state::ChannelState},
    config, event,
    paths::Paths,
    policy::{Kind, Subject, Verb},
    sandbox::Spec,
    wire::Slice,
};
use std::{
    fs,
    os::unix::fs::symlink,
    path::{Path, PathBuf},
};

#[test]
fn all_go_permission_verdicts_and_shell_boundaries_match() {
    let rows: Vec<Value> =
        serde_json::from_str(include_str!("data/go-permission-verdicts.json")).unwrap();
    assert_eq!(rows.len(), 1536);
    for row in rows {
        let bits = row["bits"].as_u64().unwrap();
        let verdict = security::judge(security::Facts {
            ruled: serde_json::from_value(row["rule"].clone()).unwrap(),
            mode: row["mode"].as_str().unwrap(),
            bare: bits & 1 != 0,
            hidden: bits & 2 != 0,
            compound: bits & 4 != 0,
            control: bits & 8 != 0,
            permitted: bits & 16 != 0,
            egress: bits & 32 != 0,
            outside: bits & 64 != 0,
        });
        assert_eq!(json!(verdict), row["verdict"], "{row}");
    }
    let rows: Vec<Value> =
        serde_json::from_str(include_str!("data/go-command-boundaries.json")).unwrap();
    for row in rows {
        assert_eq!(
            json!(scope::bash_path_candidates(
                row["command"].as_str().unwrap(),
                Path::new("/work/repo")
            )),
            row["paths"],
            "{row}"
        );
    }
    let rows: Vec<Value> =
        serde_json::from_str(include_str!("data/go-permission-hosts.json")).unwrap();
    for row in rows {
        assert_eq!(
            json!(security::url_host(row["raw"].as_str().unwrap())),
            row["host"],
            "{row}"
        );
    }
}

fn spawn(state: &mut ChannelState, id: &str, parent: &str, directories: Slice<String>) {
    state.apply(
        &event::Event::new(
            "fixture",
            "",
            event::AGENT_SPAWNED,
            json!({"id":id,"parent":parent,"name":id,"directories":directories}),
        )
        .unwrap(),
    );
}
#[test]
fn child_scope_revocation_replay_empty_grants_and_symlinks_never_widen_access() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path().join("project");
    let extra = temp.path().join("extra");
    fs::create_dir_all(root.join("sub")).unwrap();
    fs::create_dir_all(&extra).unwrap();
    symlink(&extra, root.join("escape")).unwrap();
    let mut state = ChannelState::new("", "");
    state.dir = root.to_string_lossy().into_owned();
    let cfg = config::Effective::default();
    spawn(&mut state, "root", "", Slice::default());
    spawn(&mut state, "empty", "root", Vec::new().into());
    assert_eq!(
        scope::agent_directories(&state, &cfg, "root"),
        std::slice::from_ref(&root)
    );
    assert!(scope::agent_directories(&state, &cfg, "empty").is_empty());
    assert!(scope::child_scope(&state, &cfg, "empty", &[]).is_err());
    assert!(scope::child_scope(&state, &cfg, "root", &["escape".into()]).is_err());
    let grant = scope::child_scope(&state, &cfg, "root", &["sub".into()]).unwrap();
    spawn(&mut state, "child", "root", grant);
    assert!(
        scope::child_scope(
            &state,
            &cfg,
            "child",
            &[root.to_string_lossy().into_owned()]
        )
        .is_err()
    );
    let event = event::Event::new(
        "fixture",
        "",
        event::CHANNEL_DIR_ADDED,
        event::DirPayload {
            dir: extra.to_string_lossy().into_owned(),
            source: "human".into(),
        },
    )
    .unwrap();
    state.apply(&event);
    spawn(
        &mut state,
        "revoked",
        "root",
        vec![extra.to_string_lossy().into_owned()].into(),
    );
    assert_eq!(
        scope::agent_directories(&state, &cfg, "revoked"),
        std::slice::from_ref(&extra)
    );
    state.apply(
        &event::Event::new(
            "fixture",
            "",
            event::CHANNEL_DIR_REMOVED,
            event::DirPayload {
                dir: extra.to_string_lossy().into_owned(),
                source: String::new(),
            },
        )
        .unwrap(),
    );
    assert!(scope::agent_directories(&state, &cfg, "revoked").is_empty());
    assert!(scope::child_scope(&state, &cfg, "revoked", &[]).is_err());
    assert_eq!(
        scope::bash_path_candidates("cat escape/secret", &root),
        [extra.join("secret")]
    );
}

fn harness(temp: &Path) -> Paths {
    Paths {
        config: temp.join("harness/config"),
        data: temp.join("harness/data"),
        cache: temp.join("harness/cache"),
        socket: temp.join("harness/socket"),
        shared_skills: temp.join("skills"),
    }
}
#[test]
fn credentials_stay_hidden_even_as_the_working_directory_and_only_own_scratch_is_exempt() {
    let temp = tempfile::tempdir().unwrap();
    let home = temp.path().join("home");
    let work = home.join(".ssh/task");
    fs::create_dir_all(&work).unwrap();
    let paths = harness(temp.path());
    let cfg = config::Effective::default();
    let hidden =
        security::hidden_paths(&cfg, std::slice::from_ref(&work), &paths, &home, None, 123);
    assert!(hidden.contains(&home.join(".ssh")));
    assert!(hidden.contains(&temp.path().join(".secrets")));
    let subject = Subject::paths([work.join("key").to_string_lossy().into_owned()]);
    assert!(security::hidden_from(&subject, &work, &hidden, &[]).is_some());
    let own = paths.data.join("sheets/channel");
    let subject = Subject::paths([own.join("s1").to_string_lossy().into_owned()]);
    assert!(security::hidden_from(&subject, &work, &hidden, &[]).is_some());
    assert!(security::hidden_from(&subject, &work, &hidden, &[own]).is_none());
    let subject = Subject::paths([paths
        .data
        .join("other/channel")
        .to_string_lossy()
        .into_owned()]);
    assert!(
        security::hidden_from(
            &subject,
            &work,
            &hidden,
            &[paths.data.join("sheets/channel")]
        )
        .is_some()
    );
}

#[test]
fn linked_git_metadata_requires_an_exact_grant_backlink_and_existing_controls() {
    let temp = tempfile::tempdir().unwrap();
    let work = temp.path().join("work");
    let common = temp.path().join("repository/.git");
    let own = common.join("worktrees/work");
    for path in [
        &work,
        &own,
        &common.join("hooks"),
        &common.join("objects"),
        &common.join("refs"),
        &common.join("logs"),
    ] {
        fs::create_dir_all(path).unwrap();
    }
    fs::write(work.join(".git"), format!("gitdir: {}\n", own.display())).unwrap();
    fs::write(own.join("commondir"), "../..\n").unwrap();
    fs::write(
        own.join("gitdir"),
        format!("{}\n", work.join(".git").display()),
    )
    .unwrap();
    fs::write(common.join("config"), "").unwrap();
    let mut denied = Spec::default();
    security::grant_git_metadata(
        &mut denied,
        std::slice::from_ref(&work),
        std::slice::from_ref(&common),
    );
    assert!(denied.writable.is_empty());
    fs::write(own.join("config.worktree"), "").unwrap();
    let mut denied = Spec::default();
    security::grant_git_metadata(&mut denied, std::slice::from_ref(&work), &[]);
    assert!(denied.writable.is_empty());
    let mut allowed = Spec::default();
    security::grant_git_metadata(
        &mut allowed,
        std::slice::from_ref(&work),
        std::slice::from_ref(&common),
    );
    assert!(allowed.writable.contains(&own));
    assert!(allowed.writable.contains(&common.join("objects")));
    assert!(allowed.read_only.contains(&common.join("config")));
    assert!(allowed.read_only.contains(&own.join("config.worktree")));
    let alias = temp.path().join("alias");
    symlink(&common, &alias).unwrap();
    let mut denied = Spec::default();
    security::grant_git_metadata(&mut denied, std::slice::from_ref(&work), &[alias]);
    assert!(denied.writable.is_empty());
    fs::write(own.join("gitdir"), "/another/work/.git").unwrap();
    let mut denied = Spec::default();
    security::grant_git_metadata(&mut denied, &[work], &[common]);
    assert!(denied.writable.is_empty());
}

#[test]
fn readonly_roles_remove_network_overlapping_writable_grants_and_git_metadata() {
    let temp = tempfile::tempdir().unwrap();
    let work = temp.path().join("work");
    fs::create_dir_all(&work).unwrap();
    let paths = harness(temp.path());
    let mut cfg = config::Effective::default();
    cfg.sandbox = config::effective::SandboxState {
        enabled: true,
        network: true,
        writable: vec![
            temp.path().to_owned(),
            work.join("nested"),
            temp.path().join("cache"),
            temp.path().join("repo/.git/objects"),
        ],
        git_metadata: vec![temp.path().join("repo/.git")],
        ..Default::default()
    };
    let spec = security::role_sandbox_spec(
        &cfg,
        std::slice::from_ref(&work),
        &paths,
        &temp.path().join("home"),
        None,
        "fixture",
        true,
    )
    .unwrap();
    assert!(!spec.network);
    assert_eq!(spec.writable, [temp.path().join("cache")]);
    assert!(spec.read_only.contains(&work));
    assert!(spec.readable.contains(&work));
    assert!(spec.read_only.contains(&work.join("AGENTS.md")));
    assert!(!spec.private_tmp);
    assert_eq!(spec.tmp, scope::scratch_directory(&paths, "fixture"));
}

#[test]
fn permits_cover_every_path_and_never_cover_controls_or_merged_actions() {
    let mut permits = stavlos::agent::state::Permits::default();
    permits.apply(&event::PermitPayload {
        tool: "patch".into(),
        call: "a.rs".into(),
        ..Default::default()
    });
    assert!(permits.covers("patch", &Subject::paths(["a.rs".into()])));
    assert!(!permits.covers(
        "patch",
        &Subject::paths(["a.rs".into(), "../../.bashrc".into()])
    ));
    assert!(!permits.covers("patch", &Subject::paths([])));
    permits.apply(&event::PermitPayload {
        tool: "agent_status".into(),
        call: "worker".into(),
        ..Default::default()
    });
    assert!(!permits.covers("agent_cancel", &Subject::id("worker")));
    assert!(security::egress("search", &Subject::text("query")));
    assert!(security::egress("mcp__fixture__echo", &Subject::text("x")));
    assert!(!security::egress("read", &Subject::paths(["a.rs".into()])));
    for file in [
        "AGENTS.md",
        "nested/CLAUDE.md",
        ".git/config",
        ".stavlos/roles/general.md",
        ".envrc",
    ] {
        let subject = Subject::paths([file.into()]);
        assert_eq!(
            security::control_file(
                "patch",
                &subject,
                Path::new("/work"),
                &[PathBuf::from("/work")]
            ),
            Some(file.into())
        );
        assert_eq!(
            security::judge(security::Facts {
                bare: false,
                hidden: false,
                ruled: Verb::Allow,
                compound: false,
                control: true,
                permitted: true,
                mode: "yolo",
                egress: false,
                outside: false
            }),
            Verb::Ask
        );
    }
    assert!(!security::prefix_covers(
        Kind::Url,
        "example.com",
        "https://evil.example.com"
    ));
    let mut repeated = security::RepeatCall::default();
    assert!(!repeated.repeated("read", "{}", "ask"));
    assert!(!repeated.repeated("read", "{}", "ask"));
    assert!(repeated.repeated("read", "{}", "ask"));
    assert!(!repeated.repeated("read", "{}", "ask"));
    repeated.reset();
    assert!(!repeated.repeated("read", "{}", "ask"));
}
