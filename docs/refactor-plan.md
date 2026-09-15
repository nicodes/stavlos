# Refactor plan: security, performance, maintainability, DRY (2026-09-15)

Five read-only audits (security; agent runtime and daemon; data and protocol; tools, config and models; TUI) over 27k lines of source. Findings marked **verified** were reproduced against the code. Stavlos is unreleased: the event log is wiped at every schema change, and nothing keeps backwards compatibility. Every step lands as its own commit behind `scripts/check.sh`.

## Phase 0: critical security (do first)

| # | Problem | Fix |
|---|---|---|
| 0.1 | **verified** `.stavlos/stavlos.local.json` in a cloned repo loads with no trust prompt, is excluded from the trust hash, merges into the base policy (can loosen, even override a global deny), can replace MCP commands, pass env. An auto-mode agent can write it. | Personal overrides move out of the repo (decision D3). The project layer may only tighten: `env.pass`, `mcp`, `limits`, `search` rejected in the project layer. |
| 0.2 | **verified** Default "read-only" shell allows run code: `find . -exec sh -c … \;`, `rg --pre sh`, `find -delete` are "simple" and match `find *`/`rg *`. | Decision D2: native read-only search tools and no default shell allows, or argv-parsed allows with a flag denylist. |
| 0.3 | **verified** Policy globs: a middle `*` never crosses `/` (deny `git push * --force` misses `origin/main`); malformed patterns fail silently and never match. | Compile rules once at load into matchers chosen by subject kind (command/URL/text: `*` crosses `/`; path: path semantics). Invalid patterns are load errors. |
| 0.4 | **verified** apply_patch writes `<file>.stavlos-tmp` with `os.WriteFile` (follows a planted symlink). Plus: two Update sections for one file lose the first; delete+add of one path fails; a missed `@@` anchor on an insertion appends at EOF; CRLF files get mixed endings. | A patch plan keyed by resolved path (virtual overlay): chained updates, delete+add as replace, anchor miss is an error, keep the file's line ending. Temp files via `os.CreateTemp` (O_EXCL, random) then rename. |
| 0.5 | **verified** A remembered channel allow checks only the subject's first value. Prefix allows are offered for wrapper programs missing from the denylist (`find`, `sed`, `tar`, `make`, `docker run`…). | `covers` requires every value covered. Prefixes offered only for an explicit allowlist of safe programs. |
| 0.6 | **verified** Shell directory boundary: `~user`, unexpanded `$VAR`, and in-repo symlinks bypass it (`cat ~root/.ssh/id_rsa` is simple). | `~name` and unexpanded variables count as outside; relative arguments resolve through `ResolvePath` (symlinks). (The real boundary is an OS sandbox: decision D1.) |
| 0.7 | **verified** Terminal injection: the trust dialog (and `stavlos trust` CLI) prints repo file names raw; bidi overrides and zero-width characters are not neutralised; many protocol strings (roles, providers, models, login text, RPC errors) skip sanitising. | One typed sanitising boundary from protocol data to view data, `Visible()` shows bidi/zero-width as `<U+202E>`, and a fuzz test injecting ESC/OSC into every string field asserting `View()` holds only the TUI's own SGR. |
| 0.8 | Any process with the user's uid (so any agent shell command or MCP server) can drive the daemon socket: set yolo, answer its own prompt, add `/` to the directories, trust a repo. | Capability-based socket: refuse control methods (`set_mode`, `prompt.reply`, `trust.reply`, dirs, providers) from peers descended from the daemon (SO_PEERCRED pid → ppid chain), and a per-client token for interactive clients. |
| 0.9 | Auto mode allows web egress to any host and every MCP call without a prompt. | In auto, a host not yet allowed and an MCP tool with no rule still ask. |
| 0.10 | Agents can edit control files. | `.stavlos/**`, `AGENTS.md`, `.git/config`, `.git/hooks/**` always ask, in every mode. |
| 0.11 | Hygiene: `events.db*` are 0644; stale-daemon replacement kills the pid the daemon reports; the client never checks the server's uid; env scrub is a name denylist. | `umask(077)` for the daemon and chmod of the data dir; kill by SO_PEERCRED pid; client checks peer uid; env pass as an allowlist. |

## Phase 1: concurrency correctness

- **verified** Deadlock: `takeInputs` holds `a.mu` while `senderLabel` takes the sender's `mu` (and `reminderText`); two agents with inputs from each other block forever. Fix: store the sender's name when the input is queued; never take another agent's lock while holding one's own.
- Shutdown: `Close` cancels contexts and closes the log without waiting, so cancelled turns log (or lose) events in a race. Fix: per-channel WaitGroup and a shutdown cancel cause that suppresses logging; `Close` waits, then closes the log.
- `rememberModel` does disk I/O under `d.mu.RLock`; `Agent.Info` unlock-relock trap; `Channel.Info` walks the tree three times; `escalation.Pending` returns map order.

## Phase 2: event model and storage (schema wiped)

