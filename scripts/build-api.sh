#!/usr/bin/env bash
set -euo pipefail
source scripts/rust-platform.sh
SOURCE_COMMIT="$(git rev-parse HEAD)" cargo build --locked --manifest-path api/Cargo.toml --release --target "$rust_target"
mkdir -p .artifacts/native-api
name="stavlos-api_${os_label}_${arch_label}"
cp "${CARGO_TARGET_DIR:-api/target}/$rust_target/release/stavlos-api" ".artifacts/native-api/$name"
python3 scripts/native-artifact.py record ".artifacts/native-api/$name" api "$os_label/$arch_label" "${VERSION:-devel}"
