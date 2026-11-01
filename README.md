<!-- Generated from private documentation source. Do not edit directly. Source SHA256: b82f7bfaa45eff9509a7a907d44531a1817cd2f887ff205aebcacd53b1e4f2f0 -->

# stavlos

An agent harness written in Rust, with a Ratatui terminal UI and an Axum browser API.

## Install

Linux (x86_64 or arm64), using prebuilt binaries:

With curl:

```sh
curl -fsSL https://stavlos.ai/install | sh
```

Or with mise:

```sh
mise use --global github:nicodes/stavlos@latest
```

The shell installer installs to `~/.local/bin`; add that directory to your PATH if necessary. Mise manages its own installation directory and PATH through its shell activation.

## Run

```sh
cd /path/to/project
stavlos
```

Use `/providers` to configure a provider and `/models` to select an available model. `/help` lists commands. Credentials and channel history live in your operating system's private user data directory.

A message beginning with `@name` addresses that agent; an unaddressed message goes to the main agent. Tab and Shift-Tab move focus, Enter selects, and Escape returns to the previous view. Permission prompts require an explicit answer.

## Project configuration

Project configuration lives in `.stavlos/stavlos.json`. Agent definitions are Markdown files with YAML frontmatter in the adjacent `agents/` directory. The trust prompt controls whether project-defined configuration is used. Shared skills live in `.agents/skills/<name>/SKILL.md` in the project or `~/.agents/skills/<name>/SKILL.md` for the user. The skill body uses Markdown with `name` and `description` YAML frontmatter; supporting scripts and resources stay in the package directory.

Shared project skills and resources are trust-gated together with native configuration. Project definitions override user definitions; native `skills/` definitions override shared definitions within the same scope. `STAVLOS_SKILLS_DIR` can select a different user-level shared library. Updating project skills requires renewed project trust.

## Prepare runtime files

After cloning this repository, run the following with Python 3 before loading its skills, agents, or plugins:

```sh
python3 scripts/prepare-runtime.py
```

This creates the native discovery files from the tracked functional inputs. Run it again after pulling an update; if a generated file has local edits, preserve those edits before replacing it.

## Contributor verification

Tool versions and native tasks live in `mise.toml`. Prepare the pinned helper source and run the complete gate:

```sh
git clone https://github.com/nicodes/tools.git .ci-tools
git -C .ci-tools checkout --detach f412061a7a3f0914db9e09d1159e69504aee8706
mise install
mise run check
```

The CLI, owner daemon, agent runtime and terminal interface are implemented in Rust. The Ratatui interface includes channel and agent navigation, command completion, question and permission cards, provider sign-in, configuration editing and usage views. Enter submits a message; Ctrl+J or Alt+Enter inserts a newline. Ctrl+C twice quits, and Escape twice on an empty input cancels a busy agent.

The browser API and local OAuth callback stay in root `api/`. The API reads the owner's SQLite event history, owns browser sessions and scoped WebSocket requests, and communicates with the owner through bounded control messages. Disabling the browser UI clears its sessions and closes its sockets. Inspect channel history before resubmitting a message whose acknowledgement was lost.

Published Linux x86_64 and arm64 CLIs are static Rust executables. The same executable dispatches its API and sandbox helpers; installation requires no compiler. Native Actions builders verify each architecture and a packaged browser sign-in. Archive assembly checks each executable's exact source, version, platform and SHA-256 receipt.

For source development, `cargo build --locked` builds the complete CLI with the committed browser bundle. `cargo build --locked --manifest-path api/Cargo.toml` builds the standalone API. Set `STAVLOS_API_BINARY` to its absolute path to use it during development. Release builds use `scripts/build-cli.sh` and require the matching musl C toolchain on Linux.

`mise run check` runs Rust formatting, strict Clippy, native tests, dependency scans, shell and workflow checks, browser tests, bundle verification, installer refusal checks and native artifact verification. Both build and test stages retain receipts under `.artifacts/verification`.
