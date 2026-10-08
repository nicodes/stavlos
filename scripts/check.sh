#!/usr/bin/env bash
# check.sh runs every gate a change must pass before it is committed:
# formatting, module tidiness, vet, the exhaustive-switch lint, staticcheck,
# dead code, known vulnerabilities, the complexity ratchet, the fuzz targets'
# seeds and a short run of each, the web client's tests and bundle, the race
# detector on every package, and the whole suite three times so a flaky test
# fails loudly. It names the failed gates and exits non-zero when any fails.
# CI runs exactly this (.github/workflows/check.yml).
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

group=${1:-all}
case "$group" in all|lint|test|build) ;; *) echo "usage: $0 [all|lint|test|build]" >&2; exit 2 ;; esac
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
failed=()
step() { printf '\n== %s\n' "$1"; }
if [[ "$group" == all || "$group" == lint ]]; then

step gofmt
out=$(gofmt -l cmd internal pkg) || failed+=(gofmt-execution)
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
out=$(go tool deadcode -test ./... 2>&1) || failed+=(deadcode-execution)
if [ -n "$out" ]; then echo "$out"; failed+=(deadcode); fi

# Scanner errors fail the gate just like findings; an unavailable database
# cannot certify the source as free of known called vulnerabilities.
step govulncheck
if ! go tool govulncheck ./... >"$scratch/vuln" 2>&1; then
	cat "$scratch/vuln"
	failed+=(govulncheck)
fi

# The complexity ratchet (docs/ground-up-refactor.md 0.4). No function may be
# above 30. Above 25, only the functions in scripts/gocyclo-baseline.txt may
# be, and none of them may grow: the list only ever gets shorter, and when it
# is empty the limit becomes 25, then 20.
step "gocyclo ratchet"
out=$(go tool gocyclo -over 30 -ignore '_test' .) || failed+=(gocyclo-execution)
if [ -n "$out" ]; then echo "$out"; failed+=(gocyclo); fi
go tool gocyclo -over 25 -ignore '_test' . >"$scratch/cyclo-raw" 2>"$scratch/cyclo-error"
status=$?
# gocyclo returns 1 for nonempty threshold output, including allowed baseline
# entries. Execution errors must not be mistaken for an empty passing result.
if (( status > 1 )) || [[ -s "$scratch/cyclo-error" ]] || { (( status == 1 )) && [[ ! -s "$scratch/cyclo-raw" ]]; }; then
	cat "$scratch/cyclo-error"
	failed+=(gocyclo-ratchet-execution)
fi
awk '{print $1, $2, $3}' "$scratch/cyclo-raw" | sort -k2,3 >"$scratch/cyclo"
out=$(awk 'NR==FNR { base[$2" "$3]=$1; next }
	!(($2" "$3) in base) { print "new over 25: " $0; next }
	$1 > base[$2" "$3] { print "grew past its baseline (" base[$2" "$3] "): " $0 }' scripts/gocyclo-baseline.txt "$scratch/cyclo")
if [ -n "$out" ]; then echo "$out"; failed+=(gocyclo-ratchet); fi
out=$(awk 'NR==FNR { now[$2" "$3]=1; next } !(($2" "$3) in now) { print "  " $0 }' "$scratch/cyclo" scripts/gocyclo-baseline.txt)
if [ -n "$out" ]; then echo "no longer over 25, remove from scripts/gocyclo-baseline.txt:"; echo "$out"; failed+=(gocyclo-baseline-stale); fi
rm -f "$scratch/cyclo"

fi

if [[ "$group" == all || "$group" == test ]]; then
# Every fuzz target, briefly: its seeds always run with the suite; this looks
# for something new for a few seconds each.
step fuzz
while read -r pkg name; do
	printf '%s ' "$name"
	go test -run '^$' -fuzz "^${name}\$" -fuzztime "${FUZZTIME:-3s}" "$pkg" >"$scratch/fuzz" 2>&1 || { tail -20 "$scratch/fuzz"; failed+=("fuzz:$name"); }
done < <(grep -rn --include='*_test.go' -E '^func Fuzz[A-Za-z0-9_]+\(' internal cmd pkg 2>/dev/null |
	sed -E 's|^([^:]+)/[^/]+:[0-9]+:func (Fuzz[A-Za-z0-9_]+)\(.*|./\1 \2|' | sort -u)
echo
rm -f "$scratch/fuzz"

# Every package: a hand-kept list left seven out (0.4).
step race
go test -race -count=1 ./... | grep -Ev '^(ok|\?) '
[ "${PIPESTATUS[0]}" -eq 0 ] || failed+=(race)

for i in 1 2 3; do
	step "test run $i"
	go test -count=1 ./... | grep -Ev '^(ok|\?) '
	[ "${PIPESTATUS[0]}" -eq 0 ] || failed+=("test$i")
done

fi

if [[ "$group" == all || "$group" == test || "$group" == build ]]; then
	step "web dependencies"
	if ! command -v npm >/dev/null; then
		echo "npm is required: run make install" >&2
		failed+=(npm-missing)
	elif [[ ! -d web/node_modules ]]; then
		(cd web && npm ci --no-audit --no-fund) || failed+=(web-install)
	fi
	if command -v npm >/dev/null; then
		if [[ "$group" == all || "$group" == test ]]; then
			step "web tests"
			(cd web && npm test --silent) || failed+=(web-test)
		fi
		if [[ "$group" == all || "$group" == build ]]; then
			step "web bundle"
			(cd web && npm run build --silent) || failed+=(web-build)
			if [ -n "$(git status --porcelain -- internal/web/dist)" ]; then
				git status --short -- internal/web/dist
				echo "internal/web/dist is stale: commit the rebuilt bundle"
				failed+=(web-stale)
			fi
		fi
	fi
fi
if [[ "$group" == all || "$group" == build ]]; then
	step "CLI build"
	mkdir -p .artifacts/bin
	go build -o .artifacts/bin/stavlos ./cmd/stavlos || failed+=(cli-build)
fi

if [ ${#failed[@]} -gt 0 ]; then
	printf '\nFAILED: %s\n' "${failed[*]}"
	exit 1
fi
printf '\nall gates passed\n'
