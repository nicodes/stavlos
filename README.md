<!-- Generated from private documentation source. Do not edit directly. Source SHA256: ea2e43def22be2c6be434ddd11efffaf1fe4c16e47ecc875cf072f8185d74469 -->

# stavlos

A Go agent harness with a terminal UI.

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

Project configuration lives in `.stavlos/stavlos.json`. Agent definitions are Markdown files with YAML frontmatter in the adjacent `agents/` directory. The trust prompt controls whether project-defined configuration is used.

## Repository development

Install the [komizo release](https://github.com/nicodes/komizo#install) matching `komizo.lock`. Run `komizo check` for the full verification gate and `komizo check --explain` to inspect the selected checks. `komizo run build` compiles the declared artifacts. Interactive product sessions use the CLI directly.
