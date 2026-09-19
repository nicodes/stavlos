# Ground-up refactor plan (2026-09-19)

Stavlos does what its owner wants. It got there in seven days and 560
commits, by adding the next thing to whatever was there. This plan rethinks
what is under the features, from first principles, for security, performance,
maintainability and one-place-for-each-thing, **without changing anything a
person using it can see**.

It supersedes [pre-plugin-refactor.md](pre-plugin-refactor.md) (2026-09-17),
whose findings it re-verified and carries forward, and it is built on
evidence: five read-only audits of `main` at `2c7115b`, measurements of the
repository and of a real 163 MB event log, and the bugs of the past week.

## Contents

1. [Is this the right time?](#1-is-this-the-right-time)
2. [The evidence](#2-the-evidence)
3. [What the evidence says: six principles](#3-what-the-evidence-says-six-principles)
4. [The target shape](#4-the-target-shape)
5. [Phases](#5-phases)
6. [What must survive](#6-what-must-survive)
7. [How it is delivered, and how it fails](#7-how-it-is-delivered-and-how-it-fails)
8. [Decisions needed before work starts](#8-decisions-needed-before-work-starts)
9. [Appendix: findings](#9-appendix-findings)

## 1. Is this the right time?

Yes, for three reasons the evidence gives, and with one warning.

- **The feature set has stopped moving.** The owner's words: everything is
  pretty much working as wanted. A refactor under a moving feature set is a
  merge conflict with yourself: `internal/tui/view.go` was edited in 224
  commits this week and `model.go` in 172.
- **The last plan was never run, and its findings are all still open.** The
  pre-plugin plan listed about sixty findings, most of them verified. Two days
  and about seventy commits later the security audit found no commit at all
  in `peercred`, `policy`, `shellcmd`, `sandbox`, `oauth` or `auth`. Features
  outran it. Several findings got worse: the path a tool call names is now
  resolved five times per call (it was three), the hand-written "wake the
  right agents" sites went from a concern to 16, the TUI's poll went from one
  RPC every three seconds to four or more.
- **This week's bugs were structural, not careless.** Each came from a
  decision that lives in more than one place (section 2.3). Fixing them one at
  a time is what the week has been.

The warning: a plan that is not run is worse than no plan, because it
reassures. Section 7 is about that.

## 2. The evidence

### 2.1 The repository

| | |
|---|---|
| Source / tests | 42,966 / 25,772 lines of Go, plus 949 of TypeScript |
| History | 560 commits in 7 days; 289 of them touch the TUI |
| Largest package | `internal/tui`: 15,226 lines, 35% of all source |
| Most edited files | `tui/view.go` 224 commits, `tui/model.go` 172, `tui/transcript` 134, `protocol/types.go` 61, `agent/turn.go` 59, `config/config.go` 56 |
| Coupling | `daemon` and `agent` each import 15 internal packages; `model`, `protocol` and `event` are imported by 15, 11 and 11 |
| Complexity | 1,856 functions; 30 at cyclomatic complexity 25 or more, 18 of them in the TUI; 5 sit exactly at the gate's limit of 30 |
| Longest functions | `tabBodyRows` 104 lines, `promptBox` 98, `sidebarBody` 94, `command` 93, `applyFile` 88 |
| Tests that sleep | 37 `time.Sleep` calls (the last plan counted 29) |
| Gate | `scripts/check.sh`: strong (exhaustive, staticcheck, race, suite three times), but no CI runs it, the race list is hand-kept and omits 7 packages, there is no fuzz target, no `govulncheck`, no `deadcode` |

The complexity limit deserves a sentence. It is 30, and the maximum in the
repository is 30: the limit does not bound the code, the code is parked
against it. Three times this week a function reached 31, or was about to, and
a helper was cut out of it to keep the gate green (`applyFile` twice,
`command` once). That is decomposition by accident.

### 2.2 A real event log

Measured read-only on the owner's machine: four days of use.

| | |
|---|---|
| Size | 163.5 MB, 110,314 events, growing about 27,000 events and 33 MB a day |
| Channels | 26, none archived, so a daemon start reads and folds **all of it** |
| Skew | the top three channels hold 92%: 54,835, 28,773 and 17,620 events |
| What fills it | `tool.finished` 57% of bytes, `assistant.message` 31% |
| A client opening the largest channel | is sent all 54,835 events (59 MB), to show one agent of 154 and a chat |
| Usage charts | parse JSON out of every `assistant.message` payload in range: 64–120 ms today, linear in the log's age, re-run every 3 s while a chart is open |
| One client's worst-case queue | 16,384 messages × up to 130 KB each: about 2 GB |
| A schema version bump | **deletes the file** (`eventlog/log.go:27-31`: "unreleased, so there is nothing to keep") |

The last line matters to this plan more than to any before it: there is now
something to keep, and this refactor changes the schema.

### 2.3 This week's bugs, by cause

| Bug | What happened | The structural cause |
|---|---|---|
| #36 | a variant followed an agent to a model that rejects it | the rule for "move an agent to a model" is written at four call sites; the fourth was added without the check the helper silently assumed |
| #38 | a dropped client's prompt claims lived on for two minutes | nothing owns what a connection holds, so nothing released it |
| #39 | clients reading as fast as they could were dropped as "not reading" | history (which waits) and live traffic (which never does) share one queue; patched with a ratio, not redesigned |
| Grok's 403 | a used-up plan ended six turns instead of moving the agents | what a provider's refusal looks like is matched by substring inside the transport every provider shares |
| A stale ChatGPT meter | a two-day-old reading shown as current | one provider's usage was observed, the others' not read at all: per-provider knowledge with no place to live |
| Nav layout | three changes this week each touched three files and five tests | rows are found by index arithmetic (`navTopRows`, `sidebarWebRow`, …) mirrored in the builder, the click handler, the hover handler and 42 test lines |
| Two README merge conflicts | unrelated edits collided | paragraphs are single lines of up to 2,423 characters |

### 2.4 Five audits

Each re-verified the old plan's findings in its area and looked for new ones,
marking every finding **V** (the code is unambiguous) or **S** (reasoned, needs
a failing test first). Six of the most severe were re-checked by hand
against the code while this plan was written, and all six held: the recap
state written outside the fold, the ignored `turn.ended` write, the schema
bump that deletes the log, the Discord code fences, the mode switch that
answers waiting prompts, and the sheet caps that bind one tool only. The full
list is the appendix. The headlines:

- **Security**: every Phase 1 finding of the old plan is open. New and high: a
  switch to `yolo` allows every waiting permission prompt, control-file asks
  included, and a switch to `auto` allows waiting egress asks; the normal
  decision path allows neither. Two pieces of code decide what a mode means,
  and they disagree.
- **Runtime**: the design rule is that only `apply` changes channel state.
  It is broken in one place (recap), and the rule that every side effect has
  a record is broken in about a dozen (`_ =` on commits, among them the
  human's "always allow"). One of them wedges an agent until restart.
- **Daemon and storage**: cost is proportional to the whole log, not to what
  is in use: at start, when a client opens a channel, and on every chart.
- **Clients**: the same events are turned into something readable three
  times, with three different results. The web client handles 14 of 34 event
  types, by hand-copied string. About three quarters of the TUI's 2,000-line
  transcript package is presentation-neutral logic trapped in a terminal type.
- **Models and config**: the provider table knows seven things about a
  provider; seventeen more are keyed by the provider's name elsewhere.
  Configuration is written to disk in six ways, three of which edit JSONC
  differently; one is a regular expression.

## 3. What the evidence says: six principles

Every finding above is an instance of one of these. They are the plan; the
phases are their order.

**P1. One owner for each decision.** Counted today:

| Decision | Places it is made |
|---|---|
| is this path inside the working set | 6 |
| expand and resolve a path | 3 |
| which files steer the harness (control files) | 2 lists, which differ |
| what a permission mode allows | 2, which disagree |
| write a config or state file | 6 config writers, 9 temp-file sites, 3 non-atomic writes |
| what is true of a provider | the table, plus about 17 sites keyed by name |
| is this provider at its limit | 5 |
| move an agent to a model | 4 |
| what may a connection call | 3 (descendant check, version, the web allowlist) |
| how the harness speaks to an agent | 4 idioms |
| turn an event into something a human reads | 3 |
| summarise a tool call | 3 |
| start, stop and report a background service | 3 lifecycles |
| where a row of the nav is | 3 (build, click, hover) plus the tests |

**P2. The log is the only truth, and a failed write is somebody's job.**
State changes only in the fold. A side effect happens after its record, never
before and never instead. One commit function owns what a failed write means.

**P3. Decisions are pure functions of a snapshot.** Permission, model choice
and variant fitting take a value and return a value: no locks held across
calls into other components, table-tested, fuzzable. The two that already
work this way (`pickModel`, `fitVariant`) are the two with the best tests.

**P4. Boundaries are declared where the thing is defined.** A method says
what scope may call it. A tool says what it writes, whether it sends data out,
and what follows a successful run. A provider says how it routes, caches,
limits and reports usage. A directory an agent may touch is a typed root, not
a string compared to other strings.

**P5. Clients draw; they do not interpret.** One fold turns events into
presentation-neutral items. The TUI, Discord and the browser render items.
State that changes is pushed, not polled.

**P6. Cost is proportional to what is in use.** A channel nobody has opened
costs nothing at start. A client gets a snapshot and a tail, and pages
history on demand. A chart reads a narrow table. A queue is bounded in bytes.

## 4. The target shape

Names are indicative. What matters is what each owns and what it may import.

```
internal/
  core/        event registry (types, payload codecs, input kinds with their
               rules: wakes, mid-turn, owes a reply, how it is framed), ids
  log/         the event log; forward migrations; derived tables kept in the
               same transaction (channels, agents, usage); small daemon state
  runtime/     the channel state machine (apply); commit and post-commit
               dispatch; the turn loop as prepare / call / onError / runTools;
               failover as one component; constructors for every input
  decide/      the permission pipeline, pure:
               Decide(Request{Principal, Tool, Subject, Snapshot})
                 -> Verdict{Verb, Why, Sticky, By}
  pathx/       expand, resolve, within; typed roots (os.Root)
  shell/       the command grammar
  sandbox/
  tools/       typed Tool[I]; a ToolSource registry (built-in, MCP, later Lua);
               tool metadata (writes paths, egress, after-run hooks)
  provider/    one Descriptor per provider (wire, base URL, cache routing,
               reasoning replay, variants, quota, limit detection); adapters;
               credentials; catalog; the plan-usage tracker, which alone says
               whether a provider is available
  configfs/    read a layer once into a snapshot, hash and parse the same
               bytes; Validate; a pure Merge; one writer
  present/     events -> Items; tool summaries; wording. Its JSON shape is the
               contract the TypeScript client is generated from
  daemon/      the hub (connections, byte-budgeted lanes), a Service registry,
               the method table with scopes, projects, trust
  wire/, api/  the protocol, split from domain types; capabilities on attach
  daemonctl/   connect, replace, restart: shared by every client
  tui/         sub-models, a stack of panes, a keymap, declarative nav rows
  discord/, web/   renderers of Items
```

Two things are deliberately **not** in the target: a per-agent actor model
(the single state machine per channel is the design's best idea, section 6),
and a second process for anything. It stays one binary.

## 5. Phases

Every step is its own commit behind the gate. A step marked **†** is a
security fix that cannot avoid a visible change (an extra ask, a refusal) and
waits for a decision (section 8). An **S** finding gets a failing test first;
if it cannot be made to fail, the step is dropped.

Sizes are relative: S is an afternoon, M a day or two, L most of a week.

### Status of phases 0 and 1 (pull request `refactor/phase-0-1`)

| Step | State | Notes |
|---|---|---|
| 0.1 golden behaviour | **partly** | Done: six whole TUI frames (`internal/tui/testdata/golden`, drawn at a pinned clock) and the model-facing prompt, tools and harness note for a main agent and a child (`internal/agent/testdata/golden`). To do: Discord payloads, CLI output, the web reducer against a real log. |
| 0.2 test isolation | **done** | `paths` gives any test binary directories under the temp directory; `testutil.Isolate`; the agent and daemon harnesses use it. Injecting directories into `Channel` and the navigation store moves to phase 2. |
| 0.3 no sleeping tests | to do | Needs the dispatch hook of phase 2.2; the test doubles move with it. |
| 0.4 the gate | **done** | tidy, `deadcode -test`, `govulncheck`, the complexity ratchet with a baseline of 20 functions, fuzzing, `-race ./...`, and CI. |
| 0.5 fuzz targets | **done** | Eight targets. Their first seconds found three bugs, fixed here: `web_fetch`'s address guard passed a zoned loopback (`::1%lo`) as public; `textsafe` let non-UTF-8 bytes through; the patch parser accepted a section naming no file. Also a prefix that did not cover its own command. |
| 0.6 migrations | **done** | Forward migrations with a backup; a log this build cannot read is left untouched and reported; `STAVLOS_RESET_LOG=1` is the only thing that deletes one. |
| 1.1 mode switch | **done** | A prompt says why it asks (`sticky`, `egress`); the switch answers only what the mode would. Test fails without the fix. |
| 1.2 permits and control files | **done** | Test fails without the fix. |
| 1.3 peer identity | **done** | Fails closed; the daemon is a subreaper with a reaper that never takes an exit status from a waiter. |
| 1.4 lost writes | **done** | A lost `turn.ended` no longer wedges the agent; a tool never runs without its start on the record. |
| 1.5 recap | **done** | Folded from the log. |
| 1.6 sheet limits | **done** | Hold for every tool; every read bounded. |
| 1.7 instructions | **half** | Instructions that appeared after trust are withheld and raise the prompt. The sandbox's half (the read-only guard skips paths that do not exist yet) needs placeholder files in the repository, a visible change: it moves to 3.7. |
| 1.8 Discord fences | **done** | |
| 1.9 the log is never deleted | **done** | With 0.6. |

### Status of phases 2 to 7 (pull request `refactor/ground-up`, stacked on the first)

Every row marked done is one commit behind the whole gate. Rows marked open
are not started: they are the large redesigns, and each changes what every
client receives, so they want to be reviewed (and run against a real daemon)
on their own rather than arrive in one diff.

| Step | State | Notes |
|---|---|---|
| 2.1 event registry | **half** | Input kinds declare their rule (`event.InputRule`) and a test pins the vocabulary. Event types are not yet registered with payload and origin. |
| 2.2 one commit, one dispatch | **done** | `commitLocked` dispatches its own wakes; `commitFactLocked` folds what happened even when the write fails. |
| 2.3 runtime state derived | open | |
| 2.4 one system input | **done** | Framed from the rule; the "[harness]" text prefix is gone. |
| 2.5 turn loop | open | |
| 2.6 `Host` / `Env` split | open | |
| 2.7 channel resources | **done** | `Agent.release`. |
| 3.1 `pathx`, `os.Root` | open | |
| 3.2 pure `Decide` | open | The non-† part still depends on 3.1's parsed paths. |
| 3.3 shell grammar | **done** | Keywords, launcher flag arity, `eval`, here-strings, same-line literals. A prefix is offered only for words that read back as themselves (found by the fuzzer). |
| 3.4 trust snapshot | open | |
| 3.5 one config writer | **done** | `statefile.WriteAtomic` everywhere but the patch tool (its own semantics) and `init` (a new file). |
| 3.6 who may call what | **half** | Scope is on the route and dispatch enforces it; a web connection cannot name itself. The owner token and the `internal` scope for the Discord bridge are open. |
| 3.7 sandbox level | open | † |
| 3.8 web listener | **half** | Sign-out ends the session's sockets; a sheet is served only to a frame. Moving the session out of the cookie and the code out of `argv` are open. |
| 3.9 HTTP client | **done** | `httpx.New`, redirects stay on the host asked. |
| 4.1, 4.3, 4.4 providers | **done** | `chatcompletions.Traits`, the quota source table, the `market` snapshot and `retarget`, `IsLimit` out of the transport. |
| 4.2 registry split | open | |
| 5.1 lazy channels | open | |
| 5.2 snapshot and pages | open | |
| 5.3 lanes in bytes | **half** | The queue is bounded at 64 MB; separate lanes wait for 5.2, which removes pushed history. |
| 5.4 dispatcher | **half** | An event nobody watches is not encoded. Encoding still runs on the writer. |
| 5.5 usage table | **done** | Migration 5 to 6, checked against a copy of the real log: identical totals. |
| 5.6 to 5.9 | open | |
| 6.5 nav rows | **done** | Rows by id; no row arithmetic in the tests. |
| 6.1 to 6.4, 6.6, 6.7 | open | |
| 7 | open | |

### Phase 0: lock the behaviour, arm the gate (M)

Nothing else is safe without this, and it is the phase the last plan never
got. "The user notices nothing" must be checked by machine.

| # | Step |
|---|---|
| 0.1 | **Golden behaviour.** TUI frames for a scripted channel through `tuitest` (home, chat, agent chat, every dialog, inline permission and question cards, the nav, the config editor); Discord payloads for every card; CLI output of every subcommand; the model-facing system prompt and harness note; tool definitions (exists); the web reducer run against events exported from a real log. A refactor step that changes a golden file is a bug or a † step. |
| 0.2 | **Test isolation.** `testutil.Isolate(t)` replaces 55 `t.Setenv("STAVLOS_…")` lines in 24 files (33 set the config dir, 6 the data dir). Directories are injected into `Channel` and the TUI's navigation store; `paths.*` is called from `cmd` only. Today `agent.SheetDir()` reads `paths.DataDir()` directly, and four test harnesses never set it. |
| 0.3 | **No sleeping tests.** A deterministic "agent idle and inbox empty" barrier from the dispatch hook replaces the 50 ms "nothing else happens" negatives. The two `fakeModel`s, the `fakeProvider`, `fakeHost`, `waitUntil` and `eventually` move to `testutil`. |
| 0.4 | **The gate.** `go test -race ./...` instead of a list that omits `cmd/stavlos`, `buildid`, `instructions`, `peercred`, `project`, `protocol` and `sandbox`; `govulncheck`, `deadcode`, `go mod tidy -diff`, `shellcheck`; `BenchmarkChatRedraw` with a ceiling; a short run of each fuzz target. `gocyclo` ratchets 30 → 25 now, → 20 by the end. A CI workflow runs it, with Node, so the web step is never silently skipped. |
| 0.5 | **Fuzz targets** (none exist): the shell grammar, the patch parser, URL and address checks, JSONC editing, `textsafe`, path containment, and "any protocol string into `View()` yields only the TUI's own escape codes". |
| 0.6 | **Migrations instead of deletion.** Forward migrations keyed on `user_version`; deleting the log only behind an explicit flag. `schemaVersion` is 5 while the vocabulary is "schema 6"; a test hashes the event type table so a vocabulary change without a migration fails. (Decision D1.) |

### Phase 1: stop the bleeding (M)

The high-severity findings that need no new structure, fixed surgically with
a failing test each, **before** weeks of restructuring leave them open. The
later phases replace these fixes with the structure that makes them
impossible.

| # | Finding | Fix |
|---|---|---|
| 1.1 | **V** A switch to `yolo` or `auto` answers waiting prompts the decision path would never allow: control-file edits, and in `auto` egress (`daemon/handlers.go:199-205`). | Waiting prompts are re-decided, not blanket-answered. |
| 1.2 | **V** A remembered "allow for this channel" answers a control-file ask (`agent/permission.go:105-114`). | Control-file asks ignore permits. |
| 1.3 | **V** `peercred.DescendsFrom` returns false on any `/proc` error, which grants full rights; a double fork escapes it; the daemon is not a subreaper (`peercred/peercred.go:55-62`). | Fail closed; `PR_SET_CHILD_SUBREAPER`. |
| 1.4 | **V** A failed `turn.ended` write leaves `inTurn` true: the agent takes no turn until the daemon restarts (`agent/turn.go:109`, `:43`). A tool runs after a failed `tool.started` write (`permission.go:38`). | A failed write of either ends the turn in memory and faults the channel visibly. |
| 1.5 | **V** Recap state is set outside the fold: an open recap is asked for again after a restart (`agent/recap.go:77`). | A `recap.asked` event, folded. |
| 1.6 | **V** `apply_patch` may write any file name of any size into the sheets directory; the caps bind the `sheet` tool only; `Sheet()` reads with no bound (`agent/sheets.go`). | Name pattern and size enforced for every tool that writes there. |
| 1.7 | **V** Nested `AGENTS.md` files are read live and injected without being in the trust hash; the sandbox's read-only guard skips paths that do not exist yet, so a command can create one (`agent/instructions.go:27`, `sandbox/helper.go:102`). | Instructions newer than the trust snapshot are withheld and raise the trust prompt; placeholders are bound read-only. |
| 1.8 | **V** Discord wraps tool input in code fences without neutralising fences inside it, and imports no `textsafe` (`discord/permission_format.go:40`). | Fence-safe and sanitised at the one formatting point. |
| 1.9 | **V** A schema bump deletes the log. | Done in 0.6; listed here because it is the one that loses data. |

### Phase 2: the core contracts (L)

The seams everything else hangs on. Mostly inside today's `internal/agent`.

| # | Step | Findings it closes |
|---|---|---|
| 2.1 | **An event registry.** Each type declares its payload, its origin and whether clients draw it; each input kind declares whether it wakes, reaches a running turn, owes a reply, and how it is framed to the model. The `exhaustive` lint stops exempting `event.Type`. | A new event type touches about 14 files today and a new input kind 9 sites, with no lint to catch a miss; `InputResume` needed all of them. |
| 2.2 | **One commit, one dispatch.** `commit(events) error` owns the failure policy. Everything that follows a record (waking agents, renaming a temp file into place, changing runtime handles, notifying clients) runs in a post-commit dispatch. | 32 `commitLocked` calls, 19 discarding the wake list, 16 hand-written signal sites; jobs deleted before their commit; sheets written before their event, under the channel lock; a dozen swallowed commit errors. |
| 2.3 | **State only through `apply`.** Runtime-only fields that recovery cannot rebuild are derived by the history builder (`instructed`, `compactNext`, the context gauge). | Recap; instruction notes delivered again after a restart. |
| 2.4 | **One system input.** `InputSystem{Origin, WakeRule, Debt}` replaces four idioms: resume, reminder, recap (a prompt with the human's name on it) and the directory notice (an agent message with no sender, which reads "message from agent ,"). | |
| 2.5 | **The turn loop as a small machine.** `step` does about ten things. It becomes `prepare() → plan`, `call()`, `onError()` with failover as a policy object, `runTools()`. Failover state, now on the turn, in the fold and in the registry, becomes one component with one record. | |
| 2.6 | **`Host` and `tools.Env` split by concern.** `Host` mixes the log, transport, the model catalogue, the human and the project; `Env` mixes per-call data with six capabilities where nil means unavailable. | |
| 2.7 | **Channel resources.** What a channel owns (MCP servers, jobs, the scratch directory, sheets) is closed by one teardown, not three hand-written ones. | |

### Phase 3: security by construction (L)

| # | Step | Findings it closes |
|---|---|---|
| 3.1 | **`pathx` and typed roots.** One expand, one resolve, one within; every working directory, the sheets directory and the config tree are `os.Root`s. A tool parses its input once into a value that carries resolved paths. | Containment written 6 ways; a call's path resolved 5 times, with a window between each; raw model strings matched by policy (`./secrets/key` passes `deny read secrets/**`); `read` on a FIFO blocks forever; 1.6 becomes structural. |
| 3.2 | **`decide`, pure.** Ordered stages over one snapshot: base policy → overlays that only tighten → sticky rules (control files, hidden paths) → looseners (permits, hosts, mode) → the boundary. A `Verdict` says why it asks, so a mode change re-runs `Decide` (1.1 becomes structural) and a sticky ask cannot be loosened (1.2). One control-file list. Native tools are denied the sandbox's hidden paths in every mode **†**. Permits are keyed by principal kind and role. | 1C.1–1C.9 of the old plan. |
| 3.3 | **The shell grammar.** Keywords and `!` stripped; launchers a table with their flag arity; `eval`, `source`, here-strings to a shell, `coproc` and a non-literal command word make a line *opaque*: it can match a deny, never an allow. Tokenised once, shared by policy, prefix offers and the boundary. | `if true; then rm -rf /; fi`, `eval`, `sudo -u root …`, `a=rm; $a -rf /` and six more are allowed under `deny rm -rf *` today. |
| 3.4 | **Trust over a snapshot.** A project layer is read once into `map[path][]byte`; hash and parse use the same bytes; every loader reads from it, which is also the one list of loaders (two today, which differ). Size and count caps, no symlinks, regular files only. | Files hashed then read again; a clone that can stall the daemon before any prompt; a config edit that trusts whatever is on disk afterwards. |
| 3.5 | **`configfs`: one writer.** `EditJSONField`, atomic, 0600, through an `os.Root`, staged under the data directory. A path-kind allowlist for what the socket may write. | Six writers, three JSONC techniques, one of them a regular expression in which `$` in a model id expands. |
| 3.6 | **Who may call what.** Each method's descriptor carries its scope (`owner`, `bridge`, `web`, `internal`), how its target is found, and whether it mutates; dispatch enforces all of it. The web allowlist becomes `scope <= web`; the in-process Discord bridge gets `internal` instead of a pid exemption; owner scope needs a token from a 0600 file the sandbox hides. A listener fixes a connection's identity and tier; `attach` cannot. | Old 1A.3–1A.5; a web client can attach as `discord` today. |
| 3.7 | **The sandbox says what it enforces.** The probed level is channel state, shown to clients, and what happens below `Full` is a decision **†**. A read allowlist makes the read boundary real **†**. | Fails open with one log line; Landlock restricts writes only. |
| 3.8 | **The web listener.** The session travels in the WebSocket handshake from page memory, not an ambient cookie a dev server on another localhost port receives; the one-time code reaches the browser through a short-lived local redirect, not `argv`; sheets are served only to a frame; the CSP is built from configured names; sign-out closes that session's sockets. | Security N3, N4, N8, N9, N11 (mostly **S**: tests first). |
| 3.9 | **One HTTP client constructor** with a redirect policy and a per-use host allowlist, for model calls, OAuth, quota and fetch. | Only `web_fetch` has a redirect policy. |

### Phase 4: providers (M)

| # | Step | Findings it closes |
|---|---|---|
| 4.1 | **`provider.Descriptor`.** Wire kind, base URL, cache routing, reasoning replay, variants, quota (URL, auth, parser), limit detection, allowed models. About 17 of roughly 30 by-name sites go; what remains is identity. | #36's cousin waiting to happen; the 403; "supports openai and xai" in an error when there are four. |
| 4.2 | **The registry, split**: the provider table, credentials with refresh, the catalog, a plan-usage tracker, a thin resolver. | Seven jobs behind one mutex. |
| 4.3 | **Availability in one place.** The tracker alone answers "is this provider limited, and until when"; the picker takes an immutable snapshot and holds no lock while it asks. `retarget(agent, role, model)` is the one way an agent's model changes. | "Limited" computed 5 ways; the channel lock held across `CheckModel` per candidate, which stats `auth.json`, on every 20 s tick for every parked agent; model retargeting written 4 ways. |
| 4.4 | **Limit detection leaves the transport.** `stream` calls `Descriptor.IsLimit(status, body)`. | The bare word "quota" on a 403 can misclassify a refusal. |

### Phase 5: storage, the daemon and the wire (L)

| # | Step | Findings it closes |
|---|---|---|
| 5.1 | **Lazy channels.** The channel list comes from the `channels` table; a channel is folded on first use, from a persisted state snapshot plus the tail. The index gains agent → channel and "needs a wake", so an unloaded channel can still be routed to, recapped and resumed. | Start reads all 163 MB; nothing is ever archived. |
| 5.2 | **Snapshot, tail and pages.** `subscribe{tail: N, agent?}` and `history{before, agent, limit}`; `reconcile` becomes the snapshot. An index on `(channel, agent, seq)`. | 54,835 events pushed to show one agent. |
| 5.3 | **Lanes, in bytes.** Replies and prompts, live events, and history are separate lanes with a byte budget; history is paged request and response, never pushed. | The design behind #39; a 2 GB worst case. |
| 5.4 | **A dispatcher off the writer.** The log writer hands batches to a dispatcher; events are encoded per subscribed channel, once. | Every append in every channel waits for JSON encoding and per-client mutexes, even with no subscribers. |
| 5.5 | **A usage table**, written at commit beside the index that already exists. | Charts parse 41 MB of payloads. |
| 5.6 | **Push, not poll.** `channel.changed`, `agent.state`, `service.status`, `plan.usage`, `usage.delta`. | Per TUI, for ever: four or more RPCs every 3 s, Discord status every 2 s, web status every 5 s; the Discord worker and the browser poll too. |
| 5.7 | **A `Service` registry**: `Start, Stop, Status, ConfigChanged`, with `service.*` methods and one notification. | Discord, the web server and the plan poller each have their own lifecycle; close order is hand-written. |
| 5.8 | **The daemon, split** into hub, projects, trust, logins and services, each with one lock and no I/O under it. One `statefile` helper; small daemon state moves into the log's database. | Six mutexes with overlapping duties; at least 8 hand-rolled temp-and-rename writers and 3 plain writes; `navigation.json` is written by the *client* into the daemon's directory. |
| 5.9 | **The protocol, layered**: `wire` (envelope, errors, version), `api/*` by area, domain types in a leaf package. `attach` returns capabilities. A build mismatch restarts the daemon only when it is idle, or when forced. `daemonctl` holds connect, replace and restart, 200 lines that live in `main` today. | Any rebuild kills live turns. |

### Phase 6: presentation (L)

| # | Step | Findings it closes |
|---|---|---|
| 6.1 | **`present`.** Events fold into `Item{kind, who, to, title, body, tone, state, refs}`; one tool-summary table keyed by tool; one wording table. | Three folds that disagree (the web draws an input when queued, the TUI when taken; Discord drops a model move's reason); "Allow for this channel" and "Allow in this channel". |
| 6.2 | **The transcript, split.** The fold moves to `present`; `render` turns Items into lines. | About 75% of `transcript.go` is neutral logic typed as terminal lines (**S**: the proportion). |
| 6.3 | **TypeScript from Go.** The Item schema and the event-name union are generated; the reducer's switch is exhaustive by type. | `payload: any`; 14 of 34 event types handled. |
| 6.4 | **The TUI's model, split**: Nav, Services, ChatView, Composer, Status and a stack of panes (`Enter, Leave, Key, View, Hints`), each with pointer receivers and its own update. | About 97 fields; three separate modal systems; `setFocus` at complexity 29. |
| 6.5 | **Declarative nav rows**: `[]navRow{id, render, spans, visible}` built once a frame; hit-testing through the `hitSpan` that exists; tests look rows up by id. A `serviceWidget` interface supplies the Clients rows. | 42 test lines of row arithmetic; `discord.go`, `web.go` and `planusage.go` repeating one pattern. |
| 6.6 | **A keymap**: key → action per pane, hints derived from bindings; commands carry their `Run`. | 27 bindings, about 87 `key.Matches` sites, and about 81 raw `case "x":` comparisons that bypass them; a separate hand-written legend. |
| 6.7 | **Discord and the browser consume Items.** | `mirror` at complexity 30; Discord's own copy of the "sole main is implicit" rule. |

### Phase 7: what is left (M)

Vocabulary ("preset" appears 170 times in non-test code after a rename the
old plan records as done; "archetype" 20); dead code (the plugin stubs in
config, registry and `main`; `KindPlugin`; `IDToken`); legacy reply tracking;
the standalone Discord binary and its systemd unit, which exist only for each
other; `pkg/client`, which nothing outside the module can import (D5). Docs:
the PRD still presents go-plugin as the architecture, calls the sandbox a
roadmap item and says there are two providers; "three-layer" config is four;
the README is split by audience and wrapped at 80 columns, so two edits to
one paragraph stop colliding. A LICENSE.

### What "done" looks like

| Measure | Today | Target |
|---|---|---|
| Path containment implementations | 6 | 1 |
| Config writers / JSONC techniques | 6 / 3 | 1 / 1 |
| Places a mode's meaning is decided | 2 | 1 |
| Provider sites keyed by name | about 30 | identity only, about 13 |
| Files a new event type touches | about 14, unlinted | its registration, linted |
| Commit call sites that discard an error | about 12 | 0 |
| Data read at daemon start | the whole log (163 MB) | the channel table |
| Events sent to open the largest channel | 54,835 | a snapshot and a tail |
| Idle RPCs per TUI | 4+ every 3 s, 1 every 2 s, 1 every 5 s | 0 |
| Functions at complexity 25 or more / the limit | 30 / 30 | 0 / 20 |
| `time.Sleep` in tests | 37 | 0 |
| Test lines that name a nav row by number | 42 | 0 |
| Event types the browser handles | 14 of 34, by hand | all, generated |
| Fuzz targets / CI | 0 / none | 7+ / yes |

## 6. What must survive

The audits were asked for this as firmly as for faults. A ground-up refactor
that loses these has made things worse.

- **The fold.** `apply` with `effects`, commit-then-apply under one lock,
  recovery reusing it, no per-agent lock. Everything P6 needs is cheap because
  of this.
- **The single request table**, of which "owes" and "waits on" are two views,
  and transactional events: spawn with task, post with deliveries, job result
  with input.
- **Recovery that closes what a stop left open**, and a missing role that
  never widens what an agent may do.
- **The pure, table-tested deciders**: `pickModel`, `readPlan`, `fitVariant`,
  and `roleView` as a per-step snapshot.
- **The log's writer**: single-writer group commit, the `Barrier` that gives a
  gap-free handover from replay to live, per-subscription sequence dedup, the
  read-only second handle, the index kept in the event's transaction.
- **Typed methods**: `protocol.Method[P, R]` with `route` generics, the
  duplicate-route panic, one error mapper.
- **The connection's discipline**: a writer goroutine with a deadline,
  droppable stream deltas, the in-flight and line caps, and the rule that the
  daemon's lock is never held across a call into a channel.
- **The security posture that exists**: the socket born 0600, a non-dumpable
  daemon, the data-directory lock, refusal of descendant peers, the trust hash
  recomputed daemon-side, `ResolvePath` judging a symlink by where it leads,
  allow rules that cover only simple commands, permits that never override a
  deny, a layered policy that can only tighten by construction, the editor's
  `IsLocal`, revision and shadow-tree checks, the scrubbed environment.
- **The web listener's**: loopback only, the Host allowlist, refusal of a
  missing Origin, hashed single-use codes compared in constant time, the
  deny-by-default method list, an app CSP with no inline script, sheets at an
  opaque origin under `default-src 'none'`.
- **`stream.Codec`**: one retry, SSE and limit path for both adapters; the
  subscription table as the one list of providers; single-flight refresh with
  a re-check; strict config decoding with no silent fallback.
- **In the TUI**: `frame` as the single layout source, typed hit-testing, the
  `eventRenderers` map (already the shape `present` wants), channel state
  replaced whole on a switch, the render cache keyed by item revision, one
  redraw per update, epoch-guarded ticks, the packages already split out.
- **In the browser**: a core free of the view framework, a pure reducer that
  ignores an event it has applied.

## 7. How it is delivered, and how it fails

**Delivery** (as decided, D2). Not one commit, and not one reviewable diff:
this touches most of 43,000 lines. Two pull requests, both left open until
the owner merges them: `refactor/phase-0-1` (the safety net and the urgent
fixes) and `refactor/ground-up` (phases 2 to 7, stacked on the first). Each
step is its own commit behind the gate. `main`, and the binary installed from
it, are untouched until then. Nothing is reinstalled along the way.

**How it fails, from the last plan's example.**

- *Features outrun it.* The branch must not live long beside active feature
  work. Phases 0 and 1 are small and merge to `main` at once: they make
  everything safer whether or not anything else follows. The rest needs a
  feature freeze for its duration, or it needs to be phased onto `main` step
  by step instead (D2, D7).
- *It reassures instead of happening.* The appendix is a ledger. Every step's
  commit names the finding ids it closes, and this document's tables are
  updated by the same pull request. A finding is closed by a test, not by a
  sentence.
- *It changes behaviour by accident.* Phase 0's goldens are the contract. A
  changed golden is a bug or a † step.
- *It loses the log.* Phase 0.6 comes before any schema change.

## 8. Decisions needed before work starts

**Answered by the owner on 2026-09-19:** D1 migrate; D2 two pull requests,
one for phases 0 and 1 and one for phases 2 to 7, both left open so nothing
reaches `main` until they say so; D10 features hold off. D3 to D9 are still
open, and none of them blocks phases 0 to 2.

| | Decision | Recommended |
|---|---|---|
| **D1** | The refactor changes the log's schema, and a version bump deletes the log today. Keep four days of history (163 MB) through forward migrations, or wipe as before? | **Migrate.** It is the first thing a real user would lose, and the framework is needed eventually anyway. |
| **D2** | One integration branch with an umbrella pull request, as asked, or phases merged to `main` one by one? | **Phases 0 and 1 to `main` now; 2–7 on the integration branch** under a freeze. If features cannot freeze, phase everything onto `main` instead: slower, never stale. |
| **D3 †** | Native tools are denied the sandbox's hidden paths in every mode (an agent can read `auth.json` in yolo today). Visible only to someone reading their own credentials through an agent. | Yes. |
| **D4 †** | Below sandbox level `Full` (no user namespaces, which is this machine: the daemon log says `sandbox landlock`), nothing hides the socket or credentials from a command. Refuse auto and yolo, ask once per channel, or only show the level? | Show the level always; entering auto or yolo below `Full` asks once. |
| **D5 †** | A read allowlist makes the shell's read boundary real, and breaks commands that read outside the working directories and system paths. | On at `Full`, with `sandbox.readable` to extend it. |
| **D6** | The trust prompt lists what a project's config grants (mode, sandbox, allows, MCP commands), or stays a yes or no. | List the grants. |
| **D7** | Where events become Items: in the daemon, sent over the wire (one fold, thin clients, a bigger protocol change), or in each client from generated types (smaller change, three folds kept honest by a generator). | **In the daemon.** Three folds disagreeing is the finding. |
| **D8** | `pkg/client` is public and unusable outside the module. Make the protocol public too, or move the client to `internal`. | `internal`, until something outside wants it. |
| **D9** | Delete the standalone Discord binary and its systemd unit. | Yes. |
| **D10** | A feature freeze for phases 2–7. | Yes, or take D2's alternative. |

## 9. Appendix: findings

Ids are the auditors' (`RT` runtime, `SEC` security, `DS` daemon and storage,
`UI` clients, `MC` models, config and repo), followed by the old plan's where
it had one. Every finding is **V** unless marked. Phase is where it closes.

### Runtime

| Id | Finding | Evidence | Phase |
|---|---|---|---|
| RT1 | Recap state set outside the fold, before its commit | `agent/recap.go:77`; `state.go:114` | 1.5 |
| RT2 | A failed `turn.ended` write wedges the agent | `turn.go:109`, `:43` | 1.4 |
| RT3 | Jobs removed from runtime before their commit; error ignored | `jobs.go:103-104`, `:119-121` | 2.2 |
| RT4 | Sheets written before their event, under the channel lock | `sheets.go:133-139`, `:157-160`, `:192-194` | 2.2 |
| RT5 | Channel lock held across `CheckModel` and `PlanUsage` | `pick.go:243`, `:258`, `:327`, `:335`, `:397`, `:422`; `channel.go:542`, `:549` | 4.3 |
| RT6 | Four idioms for the harness speaking | `recap.go:80`; `directory.go:62`; `project.go:303`; `pick.go:400` | 2.4 |
| RT7 | A dozen swallowed commits: permit grants, `addDir`, ask events, partial output, reminders, MCP events | `permission.go:212`, `:222`, `:249`, `:263`; `turn.go:164`; `replies.go:186`; `limits.go:40`; `mcp.go:127`, `:182`, `:198`, `:267`; `compact.go:94` | 2.2 |
| RT8 | `step` has about ten responsibilities | `turn.go:115-193` | 2.5 |
| RT9 | `decide`: six stages over four snapshots; `apply_patch` special-cased for sheets | `permission.go:89-137`, `:77` | 3.2 |
| RT10 **S** | Failover state in three places; a limit error carrying usage resets `resumes` | `pick.go:291-316`; `turn.go:99`, `:164`; `state.go:287` | 2.5 |
| RT11 | 12 input literals, 5 adapters repeating lock-check-commit, retarget ×3, "limited" ×5 | as cited in 3 and 4 | 2.4, 4.3 |
| RT12 | `Host` mixes five concerns; `Env` six capabilities with nil meaning absent | `channel.go:33-52`; `tools/tools.go:25-39` | 2.6 |
| RT13 | No registry: a type touches ~14 files, an input kind 9 sites | `state.go:245-258`, `:351`; `project.go:278`; `tui/transcript/transcript.go:315`, `:322`, `:377` | 2.1 |
| RT14 | `instructed`, `compactNext`, the context gauge are lost on restart | `agent.go:33-39` | 2.3 |
| RT15 | Untested: MCP lifecycle, job adopt/timeout/stop, `sheetsPatched`, spawn limits under concurrency, `maxMoves`, `maxResumes`, `resume_at` across `Recover`, recap across a restart | grep of tests | 0, 2 |
| 1C.6 | `CanSpawn` then `Spawn` under separate locks (open for `agent_create`, fixed for clients) | `tools/orchestration.go:65`, `:68`; `channel.go:506` | 2.2 |
| 1C.7 **S** | A killed agent can still commit | `agent.go:71`; `state.go:125` | 2.2 |
| 1C.8 | MCP tool-name collisions resolved by map order | `mcp.go:68-86` | 2.6 |
| 2.8 | Every event decoded twice per commit | `state.go:86`, `:131` | 2.1 |

### Security

| Id | Finding | Evidence | Phase |
|---|---|---|---|
| SEC-N1 | A mode switch answers waiting prompts `decide` would not | `daemon/handlers.go:199-205` | 1.1, 3.2 |
| SEC-N2 | The read-only guard skips paths that do not exist yet | `sandbox/helper.go:102-103` | 1.7 |
| SEC-N3 **S** | The session cookie is sent to any localhost port | `web/server.go:335-338` | 3.8 |
| SEC-N4 **S** | The one-time code is visible in `/proc/*/cmdline` for up to two minutes | `web/server.go:171`; `tui/commands.go:232` | 3.8 |
| SEC-N5 | A web connection can attach as `discord` and as interactive | `handlers.go:121-130` | 3.6 |
| SEC-N6 | The web allowlist is a second list beside the routes | `daemon/web.go:22-28` | 3.6 |
| SEC-N7 | Sheet caps bind the `sheet` tool only; unbounded reads | `permission.go:101`; `sheets.go:47`, `:96`, `:118`, `:187` | 1.6, 3.1 |
| SEC-N8 **S** | A sheet opened in its own tab can navigate itself away | `web/sheet.go:20-27`, `:44` | 3.8 |
| SEC-N9 **S** | The request's Host goes into the CSP | `web/server.go:258-262`, `:391` | 3.8 |
| SEC-N10 | The xAI credential goes to a host the `URL` doc says it does not; no redirect policy | `quota/quota.go:40`, `:68-74` | 3.9 |
| SEC-N11 | Ten wrong codes from anyone clear everyone's; sign-out leaves sockets open | `web/server.go:209-211`, `:237-243` | 3.8 |
| 1A.1–1A.5 | Peer identity fails open; no subreaper; no scopes or token; message-counted queue; ungated shutdown | `peercred/peercred.go:55-62`; `daemon/server.go:93`, `:116`, `:142`; `handlers.go:113` | 1.3, 3.6, 5.3 |
| 1B.1–1B.8 | Trust: hashed then read again; no caps; live nested instructions; edit trusts the disk; temp and trash outside the data dir; any path writable; regex writer; modes | `config/config.go:338`, `:355`, `:1082-1121`, `:1168-1190`; `agent/instructions.go:27`; `daemon/config_editor.go:86-88`; `config/editor_files.go:28`, `:176`, `:220` | 1.7, 3.4, 3.5 |
| 1C.1–1C.5, 1C.9 | The permission pipeline (partly improved: permits and mode share a lock) | `agent/permission.go:38`, `:101-134`, `:150`, `:232`; `agent/sandbox.go:29` | 1.2, 1.4, 3.2 |
| 1D | The shell grammar's ten verified bypasses | `shellcmd/shellcmd.go:393-420` | 3.3 |
| 1E.1–1E.4 | Raw path subjects; five resolutions per call (**S**); FIFO; containment ×6 | `tools/fs.go:74`, `:80`; `patch.go:61`; `search.go:61`, `:152` | 3.1 |
| 1F.1–1F.6 | The sandbox fails open; write-only; caches writable; env prefixes | `sandbox/sandbox.go:121-124`; `agent/sandbox.go:32-35`, `:83`, `:90`; `proc/proc.go:157` | 3.7 |
| 1G | Redirect policy on one client only; OAuth env overrides; swallowed auth load errors; a refresh racing a disconnect | `tools/web_fetch.go:274`; `oauth/oauth.go:117-124`; `auth/auth.go:164-166`; `registry.go:357-366` | 3.9, 4.2 |
| 1H | Discord: fences, no `textsafe`, a literal own-post check; a key that rebinds and chooses in one press; the config editor bypasses the two-step quit | `discord/permission_format.go:40-41`; `discord/worker.go:251`; `tui/prompts.go:297-299`; `tui/handlekey.go:46-48` | 1.8, 6 |

### Daemon, storage and the wire

| Id | Finding | Evidence | Phase |
|---|---|---|---|
| DS1 | Start reads the whole log; nothing is archived | `daemon/daemon.go:116-145`; measured | 5.1 |
| DS2 | Lazy recovery is feasible; four blockers named | `daemon.go:242`; `agent/recover.go:17-39` | 5.1 |
| DS3 | Replay sends everything | `tui/events.go:196`; `protocol/types.go:534` | 5.2 |
| DS4 | One queue, counted in messages | `daemon/server.go:93`, `:154-208` | 5.3 |
| DS5 | Fan-out on the writer; encoded with no subscribers; marshalled twice | `eventlog/log.go:268-273`; `daemon.go:163-176`, `:650-658` | 5.4 |
| DS6 | Polling everywhere | `tui/events.go:46-58`; `tui/discord.go:87`; `tui/web.go:129`; `discord/worker.go:60`; `discord/bridge.go:182`; `web/src/core/client.ts:91` | 5.6 |
| DS7 | Usage SQL parses JSON per row; channel filters unindexed | `eventlog/usage.go:41`, `:57-61`, `:88-91` | 5.5 |
| DS8 | A schema bump deletes the log | `eventlog/log.go:27-31`, `:84-112` | 0.6 |
| DS9 | Six mutexes; several held across I/O | `daemon.go:32-64`; `planusage.go:159-192` | 5.8 |
| DS10 | Authorization declared nowhere; validation inline; a DB failure surfaces as invalid params | `handlers.go`; `server.go:259-279` | 3.6 |
| DS11 | One 696-line protocol file imported by agent, tools, config and escalation; any rebuild kills live turns | `cmd/stavlos/main.go:527-547` | 5.9 |
| DS12 | 8+ temp-and-rename writers, 3 plain writes, none fsynced; the client writes into the daemon's directory | `daemon/web.go:85`; `config.go:1177`, `:1190`; `navigation/state.go:20`, `:41` | 5.8 |
| DS13 | Three service lifecycles | `daemon/main.go:77-79`; `daemon.go:96-113` | 5.7 |
| DS14 | `cmdWeb` and `cmdDiscord` are one shape; daemon lifecycle lives in `main` | `cmd/stavlos/main.go:363-572` | 5.9 |
| 2.3, 2.4, 2.6 | INSERT re-prepared per event and a title UPDATE for ever; `config.Load` at seven call sites with no cache; `ChannelList` stats per channel per poll | `eventlog/log.go:244`; `index.go:43-47`; `daemon.go:392-424` | 5 |

### Clients

| Id | Finding | Evidence | Phase |
|---|---|---|---|
| UI1 | `Model`: about 97 fields across 8 concerns | `tui/model.go:68-176`, `:270-295`; `prompts.go:17-28` | 6.4 |
| UI2 | Nav rows by index arithmetic in three places and 42 test lines | `view.go:612-631`, `:705-731`; `web.go:22-25`; `planusage.go:176-178`; `sidebar.go:509-518` | 6.5 |
| UI3 | Every dispatcher is at the complexity ceiling | `gocyclo`: `inputKey`, `command`, `render.folds` 30; `keyHints`, `setFocus`, `onDaemon` 29 | 6.4, 6.6 |
| UI4 | Focus is a 13-value enum beside orthogonal modal state; three modal systems | `focus.go:10-25`, `:115-190`; `model.go:380-385`, `:418-425` | 6.4 |
| UI5 | A partial keymap: 27 bindings, ~87 matched sites, ~81 raw comparisons | `keys.go`; grep | 6.6 |
| UI6 | Ticks and polls | as DS6 | 5.6 |
| UI7 | 11 overlay kinds behind one switch; three features repeat one pattern | `overlay.go:45-55`; `pickers.go:266`; `discord.go`, `web.go`, `planusage.go` | 6.5 |
| UI8 | Events presented three times, differently | `transcript/chat.go:95-110`; `discord/worker.go:246-296`; `web/src/core/reduce.ts:61-123` | 6.1 |
| UI9 **S** | About 75% of the transcript package is presentation-neutral | `transcript.go` function map | 6.2 |
| UI10 | Tool summaries written three times | `transcript.go:1682`, `:1754`, `:1786`, `:1824`; `discord/permission_format.go:27`, `:48-75`; `reduce.ts:35-43` | 6.1 |
| UI11 | `view_test.go`: 3,991 lines, 157 substring and 141 exact-string assertions, 53 hand-built mouse events; no golden files | grep | 0.1 |
| UI12 | The browser handles 14 of 34 event types, `payload: any` | `web/src/core/types.ts:36`; `reduce.ts` | 6.3 |
| 2.13–2.16 | The frame recomputed three times; `DeepEqual` card rebuilds; `All()` copies; 86 value receivers on `Model` | `frame.go:10-39`; `transcript/permissions.go:45`; `transcript.go:1115-1121` | 6 |

### Models, config and the repository

| Id | Finding | Evidence | Phase |
|---|---|---|---|
| MC1 | `Registry` does seven jobs on one mutex | `registry/registry.go:150-175` and methods | 4.2 |
| MC2 | About 30 sites keyed by a provider's name | `chatcompletions/provider.go:41`, `:49`, `:58`; `quota/quota.go`; `registry.go:106`, `:197`, `:605`, `:765` | 4.1 |
| MC3 | Adapter duplication beyond the shared codec | `codex/complete.go:17-37` vs `chatcompletions/complete.go:20-40` | 4.1 |
| MC4 | Limit detection by substring in the shared transport (**S**: false positives) | `stream/stream.go:243-274` | 4.4 |
| MC5 | `applyFile` at the limit, twice worked around; validation, defaults and merging interleaved over four layers | `config/config.go:509-597` | 3.5 |
| MC6 | Six config writers, three JSONC techniques | `config.go:1177`, `:1190`; `discord_write.go:19-56`; `editor_files.go:280`, `:336`; `editor_fields.go:128`; `cmd/stavlos/main.go:624` | 3.5 |
| MC7 | `Effective` holds settings, catalogs, trust state and a dead field | `config.go:256-319` | 3.4 |
| MC8 | 55 env-var lines in 24 test files; tests can reach the real data directory (**S**) | grep; `agent/sheets.go:47`; `agent/harness_test.go:327-328` | 0.2 |
| MC9 | The race list omits 7 packages; the complexity limit equals the maximum; no fuzzing, vulnerability check, dead-code check or CI | `scripts/check.sh:36-55` | 0.4 |
| MC10 | Leftovers: the standalone Discord binary and unit; `pkg/client` unusable outside the module; plugin stubs; "preset" ×170 | `discord/run.go:19`; `pkg/client/client.go:14-15`; `config.go:45`, `:293`, `:516`; `registry.go:44`, `:240`, `:291` | 7 |
| MC11 | Docs the code contradicts: go-plugin as the architecture, the sandbox as roadmap, two providers, three config layers, a rename recorded as done | `docs/stavlos-prd.md:74`, `:377`, `:437`, `:544`; `docs/refactor-plan.md:32`, `:100-102` | 7 |
