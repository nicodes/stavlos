# Stavlos

An open-source agent harness in Go. It runs a tree of AI coding agents on your machine as long-lived actors you can prompt, steer, cancel, and kill, from a terminal UI or any client that speaks its JSON-RPC protocol. The design is in [docs/stavlos-prd.md](docs/stavlos-prd.md).

## Quick start

```sh
cd ~/some/project
go run ~/path/to/stavlos/cmd/stavlos     # or: go install ./cmd/stavlos, then `stavlos`
```

The TUI opens immediately. With nothing configured, the first prompt answers "no model selected"; run `/providers` to sign in and `/models` to pick a model. The first model you pick becomes your default for future channels.

Channels are global to your Stavlos data directory: running `stavlos` from any
folder shows the same channel list and resumes the last channel you viewed.
With no channels yet, the launch folder becomes the first channel's default
directory. Each channel owns its default directory; switching channels never
changes another channel's paths or grants. `stavlos new --dir /path/to/project`
creates a channel explicitly. In the TUI, **+ channel** asks for a name, then
prefills an editable directory from the current channel.

Stavlos uses your existing subscription, not platform API keys. `/providers` offers two sign-ins:

- **ChatGPT** (Plus or Pro, through the Codex sign-in). It signs in through your browser by default. A headless URL-plus-code option exists for SSH boxes once "Device code authorization for Codex" is enabled in ChatGPT's Security settings.
- **Grok** (SuperGrok, through the Grok CLI sign-in). It shows a URL and a short code.

Tokens live in `~/.local/share/stavlos/auth.json` (mode 0600) and refresh automatically. On the command line, `stavlos auth login [openai|xai]`, `stavlos auth list` and `stavlos auth logout` do the same. Model ids are `openai/gpt-5.4`, `xai/grok-4`, and so on; `/models` lists what each subscription serves.

## Using the TUI

### Talking to agents

A channel opens on its chat, where you talk to every agent. Start a message with one or more `@name`s, separated by spaces, to send it to those agents (`@` autocompletes them); the names are not part of the message, and an `@` later in it is left alone. A message with no leading name goes to `main`, the root agent, and a leading name that is no agent refuses the message. Agents answer you there with `message`. Posts, replies, questions and tool permissions show in the order they happen, and while an agent you messaged has not replied yet, a loader at the bottom names it. Tool calls and notices stay in each agent's own chat. Questions and permissions appear in the channel chat and the asking agent's chat, with their controls and submitted results in place. Space on a short reply opens that agent's chat; space or enter on a pending question or permission activates its controls.

Selecting an agent in the sidebar opens its own chat, with its tool calls and notes, where typing talks to that agent alone. The "#name" row at the top of the sidebar, or `/chat`, goes back. If an agent is busy, your message reaches it at its next step. The input grows as your message wraps; ctrl+j breaks a line and enter sends. If the agent is busy, your message reaches it at its next step. `/queue <text>` waits for the current turn to end instead, and esc pressed twice on an empty input cancels the current turn (the first press warns).

Agents reply with the `message` tool, to you or to another agent. The text an agent ends a turn with is its notes: it reaches no one and shows dimmed in its chat. Each response-required message has its own request ID. A response explicitly lists the IDs it answers in `reply_to`; other requests and `info` messages clear nothing. Whenever a turn ends with requests still owed, the agent is nudged unless it is waiting on an agent or a job. After three nudges in a row with no response it is left alone until it answers or something new arrives. A channel keeps one table of open requests, so what an agent owes and what it waits on are two views of the same record. The async panel lists both, each outstanding request with its ID, sender and excerpt, and says what the harness will do about what is owed (a reminder next turn, waiting on something first, the nudges spent, or reminders off). `"reminders": false` turns nudges off. `/recap 10` asks the main agent for a status report whenever ten minutes pass without you hearing from the channel and its agents have done something since the last one; it is an ordinary request from you, so it shows in the async panel until answered, only one is outstanding at a time, and `/recap off` stops it. See [explicit reply tracking](docs/reply-tracking.md).

