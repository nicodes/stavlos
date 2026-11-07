use serde_json::{Value, json};
use stavlos::{
    app::{self, App, Terminal},
    auth::Store,
    client::Client,
    config,
    daemon::{
        Daemon,
        server::{Scope, Socket},
    },
    model::registry::Registry,
    navigation,
    paths::Paths,
    protocol,
    tools::ServiceFuture,
};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio_util::sync::CancellationToken;
#[derive(Default)]
struct Ui(Mutex<Vec<String>>);
impl Terminal for Ui {
    fn run(&self, _: Arc<Client>, channel: String) -> ServiceFuture<'_, ()> {
        Box::pin(async move {
            self.0.lock().unwrap().push(channel);
            Ok(())
        })
    }
}
struct Fixture {
    root: tempfile::TempDir,
    app: App,
    ui: Arc<Ui>,
    owner: Arc<Daemon>,
    server: tokio::task::JoinHandle<anyhow::Result<()>>,
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
            shared_skills: home.join("skills"),
            socket: root.path().join("owner.sock"),
        };
        let registry = Registry::new(
            None,
            Some(Arc::new(Store::open(paths.auth_file()))),
            BTreeMap::new(),
        )
        .unwrap();
        let owner = Daemon::open(paths.clone(), home.clone(), registry)
            .await
            .unwrap();
        let socket = Socket::bind(&paths.socket).unwrap();
        let accepting = owner.clone();
        let server = tokio::spawn(async move {
            socket
                .serve(
                    accepting.clone(),
                    accepting.hub.clone(),
                    accepting.cancel.clone(),
                    Scope::Owner,
                )
                .await
        });
        let ui = Arc::new(Ui::default());
        let app = App {
            paths,
            home,
            terminal: ui.clone(),
            cancel: CancellationToken::new(),
        };
        Self {
            root,
            app,
            ui,
            owner,
            server,
        }
    }
    async fn run(&self, args: &[&str]) -> anyhow::Result<()> {
        self.app
            .run(&args.iter().map(|s| (*s).to_owned()).collect::<Vec<_>>())
            .await
    }
    fn project(&self, name: &str) -> String {
        let project = self.root.path().join(name);
        std::fs::create_dir(&project).unwrap();
        project.to_string_lossy().into_owned()
    }
    async fn close(self) {
        self.owner.close().await.unwrap();
        tokio::time::timeout(Duration::from_secs(3), self.server)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
    }
}
#[tokio::test]
async fn channel_commands_keep_global_selection_explicit_creation_and_name_lookup() {
    let f = Fixture::new().await;
    let first = f.project("first");
    let second = f.project("second");
    f.run(&["new", "--dir", &first, "--no-tui"]).await.unwrap();
    let a = f.owner.channels()[0].core.id.clone();
    assert!(f.ui.0.lock().unwrap().is_empty());
    f.run(&["new", "--dir", &second, "--no-tui"]).await.unwrap();
    let b = f
        .owner
        .channels()
        .iter()
        .find(|channel| channel.core.id != a)
        .unwrap()
        .core
        .id
        .clone();
    navigation::remember(&f.app.paths.data, &a, 10).unwrap();
    f.run(&[]).await.unwrap();
    assert_eq!(f.ui.0.lock().unwrap().last(), Some(&a));
    f.run(&["open", &b]).await.unwrap();
    assert_eq!(f.ui.0.lock().unwrap().last(), Some(&b));
    let info = f.owner.channel(&b).unwrap().info();
    f.run(&["open", &format!("#{}", info.name)]).await.unwrap();
    for args in [
        vec!["channels"],
        vec!["status"],
        vec!["tree", &a],
        vec!["cancel", &f.owner.channel(&a).unwrap().root().unwrap().id],
    ] {
        f.run(&args).await.unwrap();
    }
    assert!(
        f.run(&["open", "missing"])
            .await
            .unwrap_err()
            .to_string()
            .contains("no channel")
    );
    assert!(f.run(&["send"]).await.is_err());
    assert!(f.run(&["new", "--unknown"]).await.is_err());
    assert_eq!(f.owner.channels().len(), 2);
    f.close().await;
}
#[tokio::test]
async fn initialization_uses_current_go_defaults_and_never_overwrites_an_existing_configuration() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-cli-starter-config.json")).unwrap();
    for case in cases {
        let f = Fixture::new().await;
        f.run(&["init", "--model", case["model"].as_str().unwrap()])
            .await
            .unwrap();
        let path = f.app.paths.config.join("stavlos.toml");
        let raw = std::fs::read(&path).unwrap();
        assert_eq!(
            stavlos::source_toml::parse(std::str::from_utf8(&raw).unwrap()).unwrap(),
            case["expected"]
        );
        assert!(f.run(&["init"]).await.is_err());
        assert_eq!(std::fs::read(path).unwrap(), raw);
        f.close().await;
    }
}
#[test]
fn command_flags_and_resume_selection_match_frozen_go_cases() {
    let commands: Vec<Value> =
        serde_json::from_str(include_str!("data/go-cli-command-selection.json")).unwrap();
    for case in commands {
        let args = serde_json::from_value::<Option<Vec<String>>>(case["args"].clone())
            .unwrap()
            .unwrap_or_default();
        let (command, rest) = app::command(&args);
        assert_eq!(command, case["command"]);
        assert_eq!(
            json!(rest),
            if case["rest"].is_null() {
                json!([])
            } else {
                case["rest"].clone()
            }
        );
    }
    let parses: Vec<Value> =
        serde_json::from_str(include_str!("data/go-cli-flag-parsing.json")).unwrap();
    for case in parses {
        let args = serde_json::from_value::<Vec<String>>(case["args"].clone()).unwrap();
        let parsed = app::flags(&args, &["model", "root", "dir"], &["no-tui"]);
        if case["error"] != "" {
            assert!(parsed.is_err(), "{case}");
            continue;
        }
        let parsed = parsed.unwrap();
        for key in ["model", "root", "dir"] {
            assert_eq!(parsed.get(key).map(String::as_str).unwrap_or(""), case[key]);
        }
        assert_eq!(
            parsed.get("no-tui").is_some_and(|value| value == "true"),
            case["no_tui"] == true
        );
    }
    let resumes: Vec<Value> =
        serde_json::from_str(include_str!("data/go-cli-resume-selection.json")).unwrap();
    for case in resumes {
        let channels: Vec<protocol::ChannelInfo> =
            serde_json::from_value(case["channels"].clone()).unwrap();
        assert_eq!(
            navigation::resume(&channels, case["last"].as_str().unwrap()),
            case["expected"]
        );
    }
    assert_eq!(config::effective::defaults().model, "");
}

