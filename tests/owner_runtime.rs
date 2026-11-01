use serde_json::{Value, json};
use stavlos::{
    auth::Store,
    daemon::{
        Daemon,
        hub::Client,
        server::{Connection, Handler, Scope},
    },
    escalation,
    model::{self, registry::Registry},
    oauth,
    paths::Paths,
    protocol,
    wire::Time,
};
use std::{collections::BTreeMap, path::Path, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;
struct Fixture {
    root: tempfile::TempDir,
    paths: Paths,
    home: std::path::PathBuf,
    daemon: Arc<Daemon>,
}
impl Fixture {
    async fn new() -> Self {
        let root = tempfile::tempdir().unwrap();
        let home = root.path().join("home");
        std::fs::create_dir(&home).unwrap();
        let paths = Paths {
            config: home.join("config"),
            data: home.join("data"),
            cache: home.join("cache"),
            socket: root.path().join("owner.sock"),
            shared_skills: home.join("skills"),
        };
        let registry = Registry::new(
            None,
            Some(Arc::new(Store::open(paths.auth_file()))),
            BTreeMap::new(),
        )
        .unwrap();
        let daemon = Daemon::open(paths.clone(), home.clone(), registry)
            .await
            .unwrap();
        Self {
            root,
            paths,
            home,
            daemon,
        }
    }
    fn project(&self, name: &str) -> std::path::PathBuf {
        let path = self.root.path().join(name);
        std::fs::create_dir(&path).unwrap();
        path
    }
    fn create(&self, path: &Path, name: &str) -> Arc<stavlos::agent::runtime::Channel> {
        self.daemon
            .create(
                &path.to_string_lossy(),
                "",
                "",
                name,
                &CancellationToken::new(),
            )
            .unwrap()
    }
    async fn call(
        &self,
        method: &str,
        params: Value,
    ) -> std::result::Result<Value, protocol::Error> {
        call(&self.daemon, method, params).await
    }
}
async fn call(
    daemon: &Daemon,
    method: &str,
    params: Value,
) -> std::result::Result<Value, protocol::Error> {
    let request = protocol::Request {
        v: 1,
        method: method.into(),
        params: Some(serde_json::value::to_raw_value(&params).unwrap()),
        ..Default::default()
    };
    let connection = Connection {
        client: Client::new().unwrap(),
        cancel: CancellationToken::new(),
        scope: Scope::Owner,
        descendant: false,
    };
    connection.admit(&request)?;
    daemon.dispatch(&connection, request).await
}
#[tokio::test]
async fn channels_recover_their_identity_and_events_and_the_data_lock_is_released() {
    let f = Fixture::new().await;
    let project = f.project("project");
    let channel = f.create(&project, "#Docs Site");
    let id = channel.core.id.clone();
    let root = channel.root().unwrap().id.clone();
    assert_eq!(channel.info().name, "docs-site");
    assert!(
        f.daemon
            .create(
                &project.to_string_lossy(),
                "",
                "",
                "docs-site",
                &CancellationToken::new()
            )
            .is_err()
    );
    assert!(
        Daemon::open(f.paths.clone(), f.home.clone(), f.daemon.registry.clone())
            .await
            .is_err()
    );
    f.call("channel.rename", json!({"channel":id,"name":"Renamed"}))
        .await
        .unwrap();
    f.call("channel.add_dir", json!({"channel":id,"dir":"../extra"}))
        .await
        .unwrap();
    let before = f.daemon.log.last_seq(&id).unwrap();
    assert!(before >= 4);
    f.daemon.close().await.unwrap();
    f.daemon.close().await.unwrap();
    let reopened = Daemon::open(f.paths.clone(), f.home.clone(), f.daemon.registry.clone())
        .await
        .unwrap();
    let restored = reopened.channel(&id).unwrap();
    assert_eq!(restored.info().name, "renamed");
    assert_eq!(restored.root().unwrap().id, root);
    assert_eq!(reopened.log.last_seq(&id).unwrap(), before);
    assert!(
        restored
            .info()
            .dirs
            .iter()
            .any(|dir| dir.path.ends_with("extra"))
    );
    reopened.close().await.unwrap();
}
#[tokio::test]
async fn metadata_rpc_is_typed_and_errors_do_not_turn_into_successful_empty_results() {
    let f = Fixture::new().await;
    let project = f.project("project");
    let channel = f.create(&project, "");
    let id = &channel.core.id;
    let status = f.call("daemon.status", json!({})).await.unwrap();
    assert_eq!(status["channels"], 1);
    assert_eq!(status["agents"], 1);
    for method in [
        "agent.tree",
        "sheet.list",
        "roles",
        "command.list",
        "reconcile",
    ] {
        assert!(
            f.call(method, json!({"channel":id})).await.is_ok(),
            "{method}"
        );
    }
    assert_eq!(
        f.call("channel.resume", json!({"channel":"missing"}))
            .await
            .unwrap_err()
            .code,
        protocol::ERR_NOT_FOUND
    );
    assert_eq!(
        f.call("channel.create", json!([])).await.unwrap_err().code,
        protocol::ERR_INVALID_PARAMS
    );
    assert!(
        f.call("usage.series", json!({"buckets":1001}))
            .await
            .is_err()
    );
    assert!(
        f.call("usage.series", json!({"buckets":1,"agent":"a"}))
            .await
            .is_err()
    );
    assert!(f.call("usage.cache", json!({"minutes":-1})).await.is_err());
    assert_eq!(
        f.call("discord.status", json!({})).await.unwrap()["state"],
        "disconnected"
    );
    f.daemon.close().await.unwrap();
}
#[tokio::test]
async fn trust_requires_the_current_snapshot_and_publishes_before_a_directory_change() {
    let f = Fixture::new().await;
    let project = f.project("project");
    std::fs::write(project.join("AGENTS.md"), "First owner instructions").unwrap();
    let channel = f.create(&project, "");
    let id = channel.core.id.clone();
    let status = f.daemon.trust_status(&project.to_string_lossy()).unwrap();
    assert!(status.pending);
    assert_eq!(f.daemon.prompts.pending(&id).len(), 1);
    std::fs::write(project.join("AGENTS.md"), "Changed instructions").unwrap();
    let error = f.daemon.trust(&status.dir, &status.hash, true).unwrap_err();
    assert_eq!(
        error.downcast_ref::<protocol::Error>().unwrap().code,
        protocol::ERR_CONFLICT
    );
    let current = f.daemon.trust_status(&status.dir).unwrap();
    f.daemon.trust(&current.dir, &current.hash, true).unwrap();
    assert!(!channel.core.configuration().trust_pending);
    assert!(!f.daemon.trust_status(&status.dir).unwrap().pending);
    std::fs::write(project.join("AGENTS.md"), "Changed again").unwrap();
    f.daemon.project_changed(&project);
    assert!(channel.core.configuration().trust_pending);
    let other = f.project("other");
    f.daemon
        .set_directory(&id, &other.to_string_lossy(), &CancellationToken::new())
        .await
        .unwrap();
    assert!(f.daemon.prompts.pending(&id).is_empty());
    tokio::task::yield_now().await;
    assert!(f.daemon.prompts.pending(&id).is_empty());
    f.daemon.close().await.unwrap();
}
#[tokio::test]
async fn owner_configuration_edits_bind_the_opened_root_and_revision_and_reload() {
    let f = Fixture::new().await;
    let project = f.project("project");
    let channel = f.create(&project, "");
    let tree = f
        .call("config.list", json!({"scope":"system"}))
        .await
        .unwrap();
    let edit = json!({"scope":"system","root":tree["root"],"revision":tree["revision"],"path":"stavlos.json","action":"write","content":"{\"mode\":\"auto\",\"reminders\":false}"});
    let saved = f.call("config.edit", edit.clone()).await.unwrap();
    assert!(
        saved["notice"]
            .as_str()
            .unwrap()
            .starts_with("Saved to disk.")
    );
    assert!(!channel.core.configuration().reminders);
    assert!(f.call("config.edit", edit.clone()).await.is_err());
    let mut wrong = edit;
    wrong["root"] = json!("/unrelated");
    wrong["revision"] = saved["tree"]["revision"].clone();
    assert!(f.call("config.edit", wrong).await.is_err());
    let read = f
        .call(
            "config.read",
            json!({"scope":"system","root":tree["root"],"path":"stavlos.json"}),
        )
        .await
        .unwrap();
    assert!(read["content"].as_str().unwrap().contains("auto"));
    assert!(
        f.call("config.read", json!({"scope":"system","path":"../outside"}))
            .await
            .is_err()
    );
    f.daemon.close().await.unwrap();
}
#[tokio::test]
async fn changing_mode_resolves_only_permissions_the_new_mode_can_decide() {
    let f = Fixture::new().await;
    let project = f.project("project");
    let channel = f.create(&project, "");
    let id = &channel.core.id;
    let make = |name: &str, kind: &str, sticky: bool| protocol::PromptInfo {
        id: name.into(),
        channel: id.clone(),
        kind: kind.into(),
        sticky,
        ..Default::default()
    };
    let allow = f.daemon.prompts.begin(
        make("allow", "permission", false),
        CancellationToken::new(),
        None,
    );
    let control = f.daemon.prompts.begin(
        make("control", "permission", true),
        CancellationToken::new(),
        None,
    );
    let question = f.daemon.prompts.begin(
        make("question", "question", false),
        CancellationToken::new(),
        None,
    );
    f.call("channel.set_mode", json!({"channel":id,"mode":"yolo"}))
        .await
        .unwrap();
    assert_eq!(
        tokio::time::timeout(Duration::from_secs(1), allow)
            .await
            .unwrap()
            .value,
        "allow"
    );
    let pending = f.daemon.prompts.pending(id);
    assert_eq!(pending.len(), 2);
    assert!(pending.iter().any(|prompt| prompt.id == "control"));
    drop(control);
    drop(question);
    assert!(f.daemon.prompts.pending(id).is_empty());
    f.daemon.close().await.unwrap();
}
#[tokio::test]
async fn dropping_a_published_prompt_before_polling_withdraws_its_guard() {
    let manager = escalation::Manager::new(Default::default(), Arc::new(|_, _| {}));
    let waiter = manager.begin(
        protocol::PromptInfo {
            id: "trust".into(),
            kind: "trust".into(),
            ..Default::default()
        },
        CancellationToken::new(),
        None,
    );
    assert_eq!(manager.pending("").len(), 1);
    drop(waiter);
    assert!(manager.pending("").is_empty());
}
#[tokio::test]
async fn shared_channels_deliver_addressed_work_and_keep_passive_posts_passive() {
    let f = Fixture::new().await;
    let project = f.project("project");
    let home = f.create(&project, "execution");
    let root = home.root().unwrap();
    f.daemon
        .board_create(
            &home.core.id,
            &root.id,
            "Team",
            &[],
            &CancellationToken::new(),
        )
        .unwrap();
    let board = f
        .daemon
        .channels()
        .into_iter()
        .find(|channel| channel.info().board.is_some())
        .unwrap();
    let count = home
        .core
        .inspect(|state, _| state.agents[&root.id].inbox.len());
    assert!(
        f.daemon
            .post(
                &board,
                "passive note",
                "human:test",
                &CancellationToken::new()
            )
            .unwrap()
            .is_empty()
    );
    assert!(
        home.core
            .inspect(|state, _| !state.agents[&root.id].starts_turn())
    );
    f.daemon
        .post(
            &board,
            &format!("@{} task", root.name()),
            "human:test",
            &CancellationToken::new(),
        )
        .unwrap();
    assert!(
        home.core
            .inspect(|state, _| state.agents[&root.id].inbox.len())
            > count
    );
    assert_eq!(f.daemon.channel_tree(&board).len(), 1);
    assert!(f.daemon.board_for("wrong", &root.id, "Team").is_err());
    f.daemon
        .archive(&home.core.id, &CancellationToken::new())
        .await
        .unwrap();
    assert!(board.info().archived);
    f.daemon.close().await.unwrap();
}
#[tokio::test]
async fn plan_observations_survive_restart_and_remain_hidden_without_a_connected_provider() {
    let f = Fixture::new().await;
    let at = Time::now();
    f.daemon.registry.usage.observe(
        "openai",
        model::PlanUsage {
            observed: at,
            windows: vec![model::UsageWindow {
                used_percent: 42.,
                minutes: 300,
                ..Default::default()
            }]
            .into(),
            ..Default::default()
        },
    );
    assert!(f.daemon.plan_usage().plans.is_empty());
    f.daemon.close().await.unwrap();
    let registry = Registry::new(None, None, BTreeMap::new()).unwrap();
    let reopened = Daemon::open(f.paths.clone(), f.home.clone(), registry)
        .await
        .unwrap();
    assert_eq!(
        reopened.registry.usage.all()["openai"].windows[0].used_percent,
        42.
    );
    let raw: Value =
        serde_json::from_slice(&std::fs::read(f.paths.data.join("plan-usage.json")).unwrap())
            .unwrap();
    assert_eq!(raw["openai"]["history"].as_array().unwrap().len(), 1);
    reopened.close().await.unwrap();
}
#[tokio::test]
async fn reaper_preserves_owned_exit_status_and_collects_only_repeated_unowned_zombies() {
    let (mut child, ownership) = stavlos::ownedchild::spawn(|| {
        let child = std::process::Command::new("sh")
            .args(["-c", "exit 19"])
            .spawn()?;
        let pid = child.id() as i32;
        Ok((child, pid))
    })
    .unwrap();
    let pid = child.id() as i32;
    let start = zombie(pid).await;
    stavlos::ownedchild::scan(&BTreeMap::from([(pid, start)]));
    assert_eq!(child.wait().unwrap().code(), Some(19));
    drop(ownership);
    let child = std::process::Command::new("sh")
        .args(["-c", "exit 23"])
        .spawn()
        .unwrap();
    let pid = child.id() as i32;
    drop(child);
    zombie(pid).await;
    let first = stavlos::ownedchild::scan(&BTreeMap::new());
    assert!(first.contains_key(&pid));
    stavlos::ownedchild::scan(&first);
    assert!(!Path::new(&format!("/proc/{pid}")).exists());
}
async fn zombie(pid: i32) -> u64 {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            if let Ok(stat) = std::fs::read_to_string(format!("/proc/{pid}/stat"))
                && let Some(('Z', _, start)) = stavlos::ownedchild::identity(&stat)
            {
                return start;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap()
}

#[derive(serde::Deserialize)]
struct PlanCase {
    inputs: Vec<model::PlanUsage>,
    history: Vec<protocol::PlanUsagePoint>,
    from: Time,
    to: Time,
    buckets: usize,
    expected: Vec<f64>,
}
#[test]
fn plan_history_and_charts_match_120_go_scenarios() {
    let cases: Vec<PlanCase> =
        serde_json::from_str(include_str!("data/go-owner-plan-series.json")).unwrap();
    assert_eq!(cases.len(), 120);
    for case in cases {
        let mut points = stavlos::wire::Slice::default();
        for usage in case.inputs {
            stavlos::daemon::planusage::record(&mut points, &usage);
        }
        assert_eq!(
            serde_json::to_value(&points).unwrap(),
            serde_json::to_value(case.history).unwrap()
        );
        assert_eq!(
            stavlos::daemon::planusage::series(&points, case.from, case.to, case.buckets),
            case.expected
        );
    }
}

struct OfflineLogin;
impl oauth::Flow for OfflineLogin {
    fn provider(&self) -> &str {
        "zai"
    }
    fn label(&self) -> &str {
        "Offline fixture"
    }
    fn methods(&self) -> Vec<oauth::Method> {
        vec![oauth::Method {
            id: "fixture".into(),
            label: "Offline fixture".into(),
        }]
    }
    fn start(
        &self,
        method: String,
        _: CancellationToken,
    ) -> oauth::FlowFuture<'_, Arc<oauth::Pending>> {
        Box::pin(async move {
            let mut pending = oauth::Pending::default();
            pending.provider = "zai".into();
            pending.method = method;
            pending.expires_in = Duration::from_secs(300);
            Ok(Arc::new(pending))
        })
    }
    fn wait(
        &self,
        _: Arc<oauth::Pending>,
        _: CancellationToken,
    ) -> oauth::FlowFuture<'_, oauth::Tokens> {
        Box::pin(async {
            Ok(oauth::Tokens {
                access: "offline fixture credential".into(),
                ..Default::default()
            })
        })
    }
    fn refresh(&self, _: String, _: CancellationToken) -> oauth::FlowFuture<'_, oauth::Tokens> {
        Box::pin(async { Err(oauth::Error::Denied) })
    }
}
#[tokio::test]
async fn login_rpc_uses_the_native_flow_and_forgets_a_completed_login() {
    let mut f = Fixture::new().await;
    f.daemon.close().await.unwrap();
    let flow: Arc<dyn oauth::Flow> = Arc::new(OfflineLogin);
    let registry = Registry::new(
        None,
        Some(Arc::new(Store::open(f.paths.auth_file()))),
        BTreeMap::from([("zai".into(), flow)]),
    )
    .unwrap();
    f.daemon = Daemon::open(f.paths.clone(), f.home.clone(), registry)
        .await
        .unwrap();
    let login = f
        .call(
            "provider.login.start",
            json!({"provider":"zai","method":"fixture"}),
        )
        .await
        .unwrap();
    assert_eq!(login["method"], "fixture");
    assert_eq!(login["expires_in"], 300);
    let result = f
        .call("provider.login.wait", json!({"id":login["id"]}))
        .await
        .unwrap();
    assert_eq!(result["id"], "zai");
    assert_eq!(result["connected"], true);
    assert!(
        f.call("provider.login.wait", json!({"id":login["id"]}))
            .await
            .is_err()
    );
    f.call("provider.disconnect", json!({"provider":"zai"}))
        .await
        .unwrap();
    assert!(!f.daemon.registry.status("zai").unwrap().connected);
    f.daemon.close().await.unwrap();
}
