---
# Shown in /roles and read by any agent deciding whether to delegate to this role.
description: Implements features and fixes bugs in Go; runs the tests before reporting

# primary: selectable for the main agent, never spawned. subagent: only created
# with agent action create by a role that lists it in spawn. all: both (the default).
type: all

# The models good enough for this role, in the order it prefers them. The
# harness chooses among them by their plans' usage and moves the agent to the
# next when one runs out (docs/model-selection.md); no agent names a model.
# Omit the key to choose from "models" in stavlos.json instead.
models:
  - id: openai/gpt-5.1-codex
    variants: [medium, high]        # allowed for this model; the first is its default
  - id: openai/gpt-5.1-codex-mini   # no variants key: any the provider offers, provider default
  - xai/grok-4-fast                 # a bare string is shorthand for the same

# Every tool is available unless removed here: shell, read, patch,
# skill, todo and web. A bare
# deny removes a tool, so it is never offered. Any other verb, or patterns
# under a tool, only tighten the layered policy (allow → ask → deny); a
# loosening entry is a config error. message, agent, channel and ask
# cannot be removed. Agent create/cancel require spawn; shell includes job cancellation.
tools:
  shell:
    "git push*": deny
    "rm -rf*": deny
  patch: ask
  web:                          # action plus query/URL is the policy argument
    "search *": deny             # deny search while keeping fetch available
    "fetch https://*.slack.com/*": deny   # host allow-lists go in stavlos.json's policy

# Skill descriptions this role carries in context; bodies load on demand.
skills: []

# MCP servers from stavlos.json this role may reach.
mcp: []

# Roles it may create with agent action create. Omit or leave empty to forbid spawning.
spawn: [general]

# Subagent only: turns it may take before it must answer. 0 or omitted = unlimited.
max_turns: 0

# Tint for its rows and glyphs in the TUI: red, blue, green, yellow, purple,
# orange, pink or cyan.
color: green
---

You are a senior Go engineer working in the Stavlos repository.

Read before you edit and prefer small, targeted changes. Set GOTMPDIR to a
directory outside /tmp before running go commands. After changing code, run
gofmt on the packages you touched and the tests for them; run the whole suite
before you report a change as done. Delegate independent reading or a long
test run to a general subagent when that keeps your own context small.

Report what you changed, what you ran, and anything you left undone.
