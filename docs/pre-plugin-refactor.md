# Pre-plugin refactor plan (2026-09-17)

> **Superseded on 2026-09-19 by [ground-up-refactor.md](ground-up-refactor.md)**, which
> re-verified every finding here against the code (all still open, several
> worse) and carries them forward. Kept for its reasoning and its decisions.

Should the whole project be refactored before [Lua plugins](lua-plugins.md)
land? **Yes.** Two reasons, both found in the code:

1. **The plugin design assumes seams that do not exist.** It needs a
   post-commit dispatch point, a permission decision that takes a principal
   other than an agent, a tool registry, events that carry their origin, a
   system input kind, a notice event, one config reload point and a service
   lifecycle in the daemon. Today each of those is either missing or
   special-cased for MCP or Discord. Building plugins first would add a third
   special case to every one of them.
2. **Plugins raise the stakes of holes that already exist.** A trusted
   project's files become code that runs inside the daemon, so every weakness
   in the trust hash, the config editor and the daemon socket turns from
   "an agent can loosen its policy" into "an agent can run code unsandboxed
   with the credentials".

Six read-only audits produced this plan (agent runtime; daemon, storage and
protocol; security layer, tools and config; TUI; Discord bridge; models and
repo health). The previous [refactor plan](refactor-plan.md) is complete, but
it finished before the Discord bridge, the config editor, inline prompt cards
and explicit reply tracking were written; most findings sit in that newer
code, and a few contradict what the old plan records as done.

Findings are marked **V** (verified: reproduced in a unit test, or the code is
unambiguous) or **S** (suspected: reasoned from the code, not reproduced). An
S finding gets a failing test first; if the test cannot be made to fail, the
step is dropped.

## Ground rules

