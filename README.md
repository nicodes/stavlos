# Stavlos

An open-source agent harness in Go. It runs a tree of AI coding agents on your machine as long-lived actors you can prompt, steer, cancel, and kill, from a terminal UI or any client that speaks its JSON-RPC protocol. The design is in [docs/stavlos-prd.md](docs/stavlos-prd.md).

## Quick start

```sh
cd ~/some/project
go run ~/path/to/stavlos/cmd/stavlos     # or: go install ./cmd/stavlos, then `stavlos`
```

The TUI opens immediately. With nothing configured, the first prompt answers "no model selected"; run `/providers` to paste a key and `/models` to pick a model. The first model you pick becomes your default for future sessions.

Stavlos uses your existing subscription, not platform API keys. `/providers` offers two sign-ins: **ChatGPT** (Plus or Pro, through the Codex sign-in) and **Grok** (SuperGrok, through the Grok CLI sign-in). ChatGPT signs in through your browser by default (a headless URL-plus-code option exists for SSH boxes, once "Device code authorization for Codex" is enabled in ChatGPT's Security settings); Grok shows a URL and a short code. Tokens live in `~/.local/share/stavlos/auth.json` (mode 0600) and refresh automatically. `/models` picks a model; the first pick becomes your default. On the command line, `stavlos auth login [openai|xai]`, `stavlos auth list`, and `stavlos auth logout` do the same.

One preset ships built in, `general`, which can read, edit, run commands and delegate to more `general` agents; add specialised ones as `agents/<name>.md` in the global or project config. In the TUI: type to talk to the selected agent (the input grows as your message wraps; ctrl+j breaks a line, enter sends) (`/roles` picks which preset it runs as, `/variants` a model variant such as reasoning effort); if it's busy, your message reaches it at its next step. `/queue <text>` waits for the current turn to end instead, and esc pressed twice on an empty input cancels the current turn (the first press warns). Delegating to and killing child agents is the model's job (`agent_create`, `agent_kill`), and any agent can message any other in the session (`agent_prompt`; only the main agent may `agent_steer`); a finished child wakes its parent between turns, never mid-turn, and there is no wait tool. Agents run slow commands with `bash_async`, whose exit wakes them the same way; live child agents and jobs get their own tabs ("agents" and "async") after the "permission" tab in a strip under the chat (all three are always there with a count; tabbing to the strip opens the first non-empty one, and ←/→ move between them). Tab and shift+tab cycle focus top to bottom between the chat, the tab strip, the input, the meta row under it (←/→ pick YOLO, role, model or variant; enter opens its dialog), and the sidebar (ctrl+b). In the chat, up/down move item by item and Enter expands a tool call's output; in the input, up/down walk your prompt history. `/sessions` lists this directory's earlier sessions (titled by their first prompt) and resumes the one you pick in place. The "/" palette lists every command; `/help` shows a key bar at the bottom (off by default; `/help` again hides it). Permission prompts open from their tab above the input; `y` allows once, `a` allows for the session, `n` denies. `/yolo` switches the whole session to auto-approve (anything already waiting is allowed too; deny rules and model questions still apply) and shows a YOLO tag before the role until `/yolo off`.

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

Put a `.stavlos/` directory in a repository to add presets (`agents/<name>.md`), skills (`skills/<name>/SKILL.md`), MCP definitions, and policy tightening in `stavlos.json`; `AGENTS.md` at the repo root is injected into every agent. The whole layer is untrusted until you confirm it once per content hash, from the TUI prompt or `stavlos trust`.

## Status

Implemented: daemon with SQLite event log, actor scheduler, projector (cancelled-turn repair, restart recovery, compaction), built-in and orchestration tools, three-layer config with trust gate, declarative policy, escalation with claim tiers and headless default, usage accounting, JSON-RPC protocol over a Unix socket with offset replay, Go client, an opencode-style Bubble Tea TUI, ChatGPT (Codex backend) and Grok subscription adapters with device-code sign-in, models.dev metadata.

Not yet: Discord service, MCP client, go-plugin model seam, `stavlos plugin install`.

## Development

```sh
go run ./cmd/stavlos      # the client re-executes itself as the daemon
go test ./...
```

The client checks the daemon's build id on connect and restarts it when the daemon was built from older code, so editing and re-running with `go run` just works. Set `STAVLOS_KEEP_DAEMON=1` to skip that. Daemon output is in `~/.local/share/stavlos/stavlosd.log`.
