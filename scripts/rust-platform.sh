#!/usr/bin/env bash
# Sourced by builders running on their target architecture.
os_label="$(uname -s)"
case "$(uname -m)" in x86_64|amd64) arch_label=x86_64 ;; aarch64|arm64) arch_label=arm64 ;; *) echo 'Unsupported native architecture.' >&2; exit 1 ;; esac
case "$os_label/$arch_label" in
  Linux/x86_64) rust_target=x86_64-unknown-linux-musl ;;
  Linux/arm64) rust_target=aarch64-unknown-linux-musl ;;
  Darwin/x86_64) rust_target=x86_64-apple-darwin ;;
  Darwin/arm64) rust_target=aarch64-apple-darwin ;;
  *) echo 'The native executables support Linux and macOS.' >&2; exit 1 ;;
esac
if [[ -z ${NATIVE_CLI_ARTIFACTS:-} ]]; then rustup target add --toolchain 1.94.0 "$rust_target"; fi
if [[ $os_label == Linux ]]; then
  export CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER=musl-gcc
  export CARGO_TARGET_AARCH64_UNKNOWN_LINUX_MUSL_LINKER=musl-gcc
  export CC_x86_64_unknown_linux_musl=musl-gcc
  export CC_aarch64_unknown_linux_musl=musl-gcc
fi
