# Lua plugins (proposal)

Status: proposal, nothing here is implemented. It replaces the `go-plugin`
design in PRD §11 as the direction for plugins. Today the `plugins` config key
is parsed and ignored, and `stavlos plugin` reports a roadmap item.

A plugin is a Lua script the daemon runs in an embedded VM. It reacts to
channel events and may add small tools. There is no recompiling, no binary to
install, and a script can do only what the host API hands it.

## Why not go-plugin

The PRD §11 design made plugins gRPC binaries that add model providers. That
is set aside because:

- It solves the wrong problem. The chat-completions adapter is already
  generic; a config-defined provider (`baseURL` plus `apiKey`) covers most
  new providers with no plugin system at all. That is separate work and does
  not depend on this proposal.
- It costs a lot: protobuf definitions for streaming, tool calls and thinking
  blocks, a versioned API, and an installer with checksums and a lockfile.
- A plugin binary is unsandboxed and holds credentials. Letting a trusted
  project list plugins turns the trust prompt into consent to run arbitrary
  binaries.

## Why Lua

- **No ambient capabilities.** The VM is created without `io`, `os`,
  `package`, `debug` or `load` of bytecode. Everything a script reaches goes
  through host functions, and those go through the policy engine.
- **Text under the trust hash.** A project's `.stavlos/plugins/*.lua` are
  readable files covered by the existing trust prompt. There is nothing to
  download, verify or lock.
- **Pure Go.** `github.com/yuin/gopher-lua` (Lua 5.1) needs no cgo and honours
  context cancellation, which gives hooks a deadline.
- **Familiar.** People who live in a terminal already write Lua for Neovim
  and WezTerm.

Alternatives considered: Starlark (hermetic by construction, but less
expressive and far less known), WASM through wazero (any language, but brings
back a compile step), goja (JavaScript; workable, heavier).

## Three extension tiers

| Tier | Runs | For |
|---|---|---|
| JSON-RPC client (`pkg/client`) | its own process | whole front ends and bridges, such as the TUI and Discord |
| MCP server | its own process, sandboxed | tools with real dependencies |
| Lua plugin | inside the daemon | hooks and small tools |

