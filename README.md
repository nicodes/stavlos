# Stavlos

An open-source agent harness in Go. It runs a tree of AI coding agents on your machine as long-lived actors you can prompt, steer, cancel, and kill, from a terminal UI or any client that speaks its JSON-RPC protocol. The design is in [docs/stavlos-prd.md](docs/stavlos-prd.md).

## Quick start

```sh
cd ~/some/project
go run ~/path/to/stavlos/cmd/stavlos     # or: go install ./cmd/stavlos, then `stavlos`
```

The TUI opens immediately. With nothing configured, the first prompt answers "no model selected"; run `/providers` to sign in and `/models` to pick a model. The first model you pick becomes your default for future sessions.

Stavlos uses your existing subscription, not platform API keys. `/providers` offers two sign-ins:

- **ChatGPT** (Plus or Pro, through the Codex sign-in). It signs in through your browser by default. A headless URL-plus-code option exists for SSH boxes once "Device code authorization for Codex" is enabled in ChatGPT's Security settings.
- **Grok** (SuperGrok, through the Grok CLI sign-in). It shows a URL and a short code.

Tokens live in `~/.local/share/stavlos/auth.json` (mode 0600) and refresh automatically. On the command line, `stavlos auth login [openai|xai]`, `stavlos auth list` and `stavlos auth logout` do the same. Model ids are `openai/gpt-5.4`, `xai/grok-4`, and so on; `/models` lists what each subscription serves.

## Using the TUI

### Talking to agents

A session opens on its chat, where you talk to every agent. `@name` delivers your message to that agent (several mentions deliver it to each, and `@` autocompletes names); a message with no mention goes to `main`, the root agent. A mention that names no agent refuses the message. Agents answer you there with `message`. Posts and replies show in the order they happen, and while an agent you messaged has not replied yet, a loader at the bottom names it. Only posts and replies show there: tool calls, permission prompts, questions and notices stay in each agent's own chat, and the strip and sidebar badges still tell you when something is waiting on you. Space on a short reply opens that agent's chat.

Selecting an agent in the sidebar opens its own chat, with its tool calls and notes, where typing talks to that agent alone. The "# chat" row at the top of the sidebar, or `/chat`, goes back. If an agent is busy, your message reaches it at its next step. The input grows as your message wraps; ctrl+j breaks a line and enter sends. If the agent is busy, your message reaches it at its next step. `/queue <text>` waits for the current turn to end instead, and esc pressed twice on an empty input cancels the current turn (the first press warns).

Agents reply with the `message` tool, to you or to another agent. The text an agent ends a turn with is its notes: it reaches no one and shows dimmed in its chat. Every message an agent receives, from you or from another agent, is owed a reply. Whenever a turn ends with a reply still owed, the agent is nudged with another turn, unless it is waiting on an agent or a job (their result wakes it anyway). After three nudges in a row with no reply it is left alone until it replies or something new arrives. The due tab lists who is still owed. `"reminders": false` in `stavlos.json` turns nudges off.

The meta row under the input shows the selected agent's role, model and variant, then how full its context is (`31% of 200k`, orange from 70%), its tokens and its cost. Context is compacted on its own when an agent's history passes 80% of its model's window: older turns become a summary. `/compact` does it for the selected agent right away, or before its next model call if it is busy. A compaction is an item in the chat: a rule with a sweeping bar while it runs, replaced in place by `┄┄ compacted 84k → 12k tokens ┄┄` and the summary when it is done.

### Focus and keys

- Tab and shift+tab cycle focus from top to bottom: the chat, the input, the tab strip, the meta row, and the sidebar (ctrl+b).
- Space is the select key everywhere outside a text field. It opens the highlighted tab or meta-row part, picks a dialog row, selects an agent, and expands a tool call's output in the chat.
- Enter anywhere but the input closes what is open and returns to typing.
- In the chat, ↑/↓ move item by item. In the input, ↑/↓ walk your prompt history; on the start screen they recall the first prompts of this directory's earlier sessions, and `/sessions` picks one to resume.
- On the meta row, ←/→ pick the mode tag, role, model or variant, and enter opens its dialog. `/roles`, `/models` and `/variants` open the same dialogs.
- The "/" palette lists every command. `/help` shows a key bar at the bottom (off by default; `/help` again hides it).

### The tab strip

A strip under the input holds seven tabs, each always there with its count: "permission", "questions", "async", "due", "todo", "mcp" and "dirs". Tab lands on the leftmost, ←/→ move the highlight, enter or a click opens that tab's dialog, and esc returns to where you came from.

