use serde_json::{Value, json};
use stavlos::{
    agent::{
        control::{self, InstructionCache},
        journal::{Core, Journal},
    },
    config,
    event::{self, Event},
    paths::Paths,
    policy::Subject,
    protocol,
    wire::Time,
};
use std::{
    fs,
    os::unix::fs::{PermissionsExt, symlink},
    sync::{Arc, Mutex},
};
use tokio_util::sync::CancellationToken;
#[derive(Default)]
struct Memory(Mutex<Vec<Event>>);
impl Journal for Memory {
    fn append(&self, mut events: Vec<Event>, _: &CancellationToken) -> anyhow::Result<Vec<Event>> {
        let mut all = self.0.lock().unwrap();
        for event in &mut events {
            event.seq = all.len() as i64 + 1;
            event.numbered_time();
            all.push(event.clone());
        }
        Ok(events)
    }
    fn stream(&self, _: protocol::StreamNotification) {}
}
#[test]
fn original_go_control_file_hashes_preserve_unix_modes_and_content() {
    let temp = tempfile::tempdir().unwrap();
    let rows: Vec<Value> =
        serde_json::from_str(include_str!("data/go-control-file-hashes.json")).unwrap();
    assert_eq!(rows.len(), 8);
    for row in rows {
        let path = temp.path().join("file");
        if path.exists() {
            fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
        }
        fs::write(&path, row["body"].as_str().unwrap()).unwrap();
        fs::set_permissions(
            &path,
            fs::Permissions::from_mode(row["mode"].as_u64().unwrap() as u32),
        )
        .unwrap();
        assert_eq!(
            control::hash_path(&path),
            row["hash"].as_str().unwrap(),
            "{row}"
        );
    }
    let link = temp.path().join("link");
    symlink("file", &link).unwrap();
    let before = control::hash_path(&link);
    fs::write(temp.path().join("file"), "changed").unwrap();
    assert_ne!(control::hash_path(&link), before);
    let fifo = temp.path().join("fifo");
    let name = std::ffi::CString::new(fifo.as_os_str().as_encoded_bytes()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
    let began = std::time::Instant::now();
    assert!(!control::hash_path(&fifo).is_empty());
    assert!(began.elapsed() < std::time::Duration::from_secs(1));
}
#[test]
fn control_stamps_notice_new_content_modes_and_nested_instruction_changes() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path().join("work");
    fs::create_dir_all(root.join(".git/hooks")).unwrap();
    fs::write(root.join("AGENTS.md"), "same").unwrap();
    fs::write(root.join(".git/config"), "before").unwrap();
    let paths = Paths {
        config: temp.path().join("config"),
        data: temp.path().join("data"),
        cache: temp.path().join("cache"),
        socket: temp.path().join("socket"),
        shared_skills: temp.path().join("skills"),
    };
    let cfg = config::Effective::default();
    let stamp = control::stamp_control(&cfg, std::slice::from_ref(&root), &paths);
    assert!(control::control_note(&stamp).is_empty());
    fs::write(root.join(".git/hooks/pre-commit"), "owned fixture").unwrap();
    fs::write(root.join("AGENTS.md"), "changed").unwrap();
    let note = control::control_note(&stamp);
    assert!(note.contains("AGENTS.md"));
    assert!(note.contains(".git/hooks"));
    assert!(note.contains("human should review"));
    let stamp = control::hash_path(&root.join(".git/hooks"));
    fs::rename(
        root.join(".git/hooks/pre-commit"),
        root.join(".git/hooks/post-commit"),
    )
    .unwrap();
    assert_ne!(control::hash_path(&root.join(".git/hooks")), stamp);
}
#[test]
fn trust_stamp_covers_roles_commands_config_and_missing_files() {
    let temp = tempfile::tempdir().unwrap();
    fs::create_dir_all(temp.path().join(".stavlos/roles")).unwrap();
    let mut cfg = config::Effective::default();
    cfg.dir = temp.path().to_owned();
    cfg.trust_files = vec![
        "AGENTS.md".into(),
        ".stavlos/roles/worker.md".into(),
        ".stavlos/commands/check.md".into(),
    ];
    let before = control::trust_stamp(&cfg);
    assert!(before.contains("gone"));
    fs::write(
        temp.path().join(".stavlos/roles/worker.md"),
        "new trusted role",
    )
    .unwrap();
    let after = control::trust_stamp(&cfg);
    assert_ne!(before, after);
    assert_eq!(control::trust_stamp(&cfg), after);
    fs::remove_file(temp.path().join(".stavlos/roles/worker.md")).unwrap();
    assert_eq!(control::trust_stamp(&cfg), before);
}
#[test]
fn nested_instructions_attach_once_since_compaction_and_new_untrusted_files_are_withheld() {
    let temp = tempfile::tempdir().unwrap();
    fs::create_dir_all(temp.path().join("sub/deeper")).unwrap();
    fs::write(
        temp.path().join("sub/AGENTS.md"),
        "Trusted nested instructions.",
    )
    .unwrap();
    fs::write(
        temp.path().join("sub/deeper/AGENTS.md"),
        "New untrusted text.",
    )
    .unwrap();
    let mut cfg = config::Effective::default();
    cfg.dir = temp.path().to_owned();
    cfg.project_trusted = true;
    cfg.instruction_files = vec![temp.path().join("sub/AGENTS.md")];
    let core = Core::new(
        "channel".into(),
        temp.path().to_string_lossy().into_owned(),
        Time::now(),
        Arc::new(cfg.clone()),
        String::new(),
        String::new(),
        Arc::new(Memory::default()),
    );
    core.commit(
        vec![
            core.event("", event::AGENT_SPAWNED, json!({"id":"a1"}))
                .unwrap(),
        ],
        &CancellationToken::new(),
    )
    .unwrap();
    let cache = InstructionCache::default();
    let subject = Subject::paths(["sub/deeper/file.rs".into()]);
    let attachment = cache.for_subject(&core, "a1", &subject, &cfg, temp.path(), temp.path());
    assert!(attachment.note.contains("Trusted nested instructions"));
    assert!(!attachment.note.contains("New untrusted text"));
    assert!(attachment.project_changed);
    assert_eq!(attachment.carried.len(), 1);
    let attachment = cache.for_subject(&core, "a1", &subject, &cfg, temp.path(), temp.path());
    assert!(attachment.note.is_empty());
    assert!(attachment.carried.is_empty());
    assert!(attachment.project_changed);
    cache.clear();
    assert!(
        !cache
            .for_subject(&core, "a1", &subject, &cfg, temp.path(), temp.path())
            .note
            .is_empty()
    );
    cfg.project_trusted = false;
    cache.clear();
    assert!(
        cache
            .for_subject(&core, "a1", &subject, &cfg, temp.path(), temp.path())
            .note
            .is_empty()
    );
}
