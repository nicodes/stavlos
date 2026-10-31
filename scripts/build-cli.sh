#!/usr/bin/env bash
set -euo pipefail
os_label="$(uname -s)"
case "$(uname -m)" in x86_64|amd64) arch_label=x86_64 ;; aarch64|arm64) arch_label=arm64 ;; *) exit 1 ;; esac
payload=".artifacts/native-api/stavlos-api_${os_label}_${arch_label}"
if [ ! -s "$payload" ]; then bash scripts/build-api.sh; fi
trap 'rm -f internal/nativeapi/payload.gz' EXIT
python3 scripts/api-payload.py "$payload"
mkdir -p .artifacts/build
CGO_ENABLED=0 go build -tags rustapi -trimpath -buildvcs=true \
  -ldflags "-s -w -X github.com/nicodes/stavlos/internal/buildid.version=${VERSION:-devel}" \
  -o .artifacts/build/app ./cmd/stavlos
