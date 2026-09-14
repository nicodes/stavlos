package protocol

import "strings"

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