The divider over the input shows the selected agent's role, model and variant at its left end, and at its right the agent's async, todo and mcp tabs, then how full its context is (`31% · 62k/200k tokens`, orange from 70%); tokens and cost are at the top of the nav, where a click on either opens a chart of it over time. Above them, one row shows how much of your ChatGPT plan's allowance is used (its most used window), read from the headers of the model calls Stavlos already makes (nothing is polled; the last reading is kept across restarts, with its age). A click on that row, or `/plan`, charts the plan over time from the readings those calls carried. See [plan usage](docs/plan-usage.md). Under them, `cache 94%` is how much of the last hour's model calls the providers served from their prompt caches, orange under 70% and red under 40%: a low share means calls are re-sending whole conversations at full price (see [prompt caching](docs/prompt-caching.md)). A passing message such as "copied" appears at the right end of the chat row just above the divider. Context is compacted on its own when an agent's history passes 80% of its model's window, or `"compaction": {"maxTokens": 200000}` if you set one, whichever comes first (the provider's own count of the last call is used when it is larger than the estimate): older turns become a summary, and the most recent conversation that fits in `keepTokens` (15,000 by default) is kept beside it. `/compact` does it for the selected agent right away, or before its next model call if it is busy. A compaction is an item in the chat: a rule with a sweeping bar while it runs, replaced in place by `┄┄ compacted 84k → 12k tokens ┄┄` and the summary when it is done.

### Focus and keys

- Tab and shift+tab cycle focus from top to bottom: the chat, the input, the tab strip, the agent's role, model and variant on the divider, and the sidebar (ctrl+b).
- Space and enter both select, everywhere outside a text field. They open the highlighted tab or divider part, pick a dialog row, select an agent, and expand a tool call's output in the chat.
- ctrl+space returns to typing from anywhere, closing whatever is open; esc also goes back from the chat, the sidebar, the tab strip and the meta row.
- In the chat, ↑/↓ move item by item. In the input, ↑/↓ walk this channel's prompt history. Drafts and history stay with their channel when you switch; `/channels` lists every active channel, with its directory.
- On the divider, ←/→ pick the role, model or variant, and space or enter opens its dialog; a click on the mode tag before the input's › switches the mode. `/roles`, `/models` and `/variants` open the same dialogs.
- The "/" palette lists every command. `/help` shows a key bar at the bottom (off by default; `/help` again hides it).

### The tab strip

The tabs, each always there with its count, are "permission" (shown as `! n`, every channel's permissions), "dirs" (the channel's directories), and an agent's own "async", "nudges", "todo" and "mcp". In an agent's chat the last four sit on the divider, before the context gauge; `! dirs` sit in the strip under the input while the sidebar is hidden, and in the sidebar (with dirs behind each channel's ⚙) while it shows. Tab lands on the leftmost, ←/→ move the highlight, enter or a click opens that tab's dialog, and esc returns to where you came from.

- **permission** is a shortcut to a pending tool-permission card in the viewed channel. Channel cards read `! @main: Permission: Shell`, followed by the full command or arguments and the approval choices. They expand on highlight/hover in an agent's chat and stay fully visible in channel chat. Allow once, remembered approvals, prefix approvals, denial reasons and directory approvals work inline. Decisions from either client replace the controls with their recorded result. Project-config trust still uses its dedicated dialog.
- **Questions** are chat messages, scoped to the channel being viewed: `? @main: Which format?` in channel chat, or `? @user Which format?` in the asking agent's chat. Options (`□` unchecked, `■` checked), the custom-answer field and Submit are indented below. Submitted results keep that format. Questions have no global tab or dialog.

Message addresses read `@sender: @recipient1 @recipient2 message`, with the sender
prefix in grey. The current view makes some names implicit:

- Channel chat, your post to main alone: `Check this` (no automatic `@main`).
- Channel chat, your post: `@main @scout Check this`.
- Channel chat, an agent replying to you: `@main: Here is what I found`.
- Main's chat, main sending: `@scout @reader Review this`.
- Main's chat, an incoming message: `@scout: @main @reader Here are the results`.

Channel chat omits `@user` as a recipient. Other tagged recipients remain visible.

In an agent's own transcript, questions and their results fold to a single-line
summary like other items. Highlight or hover a pending question to show its full
text and controls; click an option or Submit directly, without an expansion
click. Leaving it folds it again and preserves the draft. Answered questions
use the normal three-line hover preview and click-to-expand behavior. The shared
channel chat shows questions in full.

In agent chat, focusing, hovering or expanding an item keeps its text colors
unchanged; the background highlight marks the selected item.

Tool permissions also appear in Discord immediately, under the requesting
agent's name. Permission buttons share one row: approval choices, **Deny**,
and **Deny with reason**. Deny acts immediately; Deny with reason opens a
required-text popup. Boundary requests offer approval of the displayed directory.
The directory sits on the heading line in regular text, and shell commands are
shown in a code block below it rather than as JSON. Decisions update the same
message. The configured permission answer timeout and headless default still apply.

Other Discord approvals follow that layout: patches use diff blocks, fetches
show their URL, searches show their query, and file/search tools show their paths,
patterns and limits. MCP tools identify the server and tool and show indented
JSON arguments. Project-trust prompts show the directory and configuration files.
- **async** holds both directions for the selected agent: under "waiting on", the agents whose answer it expects (a child it tasked, a sibling or parent it messaged) and its running shell jobs; under "owes a reply to", who waits on its reply, you first, then any agent that messaged it. Space on an agent (or on you) opens that chat.
- **todo** lists the selected agent's plan.
- **mcp** lists its MCP servers with their state, tool count and uptime.
- **dirs** edits the channel's working directories, shared by every agent: `a` adds, enter edits, ctrl+d removes an additional directory. The `default` row changes the channel's default directory while it is idle; `/dir /path` does the same directly.

### The sidebar

The sidebar (ctrl+b, shown by default when the terminal is wide enough) lists
all active channels alphabetically, with each channel's own directory on its
name row. Its `✚` creates a channel: choose a name and accept or change the
inherited default directory. Each channel's ⚙ opens its directories. Agent
trees sit under their channels, with `!` or `?` badges for pending prompts and
per-agent cost. A tree draws its main agent, the agent whose chat is open, and
every agent that is working, waiting, failed or waiting on you; an idle one
stays when a drawn agent hangs under it, so the tree keeps its shape. The rest
sit behind a row that toggles `▸ show all · 3 idle` and `▾ hide idle`, per
channel. ↑/↓ move, space selects, and `n` jumps to the next agent in the
current channel waiting on you. The sidebar scrolls independently of the chat.

Every channel has a state dot (full orange while an agent works, half while one
waits, empty when idle). The catalog refreshes even while the current channel
is idle. Space resumes a channel in place; opened agent trees remain visible,
and ← folds or unfolds them. Space on another channel's agent opens that
channel on that agent. An unavailable directory is marked rather than hiding
the channel or replacing its path with the launch folder.

Changing the default directory requires an idle channel with no running jobs,
compaction or pending agent prompts. The change preserves the channel ID,
history, agents and explicitly added absolute directories. It reloads the
target directory's project configuration, reevaluates trust, stops old MCP
servers, refreshes agent instructions, clears remembered approvals and resets
the mode to `ask`. A notice is recorded in the chat. Model and role selections
remain channel/agent settings; a missing role becomes read-only, and a model
disallowed by the target project's role must be reselected.

### Permissions and modes

Permission prompts show the command (or path) with the asking agent after it, then a fixed list of answers; ↑/↓ move and space or enter chooses:

- A plain permission offers "Allow once", "Allow for this channel" (this exact call), "Allow `<prefix>` for this channel" for a simple shell command, and "Deny", which opens a row for an optional reason the agent reads. The prefix is the first word, or two for git, go, npm, cargo, make, docker and the like: `go test` then covers every `go test …` that is not chained, piped or redirected. It is never offered for wrappers such as `bash`, `env`, `sudo` or `python`.
- A boundary prompt, for a call outside the channel's directories, offers "Allow once", "Allow and add <dir>", "Allow and add another directory…" and "Deny". "Allow and add" adds the directory to the channel's set, for every agent: the whole git checkout when the path is inside one, else the path's directory.
- The trust prompt for a project's `.stavlos/` offers "Trust this project's config" or "Not now".

Esc closes a dialog with the prompt still waiting.

`/mode` picks the channel's permission mode. A new channel starts in `ask` unless `stavlos.json` sets `"mode": "auto"` or `"yolo"` (a trusted project's overrides the global one):

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

**Delegate and message.** Delegating to child agents is the model's job (`agent_create`). Every agent has a unique name in its channel (the root is `main`; a name already taken gets a suffix, `scout-2`). Agents use `message` with a recipient array, such as `{"to":["scout","reviewer"],"text":"Review this change","kind":"request"}`. Every recipient sees the full list; aliases deduplicate, and an invalid recipient rejects the entire send. Policy checks every recipient. Each request is tracked independently for each receiving agent. Responses require `reply_to` IDs and clear only those requests, even when several came from the same sender. Info creates no debt, clears no requests and does not wake idle agents. Human requests also require explicit responses; other human-facing messages are updates. Older single-recipient string calls still work. A child idles with its context intact for follow-ups, and its MCP servers stop after ten idle minutes. There is no wait tool.

**Search.** `grep` and `glob` search file contents and names (with ripgrep when it is installed) and never ask. They are tools, not shell commands, so searching needs no shell permission.

**Run commands.** Agents run commands with one `shell` tool. No command is allowed by default: each asks until you allow it once, for the channel, or by prefix, or with a rule in `stavlos.json`. A call waits up to 15 seconds; a command still running then continues as a background job (the call returns its id and the output so far), and `background: true` skips the wait for servers. A job's exit wakes its agent the same way a response does, and `shell_kill` stops a job. Every command and MCP server runs with a scrubbed environment, without `STAVLOS_*` or any variable whose name looks like a credential, so list what a build really needs under `"env": {"pass": ["GITHUB_TOKEN"]}` in `stavlos.json`.

**The sandbox.** On Linux every command and MCP server runs inside a boundary the kernel enforces (Landlock, plus a user and mount namespace where the kernel allows them; the daemon log names the level). It may write only beneath the channel's directories, a scratch directory of the channel mounted as `/tmp`, and the caches build tools fill. The files that steer the harness or run code later stay read-only: `.git/hooks`, `.git/config`, `.stavlos`, `.envrc`, and every `AGENTS.md` or `CLAUDE.md` the project's trust covers. It cannot see Stavlos's own config, data, cache or socket, your runtime directory (where the D-Bus and agent sockets live), or credential stores such as `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.config/gh` and `~/.netrc`. Configure it in `stavlos.json` (yours, or a trusted project's, which takes precedence): `"sandbox": {"network": false, "writable": ["~/.m2"], "hide": ["~/private"]}`, or `"enabled": false` to turn it off. The build caches stay writable, so a command can still poison one.