#[tokio::test]
async fn machine_commands_use_real_owner_results_and_refuse_interaction() {
    let f = Fixture::new().await;
    let args = |args: &[&str]| args.iter().map(|v| (*v).to_owned()).collect::<Vec<_>>();
    for command in [
        vec![],
        vec!["new"],
        vec!["open", "missing"],
        vec!["trust"],
        vec!["auth", "login"],
        vec!["auth", "logout"],
        vec!["web", "open"],
        vec!["daemon"],
    ] {
        assert!(
            f.app.run_json(&args(&command)).await.is_err(),
            "{command:?}"
        );
    }
    assert!(f.owner.channels().is_empty());
    assert!(f.ui.0.lock().unwrap().is_empty());
    let project = f.project("machine");
    let channel = f
        .app
        .run_json(&args(&["new", "--dir", &project, "--no-tui"]))
        .await
        .unwrap();
    let channels = f.app.run_json(&args(&["channels"])).await.unwrap();
    assert_eq!(channels["channels"][0]["id"], channel["id"]);
    let providers = f.app.run_json(&args(&["auth", "list"])).await.unwrap();
    assert!(providers["providers"].is_array());
    let initialized = f.app.run_json(&args(&["init"])).await.unwrap();
    assert!(std::path::Path::new(initialized["path"].as_str().unwrap()).is_file());
    assert!(f.ui.0.lock().unwrap().is_empty());
    f.close().await;
}

#[test]
fn machine_binary_errors_are_one_document_before_owner_start() {
    let root = tempfile::tempdir().unwrap();
    for args in [
        vec!["--json", "version"],
        vec!["--json", "help"],
        vec!["--json", "auth", "login"],
        vec!["--json"],
    ] {
        let output = std::process::Command::new(env!("CARGO_BIN_EXE_stavlos"))
            .args(&args)
            .env("STAVLOS_CONFIG_DIR", root.path().join("config"))
            .env("STAVLOS_DATA_DIR", root.path().join("data"))
            .env("STAVLOS_CACHE_DIR", root.path().join("cache"))
            .env("STAVLOS_SOCKET", root.path().join("owner.sock"))
            .output()
            .unwrap();
        let value: Value = serde_json::from_slice(&output.stdout).unwrap();
        assert_eq!(value["v"], 1);
        assert_eq!(value["ok"], output.status.success());
        assert!(!root.path().join("owner.sock").exists());
        assert_eq!(std::fs::read_dir(root.path()).unwrap().count(), 0);
    }
}
