use base64::{Engine as _, engine::general_purpose::STANDARD};
use serde_json::Value;
use sha2::{Digest as _, Sha256};
use stavlos::{
    clip,
    toolfs::{Filesystem, resolve_path},
    toolpatch::{self, Plan},
};
use std::{
    collections::BTreeMap,
    fs,
    os::unix::fs::{PermissionsExt, symlink},
    path::Path,
};

fn environment(directory: &Path) -> Filesystem {
    Filesystem {
        directory: directory.into(),
        roots: vec![directory.into()],
        judged: BTreeMap::new(),
    }
}
#[test]
fn original_go_patch_grammar_and_hunk_matches_remain_exact() {
    let cases: Vec<Value> =
        serde_json::from_str(include_str!("data/go-tool-patches.json")).unwrap();
    assert_eq!(cases.len(), 122);
    for case in cases {
        let result = toolpatch::parse(case["patch"].as_str().unwrap()).and_then(|ops| {
            let h = ops
                .iter()
                .find(|op| op.kind == "update")
                .map_or(&[][..], |op| op.hunks.as_slice());
            toolpatch::apply_hunks(case["content"].as_str().unwrap().as_bytes(), h)
        });
        let error = case["error"].as_str().unwrap();
        if error.is_empty() {
            assert_eq!(
                result.unwrap(),
                STANDARD.decode(case["output"].as_str().unwrap()).unwrap(),
                "{}",
                case["name"]
            );
        } else {
            assert_eq!(
                format!("{:#}", result.unwrap_err()),
                error,
                "{}",
                case["name"]
            );
        }
    }
}
#[test]
fn original_go_utf8_byte_clipping_is_preserved() {
    let cases: Vec<Value> = serde_json::from_str(include_str!("data/go-tool-clips.json")).unwrap();
    for c in cases {
        let text = c["pattern"]
            .as_str()
            .unwrap()
            .repeat(c["repeat"].as_u64().unwrap() as usize);
        let n = c["n"].as_u64().unwrap() as usize;
        let hash = |text: &str| format!("{:x}", Sha256::digest(text.as_bytes()));
        assert_eq!(hash(clip::head(&text, n)), c["head"].as_str().unwrap());
        assert_eq!(hash(clip::tail(&text, n)), c["tail"].as_str().unwrap());
        assert_eq!(
            hash(&clip::middle(&text, n as i64)),
            c["middle"].as_str().unwrap()
        );
    }
}
#[test]
fn staged_sections_chain_replace_move_and_preserve_executable_modes() {
    let dir = tempfile::tempdir().unwrap();
    let env = environment(dir.path());
    fs::write(dir.path().join("a"), b"one\ntwo\n").unwrap();
    fs::set_permissions(dir.path().join("a"), fs::Permissions::from_mode(0o751)).unwrap();
    let result=toolpatch::run(&env,"*** Begin Patch\n*** Update File: a\n-one\n+uno\n*** Update File: a\n*** Move to: bin/b\n-two\n+dos\n*** Update File: bin/b\n-uno\n+eins\n*** Add File: c\n+old\n*** Delete File: c\n*** Add File: c\n+new\n*** End Patch").unwrap();
    assert!(result.contains("updated a → bin/b"));
    assert!(!dir.path().join("a").exists());
    assert_eq!(fs::read(dir.path().join("bin/b")).unwrap(), b"eins\ndos\n");
    assert_eq!(
        fs::metadata(dir.path().join("bin/b"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o751
    );
    assert_eq!(fs::read(dir.path().join("c")).unwrap(), b"new\n");
    assert_eq!(
        fs::metadata(dir.path().join("bin"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o755
    );
    assert_eq!(fs::read_dir(dir.path().join("bin")).unwrap().count(), 1);
    // A move cannot overwrite another file, including when both names resolve identically.
    for target in ["c", "bin/b"] {
        assert!(
            toolpatch::run(
                &env,
                &format!(
                    "*** Begin Patch\n*** Update File: bin/b\n*** Move to: {target}\n*** End Patch"
                )
            )
            .is_err()
        );
        assert_eq!(fs::read(dir.path().join("bin/b")).unwrap(), b"eins\ndos\n");
    }
}
#[test]
fn invalid_later_section_never_writes_and_failed_commit_rolls_back_before_deletions() {
    let dir = tempfile::tempdir().unwrap();
    let env = environment(dir.path());
    fs::write(dir.path().join("a"), b"one\n").unwrap();
    fs::write(dir.path().join("gone"), b"keep\n").unwrap();
    assert!(toolpatch::run(&env,"*** Begin Patch\n*** Update File: a\n-one\n+changed\n*** Update File: absent\n-x\n+y\n*** End Patch").is_err());
    assert_eq!(fs::read(dir.path().join("a")).unwrap(), b"one\n");
    let ops=toolpatch::parse("*** Begin Patch\n*** Update File: a\n-one\n+changed\n*** Add File: new\n+temporary\n*** Add File: missing/child\n+fail\n*** Delete File: gone\n*** End Patch").unwrap();
    let plan = Plan::stage(&env, &ops).unwrap();
    // Make a later path impossible only after staging: exercises rollback even as root.
    fs::write(dir.path().join("missing"), b"obstacle").unwrap();
    assert!(plan.write().is_err());
    assert_eq!(fs::read(dir.path().join("a")).unwrap(), b"one\n");
    assert_eq!(fs::read(dir.path().join("gone")).unwrap(), b"keep\n");
    assert!(!dir.path().join("new").exists());
    assert!(!fs::read_dir(dir.path()).unwrap().any(|e| {
        e.unwrap()
            .file_name()
            .to_string_lossy()
            .contains("stavlos-")
    }));
}
#[test]
fn judged_paths_refuse_swapped_outside_symlinks_including_parent_and_write_phase() {
    let dir = tempfile::tempdir().unwrap();
    let outside = tempfile::tempdir().unwrap();
    let mut env = environment(dir.path());
    fs::write(outside.path().join("secret"), b"private").unwrap();
    fs::write(dir.path().join("x"), b"ok").unwrap();
    env.judged.insert("x".into(), env.resolve("x"));
    fs::remove_file(dir.path().join("x")).unwrap();
    symlink(outside.path().join("secret"), dir.path().join("x")).unwrap();
    assert!(env.open_read(&env.resolve("x")).is_err());
    fs::create_dir(dir.path().join("sub")).unwrap();
    fs::write(dir.path().join("sub/f"), b"mine\n").unwrap();
    for name in ["sub/f", "sub/new"] {
        env.judged.insert(name.into(), dir.path().join(name));
    }
    let ops =
        toolpatch::parse("*** Begin Patch\n*** Update File: sub/f\n-mine\n+changed\n*** End Patch")
            .unwrap();
    let plan = Plan::stage(&env, &ops).unwrap();
    fs::remove_dir_all(dir.path().join("sub")).unwrap();
    symlink(outside.path(), dir.path().join("sub")).unwrap();
    assert!(plan.write().is_err());
    assert!(
        toolpatch::run(
            &env,
            "*** Begin Patch\n*** Add File: sub/new\n+x\n*** End Patch"
        )
        .is_err()
    );
    assert!(!outside.path().join("f").exists());
    assert!(!outside.path().join("new").exists());
    assert_eq!(fs::read(outside.path().join("secret")).unwrap(), b"private");
}
#[test]
fn explicitly_judged_outside_paths_and_internal_links_work_but_fifos_and_root_writes_fail() {
    let dir = tempfile::tempdir().unwrap();
    let other = tempfile::tempdir().unwrap();
    let mut env = environment(dir.path());
    let note = other.path().join("note");
    fs::write(&note, b"outside").unwrap();
    env.judged.insert("allowed".into(), note.clone());
    assert_eq!(
        env.read_bounded(&env.resolve("allowed"), 20).unwrap().0,
        b"outside"
    );
    toolpatch::run(
        &env,
        "*** Begin Patch\n*** Update File: allowed\n-outside\n+approved\n*** End Patch",
    )
    .unwrap();
    assert_eq!(fs::read(note).unwrap(), b"approved\n");
    fs::write(dir.path().join("real"), b"inside").unwrap();
    symlink("real", dir.path().join("link")).unwrap();
    assert_eq!(
        env.read_bounded(&dir.path().join("link"), 20).unwrap().0,
        b"inside"
    );
    assert_eq!(
        resolve_path(dir.path(), Path::new("link/new")),
        dir.path().join("real/new")
    );
    assert_eq!(
        resolve_path(dir.path(), Path::new("~/x")),
        dir.path().join("~/x")
    );
    assert!(env.write_atomic(dir.path(), b"no", 0o600).is_err());
    let fifo = dir.path().join("fifo");
    let c = std::ffi::CString::new(fifo.as_os_str().as_encoded_bytes()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(c.as_ptr(), 0o600) }, 0);
    assert!(env.open_read(&fifo).is_err());
    let ops =
        toolpatch::parse("*** Begin Patch\n*** Update File: real\n-inside\n+after\n*** End Patch")
            .unwrap();
    let plan = Plan::stage(&env, &ops).unwrap();
    symlink(other.path(), dir.path().join(".real.stavlos-tmp")).unwrap();
    plan.write().unwrap();
    assert_eq!(fs::read(dir.path().join("real")).unwrap(), b"after\n");
}
#[test]
fn updates_preserve_unmatched_non_utf8_content() {
    let ops =
        toolpatch::parse("*** Begin Patch\n*** Update File: f\n-old\n+new\n*** End Patch").unwrap();
    assert_eq!(
        toolpatch::apply_hunks(b"\xff\nold\n", &ops[0].hunks).unwrap(),
        b"\xff\nnew\n"
    );
}