- Inputs: `input.queued{id, kind, text, source, post}` and `turn.input{turn, ids}` replace prompt.queued / steer.received / note.queued / response.received and the text in user.message; recovery stops matching by text.
- Fold `usage` into `assistant.message`; `tool.started` carries only call id and name (the input is in the assistant message).
- Prompts: `ask.requested` and `ask.resolved{outcome, answer, by}`; claimed/escalated become live notifications only. Drop the never-emitted `monitor.disarmed`; `monitor.armed` becomes a flag on started.
- One name per concept: **role** everywhere (not archetype/preset), **ask** for permission/question requests, **input** for the inbox, `compaction.{started,done,failed}`.
- SQLite: `channels(id, name UNIQUE, dir, created, archived, title, last_seq)` maintained inside the append transaction (no separate `PutChannel`, no orphan channels); integer nanosecond time; no AUTOINCREMENT; a `trust(dir, hash)` table instead of one kv blob; on a schema mismatch delete the file (the current wipe leaves a 74MB file 99% free).
- A log writer goroutine with group commit: callers wait for their seq; a step's events commit as one batch; each notification is encoded once and fanned out as bytes outside any global lock.

## Phase 3: runtime

- One reducer: `agentState.apply(e)` and `channelState.apply(e)` used by the live path (build event → append → apply) and recovery (fold the log, then side effects). Most of `recover.go` goes, with its drift (turn-limit settle, dead `armed` loop, different `a.events`, context size not restored).
- Incremental projector: model history and token estimate folded per event from typed payloads; `history()` is a snapshot, not an O(events) JSON decode per step. Compaction works on the message list; summaries keep their order; recovery trims at Compacted.
- Stable prompt prefix: system prompt and tool definitions depend only on role, config and directories (cached per agent); turn budget, busy count and todos go in a trailing context block; `agent_create` is always offered and refuses when limits say so. Keeps provider prompt caches warm.
- Decision D4: an actor loop per agent (one goroutine owns the state, others send typed commands; `Info` is a published snapshot) removes lock ordering by construction.

## Phase 4: protocol and client

- Methods declared once as typed descriptors (`Method[P, R]{"channel.post"}`) shared by client and daemon; generic `Call[P,R]`; handlers registered against descriptors with channel/agent resolution done once. About 40 client wrappers and the handler boilerplate go.
- Consistent keys (`channel`, `agent`), typed results (no `map{"ok"}`), internal-error default code (today most failures report "invalid params"), one `CreateChannel(params)` and one `ReplyPrompt(params)`.
- Replay: paged with backpressure, clients resume from their last seq (the TUI always replays from 0 and a long channel can overflow the queue and drop the client in a loop).
- Live state vs history: events for history only; notifications for prompt changes, stream deltas coalesced (~50ms / 4KB) and agent state, so the TUI stops refetching the tree on nearly every event. Prompt changes reach clients once, not twice.

## Phase 5: tools, config, models

- Typed tool framework: decode input once (strictly), `Subject` and `Run` share the parsed input (apply_patch and web_fetch stop parsing twice), cached tool definitions and schemas, one place for default clamping and output clipping (the 32KB cap is written four times; job results ignore the configured cap).
- An OpenAI-wire kit shared by the codex and chatcompletions adapters (client shell, bearer header, `toTools`, argument normalisation, usage, tool-result rendering, error envelopes); fixes their drift ("ERROR:" vs "Error:", "(no output)").
- Config as one schema with defaults and per-layer rules (Search defined three times, Escalation/Compaction twice, starter config duplicating defaults).
- Correctness: retry once on a mid-stream failure before any tool call; web_fetch pages by runes; chatcompletions builds tool arguments with a builder (quadratic today); one shared HTTP transport with the dial check; web_search as a provider table and Exa through the MCP SDK; read tool limit logic; models.dev keeps only used providers and refreshes periodically; ChatGPT login redirect host; Grok poll 5xx.

## Phase 6: TUI

