#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
mkdir -p .artifacts/security/owner-rust
cp Cargo.lock .artifacts/security/owner-rust/Cargo.lock
trivy fs --quiet --scanners vuln --pkg-types library --include-dev-deps --list-all-pkgs \
  --severity HIGH,CRITICAL --exit-code 1 .artifacts/security/owner-rust
