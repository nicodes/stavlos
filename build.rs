use std::process::Command;
fn git(args: &[&str]) -> String {
    Command::new("git")
        .args(args)
        .output()
        .ok()
        .filter(|r| r.status.success())
        .map(|r| String::from_utf8_lossy(&r.stdout).trim().into())
        .unwrap_or_default()
}
fn stamp(name: &str, value: String) {
    println!("cargo:rerun-if-env-changed={name}");
    let value = std::env::var(name).unwrap_or(value);
    println!("cargo:rustc-env={name}={value}");
}
fn main() {
    println!("cargo:rerun-if-changed=build.rs");
    for path in [
        git(&["rev-parse", "--git-path", "HEAD"]),
        git(&[
            "rev-parse",
            "--git-path",
            &git(&["symbolic-ref", "-q", "HEAD"]),
        ]),
    ] {
        if !path.is_empty() {
            println!("cargo:rerun-if-changed={path}");
        }
    }
    stamp("STAVLOS_BUILD_VERSION", "devel".into());
    stamp("STAVLOS_BUILD_COMMIT", git(&["rev-parse", "HEAD"]));
    stamp(
        "STAVLOS_BUILD_TIME",
        git(&["show", "-s", "--format=%cI", "HEAD"]),
    );
    stamp(
        "STAVLOS_BUILD_MODIFIED",
        (!git(&["status", "--porcelain", "--untracked-files=no"]).is_empty()).to_string(),
    );
    let rust = std::env::var("RUSTC").unwrap_or_else(|_| "rustc".into());
    let version = Command::new(rust)
        .arg("--version")
        .output()
        .ok()
        .map(|r| String::from_utf8_lossy(&r.stdout).trim().into())
        .unwrap_or_default();
    stamp("STAVLOS_RUST_VERSION", version);
}
