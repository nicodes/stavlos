use stavlos::{buildid, daemon::sweep, navigation, protocol};
use std::{
    fs::{File, FileTimes},
    os::unix::fs::{PermissionsExt, symlink},
    time::{Duration, SystemTime},
};
#[test]
fn navigation_retains_newer_human_selections_and_ignores_archived_or_unusable_state() {
    let root = tempfile::tempdir().unwrap();
    let data = root.path().join("data");
    navigation::remember(&data, "newest", 200).unwrap();
    navigation::remember(&data, "late", 100).unwrap();
    navigation::remember(&data, "", 300).unwrap();
    assert_eq!(navigation::last(&data), "newest");
    assert_eq!(
        std::fs::metadata(data.join("navigation.json"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o600
    );
    let channels = vec![
        protocol::ChannelInfo {
            id: "archived".into(),
            archived: true,
            ..Default::default()
        },
        protocol::ChannelInfo {
            id: "first".into(),
            ..Default::default()
        },
        protocol::ChannelInfo {
            id: "newest".into(),
            ..Default::default()
        },
    ];
    assert_eq!(navigation::resume(&channels, "newest"), "newest");
    assert_eq!(navigation::resume(&channels, "archived"), "first");
    assert!(navigation::resume(&[], "newest").is_empty());
    std::fs::write(data.join("navigation.json"), "{broken}").unwrap();
    assert!(navigation::last(&data).is_empty());
    std::fs::remove_file(data.join("navigation.json")).unwrap();
    symlink(root.path().join("missing"), data.join("navigation.json")).unwrap();
    assert!(navigation::last(&data).is_empty());
}
#[test]
fn overflow_sweep_expires_only_old_output_and_cannot_follow_cache_symlinks_outside_its_root() {
    let root = tempfile::tempdir().unwrap();
    let cache = root.path().join("tmp");
    let output = cache.join("channel/output");
    std::fs::create_dir_all(&output).unwrap();
    let now = SystemTime::now();
    let old = now - Duration::from_secs(8 * 24 * 3600);
    let cutoff = now - Duration::from_secs(7 * 24 * 3600);
    for name in ["old", "fresh"] {
        let file = File::create(output.join(name)).unwrap();
        if name == "old" {
            file.set_times(FileTimes::new().set_modified(old)).unwrap();
        }
    }
    let keep = cache.join("channel/keep.txt");
    let file = File::create(&keep).unwrap();
    file.set_times(FileTimes::new().set_modified(old)).unwrap();
    let outside = root.path().join("outside");
    std::fs::create_dir(&outside).unwrap();
    let file = File::create(outside.join("old")).unwrap();
    file.set_times(FileTimes::new().set_modified(old)).unwrap();
    std::fs::create_dir(cache.join("malicious")).unwrap();
    symlink(&outside, cache.join("malicious/output")).unwrap();
    symlink(&outside, cache.join("elsewhere")).unwrap();
    assert_eq!(sweep::older(&cache, cutoff).unwrap(), 1);
    assert!(!output.join("old").exists());
    assert!(output.join("fresh").exists());
    assert!(keep.exists());
    assert!(outside.join("old").exists());
    assert_eq!(
        sweep::older(&root.path().join("missing"), cutoff).unwrap(),
        0
    );
}
#[test]
fn build_identity_is_the_executable_hash_and_version_reports_the_native_toolchain() {
    use sha2::{Digest, Sha256};
    let binary = std::fs::read(std::env::current_exe().unwrap()).unwrap();
    assert_eq!(
        buildid::id(),
        &format!("{:x}", Sha256::digest(binary))[..12]
    );
    let version = buildid::version();
    assert!(version.starts_with("stavlos "));
    assert!(version.contains("\nrust    rustc 1.94."));
    assert!(version.ends_with(&format!("build   {}\n", buildid::id())));
}
