#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
group=${1:-all}
case "$group" in all|lint|test|build) ;; *) echo 'usage: check.sh [all|lint|test|build]' >&2; exit 2 ;; esac
if [[ $group == all || $group == lint ]]; then
  cargo fmt --all --check
  cargo clippy --locked --all-targets -- -D warnings
  cargo fmt --manifest-path crates/api/Cargo.toml --check
  cargo clippy --locked --manifest-path crates/api/Cargo.toml --all-targets -- -D warnings
  bash scripts/scan-owner-rust.sh
  trivy fs --scanners vuln --severity HIGH,CRITICAL --exit-code 1 --skip-dirs crates/api/target crates/api
  actionlint
  shellcheck scripts/*.sh
fi
if [[ $group == all || $group == test ]]; then
  cargo build --locked --manifest-path crates/api/Cargo.toml --bin stavlos-api
  export STAVLOS_API_BINARY="${CARGO_TARGET_DIR:-$PWD/crates/api/target}/debug/stavlos-api"
  cargo test --locked
  cargo test --locked --manifest-path crates/api/Cargo.toml
  python3 scripts/installer_test.py
  python3 scripts/native_artifact_test.py
  npm --prefix app test --silent
fi
if [[ $group == all || $group == build ]]; then
  npm --prefix app run build --silent
  python3 scripts/verify-ui-build.py
  bash scripts/build-cli.sh
fi
