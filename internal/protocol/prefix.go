package protocol

import (
	"net/url"
	"strings"
)

// twoWordTools are commands whose first word says little on its own: "go"
// covers build, test and run alike, so their prefix takes the subcommand too.
var twoWordTools = map[string]bool{
	"git": true, "go": true, "npm": true, "npx": true, "cargo": true, "make": true,
	"docker": true, "kubectl": true, "pip": true, "yarn": true, "pnpm": true, "bun": true,
}

// CommandPrefix is the part of a shell command a human may allow for the
// rest of a session: its first word, or two words for tools like git and
// go ("go test"), when the second is a subcommand rather than a flag. It is
// "" for a compound command (pipes, ;, &&, newlines, substitutions), where a
// prefix would cover far more than what is on screen.
func CommandPrefix(cmd string) string {
	if !SimpleCommand(cmd) {
		return ""
	}
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	if twoWordTools[f[0]] && len(f) > 1 && !strings.HasPrefix(f[1], "-") {
		return f[0] + " " + f[1]
	}
	return f[0]
}

// ToolPrefix is what "allow … for this session" may remember for a tool
// call: the command prefix for shell, the host for web_fetch, "" for tools
// without a sensible prefix.
func ToolPrefix(tool, arg string) string {
	switch tool {
	case "shell":
		return CommandPrefix(arg)
	case "web_fetch":
		return URLHost(arg)
	}
	return ""
}

// ToolPrefixCovers reports whether a remembered prefix covers a call of
// tool with arg: whole-word command prefixes for shell, the host for
// web_fetch.
func ToolPrefixCovers(tool, prefix, arg string) bool {
	switch tool {
	case "shell":
		return PrefixCovers(prefix, arg)
	case "web_fetch":
		return prefix != "" && URLHost(arg) == prefix
	}
	return false
}

// URLHost is the lower-cased host of a URL ("" when it has none); a bare
// "host/path" counts as https.
func URLHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// PrefixCovers reports whether an allowed prefix covers cmd: cmd is the
// prefix itself or starts with it as whole words, and is a simple command.
func PrefixCovers(prefix, cmd string) bool {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || !SimpleCommand(cmd) {
		return false
	}
	cmd = strings.TrimSpace(cmd)
	return cmd == prefix || strings.HasPrefix(cmd, prefix+" ")
}

// SimpleCommand reports whether cmd is one command with no shell control
// operators: nothing chained, piped, substituted or spread over lines.
func SimpleCommand(cmd string) bool {
	return !strings.ContainsAny(cmd, ";|&\n`") && !strings.Contains(cmd, "$(")
}
