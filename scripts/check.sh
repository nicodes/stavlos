#!/usr/bin/env bash
# check.sh runs every gate a change must pass before it is committed:
# formatting, module tidiness, vet, the exhaustive-switch lint, staticcheck,
# dead code, known vulnerabilities, the complexity ratchet, the fuzz targets'
# seeds and a short run of each, the web client's tests and bundle, the race
# detector on every package, and the whole suite three times so a flaky test
# fails loudly. It names the failed gates and exits non-zero when any fails.
# CI runs exactly this (.github/workflows/check.yml).
set -uo pipefail
cd "$(dirname "$0")/.."

failed=()
step() { printf '\n== %s\n' "$1"; }

step gofmt
out=$(gofmt -l cmd internal pkg)
if [ -n "$out" ]; then echo "$out"; failed+=(gofmt); fi

step "go mod tidy"
go mod tidy -diff >/dev/null || { go mod tidy -diff | head -20; failed+=(tidy); }

step vet
go vet ./... || failed+=(vet)

step exhaustive
go tool exhaustive -default-signifies-exhaustive \
	-ignore-enum-types '(yaml\.v3\.Kind|html\.NodeType|model\.BlockType|tui\.[A-Za-z]+|transcript\.[A-Za-z]+)$' \
	./... || failed+=(exhaustive)

step staticcheck
go tool staticcheck ./... || failed+=(staticcheck)

# Code nothing reaches, tests counted as users.
step deadcode
out=$(go tool deadcode -test ./... 2>&1)
if [ -n "$out" ]; then echo "$out"; failed+=(deadcode); fi

# Known vulnerabilities in what the code calls. The database is fetched, so
# with no network this warns rather than fails; a finding always fails.
step govulncheck
go tool govulncheck ./... >/tmp/stavlos-vuln.$$ 2>&1
case $? in
0) ;;
3) cat /tmp/stavlos-vuln.$$; failed+=(govulncheck) ;;
*) echo "govulncheck could not run (offline?):"; tail -3 /tmp/stavlos-vuln.$$ ;;
esac
rm -f /tmp/stavlos-vuln.$$

# The complexity ratchet (docs/ground-up-refactor.md 0.4). No function may be
# above 30. Above 25, only the functions in scripts/gocyclo-baseline.txt may
# be, and none of them may grow: the list only ever gets shorter, and when it
# is empty the limit becomes 25, then 20.
step "gocyclo ratchet"
out=$(go tool gocyclo -over 30 -ignore '_test' .)
if [ -n "$out" ]; then echo "$out"; failed+=(gocyclo); fi
go tool gocyclo -over 25 -ignore '_test' . | awk '{print $1, $2, $3}' | sort -k2,3 >/tmp/stavlos-cyclo.$$
out=$(awk 'NR==FNR { base[$2" "$3]=$1; next }
	!(($2" "$3) in base) { print "new over 25: " $0; next }
	$1 > base[$2" "$3] { print "grew past its baseline (" base[$2" "$3] "): " $0 }' scripts/gocyclo-baseline.txt /tmp/stavlos-cyclo.$$)
if [ -n "$out" ]; then echo "$out"; failed+=(gocyclo-ratchet); fi
out=$(awk 'NR==FNR { now[$2" "$3]=1; next } !(($2" "$3) in now) { print "  " $0 }' /tmp/stavlos-cyclo.$$ scripts/gocyclo-baseline.txt)
if [ -n "$out" ]; then echo "no longer over 25, remove from scripts/gocyclo-baseline.txt:"; echo "$out"; failed+=(gocyclo-baseline-stale); fi
rm -f /tmp/stavlos-cyclo.$$

# Every fuzz target, briefly: its seeds always run with the suite; this looks
# for something new for a few seconds each.
step fuzz
while read -r pkg name; do
	printf '%s ' "$name"
	go test -run '^$' -fuzz "^${name}\$" -fuzztime "${FUZZTIME:-3s}" "$pkg" >/tmp/stavlos-fuzz.$$ 2>&1 || { tail -20 /tmp/stavlos-fuzz.$$; failed+=("fuzz:$name"); }
done < <(grep -rn --include='*_test.go' -E '^func Fuzz[A-Za-z0-9_]+\(' internal cmd pkg 2>/dev/null |
	sed -E 's|^([^:]+)/[^/]+:[0-9]+:func (Fuzz[A-Za-z0-9_]+)\(.*|./\1 \2|' | sort -u)
echo
rm -f /tmp/stavlos-fuzz.$$

# The web client's bundle is committed (docs/web-ui.md), so a stale one must
# not ship quietly: rebuild it and fail when the result differs from what is
# committed. Skipped where Node is not installed; `go install` never needs it.
step "web bundle"
if command -v npm >/dev/null; then
	(cd web && { [ -d node_modules ] || npm ci --no-audit --no-fund >/dev/null; } && npm test --silent && npm run build --silent) >/dev/null || failed+=(web-build)
	if [ -n "$(git status --porcelain -- internal/web/dist)" ]; then
		git status --short -- internal/web/dist
		echo "internal/web/dist is stale: commit the rebuilt bundle"
		failed+=(web-stale)
	fi
else
	echo "npm not found: skipped"
fi

# Every package: a hand-kept list left seven out (0.4).
step race
go test -race -count=1 ./... | grep -Ev '^(ok|\?) '
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
