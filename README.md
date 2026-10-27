<!-- Generated from private documentation source. Do not edit directly. Source SHA256: 11ad636eef5c95d5cce96cd0e9566125cb5021308bee10912ed0c34865129b09 -->

# stavlos

A Go agent harness with a terminal UI.

Install the published Linux binary (x86_64 or arm64) into `~/.local/bin`; add that directory to your PATH if necessary. A Go compiler is not required.

```sh
curl -fsSL https://stavlos.ai/install | sh
cd /path/to/project
stavlos
```

Use `/providers` to configure a provider and `/models` to select an available model. `/help` lists commands. Credentials and channel history live in your operating system's private user data directory.

A message beginning with `@name` addresses that agent; an unaddressed message goes to the main agent. Tab and Shift-Tab move focus, Enter selects, and Escape returns to the previous view. Permission prompts require an explicit answer.

## Project configuration

Project configuration lives in `.stavlos/stavlos.json`. Agent definitions are Markdown files with YAML frontmatter in the adjacent `agents/` directory. The trust prompt controls whether project-defined configuration is used.

Source builds remain available for development with a compatible Go toolchain:

```sh
go install github.com/nicodes/stavlos/cmd/stavlos@latest
```

## Repository development

Install the [komizo release](https://github.com/nicodes/komizo#install) matching `komizo.lock`. Run `komizo check` for the full verification gate and `komizo check --explain` to inspect the selected checks. `komizo run build` compiles the declared artifacts. Interactive product sessions use the CLI directly.
