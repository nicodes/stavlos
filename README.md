# Stavlos

An open-source agent harness in Go. It runs a tree of AI coding agents on your machine as long-lived actors you can prompt, steer, cancel, and kill, from a terminal UI or any client that speaks its JSON-RPC protocol. The design is in [docs/stavlos-prd.md](docs/stavlos-prd.md).

## Quick start

```sh
cd ~/some/project
go run ~/path/to/stavlos/cmd/stavlos     # or: go install ./cmd/stavlos, then `stavlos`
```

The TUI opens immediately. With nothing configured, the first prompt answers "no model selected"; run `/providers` to sign in and `/models` to pick a model. The first model you pick becomes your default for future channels.

Stavlos uses your existing subscription, not platform API keys. `/providers` offers two sign-ins:

- **ChatGPT** (Plus or Pro, through the Codex sign-in). It signs in through your browser by default. A headless URL-plus-code option exists for SSH boxes once "Device code authorization for Codex" is enabled in ChatGPT's Security settings.
- **Grok** (SuperGrok, through the Grok CLI sign-in). It shows a URL and a short code.

Tokens live in `~/.local/share/stavlos/auth.json` (mode 0600) and refresh automatically. On the command line, `stavlos auth login [openai|xai]`, `stavlos auth list` and `stavlos auth logout` do the same. Model ids are `openai/gpt-5.4`, `xai/grok-4`, and so on; `/models` lists what each subscription serves.

## Using the TUI

### Talking to agents

A channel opens on its chat, where you talk to every agent. Start a message with one or more `@name`s, separated by spaces, to send it to those agents (`@` autocompletes them); the names are not part of the message, and an `@` later in it is left alone. A message with no leading name goes to `main`, the root agent, and a leading name that is no agent refuses the message. Agents answer you there with `message`. Posts and replies show in the order they happen, and while an agent you messaged has not replied yet, a loader at the bottom names it. Only posts and replies show there: tool calls, permission prompts, questions and notices stay in each agent's own chat, and the strip and sidebar badges still tell you when something is waiting on you. Space on a short reply opens that agent's chat.

Selecting an agent in the sidebar opens its own chat, with its tool calls and notes, where typing talks to that agent alone. The "#name" row at the top of the sidebar, or `/chat`, goes back. If an agent is busy, your message reaches it at its next step. The input grows as your message wraps; ctrl+j breaks a line and enter sends. If the agent is busy, your message reaches it at its next step. `/queue <text>` waits for the current turn to end instead, and esc pressed twice on an empty input cancels the current turn (the first press warns).

Agents reply with the `message` tool, to you or to another agent. The text an agent ends a turn with is its notes: it reaches no one and shows dimmed in its chat. Every message an agent receives, from you or from another agent, is owed a reply. Whenever a turn ends with a reply still owed, the agent is nudged with another turn, unless it is waiting on an agent or a job (their result wakes it anyway). After three nudges in a row with no reply it is left alone until it replies or something new arrives. The due tab lists who is still owed. `"reminders": false` in `stavlos.json` turns nudges off.

The divider over the input shows the selected agent's role, model and variant at its left end, and at its right the agent's async, todo and mcp tabs, then how full its context is (`31% · 62k/200k tokens`, orange from 70%) and its cost. A passing message such as "copied" appears at the right end of the chat row just above the divider. Context is compacted on its own when an agent's history passes 80% of its model's window: older turns become a summary. `/compact` does it for the selected agent right away, or before its next model call if it is busy. A compaction is an item in the chat: a rule with a sweeping bar while it runs, replaced in place by `┄┄ compacted 84k → 12k tokens ┄┄` and the summary when it is done.

### Focus and keys