**Reach the web.**

- `web_fetch` returns one page as markdown, 20k characters at a time, with HTML boiled down to headings, text, lists, links and code. http is upgraded to https, credentials are stripped, private and local addresses are refused, cross-host redirects are reported rather than followed, and pages are cached for 15 minutes. It asks by default, and the dialog offers "Allow <host> for this channel". `"hosts"` in `stavlos.json` lists the hosts it reaches without asking (`["github.com", "*.golang.org"]`, or `["*"]` for every host; yours, plus a trusted project's), and a deny rule still wins. For a rule about paths, `policy` takes URL patterns (`"web_fetch": {"https://github.com/*": "allow"}`, matched against the URL as it will be fetched: lower-case host, https, no credentials), and roles may tighten further per URL.
- `web_search` returns title, URL and snippet. Out of the box it uses Exa's free, keyless endpoint, the same one OpenCode uses, which has no published rate limit and may change. For your own quota, configure a backend under `search` in `stavlos.json`: `{"provider": "brave" | "tavily" | "exa", "apiKey": "${env:BRAVE_KEY}"}`. It asks until you configure a backend (its keyless fallback is a third party you never chose) and is allowed once you have.

Auto mode still asks before fetching from a host that is not listed, since a fetch sends a request off the machine, and before a search while no backend is configured; yolo asks for neither. Everything fetched is handed to the model as untrusted data.

**Plan.** One `todo` tool keeps a per-agent list (a call adds steps, each free to start at any status, updates others by id, or both, and returns the list, so planning the work and starting its first step is one call) that is logged, projected into the system prompt at every call (so it survives compaction) and shown to you in the todo tab.

**Work in directories.** A channel has one set of working directories, shared by every agent: the channel directory, the directories listed under `"dirs"` in `stavlos.json` (yours for every channel, a trusted project's for its channels, any path such as `/tmp`; only those files change them), plus whatever you add. Roles and `agent_create` grant none. A call that reaches outside asks first (see Permissions and modes), and the dirs tab edits the set by hand.

**Use MCP servers.** A role's `mcp:` list starts MCP servers for that agent alone (stdio servers defined under `mcp` in `stavlos.json`). The model sees their tools as `mcp__<server>__<tool>` and calls them through the usual permission path.

**Ask you.** An agent can request one to four short questions with `ask_user`.
Each question gets its own prompt and submission, with no timeout or
auto-approval. Both the TUI and Discord show all requested questions immediately,
each as a separate inline chat card. You can review them together and answer in
any order. In the TUI, click an option or focus the card with space/enter, use
↑/↓ to move and space to toggle. Typing opens its custom-answer field; enter
saves that text, then Submit answer (or enter outside the field) sends the
checked options and custom text together. Esc returns to the chat input with
the draft retained. Submission turns that question's message into its result;
the other questions remain available. Submitted answers stay recorded,
including after reopening the channel or cancelling the remaining questions.
The agent receives the answers in original question order once all are complete.

In Discord, the option buttons are stacked vertically, followed by
**⬜ Custom answer** and **Submit answer**, each on its own row without a
surrounding container. Clicking Custom answer
opens a text popup; saving changes the button to **✅ your text**, without
submitting the question. Clicking the checked custom answer clears it, and
clicking again opens an empty popup for a new value. **Submit answer** sends
the selected options and saved custom text together. Chat replies are ordinary
agent messages.

Submitted custom text is shown as another checked choice, such as **✅ windy**,
without a “Custom answer:” label. Submitted checkbox rows are indented with
non-breaking spaces; the question heading stays flush left.

TUI questions have no X/Y counters. Pending question text uses the lighter
message color; completed answers use regular grey text. The custom-answer row
starts with `□ Reply with a custom answer…` and becomes `■ your text` when filled
in, aligned with the other choices in both the open question and its result.

## Configuration editor

Use a channel's **⚙** to edit its project config, or the gear beside the top
**Stavlos** title for system config. `/settings [project|system]` opens the same
editor. Browse Settings, Agents, Commands, Skills or all Files; use structured
fields or press **F4** for raw JSON/Markdown editing. Changes are validated and
saved directly to the files, then configuration is reloaded. Project changes
are ordinary, committable repository edits.

**Tab** switches panes, **Enter** edits/confirms a field, and **Ctrl+S** saves
multiline/raw content. In the file pane, **Ctrl+N** creates, **F2** renames and
**Ctrl+D** deletes. See [the config editor guide](docs/config-editor.md) for
conflict handling and which settings apply now, next turn, or after reconnect.

## Custom commands

Define prompt shortcuts in `.stavlos/commands/<name>.md`. The filename supplies
the slash-command name, and **`description` is the only frontmatter field**.
For example, `.stavlos/commands/cmd.md`:

```markdown
---
description: Run tests with coverage
---

Run the full test suite with coverage report and show any failures.
Focus on the failing tests and suggest fixes.
```

Run `/cmd` in the TUI. Its Markdown body is sent as a prompt to the selected
agent, or to the root agent from channel chat, using that agent's current role
and model. The `/` menu shows the description and refreshes command definitions
when opened. The prompt is loaded again when invoked.

Names use lowercase letters, digits, hyphens or underscores and start with a
letter. A nonempty description and prompt body are required. Fields such as
`agent` and `model` are rejected. Commands take no arguments; built-in commands
and their aliases take precedence over custom names.

Global definitions live in `~/.config/stavlos/commands/` (or the configured
Stavlos config directory). Trusted project definitions override global commands
with the same name. Project commands participate in the existing configuration
trust hash, just like project roles and skills.

## Other commands

```sh
stavlos new [--dir /path]       # create a channel; default directory is the shell's cwd
stavlos open <#name|id>        # open a channel by name; plain `stavlos` resumes the last viewed
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

Put a `.stavlos/` directory in a repository to add roles (`agents/<name>.md`), skills (`skills/<name>/SKILL.md`), MCP definitions, and a `stavlos.json` (plus a gitignored `stavlos.local.json`) that takes precedence over the global config once trusted. The `discord` block is global-only; other settings can be overridden by the project. Every agent follows `AGENTS.md` instructions: yours in `~/.config/stavlos/AGENTS.md`, then the repository's from its git root down to the channel directory (a directory's `CLAUDE.md` where it has no `AGENTS.md`), 32 KiB in all, and a subdirectory's with an agent's first read, search or edit there. Editing any of them asks in every mode, and an edit made outside the harness brings the trust prompt back when an agent next starts a turn. The whole layer is untrusted until you confirm it once per content hash, from the TUI prompt or `stavlos trust`.

## Discord

Use `/discord` in Stavlos for connection status and controls, or
`/discord connect` to connect using your saved global configuration. A live
indicator at the top of the sidebar is green when connected,
amber while connecting/reconnecting, dim when disconnected, and red on errors.
Click it to open the controls; it refreshes even while the panel is closed.
Discord runs as a background service in the daemon, so closing the TUI leaves it
connected. Connecting saves `discord.enabled: true` for future daemon starts;
`/discord disconnect` stops the integration and disables autoconnect. The shell
equivalents are `stavlos discord status|connect|disconnect`. Each
allowed Stavlos channel gets a Discord text channel: send tasks, reply to named
agents, answer permission and question prompts, and use `/status` or `/cancel`.
Questions appear immediately in both clients; permission and trust prompts
appear as fallback after the terminal's claim timeout. The bridge uses
outbound connections and a global-only `discord` config block with an explicit
operator list and working-directory list. See [the setup guide](docs/discord-setup.md)
for the bot, configuration and migration from the old standalone bridge.

## Status

Implemented: daemon with SQLite event log, one state machine per channel with a goroutine per agent, projector (cancelled-turn repair, restart recovery, compaction), built-in and orchestration tools, three-layer config with trust gate, declarative policy, escalation with claim tiers and headless default, usage accounting, JSON-RPC protocol over a Unix socket with offset replay, Go client, an opencode-style Bubble Tea TUI, ChatGPT (Codex backend) and Grok subscription adapters with browser and device-code sign-in, models.dev metadata, native search tools, and a Linux sandbox for commands and MCP servers.

Not yet: go-plugin model seam, `stavlos plugin install`, remote (HTTP) MCP servers, channel fork, a sandbox outside Linux.

## Development

```sh
go run ./cmd/stavlos      # the client re-executes itself as the daemon
go test ./...
```

`scripts/check.sh` runs the whole gate a change must pass: gofmt, vet, the exhaustive-switch lint, staticcheck, a cyclomatic-complexity bound of 30, the race detector on the concurrent packages, and the suite three times.

The client checks the daemon's build id on connect and restarts it when the daemon was built from older code, so editing and re-running with `go run` just works. Set `STAVLOS_KEEP_DAEMON=1` to skip that. Daemon output is in `~/.local/share/stavlos/stavlosd.log`.
