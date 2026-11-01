use base64::{Engine as _, engine::general_purpose::STANDARD};
use serde_json::{Value, json};
use stavlos::{
    auth::{Credential, Store},
    event::Event,
    eventlog::Log,
    paths::Paths,
    protocol, statefile,
    textsafe::{self, Mode},
};
use std::{
    collections::BTreeMap,
    os::unix::fs::PermissionsExt,
    sync::{Arc, Mutex},
    time::{Duration, SystemTime},
};
#[test]
fn every_original_terminal_control_case_preserves_clean_visible_and_frame_text() {
    let rows: Vec<Value> =
        serde_json::from_str(include_str!("data/go-terminal-text.json")).unwrap();
    assert_eq!(rows.len(), 271);
    for row in rows {
        let raw = STANDARD.decode(row["input"].as_str().unwrap()).unwrap();
        for (mode, key) in [
            (Mode::Clean, "clean"),
            (Mode::Visible, "visible"),
            (Mode::Frame, "frame"),
        ] {
            assert_eq!(
                textsafe::scrub(&raw, mode),
                row[key].as_str().unwrap(),
                "input {} mode {key}",
                row["input"]
            );
        }
    }
}
#[test]
fn original_mentions_and_recipient_aliases_remain_wire_compatible() {
    let data: Value = serde_json::from_str(include_str!("data/go-protocol.json")).unwrap();
    for row in data["mentions"].as_array().unwrap() {
        let (names, message) = protocol::addressees(row["input"].as_str().unwrap());
        assert_eq!(
            names,
            row["names"]
                .as_array()
                .map(|a| a
                    .iter()
                    .map(|v| v.as_str().unwrap().to_string())
                    .collect::<Vec<_>>())
                .unwrap_or_default()
        );
        assert_eq!(message, row["message"]);
    }
    for row in data["recipients"].as_array().unwrap() {
        let result = protocol::recipients(&row["input"]);
        assert_eq!(result.is_ok(), row["valid"] == true);
        if let Ok(value) = result {
            assert_eq!(json!(value), row["output"]);
        }
    }
}
#[test]
fn filesystem_locations_respect_explicit_overrides_without_reading_credentials() {
    let env = BTreeMap::from([
        ("HOME".into(), "/owned/user".into()),
        ("XDG_RUNTIME_DIR".into(), "/owned/runtime".into()),
    ]);
    let p = Paths::from_values(&env);
    assert_eq!(
        p.config,
        std::path::Path::new("/owned/user/.config/stavlos")
    );
    assert_eq!(
        p.socket,
        std::path::Path::new("/owned/runtime/stavlosd.sock")
    );
    assert_eq!(
        p.auth_file(),
        std::path::Path::new("/owned/user/.local/share/stavlos/auth.json")
    );
    let mut env = env;
    env.insert("STAVLOS_SOCKET".into(), "/owned/explicit.sock".into());
    env.insert("STAVLOS_SKILLS_DIR".into(), "/owned/library".into());
    let p = Paths::from_values(&env);
    assert_eq!(p.socket, std::path::Path::new("/owned/explicit.sock"));
    assert_eq!(p.shared_skills, std::path::Path::new("/owned/library"));
}
#[test]
fn state_replacement_preserves_existing_modes_and_failed_replacement_leaves_prior_data() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("config.json");
    statefile::write_atomic(&path, b"old", 0o640, false).unwrap();
    statefile::write_atomic(&path, b"new", 0o600, true).unwrap();
    assert_eq!(std::fs::read(&path).unwrap(), b"new");
    assert_eq!(path.metadata().unwrap().permissions().mode() & 0o777, 0o640);
    let blocked = dir.path().join("directory");
    std::fs::create_dir(&blocked).unwrap();
    assert!(statefile::write_atomic(&blocked, b"replacement", 0o600, false).is_err());
    assert!(blocked.is_dir());
    assert_eq!(std::fs::read(&path).unwrap(), b"new");
    assert_eq!(std::fs::read_dir(dir.path()).unwrap().count(), 2);
}
#[test]
fn credential_storage_retains_formats_observes_external_changes_and_keeps_secrets_out_of_debug() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("auth.json");
    std::fs::write(&path,r#"{"openai":{"type":"oauth","added":"2025-01-01T00:00:00Z","access":"fixture-access","refresh":"fixture-refresh","expires":1735689600000,"account_id":"fixture-account","email":"fixture@example.invalid"},"zai":{"type":"apikey","key":"fixture-key"}}"#).unwrap();
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
    let store = Store::open(path.clone());
    assert_eq!(store.get(" openai/// ").unwrap().secret(), "fixture-access");
    assert_eq!(store.get("zai").unwrap().secret(), "fixture-key");
    assert_eq!(path.metadata().unwrap().permissions().mode() & 0o777, 0o600);
    assert_eq!(store.providers(), vec!["openai", "zai"]);
    let credential = store.get("openai").unwrap();
    assert!(!format!("{credential:?}").contains("fixture-access"));
    assert!(!format!("{credential:?}").contains("fixture-refresh"));
    let replacement = Store::open(path.clone());
    replacement
        .set(
            "openai",
            Credential {
                access: "new-external-token-longer".into(),
                ..credential
            },
        )
        .unwrap();
    assert_eq!(
        store.get("openai").unwrap().secret(),
        "new-external-token-longer"
    );
    store.remove("zai").unwrap();
    store.remove("missing").unwrap();
    assert_eq!(store.providers(), vec!["openai"]);
    std::fs::write(&path, b"invalid fixture-secret JSON").unwrap();
    let file = std::fs::File::open(&path).unwrap();
    file.set_times(
        std::fs::FileTimes::new().set_modified(SystemTime::now() + Duration::from_secs(10)),
    )
    .unwrap();
    assert!(
        !store
            .all()
            .err()
            .unwrap()
            .to_string()
            .contains("fixture-secret")
    );
    assert!(store.get("openai").is_none());
}
#[test]
fn original_sqlite_log_preserves_every_event_raw_payload_index_and_usage_projection() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("journal.db");
    std::fs::write(&path, include_bytes!("data/go-journal.sqlite")).unwrap();
    let log = Log::open(path, false, None).unwrap();
    let expected: Value = serde_json::from_str(include_str!("data/go-journal.json")).unwrap();
    for channel in ["alpha", "beta"] {
        let events = log.read(channel, 1, 0).unwrap();
        assert_eq!(serde_json::to_value(events).unwrap(), expected[channel]);
    }
    let channels = log.channels().unwrap();
    assert_eq!(channels.len(), 2);
    assert!(channels.iter().any(|c| c.id == "alpha"
        && c.name == "renamed"
        && c.dir == "/owned/changed"
        && c.title == "雪 first line"
        && c.last_seq == 4));
    assert!(
        channels
            .iter()
            .any(|c| c.id == "beta" && c.archived && c.last_seq == 2)
    );
    assert_eq!(log.trust().unwrap()["/owned/fixture"], "fixture-hash");
    let from = "2025-01-02T02:04:05.1234Z".parse().unwrap();
    let to = "2025-01-02T04:04:05.1234Z".parse().unwrap();
    let usage = log.usage("", "", Some(from), Some(to), 4).unwrap();
    assert_eq!(json!(usage.tokens), expected["usage"]["Tokens"]);
    assert_eq!(
        usage.cost,
        expected["usage"]["Cost"]
            .as_array()
            .unwrap()
            .iter()
            .map(|v| v.as_f64().unwrap())
            .collect::<Vec<_>>()
    );
    assert_eq!(json!(log.cache_usage(from).unwrap()), expected["cache"]);
    log.close().unwrap();
}
#[test]
fn grouped_appends_preserve_global_order_and_failed_batches_do_not_consume_sequences() {
    let dir = tempfile::tempdir().unwrap();
    let committed = Arc::new(Mutex::new(Vec::new()));
    let observed = committed.clone();
    let log = Log::open(
        dir.path().join("journal.db"),
        false,
        Some(Arc::new(move |events| {
            observed
                .lock()
                .unwrap()
                .extend(events.iter().map(|e| e.global))
        })),
    )
    .unwrap();
    log.append(vec![
        Event::new(
            "a",
            "",
            "channel.created",
            json!({"name":"a","dir":"/owned"}),
        )
        .unwrap(),
    ])
    .unwrap();
    let mut threads = Vec::new();
    for _ in 0..8 {
        let log = log.clone();
        threads.push(std::thread::spawn(move || {
            for _ in 0..16 {
                log.append(vec![
                    Event::new("a", "main", "fixture.event", json!({"text":"kept"})).unwrap(),
                ])
                .unwrap();
            }
        }));
    }
    for thread in threads {
        thread.join().unwrap();
    }
    let last = log.last_seq("a").unwrap();
    assert_eq!(last, 129);
    let bad = log.append(vec![
        Event::new("a", "", "fixture.event", Value::Null).unwrap(),
        Event::new(
            "b",
            "",
            "channel.created",
            json!({"name":"a","dir":"/owned"}),
        )
        .unwrap(),
    ]);
    assert!(bad.is_err());
    assert_eq!(log.last_seq("a").unwrap(), last);
    let result = log
        .append(vec![
            Event::new("a", "", "fixture.event", Value::Null).unwrap(),
        ])
        .unwrap();
    assert_eq!(result[0].seq, last + 1);
    let seen = committed.clone();
    log.barrier(move || {
        let values = seen.lock().unwrap();
        assert!(values.windows(2).all(|w| w[0] + 1 == w[1]));
        assert_eq!(values.len(), 130)
    })
    .unwrap();
    log.close().unwrap();
    assert!(log.append(vec![]).is_err());
}
#[test]
fn older_supported_logs_are_backed_up_and_unknown_schema_bytes_remain_unchanged() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("journal.db");
    std::fs::write(&path, include_bytes!("data/go-journal.sqlite")).unwrap();
    let db = rusqlite::Connection::open(&path).unwrap();
    db.execute_batch("DROP TABLE usage; PRAGMA user_version=5")
        .unwrap();
    drop(db);
    let log = Log::open(path.clone(), false, None).unwrap();
    assert!(dir.path().join("journal.db.v5.bak").is_file());
    assert_eq!(log.read("alpha", 1, 0).unwrap().len(), 4);
    assert_eq!(
        log.cache_usage("2025-01-01T00:00:00Z".parse().unwrap())
            .unwrap()["openai"],
        [11, 7]
    );
    log.close().unwrap();
    drop(log);
    let db = rusqlite::Connection::open(&path).unwrap();
    db.pragma_update(None, "user_version", 99).unwrap();
    drop(db);
    let before = std::fs::read(&path).unwrap();
    assert!(Log::open(path.clone(), false, None).is_err());
    assert_eq!(std::fs::read(&path).unwrap(), before);
}