- A `Frame` computed once per update: section rectangles, rendered parts, click spans and dialog geometry used by `View`, the mouse and the viewport height (layout is computed separately in about six places and has drifted).
- One list component (cursor, scroll, keys, rows with actions, hit-testing) for the tab dialogs, overlays, palette, mention list and the sidebar (the sidebar's row arithmetic is written out in nine places and it cannot scroll).
- Tab descriptors `{focus, glyph, title, count, warn}` instead of string surgery.
- `model.go` (4k lines) split into sub-models: chat, input, sidebar, dialogs, prompts, sign-in; viewport refreshes from dirty flags at the end of `Update`.
- Performance: the spinner ticks only while something animates (today 12 full redraws a second when idle); rendered rows kept as `[]string` with only the visible window assembled; replay folds events without layout; an index for untied calls (replay is quadratic).
- A `tuitest` package for the duplicated test helpers.

## Phase 7: cleanup

Dead code (listed in the audits), naming (`Channel` receiver `s`, `root`/`rootArch` vs main, `yolo` names for modes), stale comments, docs (PRD, README), memory.

## Decisions (settled 2026-09-15)

- **D1** Include a Linux OS sandbox now for shell commands and MCP servers: writes limited to the channel's directories and a private tmp; the data dir, config dir, socket and credentials hidden.
- **D2** Native read-only `grep` and `glob` tools that obey the directories; no default shell allows, so every shell command asks unless the mode or a rule answers it.
- **D3** `.stavlos/stavlos.local.json` stays in the repo but is part of the trust hash and applies as a tighten-only overlay like the project layer.
- **D4** Full redesign: an actor loop per agent, one reducer for live and recovery, the TUI split into sub-models with a Frame and a shared list component.

## Phase 2–3 blueprint (vocabulary and runtime, schema 6)

**One state machine per channel.** `Channel.mu` guards the whole channel: its settings and every agent's state. State changes only through `commit(evs...)`: append to the log, then `apply` each committed event to the in-memory state, both under the lock; the side effects `apply` reports (wake an agent, cancel a turn) run after it is released. Recovery folds the same `apply` over the log, then resumes. There is no per-agent mutex, so no lock ordering between agents exists to get wrong. Model calls and tool runs happen outside the lock on each agent's turn goroutine, which reads a snapshot and commits what it did.

**Vocabulary.**
- channel: `channel.created{name, dir, model, role}`, `channel.updated{name?, model?, mode?}`, `channel.archived`, `channel.dir_added{dir, source}`, `channel.dir_removed{dir}`, `chat.posted{id, text, to}`.
- agent: `agent.spawned{id, parent, role, name, model, variant, depth}`, `agent.updated{role?, name?, model?, variant?}`, `agent.killed`, `chat.message{from, text, post}` (to the human).
- inbox: `input.queued{id, kind, text, from, from_name, post, job, parties}` with kinds prompt (human, waits for the turn), steer (human, next step), request (agent, next step, owed), info (agent, next step, never wakes), response (agent, between turns, settles), job (a job's result, between turns), reminder (harness); `input.taken{turn, ids}`: which inputs a model call consumed. Recovery's inbox is queued minus taken, by id.
- turn: `turn.started{turn}`, `assistant.message{turn, blocks, stop, model, usage, cost}` (usage folded in), `tool.started{turn, call_id, name}` (input stays in the assistant message), `tool.finished{…}`, `turn.ended{turn, reason, error}`, `turn.aborted{turn}`.
- asks: `ask.requested{id, kind, call_id, question}`, `ask.resolved{id, outcome, answer, by}`; claims and escalations are live notifications only. `permit.granted{tool, call, prefix}`.
- jobs: `job.started{id, command}`, `job.finished{id, exit_code, is_error, summary, output}`, `job.stopped{id, reason}` (no armed/disarmed: a running job always wakes its owner).
- `todo.changed{items}`, `mcp.started/failed/stopped`, `compaction.started{before}`, `compaction.done{from_seq, to_seq, summary, before, after}`, `compaction.failed{before, error}`.

**Projection.** Each agent's state holds an incremental history builder fed by `apply` (inputs taken, assistant messages, tool results, turn ends, compaction). `history()` is a copy of its messages: no event is decoded twice.

**Prompt prefix.** The system prompt and tool list depend only on the role, config and directories and are cached per agent; the turn budget, busy count and todo list go in a trailing context block of the request, so provider prompt caches stay warm.

## Progress

- **Phase 0 (security): done.** Deadlock, compiled kind-aware policy (every command in a line judged), trust-gated tighten-only local config, native grep/glob with no default shell allows, patch overlay with random temp files, permits for every value and allowlisted prefixes, the shell boundary (~user, expanded variables, symlinks), bidi controls and an SGR-only TUI frame, egress asks in auto, control files ask in every mode, kernel-reported socket peers (processes the daemon runs are refused), a private non-dumpable daemon with an environment allowlist, and the Linux sandbox (Landlock plus a user and mount namespace) for shell commands and MCP servers.
- **Phase 2 (storage): done.** A writer goroutine with group commit and barriers, the channels table as a projection maintained in the append transaction, a trust table, integer times, schema 5 (a file of another version is deleted).
- **Phases 2–3 (vocabulary and runtime): done.** Schema 6 vocabulary; one state machine per channel with one reducer for live and recovery (a serialized channel, not per-agent actors: the same guarantee with no cross-agent locks at all); the incremental projector with compaction cuts; the cached prompt prefix with a per-request state note; background jobs, MCP and the prompt prefix as runtime state; a bounded, silent Channel.Stop; the daemon never calls a channel under its own lock.
- **Phase 4 (protocol): mostly done.** Typed method descriptors shared by client and daemon (client.Do, route); role and name on the wire; every channel id travels as channel; dead fields and the prompt record hook removed. Still to do: replay from the last seq with per-channel transcripts kept, coalesced stream deltas.
- **Phase 5 (tools, models): started.** One wire vocabulary for the two OpenAI-shaped adapters; web_fetch pages by characters; shared web transports; read limits as documented; one clip rule; a search backend table. A broken stream is sent again once (Reset delta, fresh codec). Still to do: config as one schema, catalog trim.
- **Phase 6 (TUI): started.** The spinner ticks only while something animates; one frame computation decides where the chat and footer sit for layout, drawing and the mouse. Still to do: a shared list component, model.go split into sub-models, replay from the last seq.
