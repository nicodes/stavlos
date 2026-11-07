<!-- Generated from private documentation source. Do not edit directly. Source SHA256: fc7e46f38896d12042e8bd502eeab9e77444c292c0efbba91ccad3715f31ac07 -->

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

Project configuration lives in `.stavlos/stavlos.toml`. Global configuration uses `stavlos.toml` in the Stavlos configuration directory; `.stavlos/stavlos.local.toml` applies the local project layer. Current-source builds and the configuration editor use TOML, preserving comments when editing fields. Legacy JSONC requires explicit conversion. Omitted optional overrides inherit, while `models = []` clears the inherited list and explicit `false` remains false. Renaming or converting project configuration changes its raw trust hash and requires fresh approval; prior trust is never transferred. Installed releases retain their released interfaces. Agent definitions are Markdown files with YAML frontmatter in the adjacent `agents/` directory. The trust prompt controls whether project-defined configuration is used. Shared skills live in `.agents/skills/<name>/SKILL.md` in the project or `~/.agents/skills/<name>/SKILL.md` for the user. The skill body uses Markdown with `name` and `description` YAML frontmatter; supporting scripts and resources stay in the package directory.

Shared project skills and resources are trust-gated together with native configuration. Project definitions override user definitions; native `skills/` definitions override shared definitions within the same scope. `STAVLOS_SKILLS_DIR` can select a different user-level shared library. Updating project skills requires renewed project trust.

Current-source migration is manual: create the named TOML source with the same effective settings and archive the legacy JSONC v1 source outside the active configuration directory. No automatic import command is provided. Preserve explicit `false`, zero and empty arrays; omit null overrides only where they meant inheritance or defaults. Do not copy credentials into tracked source files. Existing installed settings are not migrated by a source update.

Converting project settings changes the trust snapshot. Review and approve the new snapshot explicitly; previous approvals are not reused.

For JSONC v1 conversion, `models: null` inherits and therefore omits the TOML key; `models: []` becomes `models = []` and clears the inherited list. Preserve explicit false and zero overrides. A null MCP environment value becomes an empty string; a null MCP server entry becomes an empty table, preserving the entry rather than deleting it.

## Prepare runtime files

After cloning this repository, install Mise and run setup before loading its skills, agents, or plugins:

```sh
mise run setup
```

This creates the native discovery files from the tracked functional inputs. Run it again after pulling an update; if a generated file has local edits, preserve those edits before replacing it.

## Contributor verification

`mise.toml` pins contributor SDKs. The canonical generated task block in root `mise.toml` selects an immutable shared cookbook revision and binds it to `cookbook.toml`.

```sh
mise run help
mise run setup
mise run build
mise run test
```

`mise run help` works offline before setup. `mise run setup` explicitly prepares pinned SDKs, locked dependencies and declared fixtures. `mise run build` compiles and packages the native outputs, then validates their source and artifact identity. `mise run test` runs the implemented checks against those current-source outputs. Run build again after changing source; tests do not install tools or build missing shipping artifacts.

Use `mise run fmt` to apply formatting, `mise run test:lint`, `mise run test:unit`, `mise run test:security` or `mise run test:integration` to select a group. This recipe has no separate development service; run the verified native CLI directly. `mise run clean` removes declared generated outputs. Build and test evidence stays in `.artifacts/verification`.

Build prepares the SolidJS export, Rust API, runtime discovery files and embedded CLI. The verified executable is `.artifacts/build/stavlos`. The CLI, owner daemon, agent runtime and Ratatui terminal interface remain in Rust; the browser API and OAuth callback remain under `crates/api/`. Linux x86_64 and arm64 archives retain static linking, exact source/version/platform checks and packaged browser sign-in and PTY assertions. The existing `.stavlos/` discovery and trust boundaries remain unchanged.

The gate retains strict compiler/build validation, native unit tests, shell/workflow checks, UI tests, installer refusal checks, terminal restoration, browser-session boundaries, bundle verification and dependency scans. Both build and test retain their own source and output evidence. Provider calls remain explicitly selected product operations.

## Machine commands and configuration

Prefix finite commands with `--json`, for example `stavlos --json channels`, `stavlos --json status`, `stavlos --json auth list` or `stavlos --json new --dir /absolute/project --no-tui`. Stdout is one versioned result or error document; failures exit nonzero. Human terminal commands and interactive trust/login flows use their normal interface. JSON `web status|on|off` never opens a browser or includes a one-time sign-in capability; use the explicit `web open --print` command for a sign-in link.

`STAVLOS_CONFIG_DIR`, `STAVLOS_DATA_DIR`, `STAVLOS_CACHE_DIR` and `STAVLOS_SOCKET` override their corresponding XDG/native defaults. Shared skill lookup has its own `STAVLOS_SKILLS_DIR`. Changing checkout does not relocate existing installed state.
