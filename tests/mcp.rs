use serde_json::{Value, json, value::RawValue};
use stavlos::{
    agent::mcp::{self, LaunchContext, Manager, Session},
    config,
    tools::Env,
};
use std::{collections::BTreeMap, fs, path::Path, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;
fn input(value: Value) -> Box<RawValue> {
    RawValue::from_string(value.to_string()).unwrap()
}
fn definition(root: &Path, mode: &str) -> config::Mcp {
    config::Mcp {
        command: "/usr/bin/python3".into(),
        args: vec![
            format!(
                "{}/tests/data/mcp-stdio-fixture.py",
                env!("CARGO_MANIFEST_DIR")
            ),
            root.display().to_string(),
            mode.into(),
        ],
        env: BTreeMap::from([
            ("MCP_FIXTURE_TOKEN".into(), "fixture-private".into()),
            ("STAVLOS_SANDBOX_STAGE".into(), "forged".into()),
            ("STAVLOS_SANDBOX_SPEC".into(), "forged".into()),
        ]),
        url: String::new(),
    }
}
fn not_running(pid: i32) -> bool {
    match fs::read_to_string(format!("/proc/{pid}/stat")) {
        Ok(stat) => stat
            .rsplit_once(')')
            .unwrap()
            .1
            .trim_start()
            .starts_with('Z'),
        Err(_) => true,
    }
}
async fn wait_stopped(pids: &[i32]) {
    tokio::time::timeout(Duration::from_secs(2), async {
        while pids.iter().any(|pid| !not_running(*pid)) {
            tokio::time::sleep(Duration::from_millis(2)).await;
        }
    })
    .await
    .expect("MCP process group still running after cleanup");
}
#[test]
fn original_mcp_names_compact_subjects_and_result_rendering_remain_exact() {
    let cases: Vec<Value> = serde_json::from_str(include_str!("data/go-mcp-names.json")).unwrap();
    assert_eq!(cases.len(), 20);
    for c in cases {
        assert_eq!(
            mcp::tool_name(c["server"].as_str().unwrap(), c["tool"].as_str().unwrap()),
            c["name"].as_str().unwrap()
        );
    }
    let cases: Vec<Value> = serde_json::from_str(include_str!("data/go-mcp-results.json")).unwrap();
    assert_eq!(cases.len(), 11);
    for c in cases {
        assert_eq!(
            mcp::result_text(&c["result"]).unwrap(),
            c["text"].as_str().unwrap()
        );
    }
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-mcp-subjects.json")).unwrap();
    for c in cases {
        let raw: Box<RawValue> = serde_json::from_str(c["input"].as_str().unwrap()).unwrap();
        assert_eq!(
            mcp::subject(Some(&raw)).primary(),
            c["subject"].as_str().unwrap()
        );
    }
}
#[tokio::test]
async fn actual_stdio_sdk_handshake_pagination_tool_calls_and_cancellation_keep_the_server_alive() {
    let root = tempfile::tempdir().unwrap();
    let context = LaunchContext::unsandboxed(root.path().into()).unwrap();
    let session = Session::launch(
        "fixture",
        &definition(root.path(), "ordinary"),
        &context,
        CancellationToken::new(),
    )
    .await
    .unwrap();
    assert_eq!(session.tools.len(), 5);
    let output = session
        .call(
            "echo",
            "mcp__fixture__echo",
            Some(&input(json!({"text":"hello λ"}))),
            &Env::default(),
            CancellationToken::new(),
        )
        .await;
    assert!(!output.is_error, "{output:?}");
    let value: Value = serde_json::from_str(output.output.split_once('\n').unwrap().1).unwrap();
    assert_eq!(value["arguments"], json!({"text":"hello λ"}));
    assert_eq!(value["configured"], "fixture-private");
    assert!(value["stage"].is_null() && value["spec"].is_null());
    let cancel = CancellationToken::new();
    let c = cancel.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(50)).await;
        c.cancel();
    });
    let output = session
        .call("hang", "mcp__fixture__hang", None, &Env::default(), cancel)
        .await;
    assert!(output.is_error && output.output.contains("context canceled"));
    let output = session
        .call(
            "echo",
            "mcp__fixture__echo",
            None,
            &Env::default(),
            CancellationToken::new(),
        )
        .await;
    assert!(!output.is_error, "{output:?}");
    assert!(root.path().join("cancelled.json").exists());
    let output = session
        .call(
            "child",
            "mcp__fixture__child",
            None,
            &Env::default(),
            CancellationToken::new(),
        )
        .await;
    let grandchild = output
        .output
        .lines()
        .last()
        .unwrap()
        .parse::<i32>()
        .unwrap();
    assert!(!not_running(grandchild));
    let pid = session.pid;
    session.stop().await;
    wait_stopped(&[pid, grandchild]).await;
    assert!(session.closed());
}
#[tokio::test]
async fn manager_records_outside_its_lock_restarts_exits_and_handles_idle_role_changes() {
    let root = tempfile::tempdir().unwrap();
    let context = LaunchContext::unsandboxed(root.path().into()).unwrap();
    let defs = BTreeMap::from([("fixture".into(), definition(root.path(), "ordinary"))]);
    let (events, mut received) = tokio::sync::mpsc::unbounded_channel();
    let manager = Manager::new(Arc::new(move |kind, value| {
        events.send((kind.to_owned(), value)).unwrap();
        Ok(())
    }));
    assert_eq!(manager.info(&["fixture".into(), "fixture".into()]).len(), 1);
    assert_eq!(manager.info(&["fixture".into()])[0].state, "pending");
    manager
        .ensure(
            &defs,
            &["fixture".into()],
            &context,
            CancellationToken::new(),
        )
        .await;
    assert_eq!(received.recv().await.unwrap().0, "mcp.started");
    let definitions = manager.definitions();
    assert_eq!(definitions.len(), 4);
    assert_eq!(definitions[0].name, "mcp__fixture__echo");
    assert_eq!(definitions[0].description, "[fixture] Title echo");
    assert_eq!(manager.info(&["fixture".into()])[0].state, "connected");
    let output = manager
        .call(
            "mcp__fixture__exit",
            None,
            &Env::default(),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(output.is_error);
    let event = tokio::time::timeout(Duration::from_secs(3), received.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(event.0, "mcp.failed");
    assert_eq!(manager.info(&["fixture".into()])[0].state, "stopped");
    assert!(manager.definitions().is_empty());
    manager
        .ensure(
            &defs,
            &["fixture".into()],
            &context,
            CancellationToken::new(),
        )
        .await;
    assert_eq!(received.recv().await.unwrap().0, "mcp.started");
    manager.arm_idle(Duration::from_millis(10), Arc::new(|| true));
    manager.disarm_idle();
    tokio::time::sleep(Duration::from_millis(30)).await;
    assert_eq!(manager.info(&["fixture".into()])[0].state, "connected");
    manager.arm_idle(Duration::from_millis(10), Arc::new(|| true));
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(3), received.recv())
            .await
            .unwrap()
            .unwrap()
            .0,
        "mcp.stopped"
    );
    assert!(!manager.has_servers());
    manager
        .ensure(
            &defs,
            &["missing".into()],
            &context,
            CancellationToken::new(),
        )
        .await;
    assert_eq!(received.recv().await.unwrap().0, "mcp.failed");
    assert_eq!(manager.info(&["missing".into()])[0].state, "failed");
    manager
        .ensure(
            &defs,
            &["missing".into()],
            &context,
            CancellationToken::new(),
        )
        .await;
    assert!(received.try_recv().is_err());
    manager
        .ensure(&defs, &[], &context, CancellationToken::new())
        .await;
    assert!(!manager.has_servers());
    manager
        .ensure(
            &defs,
            &["fixture".into()],
            &context,
            CancellationToken::new(),
        )
        .await;
    received.recv().await.unwrap();
    manager
        .ensure(&defs, &[], &context, CancellationToken::new())
        .await;
    assert_eq!(received.recv().await.unwrap().0, "mcp.stopped");
}
#[tokio::test]
async fn launch_refuses_unbounded_readonly_execution_and_reaps_failed_or_cancelled_startup() {
    let root = tempfile::tempdir().unwrap();
    let mut context = LaunchContext::unsandboxed(root.path().into()).unwrap();
    context.readonly = true;
    assert!(
        Session::launch(
            "fixture",
            &definition(root.path(), "ordinary"),
            &context,
            CancellationToken::new()
        )
        .await
        .is_err()
    );
    assert!(!root.path().join("server.pid").exists());
    context.readonly = false;
    context.directories_revoked = true;
    assert!(
        Session::launch(
            "fixture",
            &definition(root.path(), "ordinary"),
            &context,
            CancellationToken::new()
        )
        .await
        .is_err()
    );
    context.directories_revoked = false;
    let error = Session::launch(
        "fixture",
        &definition(root.path(), "fail"),
        &context,
        CancellationToken::new(),
    )
    .await
    .err()
    .unwrap()
    .to_string();
    assert!(
        error.contains("fixture failed") && error.len() < 4300,
        "{error}"
    );
    let pid = fs::read_to_string(root.path().join("server.pid"))
        .unwrap()
        .parse()
        .unwrap();
    wait_stopped(&[pid]).await;
    let cancel = CancellationToken::new();
    let c = cancel.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(100)).await;
        c.cancel();
    });
    assert!(
        Session::launch(
            "fixture",
            &definition(root.path(), "quiet"),
            &context,
            cancel
        )
        .await
        .is_err()
    );
    let pid = fs::read_to_string(root.path().join("server.pid"))
        .unwrap()
        .parse()
        .unwrap();
    wait_stopped(&[pid]).await;
    let manager = Manager::new(Arc::new(|_, _| Ok(())));
    let definitions = BTreeMap::from([("quiet".into(), definition(root.path(), "quiet"))]);
    let weak = Arc::downgrade(&manager);
    let task_manager = manager.clone();
    let task = tokio::spawn(async move {
        task_manager
            .ensure(
                &definitions,
                &["quiet".into()],
                &context,
                CancellationToken::new(),
            )
            .await;
    });
    tokio::time::sleep(Duration::from_millis(100)).await;
    manager.stop(None, true).await;
    tokio::time::timeout(Duration::from_secs(3), task)
        .await
        .unwrap()
        .unwrap();
    assert!(!manager.has_servers());
    drop(manager);
    assert!(weak.upgrade().is_none());
    let pid = fs::read_to_string(root.path().join("server.pid"))
        .unwrap()
        .parse()
        .unwrap();
    wait_stopped(&[pid]).await;
}
