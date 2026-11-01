use serde_json::Value;
use stavlos::{childenv, peercred};
use std::{
    ffi::OsString,
    fs,
    os::unix::{ffi::OsStringExt, net::UnixStream},
    process::{Command, Stdio},
};
#[test]
fn original_environment_allowlist_and_url_credentials_match_without_reading_owner_values() {
    let fixture: Value =
        serde_json::from_str(include_str!("data/go-child-environment.json")).unwrap();
    for case in fixture["predicates"].as_array().unwrap() {
        let name = case["name"].as_str().unwrap();
        assert_eq!(
            childenv::passes(name),
            case["passes"].as_bool().unwrap(),
            "{name}"
        );
    }
    for case in fixture["urls"].as_array().unwrap() {
        let value = case["value"].as_str().unwrap();
        assert_eq!(
            childenv::url_credential(value.as_bytes()),
            case["credential"].as_bool().unwrap(),
            "{value:?}"
        );
    }
}
#[test]
fn child_environment_preserves_only_allowed_or_explicit_values_and_extra_wins() {
    let environment = [
        "PATH=/owned/bin",
        "HOME=/owned/home",
        "GOPROXY=https://user:fixture@proxy.example/",
        "GOOGLE_API_KEY=fixture",
        "DBUS_SESSION_BUS_ADDRESS=unix:path=/owned/bus",
        "SSH_AUTH_SOCK=/owned/socket",
        "STAVLOS_SOCKET=/owned/owner",
        "BASH_ENV=/owned/startup",
        "CARGO_HOME=/owned/cargo",
        "EXPLICIT_KEY=fixture",
    ]
    .map(OsString::from);
    let filtered = childenv::filter(
        environment,
        &["EXPLICIT_KEY".into()],
        vec![OsString::from("PATH=/owned/override")],
    );
    assert_eq!(
        filtered,
        vec![
            OsString::from("PATH=/owned/bin"),
            OsString::from("HOME=/owned/home"),
            OsString::from("CARGO_HOME=/owned/cargo"),
            OsString::from("EXPLICIT_KEY=fixture"),
            OsString::from("PATH=/owned/override")
        ]
    );
    let bytes = OsString::from_vec(b"CFLAGS=owned-\xff".to_vec());
    let preserved = childenv::filter(vec![bytes.clone()], &[], Vec::new());
    assert_eq!(preserved, [bytes]);
    let pairs: Vec<_> = childenv::pairs(&filtered).collect();
    assert_eq!(pairs.last().unwrap().1, "/owned/override");
}
#[test]
fn unix_peer_identity_comes_from_the_kernel_and_unreadable_ancestry_refuses() {
    let (left, right) = UnixStream::pair().unwrap();
    for socket in [&left, &right] {
        let identity = peercred::of_self(socket).unwrap();
        assert_eq!(identity.pid, std::process::id() as i32);
        assert_eq!(identity.uid, unsafe { libc::getuid() });
    }
    assert!(peercred::of(&fs::File::open("/dev/null").unwrap()).is_err());
    assert_eq!(
        peercred::parent_from_stat("123 (odd (name)) S 42 0 0").unwrap(),
        42
    );
    assert!(peercred::parent_from_stat("broken").is_err());
    assert!(
        peercred::descends_with(10, 2, |_| Err(anyhow::anyhow!("owned missing process"))).is_err()
    );
    assert!(peercred::descends_with(10, 99, |_| Ok(10)).is_err());
    assert!(peercred::descends_with(10, 2, |pid| Ok(if pid == 10 { 3 } else { 2 })).unwrap());
    assert!(!peercred::descends_with(1, 2, |_| panic!("init requires no walk")).unwrap());
}
#[test]
fn owned_child_is_recognized_before_it_is_killed_and_reaped() {
    let mut child = Command::new("/usr/bin/sleep")
        .arg("30")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    let result = peercred::descends_from(child.id() as i32, std::process::id() as i32);
    let parent = peercred::parent_of(child.id() as i32);
    child.kill().unwrap();
    child.wait().unwrap();
    assert!(result.unwrap());
    assert_eq!(parent.unwrap(), std::process::id() as i32);
}
