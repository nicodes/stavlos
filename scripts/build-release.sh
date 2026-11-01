#!/usr/bin/env bash
set -euo pipefail
[[ ${VERSION:-} =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'A release version is required.' >&2; exit 1; }
mkdir -p dist
for arch_label in x86_64 arm64; do
  name="stavlos_Linux_$arch_label"
  input="${NATIVE_CLI_ARTIFACTS:-.artifacts/native-cli}/$name"
  python3 scripts/native-artifact.py verify "$input" cli "Linux/$arch_label" "$VERSION"
  output=".artifacts/release/Linux_$arch_label"
  mkdir -p "$output"
  cp "$input" "$output/stavlos"
  chmod 755 "$output/stavlos"
  cp README.md LICENSE "$output/"
  tar -C "$output" -czf "dist/$name.tar.gz" stavlos README.md LICENSE
 done
(cd dist && sha256sum -- *.tar.gz > checksums.txt)
