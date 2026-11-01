use serde_json::json;
use stavlos::{
    agent::{
        journal::{Core, Journal},
        sheets::{self, SheetService},
    },
    config,
    event::{self, Event},
    policy::Subject,
    protocol,
    tools::Sheets,
    wire::Time,
};
use std::{
    fs,
    os::unix::fs::{PermissionsExt, symlink},
    sync::{Arc, Mutex},
};
use tokio_util::sync::CancellationToken;
#[derive(Default)]
struct Memory {
    events: Mutex<Vec<Event>>,
    fail: Mutex<bool>,
}
impl Journal for Memory {
    fn append(&self, mut events: Vec<Event>, _: &CancellationToken) -> anyhow::Result<Vec<Event>> {
        anyhow::ensure!(!*self.fail.lock().unwrap(), "fixture write failure");
        let mut persisted = self.events.lock().unwrap();
        for event in &mut events {
            event.seq = persisted.len() as i64 + 1;
            event.numbered_time();
            persisted.push(event.clone());
        }
        Ok(events)
    }
    fn stream(&self, _: protocol::StreamNotification) {}
}
fn setup(root: &std::path::Path) -> (SheetService, Arc<Memory>) {
    let journal = Arc::new(Memory::default());
    let core = Arc::new(Core::new(
        "channel".into(),
        root.to_string_lossy().into_owned(),
        Time::now(),
        Arc::new(config::Effective::default()),
        String::new(),
        String::new(),
        journal.clone(),
    ));
    core.commit(
        vec![
            core.event("", event::AGENT_SPAWNED, json!({"id":"a1","name":"main"}))
                .unwrap(),
        ],
        &CancellationToken::new(),
    )
    .unwrap();
    (
        SheetService {
            core,
            agent: "a1".into(),
            directory: root.join("sheets/channel"),
        },
        journal,
    )
}
#[test]
fn sheets_are_private_bounded_files_with_replayable_metadata_and_monotonic_ids() {
    let temp = tempfile::tempdir().unwrap();
    let (service, journal) = setup(temp.path());
    let first = service.write("", "Findings", "<p>first</p>").unwrap();
    assert_eq!(first.id, "s1");
    assert_eq!(first.author, "main");
    let second = service.write("", "Second", "<p>second</p>").unwrap();
    service.delete(&second.id).unwrap();
    let third = service.write("", "Third", "<p>third</p>").unwrap();
    assert_eq!(third.id, "s3");
    assert_eq!(
        fs::metadata(&first.path).unwrap().permissions().mode() & 0o777,
        0o600
    );
    assert_eq!(
        fs::metadata(&service.directory)
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o700
    );
    service.write(&first.id, "", "<p>changed</p>").unwrap();
    assert_eq!(service.list()[0].title, "Findings");
    let (info, body) = service.get(&first.id).unwrap();
    assert_eq!(body, b"<p>changed</p>");
    assert_eq!(info.size, body.len() as i64);
    let mut state = stavlos::agent::state::ChannelState::new("", "");
    for event in journal.events.lock().unwrap().iter() {
        state.apply(event);
        assert!(
            !event
                .payload
                .as_ref()
                .map_or("", |payload| payload.get())
                .contains("<p>")
        );
    }
    assert_eq!(
        state
            .sorted_sheets()
            .iter()
            .map(|sheet| sheet.id.as_str())
            .collect::<Vec<_>>(),
        ["s1", "s3"]
    );
    assert_eq!(state.sheets["s1"].hash, info.hash);
    assert!(service.write("../escape", "", "x").is_err());
    assert!(service.get("../escape").is_err());
}
#[test]
fn irreversible_sheet_writes_and_deletes_follow_disk_when_the_journal_fails() {
    let temp = tempfile::tempdir().unwrap();
    let (service, journal) = setup(temp.path());
    let first = service.write("", "First", "old").unwrap();
    *journal.fail.lock().unwrap() = true;
    assert!(service.write(&first.id, "Changed", "new").is_err());
    assert_eq!(service.get(&first.id).unwrap().1, b"new");
    assert_eq!(service.list()[0].title, "Changed");
    assert!(service.delete(&first.id).is_err());
    assert!(service.list().is_empty());
    assert!(!std::path::Path::new(&first.path).exists());
    *journal.fail.lock().unwrap() = false;
    assert_eq!(service.write("", "Next", "next").unwrap().id, "s2");
}
#[test]
fn sheet_reads_refuse_escaping_symlinks_nonregular_files_and_oversized_pages() {
    let temp = tempfile::tempdir().unwrap();
    let (service, _) = setup(temp.path());
    let first = service.write("", "First", "owned").unwrap();
    let outside = temp.path().join("secret");
    fs::write(&outside, "owned fixture secret").unwrap();
    fs::remove_file(&first.path).unwrap();
    symlink(&outside, &first.path).unwrap();
    assert!(service.get(&first.id).is_err());
    fs::remove_file(&first.path).unwrap();
    fs::create_dir(&first.path).unwrap();
    assert!(service.get(&first.id).is_err());
    fs::remove_dir(&first.path).unwrap();
    let name = std::ffi::CString::new(first.path.as_str()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
    let began = std::time::Instant::now();
    assert!(service.get(&first.id).is_err());
    assert!(began.elapsed() < std::time::Duration::from_secs(1));
    fs::remove_file(&first.path).unwrap();
    fs::write(&first.path, vec![b'x'; sheets::MAX_SIZE + 1]).unwrap();
    assert!(service.get(&first.id).is_err());
}
#[test]
fn patch_guards_refuse_unknown_sheets_restore_oversize_and_record_edits_and_deletions() {
    let temp = tempfile::tempdir().unwrap();
    let (service, _) = setup(temp.path());
    let first = service.write("", "First", "old").unwrap();
    let second = service.write("", "Second", "two").unwrap();
    let unknown = Subject::paths([service
        .directory
        .join("s999.html")
        .to_string_lossy()
        .into_owned()]);
    assert!(
        service
            .guard(&unknown, temp.path())
            .err()
            .unwrap()
            .to_string()
            .contains("is no sheet")
    );
    let paths = Subject::paths([first.path.clone(), second.path.clone()]);
    let guard = service.guard(&paths, temp.path()).unwrap().unwrap();
    fs::write(&first.path, vec![b'x'; sheets::MAX_SIZE + 1]).unwrap();
    fs::write(&second.path, vec![b'x'; sheets::MAX_SIZE + 1]).unwrap();
    let note = guard.finish().unwrap();
    assert!(note.contains("s1.html"));
    assert!(note.contains("s2.html"));
    assert_eq!(service.get(&first.id).unwrap().1, b"old");
    assert_eq!(service.get(&second.id).unwrap().1, b"two");
    fs::write(&first.path, "changed").unwrap();
    fs::remove_file(&second.path).unwrap();
    service.patched(&paths, temp.path());
    assert_eq!(service.list().len(), 1);
    assert_eq!(service.list()[0].size, 7);
    assert_eq!(service.list()[0].title, "First");
}
#[test]
fn sheet_limits_count_unicode_characters_bytes_and_live_pages_separately() {
    let temp = tempfile::tempdir().unwrap();
    let (service, _) = setup(temp.path());
    assert!(service.write("", &"λ".repeat(201), "x").is_err());
    assert!(
        service
            .write("", "", &"x".repeat(sheets::MAX_SIZE + 1))
            .is_err()
    );
    service.write("", &"λ".repeat(200), "x").unwrap();
    for _ in 1..50 {
        service.write("", "", "x").unwrap();
    }
    assert!(service.write("", "", "x").is_err());
    service.delete("s1").unwrap();
    assert_eq!(service.write("", "", "x").unwrap().id, "s51");
}
