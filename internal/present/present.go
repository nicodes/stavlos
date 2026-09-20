// Package present holds what every client says the same way: the words on
// the answers to a permission prompt, and which argument of a tool call
// stands for the call. The terminal, Discord and the browser each had their
// own copy ("Allow for this channel" here, "Allow in this channel" there; a
// tool's main argument chosen by a switch, by a field list and by guessing
// at keys), so a new tool or a reworded answer was three edits and the ones
// forgotten showed. It has no dependency on any client.
package present

import "github.com/nicodes/stavlos/internal/toolname"

// The answers to a permission prompt.
const (
	AllowOnce    = "Allow once"
	AllowChannel = "Allow for this channel"
	AllowAddDir  = "Allow and add directory"
	Deny         = "Deny"
)

// AllowPrefix is the answer that allows every call starting with prefix (a
// command prefix, or a host).
func AllowPrefix(prefix string) string { return "Allow " + prefix + " for this channel" }

// primary is the argument that says what a call does, by tool.
var primary = map[string]string{
	toolname.Shell:       "command",
	toolname.ShellKill:   "id",
	toolname.Read:        "path",
	toolname.Grep:        "pattern",
	toolname.Glob:        "pattern",
	toolname.ApplyPatch:  "patch",
	toolname.Skill:       "name",
	toolname.WebFetch:    "url",
	toolname.WebSearch:   "query",
	toolname.AgentCancel: "id",
	toolname.AgentStatus: "id",
}

// PrimaryArg is the key of the argument that stands for a call of tool, or
// "" for a tool with none (its whole input is shown).
func PrimaryArg(tool string) string { return primary[tool] }

// PrimaryArgs is the whole table, for the generated TypeScript.
func PrimaryArgs() map[string]string {
	out := make(map[string]string, len(primary))
	for k, v := range primary {
		out[k] = v
	}
	return out
}
