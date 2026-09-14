# Stavlos v1 hardening refactor — plan

Status: **approved 2026-09-14** (decisions below recorded; execution in phase order). Source: six-area audit of commit 591f774 (reports in the
session's audit directory; findings below cite `file:line` as of that commit).

Ground rules for execution (unchanged from how we have been working):

- One change per commit on `feat/v1-harness`, pushed after every change, `go install` after each.
- Gate before every commit: `go vet ./...`, `gofmt -l`, `staticcheck`, and `go test ./...` three times.
- Behaviour-preserving refactors land separately from behaviour changes, so every diff is one of
  "same behaviour, new shape" or "new behaviour, minimal shape".
- The event DB may be wiped, so schema changes ship without migrations (Phase 6).
- Each phase ends with the docs (README, PRD) matching the code.

---

## 0. Headline findings

Confirmed on the live tree with throwaway tests:

| # | Finding | Where |
|---|---------|-------|
| S1 | Shell allow-globs are prefix matches over the raw command: `cat x; rm -rf ~`, `cat x \| sh`, `cat x > ~/.bashrc`, `cat <(rm -rf .)` are **allowed with no prompt** under the default read-only rules. `SimpleCommand` is only consulted on the session-prefix path, never the policy path, and it misses `>`, `<`, `<(`, `>(`. | policy.go:148-191, config.go:255-271, turn.go:212-231, prefix.go:90 |
| S2 | "Roles and projects can only tighten" is unsound: `Tighten`/`checkTightening` test one synthetic string per pattern, so an overlay `git*: allow` shadows a global `*--force*: deny`. | policy.go:81-99, config.go:447-463 |
| S3 | Session-remembered allows (`allow_always`, `allow_prefix`) override explicit **Deny** rules, because they are applied after `Decide` and replace the verb unconditionally. | turn.go:213-233 |
| S4 | web_fetch policy matches the raw URL but the fetch normalises it, so `http://`, uppercase, userinfo, `:443`, and github blob→raw rewrites all bypass host deny rules. | web.go:52-58, 219-253 |
| S5 | Daemon-wide defaults (escalation timeouts, answer default, fallback policy for recovered sessions) are loaded from `/tmp/.stavlos/stavlos.local.json`, a world-writable location, as a **trusted** layer. | daemon.go:65-72, 102-107 |
| S6 | Model/tool text reaches the terminal with escape sequences intact; a command can erase its destructive half on screen inside the permission dialog. | view.go:1512-1611, 405-512 |
| S7 | Shell, background jobs and MCP servers inherit the daemon's full environment, including provider API keys read from env. | shell.go:161, monitor.go:227, mcp.go:133 |
| P1 | One slow TUI client stalls every agent: event append and the socket `Flush` to every subscriber happen under one lock with no write deadline or per-client queue. | daemon.go:125-134, 574-601 |
| P2 | `subscribe` replay→live handover can silently drop events (live seq advances the dedupe cursor past unreplayed events); the TUI has no gap detection. | server.go:549-577, daemon.go:574-584 |
| P3 | Every stream token re-copies and re-renders the whole transcript; spinner and compaction ticks do the same at 12 fps. | model.go:373-379, 3004-3053, view.go:160-263 |
| M1 | Live state mutation and crash recovery are two hand-written reducers that have already diverged (awaiting keys by prefix vs id, monitors re-armed on restart, prompt consumption reconstructed by text match). | recover.go, agent.go, session.go, orchestrator.go |
| M2 | Data races: `preset`, `Label`, `Archetype`, `turn`, MCP session pointer read without `a.mu` while `SetRole` writes them. | turn.go:339-460, agent.go:461-468, mcp.go:345 |
| M3 | Stringly-typed vocabularies (agent/session/monitor/MCP/todo states, prompt kinds, answer values, tool names, turn reasons) switched on in 5+ packages with no constants. Tool names appear as literals in 60+ sites. | everywhere |
| D1 | Three copy-pasted streaming adapters (SSE reader, HTTP post, error mapping identical); dead API-key era: the whole `model/anthropic` package plus its SDK dependency, half of `auth`, half of `modelsdev`. | model/* |

Mechanical checks: `go vet`, `gofmt`, `govulncheck`, `go mod tidy` all clean. `staticcheck` reports 15
dead symbols. `gocyclo`: `dispatch` 115, `Recover` 94, `EventLines` 91, `mdConverter.walk` 71.
Coverage: `internal/agent` 3.2 % (only `dirs_test.go`), `pkg/client` 0 %, `cmd/*` 0 %. One test,
`TestChildResponseWakesParent`, failed once in the auditor's run (timing).

---

## 1. Phase plan

Phases are ordered so each one makes the next cheaper and safer. Sizes: S ≈ one commit,
M ≈ 2–4 commits, L ≈ 5+ commits.

### Phase 1 — Safety net (before touching the core)  [M]  ✅ done 2026-09-14

The core is only tested through the daemon integration suite. Every later phase needs unit-level
tests that pin current behaviour first.

1.1 `internal/agent`: a `fakeHost` (in-memory `Append`/`Read`/`Prompt`) and a fake `model.Provider`
    so an `Agent`/`Session` can be driven without the daemon. Table tests for `runTool` verdicts
    (policy × mode × remembered allows × boundary), turn loop end reasons, mailbox semantics.
1.2 Replay round-trip test: drive a session, capture events, `Recover` into a fresh session, compare
    `Info()` trees. This is the regression guard for M1.
1.3 `project`: golden tests for `Project` (dangling tool_use repair, compaction cut, ordering).
1.4 Snapshot test of every tool's `Def().Schema` (guards the schema-from-struct change in 4.4).
1.5 De-flake `TestChildResponseWakesParent` (wait on the child's `TurnEnded`, not the tree read).
1.6 Add `-race` to the gate for `internal/agent` and `internal/daemon`. (The detector passes today; gate = vet, gofmt, `go test -race` on those two, then `go test ./...` three times. staticcheck joins the gate once Phase 9 removes the 15 dead symbols.)
    Found on the way: a recovered agent with a pending wake (response, lost job) was never signalled — fixed.

### Phase 2 — Security fixes (small, behaviour-changing, each its own commit)  [M]  ✅ done 2026-09-14 (2.1–2.15, one commit each)

2.1 **`internal/shellcmd`** ✅ (replaces `protocol/prefix.go`): POSIX-ish tokenizer (`Words`), refuses
    unquoted `; | & \n`, backticks, `$(`, `$((`, `<(`, `>(`, and all redirections; `Prefix` refuses
    wrappers/interpreters (`bash sh env xargs sudo eval exec time nohup nice timeout watch python
    node perl…`), env assignments, and flag-first two-word tools; token-based `Covers`.
    Policy: for `shell`, an `Allow` from a *pattern* rule is downgraded to `Ask` when the command is
    not simple. Deny always wins. Fixes S1.
2.2 **`policy.Layered`** ✅: sound layering by construction — decision is the max rank across layers;
    delete `Tighten`, `samplePath`, `checkTightening`, `samplePattern`. Computed once per config
    change, not per tool call. Fixes S2.
2.3 **`session.permits`** ✅ type owning `allowAlways`/`allowPrefix`: remembered allows only downgrade
    `Ask`, never `Deny`. Server derives the canonical prefix from the prompt's own arg and puts it on
    `PromptInfo.Prefix`; the client displays it instead of recomputing (drop client-supplied
    `Prefix`). Fixes S3 and the trusted-verbatim prefix.
2.4 **web_fetch canonical subject** ✅: `PolicyArg` returns the normalised URL (lower-cased host, https,
    no userinfo, blob→raw applied) so policy, session allows, and the fetch see one string.
    `CheckRedirect` requires https on same-host hops. `publicIP` via `netip` prefixes incl.
    0.0.0.0/8, 100.64/10, 198.18/15, 240/4, 64:ff9b::/96. Search calls use the same SSRF-safe
    client and refuse redirects. Fixes S4.
2.5 **`config.LoadGlobal`** ✅: defaults + global layer only; used by daemon startup and the recovery
    fallback. Delete the `os.TempDir()` trick. Fixes S5.
2.6 **`internal/textsafe`** ✅: strip ESC/OSC/C0 (except `\n\t`) from every daemon string the TUI
    draws; permission dialog renders hidden bytes visibly as `^[`. Fixes S6.
2.7 **`internal/proc`** ✅: one process launcher (bash -c, Setpgid, SIGKILL pgid, WaitDelay, tail-capped
    buffer, scrubbed env minus `STAVLOS_*`, provider key names, `*_API_KEY|TOKEN|SECRET|PASSWORD`).
    Used by shell tool, monitors (`StartCommand` deleted; `background:true` = start + adopt at wait
    0) and MCP. Fixes S7 and removes the duplicate runner.
2.8 Boundary heuristic ✅: `..`-relative arguments and `$`-containing tokens count as candidates
    (auto mode asks). One `tools.ResolvePath` (Clean + EvalSymlinks on the existing prefix) shared
    by `read`, `apply_patch`, `outsideDir`; no `~`/`${env:}` expansion of model-supplied paths.
2.9 Recovery never widens ✅: an agent whose role vanished gets a minimal read-only preset and a
    visible label suffix, not the root preset.
2.10 Trust reply goes through ✅ `prompt.reply(id)`; daemon derives dir/hash from the prompt it issued,
    `Abs`+`Clean`s the dir and recomputes the hash.
2.11 Daemon socket ✅: umask 0077 around `Listen`; flock on `<data>/stavlosd.lock` (second daemon
    refuses instead of stealing the socket); `SO_PEERCRED` uid check; per-connection context so a
    disconnected client's `login.wait` is cancelled; per-connection in-flight cap; 4 MB line max.
2.12 `/compact` routed ✅ through the actor (`compactNext` + signal, maintenance step) so it cannot
    race the turn loop; rejected on killed agents.
2.13 Config validation ✅: `DisallowUnknownFields`, invalid policy verbs are errors (like role files
    already are), `stavlos.json` written 0600, warning when `apiKey` is literal.
2.14 OAuth loopback ✅: `Pending.Close()`, expiry armed at `Start`, daemon sweep closes; state checked
    before `error`/`cancel` paths; `html.EscapeString` in the result page; issuer overrides behind
    a dev build tag.
2.15 Agent labels ✅ constrained to `[A-Za-z0-9_-]{1,32}`; agent-to-agent messages wrapped with an
    explicit "another agent's output, not the human" marker.

### Phase 3 — Shared vocabulary  [M]  ✅ done 2026-09-14 (3.1–3.4)

3.1 Typed constants in the leaf packages, replacing every literal switch:
    `protocol.AgentState` (idle/running/blocked/waiting/killed) + `RollUp` → `SessionState`;
    `PromptKind`, `PromptAction`, `AnswerValue`; `MonitorState`, `MCPState`; `event.TodoStatus`,
    `TurnReason`, `MessageKind`, `Source{Kind,ID}` helpers. `agent.State` becomes an alias.
3.2 `internal/toolname`: typed tool-name constants, `Canonical()` mapping every legacy spelling
    (`bash`, `bash_async`, `spawn`, `write`, `edit`, …) once at TUI ingress. `config`, `tools`,
    `agent`, `protocol`, `tui` all use it; the four legacy shims in the TUI collapse to one.
3.3 Decide the fate of the `escalated` action: add `event.PromptEscalated` or drop the record call.
3.4 Enable the `exhaustive` linter on the new enums in the gate.

### Phase 4 — Tools and policy structure  [M]  ✅ done 2026-09-14 (4.1–4.7; 4.3 covered by toolname)

4.1 `policy.Subject{Kind: Command|Path|URL|ID|Text, Value}`: tools normalise before policy sees the
    argument; `PolicyArg` becomes `Subject`.
4.2 Break `tools → config`: move `config.Skill` to `tools`; `tools.ChildStatus`/`MonitorStatus`
    become `protocol.AgentInfo`+`You` / `protocol.MonitorInfo`; `tools.Artifact = event.Artifact`.
4.3 Tool registry: one entry per tool `{Name, Def, Subject, Group, ImpliedBy}`; `Builtin`,
    `DefaultTools`, presets, `toolGroup`, the five `*Names` slices and the system-prompt tool list
    all iterate it.
4.4 Schema-from-struct: `tools.SchemaOf[T]()` from tagged input structs (guarded by 1.4); removes
    the triple declaration and untagged case-insensitive decoding.
4.5 Split `web.go` into `web/fetch.go`, `web/markdown.go` (byte-buffer converter, stop at cap,
    O(n) flush), `web/search.go` (`Provider` interface; SSE parsed per event; output clipped).
4.6 `apply_patch`: per-file atomic via temp+rename with rollback, preserve mode on move, refuse
    overwriting move targets, honest description. `read` stops appending at the cap; `Limit` capped.
    `partialWriter` stops streaming after adoption. Rune-safe truncation helper everywhere.
4.7 web_search default: see decision D3.

### Phase 5 — Agent core  [L]  ✅ done 2026-09-14 (5.1 as targeted replay fixes guarded by the round-trip test; 5.2 as roleView snapshots rather than a command channel; 5.3–5.7)

5.1 **One reducer**: `(*Agent).apply(event)` / `(*Session).apply(event)`; live path = `Append` then
    `apply`; `Recover` = replay + `abortOpenTurns` + `reportLostJobs`. Log resolved target ids
    (`AgentAsked`), log `MonitorDisarmed`, log which queued prompt a turn consumed. Fixes M1.
    Guarded by 1.2.
5.2 **Actor**: `run()` drains a `cmds chan func()` between steps; `Compact`, `SetRole`, `SetModel`,
    `SetVariant`, `AddDir`, `RemoveDir`, `deliverResponse`, `fireMonitor` post closures; read-only
    `atomic.Pointer[agentView]` snapshot for `Info()`/`Status`/`Busy`. `a.mu` shrinks to the inbox.
    Fixes M2 and the lock-order hazards.
5.3 Split `turn.go`: `turn.go` (loop + `step`), `permission.go` (`permits`, `decide`), `escalate.go`,
    `prompt.go` (system prompt sections, `toolNames`, `toolDefs`; `ensureMCP` moved to step start),
    `compact.go`. `runTurn`/`runTool`/`buildContext` each under 60 lines.
5.4 Incremental `project.Projector` owned by the agent (cached history, invalidated in `record`,
    events before `Compacted.ToSeq` dropped from memory); one token estimate per step, preferring
    last real `Usage` and counting tool schemas and signatures.
5.5 Log-write failures: sticky `a.logErr` checked at each step boundary → turn ends with `error`.
5.6 Persist session allows across restart by replaying `PromptAnswered` into `permits`
    (paired with 2.3 so Deny still wins). See decision D4.
5.7 Delete dead members: `yieldFlag`, child `armed`/`IsArmed`, `hasMonitor`, monitor
    `Kind watch|timer`/`Glob`/`Seconds`, `var _ =` import pins, `mode == ""` special case.

### Phase 6 — Daemon, protocol, event log  [L]

6.1 **`daemon.hub`** ✅: per-client bounded outbound queue + writer goroutine with write deadline;
    event JSON marshalled once; overflow → drop client (TUI reconnects and resubscribes from its
    seq). `Append` holds `appendMu` only around the log write + publish enqueue. Fixes P1.
6.2 Atomic replay→live handover ✅ inside `hub.Subscribe`; delete `Log.Subscribe`/`fanout`. Client
    `deliver` never advances past a gap; TUI asserts seq continuity and resyncs. Fixes P2.
6.3 Handler table replacing ✅ the 400-line `dispatch`: `typed[P]` decode helper, domain errors
    (`ErrNotFound`, `ErrInvalid`, `ErrConflict`) wrapped with `%w`, one `toProtocolError`.
    Protocol version moves into the request envelope; the 25 `V int` fields and `withVersion`'s
    triple marshal go away.
6.4 **Event log restructure** ✅ (done with a one-time migration instead of a wipe: PRAGMA user_version 2 adds the title column and backfills it): `internal/event` becomes pure types,
    `internal/eventlog` holds SQLite; reader and writer pools; in-memory `lastSeq`; `AppendBatch`
    (fork in one tx); `sessions` gets `title`, `model`, `state`, `last_seq` columns so
    `session.list` is one query; drop the duplicate index; `payload` stays JSON.
6.5 `pkg/client`: `Options{CallTimeout}`, non-blocking notifications with a `Lagged` signal,
    `Reconnector` (dial with backoff, re-attach, resubscribe from last seq), one `ReplyPrompt`
    taking the params struct, pending-map cleanup on write error, `Notification` type.
6.6 One `daemon.Main(ctx, Options)` ✅; delete `cmd/stavlosd` (see D2); `client.Connect` launcher
    moved out of `cmd/stavlos` and unit-tested; `replaceStale` refuses when agents are live unless
    `--restart-daemon`; `buildid` computed eagerly and passed to the child via env.
6.7 Escalation: `Reply(id, client, Answer)` only ✅; trust prompt gets per-prompt timeouts (no
    default-deny after 3 min); `Trust()` uses `AnswerWhere`.

### Phase 7 — Model layer  [M]

7.1 `internal/model/stream` ✅: one `ReadSSE`, one `Complete(ctx, client, endpoint, hdr, codec,
    onDelta)` owning post/auth/error mapping/ctx-wins/partial-on-error; codex and chatcompletions
    become `Codec`s. Fixes D1.
7.2 In the shared layer ✅: "stream ended before completion" error when no terminal event; pre-first-
    byte retry with `Retry-After` (≤3, jittered); idle watchdog (120 s default, configurable);
    total-bytes cap; one `http.Client` constructor.
7.3 ✅ Registry subscription table (id, display, flow, open, allow, variants) replacing the scattered
    switches; providers built once; `Status.Models` memoised; `auth.Store` mtime-cached.
7.4 ✅ (as `Info.Capabilities{IgnoresMaxTokens}` via `model.Capable`; ReplaysReasoning dropped as nothing
    would read it; Variants stays its own interface) `model.Capabilities{SupportsMaxTokens, ReplaysReasoning, Variants}`; compaction prompt says
    "at most N words" where max tokens is unsupported. `Block.ProviderID`/`Opaque` instead of
    smuggling through `ID`/`Signature`.
7.5 ✅ (Register/KindPlugin kept: the daemon test harness registers a fake provider) Delete dead API-key era: `model/anthropic` + SDK dep (see D1 decision), `auth.Resolve/Source*/
    Credential.Key`, `modelsdev.ProviderInfo`, `openai.New`, `Registry.Register/KindPlugin`,
    `ProviderInfo.Source/Via/Env/Hint`, `openCall.partial`. Rename `openai` → `chatcompletions`.
7.6 ✅ models.dev: return stale cache immediately, refresh in background, swap pointer. The embedded
    fallback now holds the gpt-5 and grok families.

### Phase 8 — TUI  [L]

8.1 ✅ Item-based `Transcript`: `items []item{lines, kind, running, tone}` with id→item maps; no index
    shifting; `itemRange` O(1); fixes the `promptLine` misplacement bug.
8.2 ✅ (the indicator stays in the viewport content; with rows cached, a tick only re-joins them) Per-item render cache keyed by (width, details, expanded, cursor); stream buffer rendered
    separately; spinner/compaction indicator as a footer line so ticks never re-render the
    transcript. Fixes P3.
8.3 ✅ (meta row, tab strip and tab dialogs; overlays already hit-test drawn rows; `layout()`/`keyBarView` left as they are: they never render the chat, and caching them in the value-typed Model would go stale in View) Renderers return geometry (`span` lists); one `hit()` replaces `metaHit`, `tabAt`,
    `tabDialogHit`, `itemAt`, `sidebarClick`'s re-render; `layout()` only on geometry changes;
    `keyBarView` computed once per Update. Makes permission/question option clicks work.
8.4 Generic `listDialog` component for the six tabs, sidebar and overlay: cursor/wrap/filter/empty
    text; the five `*Key` handlers and the dead `listKey` collapse into it.
8.5 `Model` split into `sessionState` (zeroed wholesale on bind), `dialogs`, `uiPrefs`; `reply()`
    and `claimThen()` helpers; msg/cmd near-duplicates merged (`sessionsMsg`, `rolesMsg`,
    `providersMsg`, tick helper).
8.6 `EventLines` as a renderer table; `handleKey` split (`inputKey`); dead code removed
    (`notice`, `helpLines`, `todoLabel`, `mcpLabel`, `monitorGlyph`, `prettyJSON`, stale hotkey
    text); test-only exports to `export_test.go`.
8.7 Package split: `tui/transcript`, `tui/render`, `tui/dialog`, `tui`.
8.8 Tests: `findLine` helpers and relative-order assertions replace row-index/line-count pins;
    `TestSidebarNav` and `TestTabCyclesFocus` split into subtests.

### Phase 9 — Cleanup and docs  [S]

9.1 `stavlos init`: model default empty, create `roles/`, config via `config.File` marshal; fix
    the `plugin` message.
9.2 README: split the 4.7k-char paragraph into sections; add missing commands; drop the
    "/providers to paste a key" line. PRD: remove `agent_finish`/`spawn`/`write`/`edit`, fix §6.4
    table, §8.4 vs §10.7 contradiction, socket path, `agents/` → `roles/`, `$schema` claim, and
    mark plugin/Discord/fork as post-v1 or implement fork in the TUI.
9.3 Delete the stale 23 MB `./stavlosd` binary from the working tree.
9.4 Memory/notes updated; `gocyclo -over 30` and `staticcheck` clean added to the gate.

---

## 2. Decisions (taken 2026-09-14)

D1 delete both · D2 delete · D3 ask when unconfigured · D4 persist · D5 defer · D6 defer (PRD notes the gap) · D7 do it, last · D8 yes.

| # | Question | Recommendation (adopted) |
|---|----------|-------------------|
| D1 | Delete `internal/model/anthropic` and the `anthropic-sdk-go` dependency? Nothing imports it; PRD §8.4 says no API keys. | **Delete.** Port its two neutral-block tests to codex/openai first. If Anthropic access is wanted later it needs a credential path that does not exist today. |
| D2 | Delete `cmd/stavlosd`? README documents only `stavlos daemon`. | **Delete**, keep one `daemon.Main`. |
| D3 | `web_search` with no `search` config currently ships every query to Exa's hosted MCP with `allow` by default. Keep, or default to `ask` (with allow-for-session) when unconfigured? | **Ask when unconfigured**, allow when the user configured a provider. One click per session; no silent third-party traffic. |
| D4 | Should `allow_always`/`allow_prefix` survive a daemon restart? Today they vanish silently. | **Persist** by replaying `PromptAnswered` (5.6), only after Deny-precedence (2.3) lands. |
| D5 | Move `event`, `protocol`, `pkg/client` under `pkg/` so a third-party Go client imports no `internal`? | **Defer**; do the `event`/`eventlog` split (6.4) now, which already removes SQLite from the client's dependency tree. |
| D6 | Real shell sandbox (bubblewrap/landlock) keyed on the agent's dirs, instead of the path heuristic? | **Defer to post-v1**; 2.1 + 2.8 close the cheap holes. Flag in the PRD as the known gap. |
| D7 | Phase 8.7 (TUI package split) is the highest-churn item with the least user-visible payoff. Do it, or stop at 8.6? | **Do it**, but last, once 8.1–8.6 have made the seams obvious. |
| D8 | Protocol version moves into the request envelope (6.3) — wire-visible. Fine since both ends are in-tree? | **Yes.** |

---

## 3. Order and estimate

1 → 2 → 3 → 4 → 5 → 6 → 7 → 8 → 9. Phases 2, 3 and 7 are independent of 5/6/8 and could be
reordered if you want visible security wins first (2) or the daemon stall fix first (6.1).

Rough commit count: Phase 1 ~6, 2 ~15, 3 ~5, 4 ~8, 5 ~10, 6 ~12, 7 ~8, 8 ~14, 9 ~4 — about 80
commits. Every phase leaves the tree shippable.

## 4. What stays as it is

Log-as-source-of-truth with projection repair; the context tree (`s.ctx → a.ctx → turnCtx`);
`end()` flipping state before logging; mailbox semantics (responses never enter a running turn,
steers at the model-call boundary); `hasDef` refusing tools not offered this step; dial-time SSRF
check; `htmlToMarkdown` tag stripping; `apply_patch` staging and lenient matching; shell process
group + WaitDelay + 15 s adopt design; PKCE/state/loopback OAuth mechanics and 0600 token store;
the escalation state machine; newline JSON-RPC with per-session seq and reconcile; the Elm-style
TUI discipline (`Update` never touches the client), `Command` registry, `focusOrder`/`setFocus`
ownership, `textareaWrap`, fold/preview UX; the daemon end-to-end test harness.
