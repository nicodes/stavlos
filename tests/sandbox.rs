use serde_json::{Value, json};
use stavlos::{
    childenv,
    sandbox::{self, Level, Spec, Wire},
};
use std::{
    ffi::OsString,
    fs,
    path::Path,
    process::{Command, Output, Stdio},
};
const TEST_HELPER: &str = "STAVLOS_TEST_SANDBOX_HELPER";
const TEST_ARGS: &str = "STAVLOS_TEST_SANDBOX_ARGS";
#[test]
fn sandbox_fixture_entrypoint() {
    if std::env::var_os(TEST_HELPER).as_deref() != Some(std::ffi::OsStr::new("1")) {
        return;
    }
    let arguments: Vec<String> = serde_json::from_str(&std::env::var(TEST_ARGS).unwrap()).unwrap();
    let args: Vec<OsString> = std::iter::once(OsString::from("--"))
        .chain(arguments.into_iter().map(OsString::from))
        .collect();
    match sandbox::entry(&args) {
        Ok(code) => std::process::exit(code),
        Err(error) => {
            eprintln!("owned sandbox fixture: {error:#}");
            std::process::exit(126);
        }
    }
}
fn run(wire: Wire, arguments: &[String]) -> Output {
    let environment = childenv::process(&[], Vec::new());
    Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "sandbox_fixture_entrypoint",
            "--nocapture",
            "--test-threads=1",
        ])
        .env_clear()
        .envs(childenv::pairs(&environment))
        .env(TEST_HELPER, "1")
        .env(TEST_ARGS, serde_json::to_string(arguments).unwrap())
        .env(sandbox::SPEC_ENV, serde_json::to_string(&wire).unwrap())
        .env_remove(sandbox::STAGE_ENV)
        .stdin(Stdio::null())
        .output()
        .unwrap()
}
fn probe() -> sandbox::Report {
    sandbox::probe_with(|wire| {
        let output = run(wire, &["/bin/true".into()]);
        if !output.status.success() {
            anyhow::bail!(
                "{}: {}",
                output.status,
                String::from_utf8_lossy(&output.stderr)
            );
        }
        Ok(())
    })
}
#[test]
fn real_helper_enforces_filesystem_and_unix_socket_boundaries_without_changing_parent() {
    let report = probe();
    eprintln!("owned kernel fixture: {} ({})", report.level, report.why);
    if report.level == Level::None {
        eprintln!("owned kernel fixture unavailable: {}", report.why);
        return;
    }
    let owned = tempfile::tempdir().unwrap();
    let allowed = owned.path().join("allowed");
    let denied = owned.path().join("denied");
    let scratch = owned.path().join("scratch");
    for path in [&allowed, &denied, &scratch] {
        fs::create_dir(path).unwrap();
    }
    fs::write(allowed.join("read.txt"), "allowed fixture").unwrap();
    fs::write(allowed.join("control"), "original control").unwrap();
    fs::write(allowed.join("hidden"), "hidden fixture").unwrap();
    fs::write(denied.join("read.txt"), "denied fixture").unwrap();
    let script = r#"import json,os,pathlib,socket,sys
a,d=map(pathlib.Path,sys.argv[1:3]);r={}
r['read_allowed']=(a/'read.txt').read_text()
(a/'write.txt').write_text('owned write');r['write_allowed']=True
try:r['read_denied']=(d/'read.txt').read_text()
except OSError:r['read_denied']='refused'
try:(d/'write.txt').write_text('must not land');r['write_denied']='landed'
except OSError:r['write_denied']='refused'
try:s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM);r['unix']='created';s.close()
except OSError:r['unix']='refused'
try:(a/'control').write_text('changed control');r['control']='changed'
except OSError:r['control']='refused'
r['hidden']=(a/'hidden').read_text()
r['spec_removed']=os.getenv('STAVLOS_SANDBOX_SPEC') is None
r['stage_removed']=os.getenv('STAVLOS_SANDBOX_STAGE') is None
r['cache']=os.environ['CARGO_HOME'];pathlib.Path(r['cache'],'proof').write_text('cache works')
print(json.dumps(r))
"#;
    let spec = Spec {
        writable: vec![allowed.clone()],
        read_only: vec![allowed.join("control")],
        hidden: vec![allowed.join("hidden")],
        tmp: scratch.clone(),
        private_tmp: true,
        caches: true,
        network: false,
        ..Spec::default()
    };
    let output = run(
        Wire {
            spec,
            mounts: report.level == Level::Full,
            probe: false,
        },
        &[
            "/usr/bin/python3".into(),
            "-I".into(),
            "-c".into(),
            script.into(),
            allowed.to_string_lossy().into_owned(),
            denied.to_string_lossy().into_owned(),
        ],
    );
    assert!(
        output.status.success(),
        "{}\n{}\n{}",
        output.status,
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    let start = output.stdout.iter().position(|b| *b == b'{').unwrap();
    let result: Value = serde_json::from_slice(&output.stdout[start..]).unwrap();
    assert_eq!(result["read_allowed"], "allowed fixture");
    assert_eq!(result["write_allowed"], true);
    assert_eq!(result["read_denied"], "refused");
    assert_eq!(result["write_denied"], "refused");
    assert_eq!(result["unix"], "refused");
    assert_eq!(result["spec_removed"], true);
    assert_eq!(result["stage_removed"], true);
    assert!(!denied.join("write.txt").exists());
    if report.level == Level::Full {
        assert_eq!(result["control"], "refused");
        assert_eq!(result["hidden"], "");
        assert_eq!(
            fs::read_to_string(allowed.join("hidden")).unwrap(),
            "hidden fixture"
        );
        assert!(
            result["cache"]
                .as_str()
                .unwrap()
                .starts_with("/tmp/caches/")
        );
    } else {
        assert!(
            result["cache"]
                .as_str()
                .unwrap()
                .starts_with(&scratch.to_string_lossy().to_string())
        );
    }
    // The restriction was confined to the owned child, including namespace mounts.
    assert_eq!(
        fs::read_to_string(denied.join("read.txt")).unwrap(),
        "denied fixture"
    );
    fs::write(denied.join("parent-write.txt"), "parent remains unconfined").unwrap();
}
#[test]
fn command_wrapper_removes_untrusted_stage_markers_and_grants_only_the_selected_program() {
    let helper = Path::new("/owned/stavlos");
    let spec = Spec {
        readable: vec!["/owned/support".into()],
        network: true,
        ..Spec::default()
    };
    let environment = vec![
        OsString::from("PATH=/usr/bin:/bin"),
        OsString::from("STAVLOS_SANDBOX_STAGE=2"),
        OsString::from("STAVLOS_SANDBOX_SPEC=forged"),
    ];
    let command = sandbox::command(
        std::ffi::OsStr::new("/usr/bin/true"),
        &[],
        &environment,
        Some(&spec),
        Level::Full,
        helper,
    )
    .unwrap();
    assert_eq!(command.get_program(), helper);
    let variables: std::collections::BTreeMap<_, _> = command.get_envs().collect();
    assert!(!variables.contains_key(std::ffi::OsStr::new(sandbox::STAGE_ENV)));
    let actual: Wire = serde_json::from_str(
        variables[std::ffi::OsStr::new(sandbox::SPEC_ENV)]
            .unwrap()
            .to_str()
            .unwrap(),
    )
    .unwrap();
    assert!(actual.mounts);
    assert_eq!(
        actual.spec.readable,
        [
            std::path::PathBuf::from("/owned/support"),
            std::path::PathBuf::from("/usr/bin/true")
        ]
    );
    let encoded = serde_json::to_value(actual).unwrap();
    assert_eq!(encoded["network"], true);
    assert_eq!(encoded["mounts"], true);
    assert!(encoded.get("probe").is_none());
    assert_eq!(
        json!(sandbox::linux::namespace_flags(false) & libc::CLONE_NEWNET != 0),
        true
    );
}