Lua plugins are the small tier. They do not add model providers, they do not
extend the TUI (the TUI is one client among several; a daemon-side plugin
could not reach Discord's rendering either), and they are not the way to
build a bridge. Scripting the TUI itself is a separate, later host: see
[TUI scripting](#tui-scripting-later).

## What a plugin can do

### Hooks

A script registers functions against event types. The payload is a read-only
table decoded from the event.

```lua
-- ~/.config/stavlos/plugins/lint.lua
stavlos.on("turn.ended", function(ev)
  if ev.agent ~= "main" then return end
  local out = stavlos.shell("golangci-lint run ./...")  -- through policy
  if out.code ~= 0 then
    stavlos.steer(ev.agent, "Lint fails:\n" .. out.text)
  end
end)
```

The permission hook is a decision rather than a reaction. It is not a logged
event: it runs while a tool call is being decided, before `tool.started`.

```lua
stavlos.permission(function(call)
  if call.tool == "shell" and call.subject:match("^git push") then
    return "ask"          -- "allow" is not accepted here
  end
end)
```

A permission hook can only tighten, the same rule roles follow: it may turn
allow into ask or deny, and ask into deny. It can never loosen a verdict, and
deny rules stay the floor in every mode.

### Small tools

```lua
stavlos.tool{
  name = "jira_ticket",
  description = "Fetch a Jira ticket by key",
  input = { key = { type = "string", required = true } },
  run = function(input)
    return stavlos.fetch("https://jira.example.com/rest/api/2/issue/" .. input.key).text
  end,
}
```

A Lua tool is offered to agents as `lua__<plugin>__<name>`, in the manner of
`mcp__server__tool`, and has its own policy key, so rules and roles cover it
like any other tool. It defaults to `ask`.

### Host API (first cut)

| Function | Effect | Gate |
|---|---|---|
| `stavlos.on(type, fn)` | register an event hook | none |
| `stavlos.permission(fn)` | register a tighten-only permission hook | none |
| `stavlos.tool{…}` | register a tool | the tool's own policy key |
| `stavlos.steer(agent, text)` | message an agent as `system` | none (rate limited) |
| `stavlos.notice(text)` | note in the channel chat | none |
| `stavlos.shell(cmd)` | run a command | `shell` policy, sandbox, channel dirs |
| `stavlos.fetch(url)` | HTTP GET | `web_fetch` policy and host rules |
| `stavlos.read(path)` | read a file | channel directory boundary |
| `stavlos.log(text)` | daemon log | none |

Host calls made from a plugin are decided by policy as the plugin, not as the
agent that triggered the hook. A call that comes out `ask` is denied when it
comes from a hook (nobody is waiting on a hook) and escalates normally when it
comes from a tool call.

## Where plugins live and who trusts them

- `~/.config/stavlos/plugins/*.lua` — global, trusted like the global config.
- `.stavlos/plugins/*.lua` — per project, included in the trust hash, loaded
  only once the project is trusted. Changing a script re-prompts, as changing
  `stavlos.json` does.

The `plugins` key in `stavlos.json` becomes an optional list of script names
to disable (`"plugins": {"disable": ["lint"]}`); presence in the directory is
what enables one. Until this lands, the ignored `plugins` array and the
`stavlos plugin` stub should be removed so that strict config stays honest.

## Runtime rules

These follow from how the daemon already works and are not negotiable:

- **Never under the channel lock.** The state machine (`agent/state.go`)
  commits under `Channel.mu`. Hooks run after the commit, outside the lock,
  from the list of agents and events the commit returns. The permission
  hook runs in the permission path (`agent/permission.go`), which already
  blocks on escalation outside the lock.
- **Results are events.** Recovery folds the log and must never re-run a
  hook. A steer, a notice, a tightened verdict and a Lua tool's result are
  each recorded (`plugin.steered`, `plugin.verdict`, …) and replayed from the
  log. Hooks do not fire during recovery or replay.
- **One VM per channel per plugin.** An `LState` is not goroutine-safe.
  Invocations for a channel are serialized; globals persist for the life of
  the channel's VM and are lost on daemon restart. There is no cross-channel
  state and no persistence API in the first cut.
- **Deadlines and budgets.** Each invocation gets a context deadline (default
  5s for hooks, the tool timeout for tools) and a memory cap on the VM. A
  hook that times out or errors is logged as `plugin.failed` and shown as a
  notice; after repeated failures the plugin is disabled for that channel.
  A failing permission hook leaves the verdict unchanged.
- **No hook loops.** Events caused by a plugin carry its name, and a plugin
  never receives its own events. `steer` is rate limited per agent.
- **Text is sanitized.** Anything a plugin emits passes through `textsafe`
  like model output.

## First slice

1. `internal/plugin`: load global scripts, one VM per channel, `on`, `log`,
   `notice`, `steer`, deadlines, `plugin.*` events.
2. The tighten-only `stavlos.permission` hook.
3. Project plugins under the trust hash.
4. `shell`, `fetch`, `read` through policy.
5. `stavlos.tool`.

Steps 1 and 2 are enough to tell whether the hook model earns its place. The
host API stays unstable until a release; the event payloads a hook sees are
the protocol's event types and change with them.

## TUI scripting (later)

The TUI could embed the same VM, as a second and much narrower host. It is a
different plugin system from the daemon's: different API, different trust,
and it comes after the daemon hooks.

The dividing rule: **if it should still work with the TUI closed, it belongs
in a daemon plugin.** Policy, steering and tools are daemon matters; a
client-side behavior never reaches Discord or any other client. A TUI script
affects presentation and input on one person's terminal, and gets no shell,
network or file access at all.

### What it is for

| Function | Effect |
|---|---|
| `tui.bind(key, action)` | map a key to a command or a sequence of actions |
| `tui.command{name, run}` | add a `/` palette command, for instance one that expands to a prompt |
| `tui.segment(where, fn)` | a short string on the divider or sidebar header, like a statusline component (git branch, a cost-budget warning) |
| `tui.notify(fn)` | called when an agent starts waiting on you; ring the bell, raise a desktop notification |
| `tui.theme{…}` | colors and glyphs, with room for conditions (terminal, time of day) |

Notifications sit on the client on purpose: whether to ring a bell depends on
where the person is sitting, not on the channel.

### What it is not for

- **Item renderers and layout.** Rendering is the hot path: per-item caches
  brought a chat redraw to about half a millisecond, and a Lua call per item
  per frame would undo that and complicate cache invalidation. It would also
  freeze `tui/transcript` and `tui/render` internals into a public API.
- **Anything semantic.** See the dividing rule above.

### Rules

- **Global only.** Scripts load from `~/.config/stavlos/tui/*.lua`. A
  repository never restyles or rebinds someone's client, so there is no
  project tier and nothing for the trust prompt to cover.
- **The update loop is single-threaded.** A segment or binding runs
  synchronously under a deadline of a few milliseconds; anything slower runs
  as a `tea.Cmd` and delivers its result as a message. A script that
  overruns or errors is disabled with a notice.
- **Segments are cached.** A segment function is re-evaluated on the events
  it names (or a timer), not on every frame, and its output passes through
  `textsafe` and is truncated to its slot.

### Cost, and the cheaper first step

This is a second host API to document and keep stable. Keybindings and theme
do not strictly need a VM: a `keys` and a `theme` block in the global config
cover most of what people want. Do that first. If `internal/plugin` keeps the
VM setup, sandboxing and deadlines reusable, a TUI host later is mostly a
second small API table, starting with `bind`, `command`, `segment` and
`notify`.

## Open questions

- Should hooks see events from every agent or only from agents whose role
  lists the plugin, the way roles list `mcp` servers?
- Does a plugin need per-channel persistent state, and if so is it an event
  (`plugin.state`) or a side file?
- Should a TUI/Discord surface list loaded plugins and their failures (a
  `plugins` tab), or is the notice enough?
- Is a TUI Lua host worth it once `keys` and `theme` config exist, or do
  segments and notifications fit in config too?