- **permission** holds the permission prompts. The permission and questions dialogs show the selected agent's prompt first and the oldest one otherwise, while the strip counts every prompt in the session.
- **questions** holds `ask_user` batches (see below).
- **async** shows what the selected agent is waiting on: the agents whose answer it expects (a child it tasked, a sibling or parent it messaged) and its running shell jobs.
- **due** is the other direction: who is waiting on the selected agent's reply, you first, then any agent that messaged it. Space opens that chat.
- **todo** lists the selected agent's plan.
- **mcp** lists its MCP servers with their state, tool count and uptime.
- **dirs** edits its working directories: `a` adds, enter replaces, ctrl+d removes; the session directory stays.

### The sidebar

The sidebar (ctrl+b) is the swarm nav: the session directory, its tokens and cost, the swarm state (`3 working · 1 waiting`), then a "channels" heading over the "# chat" row and, one level in, the agent tree with a `!` or `?` badge on any agent whose permission or question is pending and its cost at the right edge. ↑/↓ move, space selects, `n` jumps to the next agent waiting on you, and a click on a row selects it.

Under the tree, a folded "sessions" section lists this directory's other sessions, each with a state dot (full orange while an agent works, half while one waits, empty when idle). Space unfolds it, and space on a session resumes it in place.

### Permissions and modes

Permission prompts show the command (or path) with the asking agent after it, then a fixed list of answers; ↑/↓ move and space chooses:

- A plain permission offers "Allow once", "Allow for this session" (this exact call), "Allow `<prefix>` for this session" for a simple shell command, and "Deny", which opens a row for an optional reason the agent reads. The prefix is the first word, or two for git, go, npm, cargo, make, docker and the like: `go test` then covers every `go test …` that is not chained, piped or redirected. It is never offered for wrappers such as `bash`, `env`, `sudo` or `python`.
- A boundary prompt, for a call outside the agent's directories, offers "Allow once", "Allow and add <dir>", "Allow and add another directory…" and "Deny". "Allow and add" puts the directory on that agent: the whole git checkout when the path is inside one, else the path's directory.
- The trust prompt for a project's `.stavlos/` offers "Trust this project's config" or "Not now".

Esc closes a dialog with the prompt still waiting.

`/mode` picks the session's permission mode:

- **ask**, the default, prompts for every policy ask and every call outside an agent's directories.
- **auto** (`/auto`) approves permissions inside the agent's directories; a call outside them is denied, and the agent is told that auto mode does not allow it (switch to ask to grant a directory with "Allow and add").
- **yolo** (`/yolo`) approves everything a policy would ask about, directories included.

Switching a mode on answers whatever is waiting as that mode would have: yolo allows every permission prompt, auto allows the ones inside the directories and denies the ones outside. Deny rules, model questions and the trust prompt apply in every mode. An ASK, AUTO or YOLO tag before the role always shows the mode: clicking AUTO or YOLO goes back to ask, clicking ASK opens `/mode`. Auto is prompt-free inside the directories only as far as the harness can see: paths in shell commands come from inspecting the command line, not from a sandbox, so keep deny rules for what must never run.

## Roles

One role ships built in, `general`, which can read, edit, run commands and delegate to more `general` agents. Add specialised ones as `roles/<name>.md` in the global or project config; `.stavlos/roles/coder.md` in this repository shows every key:

- `mode`: primary, subagent or all
- `models`: a whitelist, with the variants allowed per model
- `tools`: every tool is available by default; `<tool>: deny` removes one, and policy rules nested under a tool tighten it
- `spawn`, `max_turns`, `color`, `dirs`, `skills`, `mcp`

The role decides what `/roles`, `/models` and `/variants` offer, and the daemon enforces it.

## What agents can do

**Delegate and message.** Delegating to child agents is the model's job (`agent_create`). Every agent has a unique name in its session (the root is `main`; a name already taken gets a suffix, `scout-2`), and agents talk with one tool, `message`, addressed by name to any other agent or to you as `user`. To an agent waiting on the sender, such as a parent waiting on its child's task, a message is the answer: it wakes that agent between turns, never mid-turn. To any other agent it is a new message that reaches it at its next step, even mid-turn, and the sender waits for the answer. A role can keep its agents from messaging you with a deny rule on `message` for `user`. A child idles with its context intact for follow-ups for the rest of the session: nothing kills it, and its MCP servers stop after ten idle minutes. There is no wait tool.