- Tab and shift+tab cycle focus from top to bottom: the chat, the input, the tab strip, the agent's role, model and variant on the divider, and the sidebar (ctrl+b).
- Space is the select key everywhere outside a text field. It opens the highlighted tab or divider part, picks a dialog row, selects an agent, and expands a tool call's output in the chat.
- Enter anywhere but the input closes what is open and returns to typing.
- In the chat, ↑/↓ move item by item. In the input, ↑/↓ walk your prompt history; on the start screen they recall the first prompts of this directory's earlier channels, and `/channels` picks one to resume.
- On the divider, ←/→ pick the role, model or variant, and enter opens its dialog; a click on the mode tag before the input's › switches the mode. `/roles`, `/models` and `/variants` open the same dialogs.
- The "/" palette lists every command. `/help` shows a key bar at the bottom (off by default; `/help` again hides it).

### The tab strip

The tabs, each always there with its count, are "permission" and "questions" (shown as `! n · ? n`, every channel's prompts), "dirs" (the channel's directories), and an agent's own "async", "todo" and "mcp". In an agent's chat the last three sit on the divider, before the usage; `! ? dirs` sit in the strip under the input while the sidebar is hidden, and in the sidebar (with dirs behind each channel's ⚙) while it shows. Tab lands on the leftmost, ←/→ move the highlight, enter or a click opens that tab's dialog, and esc returns to where you came from.

- **permission** holds the permission prompts. The permission and questions dialogs show the selected agent's prompt first and the oldest one otherwise, while the strip counts every prompt in the channel.
- **questions** holds `ask_user` batches (see below).
- **async** holds both directions for the selected agent: under "waiting on", the agents whose answer it expects (a child it tasked, a sibling or parent it messaged) and its running shell jobs; under "owes a reply to", who waits on its reply, you first, then any agent that messaged it. Space on an agent (or on you) opens that chat.
- **todo** lists the selected agent's plan.
- **mcp** lists its MCP servers with their state, tool count and uptime.
- **dirs** edits the channel's working directories, shared by every agent: `a` adds, enter replaces, ctrl+d removes; the channel directory stays.

### The sidebar

The sidebar (ctrl+b) is the swarm nav: the channel directory, its tokens and cost, then a "channels" heading over its "✚" at the right of the "channels" title (a popup names a new channel in this directory) and this channel's "#name" row and, one level in, the agent tree with an orange `!` or `?` in place of the dot of any agent or channel whose permission or question is pending, and each agent's cost at the right edge. ↑/↓ move, space selects, `n` jumps to the next agent waiting on you, and a click on a row selects it.

Under the tree, a folded "channels" section lists this directory's other channels, each with a state dot (full orange while an agent works, half while one waits, empty when idle). Space unfolds it, and space on a channel resumes it in place.

### Permissions and modes

Permission prompts show the command (or path) with the asking agent after it, then a fixed list of answers; ↑/↓ move and space chooses:

- A plain permission offers "Allow once", "Allow for this channel" (this exact call), "Allow `<prefix>` for this channel" for a simple shell command, and "Deny", which opens a row for an optional reason the agent reads. The prefix is the first word, or two for git, go, npm, cargo, make, docker and the like: `go test` then covers every `go test …` that is not chained, piped or redirected. It is never offered for wrappers such as `bash`, `env`, `sudo` or `python`.
- A boundary prompt, for a call outside the channel's directories, offers "Allow once", "Allow and add <dir>", "Allow and add another directory…" and "Deny". "Allow and add" adds the directory to the channel's set, for every agent: the whole git checkout when the path is inside one, else the path's directory.
- The trust prompt for a project's `.stavlos/` offers "Trust this project's config" or "Not now".

Esc closes a dialog with the prompt still waiting.

`/mode` picks the channel's permission mode:

- **ask**, the default, prompts for every policy ask and every call outside the channel's directories.
- **auto** (`/auto`) approves permissions inside the channel's directories; a call outside them is denied, and the agent is told that auto mode does not allow it (switch to ask to grant a directory with "Allow and add").
- **yolo** (`/yolo`) approves everything a policy would ask about, directories included.

Switching a mode on answers whatever is waiting as that mode would have: yolo allows every permission prompt, auto allows the ones inside the directories and denies the ones outside. Deny rules, model questions and the trust prompt apply in every mode. An ASK, AUTO or YOLO tag before the role always shows the mode: clicking AUTO or YOLO goes back to ask, clicking ASK opens `/mode`. Auto is prompt-free inside the directories as far as the harness can see paths on the command line; what a command does beyond that is held by the sandbox (below), so keep deny rules for what must never run.

## Roles

One role ships built in, `general`, which can read, edit, run commands and delegate to more `general` agents. Add specialised ones as `agents/<name>.md` in the global or project config; `.stavlos/agents/coder.md` in this repository shows every key:

- `type`: primary, subagent or all
- `models`: a whitelist, with the variants allowed per model
- `tools`: every tool is available by default; `<tool>: deny` removes one, and policy rules nested under a tool tighten it
- `spawn`, `max_turns` (unlimited unless set), `color`, `skills`, `mcp` (roles carry no directories: those are the channel's)

The role decides what `/roles`, `/models` and `/variants` offer, and the daemon enforces it.

## What agents can do

**Delegate and message.** Delegating to child agents is the model's job (`agent_create`). Every agent has a unique name in its channel (the root is `main`; a name already taken gets a suffix, `scout-2`), and agents talk with one tool, `message`, addressed by name to any other agent or to you as `user`. Each message has a kind. A request (the default) asks for something: the recipient owes a reply, the sender waits, and it reaches the recipient at its next step, even mid-turn. A response answers a request, such as a child finishing its task: it settles it and wakes the agent waiting on it between turns, never mid-turn. Info needs no reply (thanks, an acknowledgement): nobody owes or waits, and an idle recipient is not woken for it. So an agent that is waiting on you can still be asked a question first: that question is a request, and your answer later is a response. A role can keep its agents from messaging you with a deny rule on `message` for `user`. A child idles with its context intact for follow-ups for the rest of the channel: nothing kills it, and its MCP servers stop after ten idle minutes. There is no wait tool.

**Search.** `grep` and `glob` search file contents and names (with ripgrep when it is installed) and never ask. They are tools, not shell commands, so searching needs no shell permission.

**Run commands.** Agents run commands with one `shell` tool. No command is allowed by default: each asks until you allow it once, for the channel, or by prefix, or with a rule in `stavlos.json`. A call waits up to 15 seconds; a command still running then continues as a background job (the call returns its id and the output so far), and `background: true` skips the wait for servers. A job's exit wakes its agent the same way a response does, and `shell_kill` stops a job. Every command and MCP server runs with a scrubbed environment, without `STAVLOS_*` or any variable whose name looks like a credential, so list what a build really needs under `"env": {"pass": ["GITHUB_TOKEN"]}` in `stavlos.json`.

**The sandbox.** On Linux every command and MCP server runs inside a boundary the kernel enforces (Landlock, plus a user and mount namespace where the kernel allows them; the daemon log names the level). It may write only beneath the channel's directories, a scratch directory of the channel mounted as `/tmp`, and the caches build tools fill. The files that steer the harness or run code later stay read-only: `.git/hooks`, `.git/config`, `.stavlos`, `AGENTS.md` and `.envrc`. It cannot see Stavlos's own config, data, cache or socket, your runtime directory (where the D-Bus and agent sockets live), or credential stores such as `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.config/gh` and `~/.netrc`. Configure it in the global `stavlos.json` only: `"sandbox": {"network": false, "writable": ["~/.m2"], "hide": ["~/private"]}`, or `"enabled": false` to turn it off. The build caches stay writable, so a command can still poison one.

**Reach the web.**

- `web_fetch` returns one page as markdown, 20k characters at a time, with HTML boiled down to headings, text, lists, links and code. http is upgraded to https, credentials are stripped, private and local addresses are refused, cross-host redirects are reported rather than followed, and pages are cached for 15 minutes. It asks by default, and the dialog offers "Allow <host> for this channel". The policy in `stavlos.json` allow-lists hosts (`"web_fetch": {"https://github.com/*": "allow"}`, matched against the URL as it will be fetched: lower-case host, https, no credentials), and roles may tighten further per URL.
- `web_search` returns title, URL and snippet. Out of the box it uses Exa's free, keyless endpoint, the same one OpenCode uses, which has no published rate limit and may change. For your own quota, configure a backend under `search` in `stavlos.json`: `{"provider": "brave" | "tavily" | "exa", "apiKey": "${env:BRAVE_KEY}"}`. It asks until you configure a backend (its keyless fallback is a third party you never chose) and is allowed once you have.

Auto mode approves fetches like any read-only call. Everything fetched is handed to the model as untrusted data.

**Plan.** `todo_add` and `todo_update` keep a per-agent list that is logged, projected into the system prompt at every call (so it survives compaction) and shown to you in the todo tab.

**Work in directories.** A channel has one set of working directories, shared by every agent: the channel directory, the directories listed under `"dirs"` in the global `stavlos.json` (every channel gets them, and only that file changes them), plus whatever you add. Roles and `agent_create` grant none. A call that reaches outside asks first (see Permissions and modes), and the dirs tab edits the set by hand.

**Use MCP servers.** A role's `mcp:` list starts MCP servers for that agent alone (stdio servers defined under `mcp` in `stavlos.json`). The model sees their tools as `mcp__<server>__<tool>` and calls them through the usual permission path.

**Ask you.** An agent can ask you one to four short questions with `ask_user`. Each batch is a prompt of its own kind that waits until you answer it (no timeout, no auto-approval), in the questions tab. Every question is its text plus a checklist: space toggles options, the last row takes something typed, enter confirms and moves on, and the last answer sends the batch.

## Other commands

```sh
stavlos new                    # start another channel in this directory
stavlos open <#name|id>        # open a channel by name (plain `stavlos` opens this directory's)
stavlos channels               # list channels (they survive daemon restarts)
stavlos tree <channel>         # agent tree with state and cost
stavlos send|steer|cancel|kill <agent> [text]
stavlos trust [dir]            # confirm a project's .stavlos/ layer
stavlos init [--model p/m]     # write a starter global config
stavlos status                 # daemon status
stavlos auth login|list|logout # subscription sign-in
stavlos daemon                 # run the daemon in the foreground (there is no separate binary)
stavlos --version              # version, commit and build of this binary
```

## Project configuration

Put a `.stavlos/` directory in a repository to add roles (`agents/<name>.md`), skills (`skills/<name>/SKILL.md`), MCP definitions, and policy tightening in `stavlos.json`; `AGENTS.md` at the repo root is injected into every agent. The whole layer is untrusted until you confirm it once per content hash, from the TUI prompt or `stavlos trust`.

## Status

Implemented: daemon with SQLite event log, one state machine per channel with a goroutine per agent, projector (cancelled-turn repair, restart recovery, compaction), built-in and orchestration tools, three-layer config with trust gate, declarative policy, escalation with claim tiers and headless default, usage accounting, JSON-RPC protocol over a Unix socket with offset replay, Go client, an opencode-style Bubble Tea TUI, ChatGPT (Codex backend) and Grok subscription adapters with browser and device-code sign-in, models.dev metadata, native search tools, and a Linux sandbox for commands and MCP servers.

Not yet: Discord service, go-plugin model seam, `stavlos plugin install`, remote (HTTP) MCP servers, channel fork, a sandbox outside Linux.

## Development

```sh
go run ./cmd/stavlos      # the client re-executes itself as the daemon
go test ./...
```

`scripts/check.sh` runs the whole gate a change must pass: gofmt, vet, the exhaustive-switch lint, staticcheck, a cyclomatic-complexity bound of 30, the race detector on the concurrent packages, and the suite three times.

The client checks the daemon's build id on connect and restarts it when the daemon was built from older code, so editing and re-running with `go run` just works. Set `STAVLOS_KEEP_DAEMON=1` to skip that. Daemon output is in `~/.local/share/stavlos/stavlosd.log`.
