use serde::Deserialize;
use stavlos::{
    config::snapshot::{Snapshot, Source},
    instructions::{self, File},
};
use std::{
    collections::BTreeMap,
    fs,
    os::unix::fs::symlink,
    path::{Path, PathBuf},
};
fn write(root: &Path, relative: &str, bytes: &[u8]) {
    let path = root.join(relative);
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(path, bytes).unwrap();
}
#[derive(Deserialize)]
struct ExpectedEntry {
    name: String,
    directory: bool,
}
#[derive(Deserialize)]
struct Case {
    subdir: String,
    input: BTreeMap<String, String>,
    files: Vec<String>,
    hash: String,
    chain: Vec<File>,
    nested: Vec<String>,
    list: BTreeMap<String, Vec<ExpectedEntry>>,
}
#[test]
fn original_go_project_trust_hashes_chains_and_directory_views_match() {
    let cases: Vec<Case> =
        serde_json::from_str(include_str!("data/go-trust-snapshots.json")).unwrap();
    for case in cases {
        let owned = tempfile::tempdir().unwrap();
        let root = owned.path().join("repository");
        fs::create_dir_all(root.join(".git")).unwrap();
        for (path, bytes) in &case.input {
            write(&root, path, bytes.as_bytes());
        }
        let directory = root.join(&case.subdir);
        let snapshot = Snapshot::take(&directory, owned.path()).unwrap();
        assert_eq!(snapshot.files, case.files);
        assert_eq!(snapshot.hash, case.hash);
        let chain: Vec<_> = snapshot
            .chain
            .iter()
            .map(|f| File {
                path: f.path.strip_prefix(&root).unwrap().to_owned(),
                text: f.text.clone(),
            })
            .collect();
        assert_eq!(chain, case.chain);
        assert_eq!(
            instructions::nested(&directory)
                .iter()
                .map(|p| p
                    .strip_prefix(&root)
                    .unwrap()
                    .to_string_lossy()
                    .into_owned())
                .collect::<Vec<_>>(),
            case.nested
        );
        for (path, entries) in case.list {
            assert_eq!(
                snapshot
                    .read_dir(&directory.join(path))
                    .unwrap()
                    .iter()
                    .map(|e| (&e.name, e.directory))
                    .collect::<Vec<_>>(),
                entries
                    .iter()
                    .map(|e| (&e.name, e.directory))
                    .collect::<Vec<_>>()
            );
        }
    }
}
#[test]
fn snapshot_keeps_hashed_bytes_and_refuses_aliased_directories_fifos_and_large_files() {
    let owned = tempfile::tempdir().unwrap();
    let root = owned.path().join("repository");
    fs::create_dir_all(&root).unwrap();
    write(&root, ".stavlos/agents/reviewer.md", b"hashed role");
    write(&root, "AGENTS.md", b"hashed instructions");
    let snapshot = Snapshot::take(&root, owned.path()).unwrap();
    write(&root, ".stavlos/agents/reviewer.md", b"changed role");
    write(&root, "AGENTS.md", b"changed instructions");
    assert_eq!(
        snapshot
            .read_file(&root.join(".stavlos/agents/reviewer.md"))
            .unwrap(),
        b"hashed role"
    );
    assert_eq!(snapshot.chain[0].text, "hashed instructions");
    assert_ne!(
        snapshot.hash,
        Snapshot::take(&root, owned.path()).unwrap().hash
    );
    assert!(snapshot.read_file(&root.join("not-hashed")).is_err());
    symlink(owned.path(), root.join(".agents")).unwrap();
    assert!(
        Snapshot::take(&root, owned.path())
            .unwrap_err()
            .to_string()
            .contains("real directory")
    );
    fs::remove_file(root.join(".agents")).unwrap();
    let fifo = root.join(".stavlos/fifo");
    let name = std::ffi::CString::new(fifo.as_os_str().as_encoded_bytes()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(name.as_ptr(), 0o600) }, 0);
    assert!(
        Snapshot::take(&root, owned.path())
            .unwrap_err()
            .to_string()
            .contains("not a regular file")
    );
    fs::remove_file(fifo).unwrap();
    let oversized = fs::File::create(root.join(".stavlos/oversized")).unwrap();
    oversized.set_len((16 << 20) + 1).unwrap();
    assert!(
        Snapshot::take(&root, owned.path())
            .unwrap_err()
            .to_string()
            .contains("larger than")
    );
}
#[test]
fn instructions_scope_precedence_skips_and_unicode_byte_budget_are_preserved() {
    let owned = tempfile::tempdir().unwrap();
    let root = owned.path().join("repository");
    fs::create_dir_all(root.join(".git")).unwrap();
    write(&root, "AGENTS.md", b"root");
    write(&root, "a/CLAUDE.md", b"a");
    write(&root, "a/b/AGENTS.md", b"b");
    write(&root, "a/b/file", b"");
    write(&root, "a/b/CLAUDE.md", b"ignored");
    write(&root, "node_modules/x/AGENTS.md", b"ignored");
    write(&root, ".cache/AGENTS.md", b"ignored");
    assert_eq!(
        instructions::chain(&root.join("a/b"), owned.path())
            .iter()
            .map(|f| f.text.as_str())
            .collect::<Vec<_>>(),
        ["root", "a", "b"]
    );
    assert_eq!(
        instructions::between(&root, &root.join("a/b/file"))
            .iter()
            .map(|f| f.text.as_str())
            .collect::<Vec<_>>(),
        ["a", "b"]
    );
    assert!(instructions::between(&root, &root.join("node_modules/x/file")).is_empty());
    assert!(instructions::between(&root, owned.path()).is_empty());
    let files = [
        File {
            path: PathBuf::from("/r/AGENTS.md"),
            text: "αβγ".into(),
        },
        File {
            path: PathBuf::from("/r/a/AGENTS.md"),
            text: "later".into(),
        },
    ];
    let rendered = instructions::section(&files, 5, owned.path());
    assert!(rendered.contains("αβ\n\n[cut here"));
    assert!(!rendered.contains("\nlater\n"));
    assert!(instructions::section(&[], 32, owned.path()).is_empty());
}
