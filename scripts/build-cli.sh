#!/usr/bin/env bash
set -euo pipefail
source scripts/rust-platform.sh
mkdir -p .artifacts/build .artifacts/native-cli
name="stavlos_${os_label}_${arch_label}"
if [[ -n ${NATIVE_CLI_ARTIFACTS:-} ]]; then
  python3 scripts/native-artifact.py verify "$NATIVE_CLI_ARTIFACTS/$name" cli "$os_label/$arch_label" "${VERSION:-devel}"
  cp "$NATIVE_CLI_ARTIFACTS/$name" .artifacts/build/app
else
  export STAVLOS_BUILD_VERSION="${VERSION:-devel}"
  SOURCE_COMMIT="$(git rev-parse HEAD)"
  export SOURCE_COMMIT
  cargo build --locked --release --target "$rust_target" --bin stavlos
  cp "${CARGO_TARGET_DIR:-target}/$rust_target/release/stavlos" ".artifacts/native-cli/$name"
  python3 scripts/native-artifact.py record ".artifacts/native-cli/$name" cli "$os_label/$arch_label" "$STAVLOS_BUILD_VERSION"
  cp ".artifacts/native-cli/$name" .artifacts/build/app
fi
chmod 755 .artifacts/build/app
