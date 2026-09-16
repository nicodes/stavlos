#!/usr/bin/env bash
# check.sh runs every gate a change must pass before it is committed:
# formatting, vet, the exhaustive-switch lint, staticcheck, the complexity
# bound, the race detector on the concurrent packages, and the whole suite
# three times so a flaky test fails loudly. It names the failed gates and
# exits non-zero when any fails.
set -uo pipefail
cd "$(dirname "$0")/.."

failed=()
step() { printf '\n== %s\n' "$1"; }

step gofmt
out=$(gofmt -l cmd internal pkg)
if [ -n "$out" ]; then echo "$out"; failed+=(gofmt); fi

step vet
go vet ./... || failed+=(vet)

step exhaustive
go tool exhaustive -default-signifies-exhaustive \
	-ignore-enum-types '(event\.Type|yaml\.v3\.Kind|html\.NodeType|model\.BlockType|tui\.[A-Za-z]+|transcript\.[A-Za-z]+)$' \
	./... || failed+=(exhaustive)

step staticcheck
go tool staticcheck ./... || failed+=(staticcheck)

step "gocyclo -over 30"
out=$(go tool gocyclo -over 30 -ignore '_test' .)
if [ -n "$out" ]; then echo "$out"; failed+=(gocyclo); fi

step race
go test -race -count=1 \
	./internal/agent/ ./internal/daemon/ ./internal/proc/ ./internal/tools/ ./internal/shellcmd/ \
	./internal/textsafe/ ./internal/oauth/ ./internal/eventlog/ ./internal/escalation/ \
	./internal/model/... ./internal/modelsdev/ ./internal/auth/ ./internal/config/ ./internal/policy/ \
	./internal/tui/... ./internal/discord/ ./pkg/client/ | grep -v '^ok '
[ "${PIPESTATUS[0]}" -eq 0 ] || failed+=(race)

for i in 1 2 3; do
	step "test run $i"
	go test -count=1 ./... | grep -Ev '^(ok|\?) '
	[ "${PIPESTATUS[0]}" -eq 0 ] || failed+=("test$i")
done

if [ ${#failed[@]} -gt 0 ]; then
	printf '\nFAILED: %s\n' "${failed[*]}"
	exit 1
fi
printf '\nall gates passed\n'
