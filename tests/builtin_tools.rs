use serde_json::{Value, json, value::RawValue};
use stavlos::{
    event, policy,
    tools::{self, *},
};
use std::{
    collections::BTreeMap,
    fs,
    path::PathBuf,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio_util::sync::CancellationToken;
fn raw(input: &str) -> Box<RawValue> {
    serde_json::from_str(input).unwrap()
}
struct Todo;
impl Todos for Todo {
    fn list(&self) -> Vec<event::TodoItem> {
        vec![]
    }
    fn edit(
        &self,
        add: Vec<TodoAdd>,
        updates: Vec<TodoUpdate>,
    ) -> anyhow::Result<Vec<event::TodoItem>> {
        let mut items = vec![event::TodoItem {
            id: "t1".into(),
            text: "Existing".into(),
            status: "pending".into(),
        }];
        for u in updates {
            anyhow::ensure!(u.id == "t1", "unknown todo {}", u.id);
            if !u.status.is_empty() {
                items[0].status = u.status;
            }
            if !u.text.is_empty() {
                items[0].text = u.text;
            }
        }
        for a in add {
            items.push(event::TodoItem {
                id: format!("t{}", items.len() + 1),
                text: a.text,
                status: if a.status.is_empty() {
                    "pending".into()
                } else {
                    a.status
                },
            });
        }
        Ok(items)
    }
}
struct Sheet;
impl Sheets for Sheet {
    fn write(&self, id: &str, title: &str, html: &str) -> anyhow::Result<SheetRef> {
        let id = if id.is_empty() { "s1" } else { id };
        Ok(SheetRef {
            id: id.into(),
            title: title.into(),
            author: "agent".into(),
            path: format!("/sheets/{id}.html"),
            size: html.len() as i64,
        })
    }
    fn list(&self) -> Vec<SheetRef> {
        vec![SheetRef {
            id: "s1".into(),
            title: "Existing".into(),
            author: "agent".into(),
            path: "/sheets/s1.html".into(),
            size: 12,
        }]
    }
    fn delete(&self, id: &str) -> anyhow::Result<()> {
        anyhow::ensure!(id != "missing", "unknown sheet");
        Ok(())
    }
}
struct Ask;
impl Asker for Ask {
    fn ask<'a>(
        &'a self,
        _: Vec<event::Question>,
        _: CancellationToken,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = Answers> + Send + 'a>> {
        Box::pin(async {
            Answers {
                answers: vec![" yes ".into(), "".into()],
                error: None,
            }
        })
    }
}
fn statuses() -> Vec<ChildStatus> {
    vec![ChildStatus {
        id: "a1".into(),
        name: "lead".into(),
        you: true,
        state: "running".into(),
        turn: 7,
        cost_usd: 1.234,
        pending_replies: vec![event::ReplyRequest {
            id: "r1".into(),
            from_name: "child".into(),
            text: "  Some \n words ".into(),
            ..Default::default()
        }]
        .into(),
        awaiting_replies: vec![event::ReplyRequest {
            from_name: "other".into(),
            ..Default::default()
        }]
        .into(),
        ..Default::default()
    }]
}
struct Orch;
impl Orchestrator for Orch {
    fn spawn<'a>(&'a self, _: Spawn, _: CancellationToken) -> ServiceFuture<'a, (String, String)> {
        Box::pin(async { Ok(("a2".into(), "child".into())) })
    }
    fn message(&self, caller: &str, message: Delivery) -> anyhow::Result<String> {
        Ok(json!({"caller":caller,"to":message.to,"text":message.text,"kind":message.kind,"reply":message.reply_to}).to_string())
    }
    fn cancel(&self, _: &str, _: &str) -> anyhow::Result<()> {
        Ok(())
    }
    fn status(&self, _: &str, _: &str) -> anyhow::Result<Vec<ChildStatus>> {
        Ok(statuses())
    }
    fn can_spawn(&self, _: &str) -> (bool, String) {
        (true, String::new())
    }
    fn archetypes(&self, _: &str) -> Vec<String> {
        vec!["worker".into()]
    }
}
struct Board;
impl Boards for Board {
    fn create<'a>(
        &'a self,
        caller: &'a str,
        name: &'a str,
        members: Vec<String>,
        _: CancellationToken,
    ) -> ServiceFuture<'a, String> {
        Box::pin(
            async move { Ok(json!({"caller":caller,"name":name,"members":members}).to_string()) },
        )
    }
    fn list<'a>(&'a self, caller: &'a str, _: CancellationToken) -> ServiceFuture<'a, String> {
        Box::pin(async move { Ok(format!("boards for {caller}")) })
    }
    fn read<'a>(
        &'a self,
        caller: &'a str,
        channel: &'a str,
        from: i64,
        limit: usize,
        _: CancellationToken,
    ) -> ServiceFuture<'a, String> {
        Box::pin(async move { Ok(format!("{caller}/{channel} from {from} limit {limit}")) })
    }
    fn message<'a>(
        &'a self,
        caller: &'a str,
        message: Delivery,
        _: CancellationToken,
    ) -> ServiceFuture<'a, String> {
        Box::pin(async move {
            Ok(json!({"caller":caller,"channel":message.channel,"to":message.to,"text":message.text,"kind":message.kind,"reply":message.reply_to}).to_string())
        })
    }
}
fn subject_value(subject: policy::Subject) -> Value {
    let kind = match subject.kind {
        policy::Kind::Text => 0,
        policy::Kind::Command => 1,
        policy::Kind::Path => 2,
        policy::Kind::Url => 3,
        policy::Kind::Id => 4,
    };
    json!({"Kind":kind,"Values":subject.values})
}
fn normalize(mut value: Value) -> Value {
    if let Some(object) = value.as_object_mut() {
        for key in ["to", "reply", "members"] {
            if object.get(key).is_some_and(Value::is_null) {
                object.insert(key.into(), json!([]));
            }
        }
    }
    value
}
#[tokio::test]
async fn original_go_tool_journeys_preserve_validation_output_and_policy_subjects() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-builtin-tools.json")).unwrap();
    assert_eq!(cases.len(), 129);
    for (index, case) in cases.iter().enumerate() {
        let dir = tempfile::tempdir().unwrap();
        for (name, body) in [
            ("text", &b"one\r\ntwo\n\xce\xbb\xe9\x9b\xaa\n"[..]),
            ("empty", &b""[..]),
            ("png", &b"\x89PNG\r\n\x1a\n\0"[..]),
            ("a/x.go", &b"Hello\nhello\n"[..]),
            ("a/notes.md", &b"Hello\n"[..]),
            (".hidden", &b"Hello\n"[..]),
        ] {
            let path = dir.path().join(name);
            fs::create_dir_all(path.parent().unwrap()).unwrap();
            fs::write(path, body).unwrap();
        }
        let env = Env {
            filesystem: stavlos::toolfs::Filesystem {
                directory: dir.path().into(),
                ..Default::default()
            },
            agent: "agent".into(),
            skills: BTreeMap::from([(
                "fixture".into(),
                Skill {
                    name: "Fixture".into(),
                    directory: "/skills/fixture".into(),
                    body: "Full instructions λ".into(),
                    ..Default::default()
                },
            )]),
            orchestrator: Some(Arc::new(Orch)),
            boards: Some(Arc::new(Board)),
            todo: Some(Arc::new(Todo)),
            sheets: Some(Arc::new(Sheet)),
            ask: Some(Arc::new(Ask)),
            ripgrep: Some(PathBuf::new()),
            ..Default::default()
        };
        let input = raw(case["input"].as_str().unwrap());
        let name = case["tool"].as_str().unwrap();
        let result = tools::run(name, Some(&input), &env, CancellationToken::new()).await;
        assert_eq!(
            result.is_error,
            case["error"].as_bool().unwrap(),
            "case {index} {name}: {result:?}"
        );
        let actual = result.output.replace(dir.path().to_str().unwrap(), "$ROOT");
        let expected = case["output"].as_str().unwrap();
        if let (Ok(actual), Ok(expected)) = (
            serde_json::from_str::<Value>(&actual),
            serde_json::from_str::<Value>(expected),
        ) {
            assert_eq!(
                normalize(actual),
                normalize(expected),
                "case {index} {name}"
            );
        } else {
            assert_eq!(actual, expected, "case {index} {name}");
        }
        let subject = tools::subject(name, Some(&input));
        let scoped = tools::policy_subject(name, Some(&input), subject.clone());
        assert_eq!(
            subject_value(subject),
            case["subject"],
            "case {index} {name}"
        );
        assert_eq!(subject_value(scoped), case["policy"], "case {index} {name}");
    }
}
#[test]
fn original_go_readability_pass_and_wait_wrapper_remain_exact() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-web-markdown.json")).unwrap();
    assert_eq!(cases.len(), 63);
    for (index, case) in cases.iter().enumerate() {
        let base = case["base"]
            .as_str()
            .filter(|s| !s.is_empty())
            .map(|s| reqwest::Url::parse(s).unwrap());
        assert_eq!(
            tools::markdown::html_to_markdown(
                case["html"].as_str().unwrap().as_bytes(),
                base.as_ref()
            ),
            case["markdown"].as_str().unwrap(),
            "HTML case {index}"
        );
    }
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-until-changed.json")).unwrap();
    for c in cases {
        assert_eq!(
            tools::until_changed(c["command"].as_str().unwrap(), true),
            c["wrapped"].as_str().unwrap()
        );
    }
}
#[test]
fn original_finite_operator_requests_keep_their_scope() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-operator-requests.json")).unwrap();
    assert_eq!(cases.len(), 36);
    for c in cases {
        let result = tools::operator::message(c["input"].as_str().unwrap());
        if c["error"].as_str().unwrap().is_empty() {
            assert_eq!(
                serde_json::from_slice::<Value>(&result.unwrap()).unwrap(),
                c["request"]
            );
        } else {
            assert!(result.is_err(), "{}", c["input"]);
        }
    }
    assert!(tools::operator::message("[]").is_err());
    assert!(tools::operator::message("{}{}").is_err());
    assert!(tools::operator::message(&" ".repeat(4097)).is_err());
}
#[test]
fn advertised_tools_and_role_action_restrictions_keep_the_current_contract() {
    let names = tools::definitions()
        .iter()
        .map(|d| d.name.as_str())
        .collect::<Vec<_>>();
    assert_eq!(
        names,
        [
            "agent", "ask", "channel", "glob", "grep", "message", "operator", "patch", "read",
            "sheet", "shell", "skill", "todo", "web"
        ]
    );
    for (name, actions) in [
        ("agent", json!(["status"])),
        ("channel", json!(["list", "read"])),
    ] {
        let def = tools::definition(name, false).unwrap();
        let schema: Value = serde_json::from_str(def.schema.unwrap().get()).unwrap();
        assert_eq!(schema["properties"]["action"]["enum"], actions);
    }
    let def = tools::definition("message", true).unwrap();
    let schema: Value = serde_json::from_str(def.schema.unwrap().get()).unwrap();
    assert!(schema["properties"].get("kind").is_none());
    assert!(schema["properties"].get("expect_response").is_some());
}
#[derive(Default)]
struct JobRuntime {
    jobs: Mutex<Vec<stavlos::proc::Job>>,
}
impl Jobs for JobRuntime {
    fn adopt(&self, _: &str, job: stavlos::proc::Job, _: Duration) -> anyhow::Result<String> {
        self.jobs.lock().unwrap().push(job);
        Ok("m1".into())
    }
    fn stop(&self, _: &str) -> anyhow::Result<()> {
        self.jobs.lock().unwrap()[0].kill();
        Ok(())
    }
    fn has(&self, id: &str) -> bool {
        id == "m1" && !self.jobs.lock().unwrap().is_empty()
    }
}
#[tokio::test]
async fn shell_streams_inline_transfers_slow_jobs_and_kills_whole_groups_on_cancel() {
    let dir = tempfile::tempdir().unwrap();
    let partial = Arc::new(Mutex::new(Vec::new()));
    let copy = partial.clone();
    let mut env = Env {
        filesystem: stavlos::toolfs::Filesystem {
            directory: dir.path().into(),
            ..Default::default()
        },
        partial: Some(Arc::new(move |bytes| {
            copy.lock().unwrap().extend_from_slice(bytes)
        })),
        ..Default::default()
    };
    let r = tools::run(
        "shell",
        Some(&raw(r#"{"command":"printf 'hi\\n'; exit 3"}"#)),
        &env,
        CancellationToken::new(),
    )
    .await;
    assert!(r.is_error);
    assert_eq!(r.output, "hi\n\n[exit status 3]");
    assert_eq!(&*partial.lock().unwrap(), b"hi\n");
    let cancel = CancellationToken::new();
    let c = cancel.clone();
    let start = std::time::Instant::now();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(100)).await;
        c.cancel();
    });
    let r = tools::run(
        "shell",
        Some(&raw(
            r#"{"command":"printf 'start\\n'; sleep 30; printf never"}"#,
        )),
        &env,
        cancel,
    )
    .await;
    assert!(r.is_error && r.output.contains("start") && !r.output.contains("never"));
    assert!(start.elapsed() < Duration::from_secs(4));
    let jobs = Arc::new(JobRuntime::default());
    env.jobs = Some(jobs.clone());
    partial.lock().unwrap().clear();
    let r = tools::run(
        "shell",
        Some(&raw(
            r#"{"command":"echo background; sleep 30","background":true}"#,
        )),
        &env,
        CancellationToken::new(),
    )
    .await;
    assert!(!r.is_error && r.output.contains("started job m1"));
    assert!(partial.lock().unwrap().is_empty());
    let job = jobs.jobs.lock().unwrap()[0].clone();
    assert!(!job.is_done());
    let r = tools::run(
        "shell",
        Some(&raw(r#"{"action":"kill","id":"m1"}"#)),
        &env,
        CancellationToken::new(),
    )
    .await;
    assert_eq!(r, Output::ok("stopped job m1"));
    assert!(!job.wait_async().await.success());
    let r = tools::run(
        "shell",
        Some(&raw(
            r#"{"command":"echo before; sleep 2; echo after","wait":1}"#,
        )),
        &env,
        CancellationToken::new(),
    )
    .await;
    assert!(!r.is_error && r.output.contains("continuing as job m1"));
    let job = jobs.jobs.lock().unwrap().last().unwrap().clone();
    assert!(job.wait_async().await.success());
    assert!(job.output().contains("after"));
}
#[tokio::test]
async fn overflow_keeps_complete_private_output_and_portable_search_agrees_with_ripgrep() {
    use std::os::unix::fs::PermissionsExt as _;
    let dir = tempfile::tempdir().unwrap();
    let env = Env {
        max_output: 10,
        overflow: dir.path().join("overflow"),
        ..Default::default()
    };
    let output = "λ雪🙂 output ".repeat(20);
    let clipped = env.clip(&output);
    assert!(clipped.contains("the whole output"));
    let files = fs::read_dir(&env.overflow)
        .unwrap()
        .map(|e| e.unwrap().path())
        .collect::<Vec<_>>();
    assert_eq!(files.len(), 1);
    assert_eq!(fs::read_to_string(&files[0]).unwrap(), output);
    assert_eq!(
        fs::metadata(&files[0]).unwrap().permissions().mode() & 0o777,
        0o600
    );
    let mut env = Env {
        filesystem: stavlos::toolfs::Filesystem {
            directory: dir.path().into(),
            roots: vec![dir.path().into()],
            ..Default::default()
        },
        ..Default::default()
    };
    for (name, contents) in [
        ("a/x.go", "package a\nfunc Hello() {}\n"),
        ("a/b/y.go", "hello world\n"),
        ("a/notes.md", "Hello in markdown\n"),
        (".hidden/z", "Hello\n"),
        ("bin.dat", "Hello\0\x01"),
    ] {
        let p = dir.path().join(name);
        fs::create_dir_all(p.parent().unwrap()).unwrap();
        fs::write(p, contents).unwrap();
    }
    for backend in [Some(PathBuf::new()), None] {
        env.ripgrep = backend;
        for (name, input, expected) in [
            (
                "grep",
                r#"{"pattern":"hello","ignore_case":true,"glob":"*.go"}"#,
                "a/b/y.go:1:hello world\na/x.go:2:func Hello() {}",
            ),
            ("glob", r#"{"pattern":"*.go"}"#, "a/b/y.go\na/x.go"),
            ("glob", r#"{"pattern":"a/*.go"}"#, "a/x.go"),
            ("grep", r#"{"pattern":"--pre=sh"}"#, "no matches"),
        ] {
            let r = tools::run(name, Some(&raw(input)), &env, CancellationToken::new()).await;
            assert!(!r.is_error, "{r:?}");
            let mut lines = r.output.lines().collect::<Vec<_>>();
            lines.sort();
            assert_eq!(lines.join("\n"), expected);
        }
    }
}
