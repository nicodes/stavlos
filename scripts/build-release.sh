#!/usr/bin/env bash
set -euo pipefail
mkdir -p dist
trap 'rm -f internal/nativeapi/payload.gz' EXIT
for arch in amd64 arm64; do
  case "$arch" in amd64) label=x86_64 ;; arm64) label=arm64 ;; esac
  python3 scripts/api-payload.py ".artifacts/native-api/stavlos-api_Linux_$label"
  output=".artifacts/release/Linux_$label"
  mkdir -p "$output"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -tags rustapi -trimpath -buildvcs=true \
    -ldflags "-s -w -X github.com/nicodes/stavlos/internal/buildid.version=$VERSION" \
    -o "$output/stavlos" ./cmd/stavlos
  python3 - "$output/stavlos" "$arch" <<'IDENTITY'
import subprocess,sys
info=subprocess.check_output(['go','version','-m',sys.argv[1]],text=True)
source=subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()
for value in ['GOOS=linux','GOARCH='+sys.argv[2],'CGO_ENABLED=0','vcs.revision='+source]:
    assert value in info,value
IDENTITY
  cp README.md LICENSE "$output/"
  tar -C "$output" -czf "dist/stavlos_Linux_$label.tar.gz" stavlos README.md LICENSE
done
(cd dist && sha256sum -- *.tar.gz > checksums.txt)
