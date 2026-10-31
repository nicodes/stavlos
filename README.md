<!-- Generated from private documentation source. Do not edit directly. Source SHA256: 31565eda89a6fc2bf235f27b7c055c89fb8308a2d18df748d452ff4c88231f86 -->

# stavlos

An agent harness with a terminal UI and a Rust/Axum browser API.

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
git -C .ci-tools checkout --detach 091101094fedf5ce2e67c339c1c9c842d6a662dc
mise install
mise run check
```

The browser API and local OAuth callback are implemented in root `api/`. Rust owns browser sessions, one-time sign-in codes, Host/Origin checks, sheet capabilities, the fourteen scoped WebSocket methods, event replay, usage queries and browser message routing. The Go terminal client and agent runtime remain independent native components. Browser RPC requests are handled in Rust and are never forwarded to the Go daemon's RPC dispatcher.

The API reads the existing SQLite event history without changing its schema. The agent runtime publishes bounded observations and accepts concrete, already-routed message deliveries. Rust records each admitted delivery before execution; an entry whose acknowledgement is lost remains for reconciliation and is never replayed automatically. Inspect channel history before resubmitting an uncertain post. Disabling the browser UI clears its sessions and closes its sockets.

Release CLIs embed their matching native Rust executable, so installation needs no Rust or Go toolchain. Linux x86_64 and arm64 use static musl APIs. Two native Actions builders test the API and a real packaged CLI sign-in, and archive assembly verifies each payload's exact source revision and SHA256 receipt. A contributor archive build requires the matching `.artifacts/native-api/` payload; `bash scripts/build-api.sh` builds the current platform and requires its musl C toolchain on Linux.

For source development, run `cargo build --locked --manifest-path api/Cargo.toml` and set `STAVLOS_API_BINARY` to the absolute built executable when running Go commands. `mise run check` supplies the test path and retains the complete Go race/fuzz/static/dependency/complexity, web bundle and installer checks. It also runs Rust formatting, Clippy, tests and dependency scanning. Native build and test commands retain source and output checks, with receipts in `.artifacts/verification`. End users install the product’s published distribution.
