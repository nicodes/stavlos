# File-backed configuration editor

The TUI has one editor for project and system configuration:

- A channel's **⚙** opens that channel's `.stavlos/` directory.
- The **⚙** beside the top **Stavlos** title opens the system config directory.
- `/settings` or `/config` opens project configuration; `/settings system`
  opens system configuration. `/dir` manages the running channel's directory.

The popup displays its scope and absolute directory. Files are written directly
there, so project edits appear in `git diff` and can be committed normally.
System files live in the configured global directory, normally
`~/.config/stavlos/`.

## Navigation and editing

The left pane has Settings, Agents, Commands, Skills and Files sections. The
Files section includes supporting files and directories, not just known config
definitions. Click a section or use left/right while the file pane is focused.
Up/down selects a file; Enter or a click opens it. Directory rows fold/unfold.
Tab switches panes.

The right pane provides:

- **Settings:** all supported `stavlos.json` and project `stavlos.local.json`
  fields, including nested fields. Arrays and flexible maps such as policy/MCP
  configuration use a JSON value editor.
- **Agents:** role frontmatter fields and the Markdown prompt body.
- **Commands:** the description and prompt body.
- **Skills:** name, description and instructions.
- **Raw editing:** F4 switches between structured fields and the complete file.
  Other text files open directly in the raw editor.

Select a field with Enter or click it. Booleans toggle and enumerated settings
cycle through their choices, saving immediately. Confirm single-line values
with Enter. Use **Ctrl+S** for raw files, JSON values and Markdown bodies.
Strings are entered as text; numeric/JSON values must be valid JSON (`null` can
clear an optional value). The raw editor allows removal of fields and any other
file-level changes.

In the file pane:

| Key | Action |
| --- | --- |
| Ctrl+N | Choose a new relative file path and edit its template |
| F2 | Rename a file or directory |
| Ctrl+D | Delete the selected file/directory after confirmation |
| Ctrl+L | Reload the files from disk |
| Esc | Close, with confirmation if an edit is unsaved |

File paths are relative to the displayed config directory. Agent, command and
skill files receive appropriate initial templates. New files are written when
saved, not just when opened. Text editing supports UTF-8 files up to 4 MiB;
binary files can be managed through the file tree but not edited as text.
Symlinked paths are not writable through the scoped editor.

## Validation, conflicts and reload

A save is first applied to a temporary copy and checked with the normal config,
role, command and skill loaders. Invalid changes leave the real file untouched
and the edit remains available to fix. File replacement is atomic and preserves
the existing file permissions. Renames use the filesystem's rename operation.

The editor checks a revision of the config tree before saving. If another editor
or process changes files, reload before retrying rather than overwriting those
changes. Structured JSONC edits replace the target value while retaining
unrelated comments and formatting. Frontmatter edits retain YAML comments;
the edited frontmatter can be reformatted by the YAML encoder. Raw edits save
the entered file contents.

Configuration is reloaded after a successful save:

- Policy, limits, hosts, directories, roles, commands, skills and agent config
  become available through the live channel configuration. Active model work
  adopts configuration at its next normal step.
- File-defined model, mode and root-agent defaults affect new channels; they
  do not silently replace a running channel's selected values.
- Global escalation settings apply to new prompts. Already-open prompts retain
  their original timers.
- Discord connection changes require reconnecting. Plugin changes require a
  daemon restart.

Human edits to an already-trusted project carry its trust hash forward.
Previously untrusted project configuration still needs the normal trust decision.
The save result reports reload problems and the relevant application timing.
