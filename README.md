<!-- Generated from private documentation source. Do not edit directly. Source SHA256: 4d885d01263648621e82b2953e2d2cb17defb44e5146f37838a70b6e0860aa98 -->

# stavlos

A Go agent harness with a terminal UI.

Installation requires Go 1.26.6 or later. The installer builds the Go module into `~/.local/bin`; add that directory to your PATH if necessary.

```sh
curl -fsSL https://raw.githubusercontent.com/nicodes/stavlos/main/install.sh | sh
cd /path/to/project
stavlos
```

Use `/providers` to configure a provider and `/models` to select an available model. `/help` lists commands. Credentials and channel history live in your operating system's private user data directory.

A message beginning with `@name` addresses that agent; an unaddressed message goes to the main agent. Tab and Shift-Tab move focus, Enter selects, and Escape returns to the previous view. Permission prompts require an explicit answer.

## Project configuration

Project configuration lives in `.stavlos/stavlos.json`. Agent definitions are Markdown files with YAML frontmatter in the adjacent `agents/` directory. The trust prompt controls whether project-defined configuration is used.

## Repository development

Install the [komizo release](https://github.com/nicodes/komizo#install) matching `komizo.lock`. Run `komizo check` for the full verification gate and `komizo check --explain` to inspect the selected checks. `komizo run build` compiles the declared artifacts. Interactive product sessions use the CLI directly.
