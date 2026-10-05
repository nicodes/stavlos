// Package toolname is the one list of built-in tool names. Every package
// that names a tool — the tool itself, the default policy, the roles,
// the turn loop's prompt sections, the prefix logic, the TUI's glyphs and
// argument pickers — spells it from here, so adding or renaming a tool is
// one edit and a typo is a compile error.
//
// The constants are untyped strings: tool names travel as strings in
// model.ToolDef, policy.Rule and the event log, and a typed name would
// add a conversion at every one of those seams without catching more.
package toolname

import "encoding/json"

const (
	Shell      = "shell"
	ShellKill  = "shell_kill"
	Read       = "read"
	Grep       = "grep"
	Glob       = "glob"
	ApplyPatch = "patch"
	Skill      = "skill"
	WebFetch   = "fetch"
	WebSearch  = "search"
	Web        = "web"
	Todo       = "todo"
	AskUser    = "ask"
	Sheet      = "sheet"

	Message = "message"
	Agent   = "agent"
	Channel = "channel"

	// Legacy operation names remain useful for old logs and policy migration.
	AgentCreate = "agent_create"
	AgentCancel = "agent_cancel"
	AgentStatus = "agent_status"

	// MCPPrefix starts the model-facing name of an MCP server's tool:
	// mcp__<server>__<tool>.
	MCPPrefix = "mcp__"
)

// Groups of tools offered together.
var (
	// Messaging is offered to every agent: any agent may message any other
	// in its channel, or the human, and see the tree.
	Messaging = []string{Message, Agent, Channel}
	// Ask is offered to every agent: asking the human is never a role choice.
	Ask = []string{AskUser}
)

// Expand is a role's tools: list with duplicates dropped, in order.
func Expand(names []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range names {
		add(n)
	}
	return out
}

// User is the name that stands for the human: a message's recipient
// ("@user"), and the sender of what the human types.
const User = "user"

// Canonical accepts old configuration and transcript names without exposing
// aliases as additional model-facing tools.
func Canonical(name string) string {
	switch name {
	case "apply_patch":
		return ApplyPatch
	case "web_fetch":
		return WebFetch
	case "web_search":
		return WebSearch
	case "ask_user":
		return AskUser
	}
	return name
}

// Operation identifies an agent action for presentation and context clearing.
// Legacy log entries already carry the operation as their tool name.
func Operation(name string, input json.RawMessage) string {
	if name != Agent && name != Web && name != Shell {
		return Canonical(name)
	}
	var in struct{ Action string }
	_ = json.Unmarshal(input, &in)
	if name == Web {
		switch in.Action {
		case "fetch":
			return WebFetch
		case "search":
			return WebSearch
		}
		return name
	}
	if name == Shell {
		if in.Action == "kill" {
			return ShellKill
		}
		return name
	}
	switch in.Action {
	case "create":
		return AgentCreate
	case "cancel":
		return AgentCancel
	case "status":
		return AgentStatus
	}
	return name
}

// PolicyRule migrates old tool names, preserving each agent action's scope.
// Agent subjects are "<action> <target>", including the space for an empty target.
func PolicyRule(name, pattern string) (string, string) {
	name = Canonical(name)
	switch name {
	case Shell:
		if pattern != "*" {
			return Shell, "run " + pattern
		}
		return Shell, pattern
	case AgentCreate:
		return Agent, "create " + pattern
	case AgentCancel:
		return Agent, "cancel " + pattern
	case AgentStatus:
		return Agent, "status " + pattern
	case WebFetch:
		return Web, "fetch " + pattern
	case WebSearch:
		return Web, "search " + pattern
	case ShellKill:
		return Shell, "kill " + pattern
	}
	return Canonical(name), pattern
}