**Run commands.** Agents run commands with one `shell` tool. Read-only commands such as `grep`, `rg`, `find`, `ls`, `cat` and `git status`/`log`/`diff` run without a prompt, but only as one simple command: chain, pipe or redirect one and it asks. A call waits up to 15 seconds; a command still running then continues as a background job (the call returns its id and the output so far), and `background: true` skips the wait for servers. A job's exit wakes its agent the same way a response does, and `shell_kill` stops a job. Every command and MCP server runs with a scrubbed environment, without `STAVLOS_*` or any variable whose name looks like a credential, so list what a build really needs under `"env": {"pass": ["GITHUB_TOKEN"]}` in `stavlos.json`.

**Reach the web.**

- `web_fetch` returns one page as markdown, 20k characters at a time, with HTML boiled down to headings, text, lists, links and code. http is upgraded to https, credentials are stripped, private and local addresses are refused, cross-host redirects are reported rather than followed, and pages are cached for 15 minutes. It asks by default, and the dialog offers "Allow <host> for this session". The policy in `stavlos.json` allow-lists hosts (`"web_fetch": {"https://github.com/*": "allow"}`, matched against the URL as it will be fetched: lower-case host, https, no credentials), and roles may tighten further per URL.
- `web_search` returns title, URL and snippet. Out of the box it uses Exa's free, keyless endpoint, the same one OpenCode uses, which has no published rate limit and may change. For your own quota, configure a backend under `search` in `stavlos.json`: `{"provider": "brave" | "tavily" | "exa", "apiKey": "${env:BRAVE_KEY}"}`. It asks until you configure a backend (its keyless fallback is a third party you never chose) and is allowed once you have.

Auto mode approves fetches like any read-only call. Everything fetched is handed to the model as untrusted data.

**Plan.** `todo_add` and `todo_update` keep a per-agent list that is logged, projected into the system prompt at every call (so it survives compaction) and shown to you in the todo tab.

**Work in directories.** Every agent may read, edit and run commands in the session directory plus its role's `dirs`. A parent can grant a child directories from its own set at `agent_create`. A call that reaches outside asks first (see Permissions and modes), and the dirs tab edits the set by hand.

**Use MCP servers.** A role's `mcp:` list starts MCP servers for that agent alone (stdio servers defined under `mcp` in `stavlos.json`). The model sees their tools as `mcp__<server>__<tool>` and calls them through the usual permission path.

**Ask you.** An agent can ask you one to four short questions with `ask_user`. Each batch is a prompt of its own kind that waits until you answer it (no timeout, no auto-approval), in the questions tab. Every question is its text plus a checklist: space toggles options, the last row takes something typed, enter confirms and moves on, and the last answer sends the batch.

## Other commands

```sh
stavlos resume [id]            # reattach to the latest session for this directory
stavlos sessions               # list sessions (they survive daemon restarts)
stavlos tree <session>         # agent tree with state and cost
stavlos send|steer|cancel|kill <agent> [text]
stavlos trust [dir]            # confirm a project's .stavlos/ layer
stavlos init [--model p/m]     # write a starter global config
stavlos status                 # daemon status
stavlos auth login|list|logout # subscription sign-in
stavlos daemon                 # run the daemon in the foreground (there is no separate binary)
stavlos --version              # version, commit and build of this binary
```

## Project configuration

Put a `.stavlos/` directory in a repository to add roles (`roles/<name>.md`), skills (`skills/<name>/SKILL.md`), MCP definitions, and policy tightening in `stavlos.json`; `AGENTS.md` at the repo root is injected into every agent. The whole layer is untrusted until you confirm it once per content hash, from the TUI prompt or `stavlos trust`.

## Status

Implemented: daemon with SQLite event log, actor scheduler, projector (cancelled-turn repair, restart recovery, compaction), built-in and orchestration tools, three-layer config with trust gate, declarative policy, escalation with claim tiers and headless default, usage accounting, JSON-RPC protocol over a Unix socket with offset replay, Go client, an opencode-style Bubble Tea TUI, ChatGPT (Codex backend) and Grok subscription adapters with browser and device-code sign-in, models.dev metadata.

Not yet: Discord service, go-plugin model seam, `stavlos plugin install`, remote (HTTP) MCP servers, session fork in the TUI (the protocol supports it), a sandbox for shell commands.

## Development

```sh
go run ./cmd/stavlos      # the client re-executes itself as the daemon
go test ./...
```

`scripts/check.sh` runs the whole gate a change must pass: gofmt, vet, the exhaustive-switch lint, staticcheck, a cyclomatic-complexity bound of 30, the race detector on the concurrent packages, and the suite three times.

The client checks the daemon's build id on connect and restarts it when the daemon was built from older code, so editing and re-running with `go run` just works. Set `STAVLOS_KEEP_DAEMON=1` to skip that. Daemon output is in `~/.local/share/stavlos/stavlosd.log`.
