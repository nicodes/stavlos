# Stavlos

An open-source agent harness in Go. It runs a tree of AI coding agents on your machine as long-lived actors you can prompt, steer, cancel, and kill, from a terminal UI or any client that speaks its JSON-RPC protocol. The design is in [docs/stavlos-prd.md](docs/stavlos-prd.md).

## Quick start

```sh
cd ~/some/project
go run ~/path/to/stavlos/cmd/stavlos     # or: go install ./cmd/stavlos, then `stavlos`
```

The TUI opens immediately. With nothing configured, the first prompt answers "no model selected"; run `/providers` to paste a key and `/models` to pick a model. The first model you pick becomes your default for future sessions.

Stavlos uses your existing subscription, not platform API keys. `/providers` offers two sign-ins: **ChatGPT** (Plus or Pro, through the Codex sign-in) and **Grok** (SuperGrok, through the Grok CLI sign-in). ChatGPT signs in through your browser by default (a headless URL-plus-code option exists for SSH boxes, once "Device code authorization for Codex" is enabled in ChatGPT's Security settings); Grok shows a URL and a short code. Tokens live in `~/.local/share/stavlos/auth.json` (mode 0600) and refresh automatically. `/models` picks a model; the first pick becomes your default. On the command line, `stavlos auth login [openai|xai]`, `stavlos auth list`, and `stavlos auth logout` do the same.

One role ships built in, `general`, which can read, edit, run commands and delegate to more `general` agents; add specialised ones as `roles/<name>.md` in the global or project config (see `.stavlos/roles/coder.md` in this repository for every key: a `mode` of primary, subagent or all; a `models` whitelist with the variants allowed per model; `tools` with the policy rules nested under each tool; `spawn`, `max_turns`, `color`). The role decides what `/roles`, `/models` and `/variants` offer, and the daemon enforces it. In the TUI: type to talk to the selected agent (the input grows as your message wraps; ctrl+j breaks a line, enter sends) (`/roles` picks which preset it runs as, `/variants` a model variant such as reasoning effort); if it's busy, your message reaches it at its next step. `/queue <text>` waits for the current turn to end instead, and esc pressed twice on an empty input cancels the current turn (the first press warns). Delegating to child agents is the model's job (`agent_create`), and any agent can message any other in the session with `agent_message`, which reaches it at its next step even mid-turn; a child answers with `agent_response`, which wakes its parent between turns, never mid-turn; the child then idles with its context intact for follow-ups for the rest of the session (nothing kills it; its MCP servers stop after ten idle minutes), and there is no wait tool. Agents run slow commands with `bash_async`, whose exit wakes them the same way; and plan multi-step work with `todo_add` and `todo_update`, a per-agent list that is logged, projected into the system prompt at every call (so it survives compaction) and shown to you. Every agent may read, edit and run commands in the session directory plus its role's `dirs`; a parent can grant a child directories from its own set at `agent_create`, and a call that reaches outside asks first (`a` allows and adds the directory to that agent). A role's `mcp:` list starts MCP servers for that agent alone (stdio servers defined under `mcp` in `stavlos.json`), whose tools the model sees as `mcp__<server>__<tool>` and calls through the usual permission path. Live child agents, jobs, the todo list, MCP servers and working directories get their own tabs ("agents", "async", "todo", "mcp", "dirs") after the "permission" tab in a strip under the input (all six are always there with a count; tab lands on the leftmost, ←/→ move the highlight, enter or a click opens that tab's dialog, and esc returns to where you came from). Tab and shift+tab cycle focus top to bottom between the chat, the input, the meta row under it (←/→ pick YOLO, role, model or variant; enter opens its dialog), the tab strip, and the sidebar (ctrl+b). In the chat, up/down move item by item and Enter expands a tool call's output; in the input, up/down walk your prompt history. On the start screen, ↑/↓ in the input recall the first prompts of this directory's earlier sessions, and `/sessions` picks one to resume. The "/" palette lists every command; `/help` shows a key bar at the bottom (off by default; `/help` again hides it). Permission prompts open from their tab above the input; `y` allows once, `a` allows for the session, `n` denies. `/yolo` switches the whole session to auto-approve (anything already waiting is allowed too; deny rules and model questions still apply) and shows a YOLO tag before the role until `/yolo off`.

Other commands:

```sh
stavlos resume [id]            # reattach to the latest session for this directory
stavlos sessions               # list sessions (they survive daemon restarts)
stavlos tree <session>         # agent tree with state and cost
stavlos send|steer|cancel|kill <agent> [text]
stavlos trust [dir]            # confirm a project's .stavlos/ layer
stavlos daemon                 # run stavlosd in the foreground
```

Model ids are `openai/gpt-5.4`, `xai/grok-4`, and so on; `/models` lists what each subscription serves.

## Project configuration

Put a `.stavlos/` directory in a repository to add roles (`roles/<name>.md`), skills (`skills/<name>/SKILL.md`), MCP definitions, and policy tightening in `stavlos.json`; `AGENTS.md` at the repo root is injected into every agent. The whole layer is untrusted until you confirm it once per content hash, from the TUI prompt or `stavlos trust`.

## Status

Implemented: daemon with SQLite event log, actor scheduler, projector (cancelled-turn repair, restart recovery, compaction), built-in and orchestration tools, three-layer config with trust gate, declarative policy, escalation with claim tiers and headless default, usage accounting, JSON-RPC protocol over a Unix socket with offset replay, Go client, an opencode-style Bubble Tea TUI, ChatGPT (Codex backend) and Grok subscription adapters with device-code sign-in, models.dev metadata.

Not yet: Discord service, go-plugin model seam, `stavlos plugin install`, remote (HTTP) MCP servers.

## Development

```sh
go run ./cmd/stavlos      # the client re-executes itself as the daemon
go test ./...
```

The client checks the daemon's build id on connect and restarts it when the daemon was built from older code, so editing and re-running with `go run` just works. Set `STAVLOS_KEEP_DAEMON=1` to skip that. Daemon output is in `~/.local/share/stavlos/stavlosd.log`.