- **Behaviour is frozen.** A person using the TUI, the CLI or Discord must not
  notice a difference. Steps marked **†** are security fixes that cannot avoid
  a visible change (an extra ask, a refusal); each is listed under
  [Decisions](#decisions) and waits for an answer.
- Nothing else is sacred: the event log is wiped (schema bump), the wire
  protocol, package layout, config internals and every type may change. No
  compatibility code is written, and what exists is deleted.
- Priority inside every phase: security, then performance, then
  maintainability, then DRY.
- Every step is its own commit behind `scripts/check.sh`, followed by
  `go install ./cmd/stavlos`.

## Phase 0: lock the behaviour, widen the gate

The refactor is only safe if "the user notices nothing" is checked by machine.

| # | Step |
|---|---|
| 0.1 | **Behaviour lock.** Golden snapshots taken before any change: TUI frames for a scripted channel (home, chat, agent chat, every dialog, inline permission and question cards, sidebar, config editor) through `tuitest` and the pty probe; Discord payloads for every card type; tool definitions (exists); CLI output of every subcommand; the model-facing system prompt and harness-state note. A refactor step that changes a golden file is a bug or a † step. |
| 0.2 | `go get github.com/gorilla/websocket@v1.5.3` (V: only discordgo and one test import it; closes the open Dependabot alert). Bump `x/ansi`, the MCP SDK and sqlite. |
| 0.3 | Gate additions: `govulncheck`, `deadcode`, `go mod tidy -diff`, `shellcheck`, the race detector on **every** package (today a hand list that omits project, sandbox, peercred, protocol, event, instructions and cmd/stavlos), `BenchmarkChatRedraw` with a ceiling, and a short run of each fuzz target. `gocyclo` ratchets from 30 (the current maximum, so it blocks nothing) to 25 now and 20 by the end. |
| 0.4 | Fuzz targets that the old plan promised and never got (V: zero `func Fuzz` in the repo): `shellcmd.Commands`, the patch parser, `parseWebURL`/`publicIP`, JSONC editing, `textsafe`, and "every protocol string into `View()` yields only the TUI's own SGR". |
| 0.5 | One test isolation helper in `internal/testutil` (config, data, cache dirs, socket, `HOME`) replacing about 30 ad hoc `t.Setenv` groups. S: TUI tests can write navigation state into the real data directory. The three `fakeModel`s, two `fakeProvider`s and `waitUntil`/`eventually` move there too. |
| 0.6 | A CI workflow that runs `scripts/check.sh` (there is no `.github`). |
| 0.7 | `schemaVersion` is 5 while the vocabulary is schema 6 (V, `eventlog/log.go:31`): the last vocabulary change did not wipe old logs. Bump it now, and add a test that hashes the event type table so a vocabulary change without a bump fails. |

## Phase 1: security

### 1A. Who may drive the daemon

The socket is all-or-nothing: a peer that is not a descendant of the daemon
gets every method, including `config.edit`, `trust.reply`, `set_mode`, the
directory methods and `daemon.shutdown`.

| # | Finding | Fix |
|---|---|---|
| 1A.1 | **V** `peercred.DescendsFrom` returns false on any `/proc` error (`peercred.go:60`). A process that connects, forks and lets the parent be reaped is no longer a descendant. | Fail closed: an unreadable ancestry is a refusal. |
| 1A.2 | **V** A double fork reparents to init and escapes the check; the comment admits it. The daemon never sets `PR_SET_CHILD_SUBREAPER`. Below sandbox level `Full` nothing hides the socket, so any shell command can set yolo, answer its own prompt or trust a repo. | The daemon becomes a subreaper, so orphans stay its descendants (and it reaps them). |
| 1A.3 | **V** No per-client token exists, though old plan 0.8 lists one. The in-daemon Discord bridge passes through a `cred.PID != os.Getpid()` exemption (`server.go:108`) with full rights. | **Capability scopes per connection.** A connection attaches with a scope: `owner` (TUI, CLI), `bridge` (post, reply to asks, read; no mode, dirs, trust, providers, config, shutdown), `internal` (in-process services, named, not a pid exemption). Handlers declare the scope they need in the method table. Owner scope requires a token read from a 0600 file in the data directory, which the sandbox hides. Plugins need the same mechanism, so it is built once here. |
| 1A.4 | **S** Below `Full`, an agent can unlink the socket and bind its own; the client checks only the uid. | The client also verifies the peer pid against the daemon's lock file. |
| 1A.5 | **V** Out-queue bounded by message count, not bytes; concurrent requests on one connection can reorder two posts; any client with another build id shuts down a daemon with live turns. | Byte bound; per-connection ordering for mutating methods; the restart refuses while turns run unless forced (old open item 6.6). |

### 1B. Trust and config integrity

| # | Finding | Fix |
|---|---|---|
| 1B.1 | **V** `config.Load` hashes the project files, then reads them again to parse (`config.go:316`, `333-348`). A swap in between loads content nobody trusted. With plugins that content is code. | Read the project layer once into a snapshot (`map[path][]byte`); hash and parse the same bytes. Every loader (config, roles, commands, skills, instructions, later plugins) reads from the snapshot, which is also the one place loaders are enumerated (today `Load` and `validateEditorTree` each list them, and differ). |
| 1B.2 | **V** `ProjectHash` reads every file under `.stavlos` with no size cap and follows symlinks (`config.go:1039`). A cloned repo can stall or exhaust the daemon before any prompt. | Size and count caps, symlinks refused, non-regular files refused. |
| 1B.3 | **V** Nested `AGENTS.md`/`CLAUDE.md` files are read live from disk and injected into tool results without being in the trust hash (`agent/instructions.go:20-45`). A sandboxed shell can create `sub/AGENTS.md` and steer every agent that touches `sub/`. Files past `maxDirs=2000` are unhashed as well. | Instructions are served from the trusted snapshot only; a file that is new or changed since trust is withheld and raises the trust prompt. |
| 1B.4 | **V** After a `config.edit`, the daemon hashes whatever is on disk and stores it as trusted (`daemon/config_editor.go:72-91`). Anything written during the edit is trusted unseen. | The new hash is derived from the validated snapshot the editor wrote. |
| 1B.5 | **S** The editor checks the path with `Lstat`, then writes later; in project scope the daemon writes unsandboxed into a directory an agent can write. **V** the trash directory is created in the repo root; each edit copies the config tree, possibly holding a literal Discord token, to the system temp dir and leaves it on a crash. | All config writes go through `os.Root` (or `openat2` with `RESOLVE_BENEATH`); staging lives under the 0700 data directory and is removed on start. |
| 1B.6 | **V** `config.edit` in system scope writes any relative path under `~/.config/stavlos`. Once `plugins/*.lua` lives there, it installs daemon code. | A path-kind allowlist (config, roles, commands, skills); plugin files are never writable through the socket. |
| 1B.7 | **V** `SetGlobalModel` rewrites every `"model": "…"` in `stavlos.json` with a regex, comments and nested keys included, non-atomically (`config.go:1104`). | One JSONC writer (`EditJSONField`) and one atomic write, used by the model picker, the Discord toggle and the editor (three techniques today). |
| 1B.8 | **V** File modes: `initConfig` makes 0755 directories; `atomicConfigWrite` keeps an existing 0644 on a file that may hold a token; the editor shows `discord.token` unmasked. | 0700/0600 enforced on write and checked on load when the file holds a secret; secret fields masked in the editor. |

A trusted project may set anything the global file can, including `mode`,
`sandbox`, policy allows and MCP commands. That is the owner's decision of
2026-09-15 and stays; `refactor-plan.md` still says "tighten-only" and is
corrected. What it means once plugins exist is decision **D1**.

### 1C. One permission pipeline

`(*Agent).decide` reads channel state in four separate lock acquisitions,
mixes tightening and loosening stages, and is bound to an agent's tool call.

| # | Finding | Fix |
|---|---|---|
| 1C.1 | **V** No consistent snapshot (`permission.go:96-120`). | `Decide(Request{Principal, Tool, Subject, Snapshot}) Verdict{Verb, Why, Sticky, By}`: a pure function over one snapshot, with explicit ordered stages: base policy → overlays (role; later the Lua hook), tighten only → sticky rules (control files, hidden paths) → looseners (permits, hosts, mode) → boundary. A sticky ask can never be loosened. |
| 1C.2 | **V** A remembered "allow for channel" answers a control-file ask, ending "control files always ask"; permits are channel-wide, so one earned under a broad role answers a tighter role's ask (`permission.go:99-111`). | Sticky asks ignore permits; permits are keyed by principal kind and role. |
| 1C.3 | **V** Two "control file" lists that differ (`permission.go:145` covers `.git`; `sandbox.go:29` covers `.git/hooks` and `.git/config`), and the check runs only for `apply_patch`. | One list in one package; the check is tool metadata ("writes paths"), not a tool name. |
| 1C.4 | **V** In yolo, native `read`/`grep`/`apply_patch` run in the daemon and skip the boundary, so `read ~/.local/share/stavlos/auth.json` succeeds although the sandbox hides it from shell. | The sandbox's hidden list is a hard deny for native tools in every mode. † only for someone reading their own credentials through an agent. |
| 1C.5 | **V** `runTool` ignores the error from logging `tool.started` and runs the tool anyway (`permission.go:38`): side effects with no record. | A failed log write ends the turn before the tool runs. |
| 1C.6 | **V** `agent_create` checks `CanSpawn` and then `Spawn` under separate locks; `spawnLocked` never rechecks. Concurrent agents overshoot `MaxAgents` and `MaxDepth`. | The limit is checked inside the spawn commit. |
| 1C.7 | **S** `Kill` commits `agent.killed` and cancels afterwards; the victim can still commit events. | `apply` rejects turn events from a killed agent. |
| 1C.8 | **V** `mcpToolName` maps every disallowed character to `_` and looks up through a map: server `a` tool `b__c` equals server `a__b` tool `c`, first match at random (`mcp.go:68`, `310`). | One shared namer for `mcp__` and the future `lua__`: `__` forbidden inside server, plugin and tool names; collisions are load errors. |
| 1C.9 | **V** `Answer.Client` carries the magic values `"yolo"` and `"auto"`. | `Verdict.By` is a typed origin (human client, mode, permit, policy, hook). |

### 1D. Shell analysis

**V** (each decided *allow* under `deny rm -rf *` plus `shell: allow`):
`if true; then rm -rf /; fi`, `while :; do …; done`, `! rm -rf /`,
`eval 'rm -rf /'`, `sudo -u root rm -rf /`, `env -u X rm -rf /`,
`busybox rm -rf /`, `bash <<< '…'`, `coproc rm …`, `a=rm; $a -rf /`.
`launched()` (`shellcmd.go:400`) does not drop shell keywords, `eval` or
`coproc`, and launcher flags that take an argument break it.

Fix: `shellcmd` gets a real grammar pass. Keywords and `!` are stripped;
launchers are a table with their flag arity; `eval`, `source`, here-strings to
a shell, `coproc` and a command word that is not a literal make the line
*opaque*. An opaque line can match a deny by substring of any word but can
never match an allow, in any mode. The line is tokenised once (four times
today) and the parse is shared by policy, prefix offers and the boundary.

### 1E. Paths

| # | Finding | Fix |
|---|---|---|
| 1E.1 | **V** Path rules match the raw model string: with deny `read secrets/**`, the inputs `./secrets/key`, `/repo/secrets/key` and `x/../secrets/key` decide allow (`fs.go:74`, `patch.go:61`, `search.go:61`). `grep`'s subject is only its root under a different tool key, so grepping the parent bypasses a file deny. | Subjects are resolved paths, expressed relative to the working directory; `grep` and `glob` results are filtered by the `read` rules. |
| 1E.2 | **S** `Subject`, `outsideDir` and `Run` each resolve the path again; a background job can turn a directory into a symlink in between, and `apply_patch` then writes outside with daemon privileges. | Typed tools (3.4): parse once into an input that carries resolved paths; open through `os.Root`/`RESOLVE_BENEATH`. |
| 1E.3 | **V** `read` on a FIFO blocks a daemon goroutine forever. | Regular files only; open non-blocking. |
| 1E.4 | Containment is written four ways (`anyWithin`, `inDirs`, `controlFile`, `bashPathCandidates`), expansion three ways. | One `pathx` package: expand, resolve, within. |

### 1F. Sandbox

| # | Finding | Fix |
|---|---|---|
| 1F.1 | **V** Fails open: without Landlock `Wrap` returns `(None, nil)` and everything runs unsandboxed, with one daemon log line. On ABI < 4 `network: false` is silently unenforced. | The probed level is channel state, shown to clients; what happens below `Full` is **D2**. |
| 1F.2 | **V** Landlock restricts writes only. The shell read boundary is text matching that misses `cat</etc/passwd`, `{cat,/etc/passwd}`, `tar -C/ …`, `python3 -c …`, `d=/etc; cat $d/passwd`. The hidden list is a denylist that omits `~/.claude`, `~/.codex`, browser profiles, `~/.config/*` tokens, shell history. | A read allowlist (working directories, system directories, toolchains, caches) is the real boundary: **D3** †. The text heuristic stays only as the source of the friendly "outside its directories" ask. |
| 1F.3 | **S** Root-equivalent sockets are reachable: `/var/run/docker.sock`, the D-Bus session bus (`systemd-run --user`) at level `Landlock`, tmux and X11 sockets when the working directory is under `/tmp`. Landlock does not mediate `connect()` on pathname sockets. | Hidden in the mount namespace at `Full`; at lower levels see D2. |
| 1F.4 | **V** Hiding is skipped silently when the path is a symlink (stow-managed `~/.netrc`, `~/.npmrc`). `~/.cache`, the Go build and module caches are writable: poisoned artefacts run outside the sandbox later. | Resolve before hiding. Caches become per-channel overlay copies or read-only with a private `GOCACHE`; measure build times before choosing. |
| 1F.5 | **V** In auto mode `egress()` ignores shell, so `curl -d @file host` runs unprompted; it recognises MCP only by name prefix, so `lua__*` would be auto-allowed. | Egress is tool metadata. Whether shell counts as egress in auto when the network is on is **D4** †. |
| 1F.6 | **V** `proc.Env` passes `PIP_INDEX_URL`, `GOPROXY`, `NPM_CONFIG_REGISTRY`, which can embed credentials. | Strip userinfo from URL-valued variables. |

### 1G. Network and credentials

- **V** `publicIP` treats zoned IPv6 (`fe80::1%eth0`, `fd00::1%lo`), `::127.0.0.1` and 6to4 loopback as public (`web_fetch.go:345`); a same-host redirect to another port is followed. Fix: strip the zone, unmap, compare host and port.
- **V** No redirect policy on the model, OAuth and models.dev clients: a 307/308 re-sends the conversation or the refresh token (`ChatGPT-Account-Id` survives cross-host). Fix: one `httpx` client constructor with `CheckRedirect = ErrUseLastResponse`, https required unless loopback, response size limits, shared tuned transport.
- **V** `STAVLOS_OAUTH_*` overrides ship in the production binary and point logins at any URL. Fix: test build tag, or loopback only.
- **V** A 2xx token response with an empty `access_token` is stored and sent as `Bearer ` for an hour. Fix: validate in `tokens()`.
- **V** A refresh that finishes after `Disconnect` writes the credential back (`registry.go:369`, `305`). Fix: `Disconnect` takes the refresh mutex and marks the provider disconnected.
- **V** `Store.Get` swallows load errors (corrupt `auth.json` reads as "not connected"); login polls die on the first 5xx or network error; the browser login listens on `127.0.0.1` and redirects to `localhost` (old plan items, not done).
- **V** `openBrowserCmd` passes a daemon-supplied URL to `xdg-open` with no scheme check. Fix: `https://`, or `http://` on loopback.

### 1H. What people see before they approve

| # | Finding | Fix |
|---|---|---|
| 1H.1 | **V** Discord permission and trust cards wrap model-written commands, patches and file names in ``` without neutralising fences or bidi text (`discord/permission_format.go:40`). A command can close the block and render a fake "✔️ **Allowed**", a masked link or a spoiler. Nothing in the package imports `textsafe`; old phase 0.7 never reached Discord. | One sanitiser and a fence-safe encoder in the shared presentation package (3.10). |
| 1H.2 | **V** path, **S** impact: in the TUI, when the bound permission is resolved elsewhere, the next keypress rebinds to another prompt, resets the selection to 0 and chooses it; option 0 is always "Allow once" or "Trust" (`prompts.go:274-297`, `permissions.go:20-35`). | A key never both rebinds and chooses: when the prompt identity changes under the keys, that key is swallowed. |
| 1H.3 | **V** TUI sanitising is per field by hand and misses provider, model, role, variant, sign-in, MCP server and tool names, prompt `Tool`/`Kind`, and the config editor's paths and values. SGR restyling, newlines and tabs get through. | Clean once at the client decode boundary (generated `Clean()` per protocol type); `sanitize.go`'s lists go. The Phase 0 fuzz target enforces it. |
| 1H.4 | **S** A long `allow_prefix` is visible only in a button label clipped to 80 characters. **V** earlier continuation parts of a split card are never retired. **V** a dead `dir`/`dir-submit` modal path would add a user-typed directory if a custom id were forged. **V** own-post suppression compares the literal `"human:discord"`, not the client id. | Prefix shown in the body; parts tracked and retired; dead path deleted; compare `AttachResult.ClientID`. |
| 1H.5 | **V** The config editor handles keys before `handleKey`, bypassing the two-step quit. | Folded into the focus state machine (3.13). |

## Phase 2: performance

### Daemon and storage

| # | Finding (all V) | Fix |
|---|---|---|
| 2.1 | Commit fan-out runs on the single log-writer goroutine: every event is JSON-encoded (twice: `eventLine`, then `notification`) even with no subscriber, under `streams.mu`, `d.mu` and each client's mutex. Every channel's appends queue behind it. | The writer hands committed batches to a dispatcher goroutine; encoding is lazy, once per event, only for subscribed channels. Plugin hooks later hang off this dispatcher, never off `onCommit`. |
| 2.2 | Startup reads every event of every non-archived channel; archived channels are never dropped from memory. | Channels recover lazily on first resume or subscribe; the list comes from the `channels` table. |
| 2.3 | The INSERT and `index()` are re-prepared per event; `titleOf` decodes every post forever, even once a title exists. | Prepared statements; title indexing stops once set. |
| 2.4 | `config.Load` (a walk of up to 2,000 directories plus a hash of `.stavlos/**`) runs on every `command.list`/`command.run`, for every channel on any model pick, and three times plus a tree copy per editor save. | The 1B.1 snapshot is cached per directory, keyed by (path, mtime, size) of its inputs, invalidated by the editor and by `reloadChannel`. |
| 2.5 | Clients poll because the protocol has no agent-state, channel-lifecycle or service-status notification (the old plan records one as done; it does not exist). The TUI sends `channel.list` plus a `tree` per open tree every 3 s and a Discord status RPC every 2 s, forever; each Discord worker sends `AgentTree` and `PromptList` every 5 s, and the bridge calls `ChannelList` and Discord's `GuildChannels` every 5 s (about 17k REST calls a day while idle). | Notifications: `agent.state`, `channel.listed` (created, archived, renamed, state dot), `service.status`; `ask.resolved` carries the answer; `PromptList` takes a filter. Both clients drop their polling loops. |
| 2.6 | `ChannelList` does a query plus `Info()`, an `os.Stat` and `esc.Pending` per channel; `agentChannel` scans every channel per `agent.*` call. | An agent → channel index; directory state cached and refreshed on the catalog notification. |

### Runtime

| # | Finding (all V) | Fix |
|---|---|---|
| 2.7 | Every commit holds `Channel.mu` across the group-commit wait (25 sites), so `Info`, `Tree`, `Name`, `Dir`, `Mode` stall on disk latency. `Info()` also stats the directory under the lock. | Readers take an immutable snapshot published after each commit (`atomic.Pointer`); only writers take the lock. No filesystem call under `c.mu`. |
| 2.8 | A live event is marshalled once and decoded twice under the lock (`state.go:105`, `project.go:51`), against the old plan's "no event is decoded twice". | `commit` carries the typed payload beside the bytes; `apply` and the history builder take the typed value. Only recovery decodes. |
| 2.9 | `History()` re-normalises the whole history each step, and `EstimateTokens` is O(n) once or twice per step. | Normalise incrementally in the builder; keep a running token estimate. |
| 2.10 | Role policy is re-parsed and its regexps recompiled on every tool call (`PresetPolicy()`), against "compile once at load". `toolEnv` rebuilds the sandbox spec per call (a `MkdirAll`, a dozen stats, `EvalSymlinks` per directory), even for `todo`. `mcpDefs` re-marshals every MCP schema each step, before the prefix-cache check. `Def()` reflects and marshals per call. `expandShellVars` rebuilds the environment map per token. | Compiled policy lives on the role view; the sandbox spec is cached per (config, directories); tool definitions are built once per registry entry. |
| 2.11 | Killed agents keep their state, history builder and handle for the channel's life. MCP start ignores turn cancellation (up to 30 s per server). | Release on kill; start under the turn context. |
| 2.12 | `grep` compiles a Go regexp even when `rg` does the search, rejecting valid `rg` patterns. | Compile only on the walk fallback. |

### Models

Both adapters rebuild tool definitions and the converted message list on
every call; the cached prompt prefix stops at `model.Request`. Cache the
wire-encoded system prompt and tools per adapter, keyed by the prefix key. One
shared transport. The catalog refreshes periodically instead of once per
process, and a cold start no longer waits up to 15 s on the network.

### TUI

| # | Finding | Fix |
|---|---|---|
| 2.13 | **V** `computeFrame` runs two or three times per message (the old plan says once); within a frame the tabs are built about four times and the sidebar rows about five; the divider is rebuilt per hit test; `sidebarHover` re-renders the sidebar to map one `y`. With all-motion mouse reporting this runs on every mouse move. | One `Frame` per `Update`, holding divider, tabs, sidebar lines and click spans; `View` and the mouse only read it. Motion events that cross no span boundary do not redraw. |
| 2.14 | **V** While an inline card has focus, every blink and mouse move runs `refreshViewport`, which rebuilds every visible card uncached (`reflect.DeepEqual`, hard wrapping) and mutates the transcript during render. | `Ensure*` runs when a prompt is upserted; cards are cached by (prompt, draft revision, width). |
| 2.15 | **V** `Transcript.All()` copies the whole chat whenever a stream tail exists; `ItemRange`, `ItemFolds`, `ItemAgent` are O(lines); `itemAtRow` scans a map; `whoKey()` builds a string compared against every cached item; `time.Now()` per item; `assemble` allocates a map sized to the chat on each spinner tick. | Per-item indexes, a sorted row slice with binary search, a revision counter, one clock read per frame. |
| 2.16 | **S** 68 methods take the multi-kilobyte `Model` by value. | Pointer receivers on sub-models; measure before and after with the benchmark. |

### Discord

The prompt store copies its whole map to read one key and rewrites and fsyncs
the whole file, patches included, twice per prompt; change detection
re-marshals every prompt each tick. Give it `get`, a per-prompt revision, and
one write per change. **V** there is no gateway RESUME: every drop, Discord's
routine reconnect request included, re-identifies, re-runs four REST calls
(the slash-command overwrite among them), redials the daemon and rewrites
every card twice, and messages typed in the gap are lost. Resume sessions;
rebuild only on an invalid session. **V** one Discord 4xx cancels the daemon
link for every channel, and a deterministic rejection loops forever: isolate
failures per card and per channel.

## Phase 3: the seams (maintainability)

These are the structural changes. 3.1 to 3.7 are what the plugin host
attaches to.

**3.1 Event registry and origin.** Adding an event type touches
`event/types.go`, three switches in `agent/state.go`, `project.go`,
`eventlog/index.go`, four places in the TUI transcript, four in
`tui/events.go`, the test fixtures, the Discord worker and the PRD; no lint
catches a miss, because `check.sh` exempts `event.Type` from `exhaustive` and
every switch has a silent `default`. Add `event.Registry`: type → payload
constructor and flags (in history, rendered, replayed). Events gain an
`Origin{Kind, Name}` (human client, agent, harness, service; later plugin),
which also replaces the input `source` string that is built in three places
and never stored. Turn the lint on. Add one generic
`notice{level, origin, text}` event: config-reload and Discord problems
travel as RPC result strings today, and `plugin.failed` will need it.

**3.2 One commit, one dispatch.** `commitLocked` returns only the agents to
wake and drops the committed events; lock → commit → unlock → signal is
hand-written at about 12 sites, and 13 more discard the wake list. One
`Channel.commit(evs…)` owns the lock, append, apply and unlock, then calls one
`dispatch(committed, effects)`. Recovery folds `apply` and never dispatches.
The per-channel plugin queue is later one more consumer of `dispatch`.

**3.3 Principals.** `Decide` (1C.1) and `Channel.Authorize(ctx, principal,
call, onAsk)` work for any principal: `{Kind: agent | service, ID, Overlay}`.
Roles become one overlay source. Escalation stops being a method on `*Agent`.

**3.4 Tool registry and typed tools.** `runTool` tries the static set, then
falls back to MCP; definitions, prompt text, `egress()`, the TUI and Discord
each special-case the `mcp__` prefix. Replace with a per-agent
`ToolSource{Defs, Lookup, PromptSection}` implemented by built-ins and MCP
(and later Lua), whose entries carry metadata: policy key, default verb,
egress, writes paths, source. Tools become `Tool[I]` with
`Parse(raw) (I, error)`, `Subject(I)`, `Run(ctx, env, I)`: strict decoding
once (decode errors in `Subject` are swallowed today; `apply_patch` and
`web_fetch` parse two or three times) and resolved paths carried from the
decision to the run (1E.2). Typed entry points (`tools.Shell(ctx, env, in)`)
and a `Channel.baseEnv(cfg)` let a caller that is not in a turn use them.

**3.5 System inputs.** `From == ""` means the human, so a harness message
owes the user a reply, and `SetDir`'s info input renders as "[message from
agent , …]" (V, `directory.go:62`, `project.go:295`). Add `InputSystem` with
an origin, its own framing, a wake rule and no reply debt. The human-post →
steer pair is built in three places; make one queueing function, which is
also where rate limits go.

**3.6 Channel runtime resources.** `Stop`, `Archive`, `SetDir` and
`SetConfig` each tear MCP servers down ad hoc. One `resources` interface
(start, reconfigure, stop) covers MCP servers and jobs, later plugin VMs.
Shutdown order: channels, then services, then the log.

**3.7 Daemon split.** `Daemon` owns the channel registry, client hub, trust
store, logins, config editing, Discord lifecycle and command loading. Split
into `hub` (clients, subscribe, streams, dispatcher), `trust`, `logins`,
`projects` and `services`. The sequence `config.Load → SetConfig →
maybeTrustPrompt` is copied in eight places that disagree (two skip the trust
prompt; one holds `editorMu`): one `projects.reloadChannel(c)`. Discord
becomes a `Service{Start, Close, ConfigChanged, Status}` in a list; the
`discord.*` RPCs become `service.*`. About 20 handlers repeat
lookup-and-bail: `routeChannel`/`routeAgent` wrappers on the method table,
which also carries the 1A.3 scope. `cmd/stavlos/main.go`'s connect, replace
and restart logic moves to a `daemonctl` package that `stavlos-discord` can
use.

**3.8 Config.** `config.go` (1,100 lines) holds the schema, the layer merge,
role and skill parsing, the trust hash and JSONC writing: split into
`schema`, `layers`, `roles`, `trust`, `jsonc`. `Escalation`, `Compaction`,
`Sandbox` and `Search` each exist as a `File` struct and again as an
anonymous `Effective` struct: one schema. `applyFile` (complexity 29) becomes
a per-field table that says which layers may set a field, so "global only"
(`discord`, later `providers`) is data. `config` stops importing `protocol`;
the editor's wire types move out.

**3.9 Protocol layering and an honest client.** `pkg/client` exposes
`internal/protocol` types and imports `internal/peercred`, so nothing outside
the module can compile against it, while the plugin doc calls it the tier for
bridges. `tools`, `escalation` and `agent` import `protocol` for domain types
parked there to avoid a cycle, and both clients decode raw event payloads, so
the storage schema is the wire contract. Domain types move to a leaf package;
`protocol` and the event payloads clients may read become public (**D6**);
presentation text (`PermissionResult`, `ModeSummary`) leaves `protocol`. The
client gains a follow helper (attach, resume, reconcile, subscribe, reconnect
with backoff, notification pump), which both clients hand-write today; this
closes old open item 6.5 (the TUI quits on disconnect) and replaces the
bridge's three backoff loops.

**3.10 Shared presentation.** The TUI and Discord each implement, with
wording already drifting: the addressed chat line and its "sole main is
implicit" rule; permission options and their mapping to replies ("Allow in
this channel" against "Allow for this channel"); tool titles ("Web fetch"
against "Fetch"); the permission subject per tool; question drafts, answers
and result text; prompt reconstruction from `ask.requested`; agent-name
tracking. One `internal/present` package turns protocol values into neutral
view models (title, body blocks, options, result), sanitised once (1H.1,
1H.3); each client only draws them. The golden files of 0.1 pin today's
wording per client, so unifying text is a separate, visible choice.

**3.11 Discord.** Permission cards run through code named "question"
throughout; compatibility code for cards nothing emits ("Old cards", V2
migration, select menus, multi-question drafts, "older bridges") is deleted;
19 test files that follow the feature history are regrouped; `mirror` (30),
`syncPrompts` (26) and `statusView` (25) are split. The bridge runs in-daemon
*and* as `cmd/stavlos-discord`, sharing a flock, and the docs already call
one of them legacy: keep one (**D5**). `cmd/discord-probe/` (empty) goes.

**3.12 Models.** Finish the shared OpenAI-wire kit: argument normalisation
("invalid JSON becomes `{}`", silently, twice), three error shapes, headers
and the user agent, 401 handling (codex only). Provider quirks
(`p.name == "xai" && contains "mini"`, the fixed 16,000 `max_tokens`,
`reasoning_effort`) become per-provider data. `Models()`, the catalog
keep-list, zeroed prices, status and the error strings are hard-wired to the
two subscriptions: generalise them to a provider table so a config-defined
OpenAI-compatible provider is an entry. Adding that provider is a feature,
outside this plan. S: `call_%d` ids repeat across turns in chatcompletions.

**3.13 TUI.**

- **One focus state machine.** Modal state is ten independent fields
  (`focus`, `ov`, `cfgEditor`, `hoverFocus`, `permEdit` and `dirEdit` as
  magic strings, `q.typing`, a nine-value string `mode`, `switching`,
  `loading`); `setFocus` (complexity 29) is the transition table as
  if-chains, and "leave an inline card" is copied five times. The config
  editor joins it, using `dialog` and `list` like every other modal.
- **Sub-models.** `Model` has about 45 fields and four embedded structs;
  input, chat navigation, sidebar and dialogs become sub-models. `view.go`
  (1,760 lines) splits by area; `transcript.go` (2,060) into fold, event
  renderers, tool-argument formatting, markdown and diff.
- **Keys.** `keyMap` covers 85 sites, about 60 raw string comparisons bypass
  it, and the key legend is 126 literals derived from nothing. Every key
  moves into the map; `handleKey` goes key → action id → function; hints come
  from the bindings.
- **Theme.** Styles are package variables built at init, with about 50
  inline glyph literals, and glyph strings double as line-type identity
  (`GlyphPrompt == GlyphAnswer`). A `Theme{Colors, Glyphs}` value rebuilds
  the styles on `Set`; `Line` gets a semantic kind and the glyph is resolved
  at render time.
- **Commands and segments.** The palette table and the dispatch switch list
  every alias separately: commands carry `Run`. The divider is hand-assembled
  and sidebar clicks index magic header rows (`sidebarDiscordRow = 4`): both
  become lists of `segment{render, click, priority}`.
- **One ask card.** `questions.go` and `permissions.go` mirror each other
  method for method, as do their transcript twins; the permission body is
  drawn twice by different code and the shell command is extracted three
  ways. One generic card.
- **Needs-you message.** One `needsYouMsg` emitted where a prompt is first
  seen or an agent starts waiting on the human: the hook point for the
  notifications in the plugin doc.

## Phase 4: DRY and cleanup

- **Vocabulary.** "preset" appears 126 times in non-test code and
  "archetype" 19, the wire (`MPresets`, `PresetInfo`), the model-facing
  prompt and error text included; the old plan records "role everywhere" as
  done. Rename with type information. The CLI flag `--root archetype` and the
  trust prompt's word "presets" are user-visible: **D7**. `superChat` →
  channel chat. `Channel` receivers still named `s` in `orchestrator.go` and
  as `s := a.c` across the package.
- **Dead and compatibility code.** The `plugins` key, `Effective.Plugins`,
  the `stavlos plugin` stub and "Plugins: restart" (until the real thing);
  `KindPlugin`, `Registry.Register`, `pluginStatus`, `ProviderInfo.Kind`;
  legacy reply tracking (`replyDebt.legacy`, `settleLegacy`, `lastPost`, the
  empty-`Kind` branch, `ChatPayload.Post` beside `Posts`);
  `Recipients.UnmarshalJSON`; "legacy" protocol fields; `awaiting` counts
  beside `waits` sets; the inbox projected twice (`agentState.inbox` and the
  builder); the dead `"agent:"` source branch; `ErrTrust`,
  `ErrInvalidRequest`, `ProviderListParams`, `QuestionPosition`,
  `ExtraBody`, `Tokens.IDToken`, `MCP.URL`; stale comments naming anthropic,
  API keys and bash aliases.
- **Small duplicates.** Atomic write (five sites, one of them not atomic) →
  `fsx.WriteAtomic`; `contains` → `slices.Contains`; the 32 KB cap (four
  sites); `limitedWriter`/`cappedBuffer`; URL and host normalisers
  (`urlHost`/`parseWebURL`, `hostsAllow`/`validHost`); `mcpNamePrefix`
  against `toolname.MCPPrefix`; "label (role)" built six times; the list row
  idiom seven times; string literals `"request"`, `"response"`, `"user"`,
  `"question"` beside their constants; escalation defaults applied twice.
- **Tests.** Split `tui/view_test.go` (3,200 lines), `daemon_test.go`
  (2,800) and `render_test.go` (1,000) by area; replace the 29 `time.Sleep`
  calls with waits on events; tests stop mutating package globals
  (`render.highlight`, `transcript.ShowThinking`); add tests for `event`
  payload round trips and `model` cost and usage.
- **Docs.** The PRD still describes go-plugin providers as architecture,
  says sandboxing is on the roadmap, describes fork as existing and calls
  the local layer tighten-only in one place and trusted in another; the
  README's "Not yet" lists the go-plugin seam. Fix them, add an
  `ARCHITECTURE.md` with the package graph and the layering rules (enforced
  by a `go list` check in the gate), and a LICENSE (**D8**).

## Then plugins

| The plugin design needs | Provided by |
|---|---|
| Hooks after commit, outside `Channel.mu`, never on recovery | 3.2, 2.1 |
| "Events caused by a plugin carry its name", no hook loops | 3.1 origin |
| `plugin.*` events and notices without touching a dozen files | 3.1 registry, notice |
| Tighten-only permission hook that yolo and permits cannot flatten | 1C.1 stages, sticky verdicts |
| Host calls "decided by policy as the plugin" | 3.3 principals, 1C.2 permits by principal |
| `stavlos.shell`/`fetch`/`read` outside a turn | 3.4 typed entry points, `baseEnv` |
| `lua__plugin__name` tools with a policy key, default ask, not auto-allowed | 3.4 registry metadata, 1C.8 namer, 1F.5 |
| `stavlos.steer` as `system`, no reply debt, rate limited | 3.5 |
| One VM per channel with start, reconfigure, stop | 3.6 |
| A host owned by the daemon, told about config reloads | 3.7 services, `reloadChannel` |
| Project plugins under the trust hash, safe to execute | 1B.1–1B.4 (the hash already covers `.stavlos/**`) |
| Plugin files not writable through the socket; scoped connections | 1B.6, 1A.3 |
| TUI keys, theme, commands, segments, notify | 3.13 |
| Bridges as JSON-RPC clients | 3.9, 3.11 |

## Order and size

Phase 0 first, in full. Then 1A, 1B, 1D, 1G and 1H, which are local fixes.
1C and 1E are delivered *by* 3.3 and 3.4, so those two seams are pulled
forward and done as the security work, with 3.1 and 3.2 before them because
they change the event schema (one wipe, not two). 1F waits on D2–D4. After
that Phase 2 (2.5 needs 3.1's notifications; 2.7–2.8 ride on 3.2), the rest
of Phase 3 (3.7 → 3.8 → 3.9 → 3.10 → 3.11, with 3.12 and 3.13 independent),
and Phase 4 last, except the dead-code deletions, which go first wherever
they shrink a file about to be restructured.

Roughly: Phase 0 about 10 commits, Phase 1 about 45, Phase 2 about 25,
Phase 3 about 60, Phase 4 about 20.

## Not doing

- Per-agent actor loops. The serialized channel is simpler and has no
  cross-agent locks; 2.7 removes its one real cost.
- Replacing SQLite, Bubble Tea or discordgo. None of the findings needs it.
- Moving Discord to a plugin. It is a JSON-RPC client and stays one.
- New features: the config-defined provider, `keys`/`theme` config and
  plugins themselves come after, each as its own piece of work.

## Decisions

- **D1 Trust and code.** A trusted project can already set yolo, disable the
  sandbox and define MCP commands, so trust already means "run this repo's
  code". Keep that and let `.stavlos/plugins/*.lua` ride the same prompt, or
  make the prompt list what the config grants (mode, sandbox, allows, MCP
  commands, plugins)? The second is a visible change. *Recommended: list the
  grants.*
- **D2 † Sandbox below `Full`.** Refuse auto and yolo, ask once per channel,
  or only show the level? *Recommended: show the level always; entering auto
  or yolo below `Full` asks for confirmation once.*
- **D3 † Read allowlist.** Landlock read restrictions make the boundary real
  but break commands that read outside the working directories and the
  allowlisted system paths. *Recommended: on at `Full`, with `sandbox.readable`
  to extend it.*
- **D4 † Shell as egress in auto.** With the network on, should auto mode
  still ask for shell commands that can reach the network? Every shell
  command can, so this means "auto implies `network: false` unless the
  channel opts in". *Recommended: yes.*
- **D5 Discord run mode.** In-daemon service or separate supervised process?
  *Recommended: separate process started and supervised by the daemon, on a
  `bridge`-scoped connection: discordgo and untrusted Discord JSON leave the
  process that holds `auth.json`.*
- **D6 Public client.** Publish `protocol` and the readable event payloads
  under `pkg/`, or move `pkg/client` to `internal/` until there is an outside
  consumer? *Recommended: move it inside for now; publish with the first
  release.*
- **D7 Words people see.** Rename `--root archetype` and the trust prompt's
  "presets" to "role"? Keep "yolo" as the mode's name? *Recommended: role
  everywhere, keep yolo.*
- **D8 Licence.** The README says open source; there is no LICENSE file.
