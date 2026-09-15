// Package toolname is the one list of built-in tool names. Every package
// that names a tool — the tool itself, the default policy, the presets,
// the turn loop's prompt sections, the prefix logic, the TUI's glyphs and
// argument pickers — spells it from here, so adding or renaming a tool is
// one edit and a typo is a compile error.
//
// The constants are untyped strings: tool names travel as strings in
// model.ToolDef, policy.Rule and the event log, and a typed name would
// add a conversion at every one of those seams without catching more.
package toolname

const (
	Shell      = "shell"
	ShellKill  = "shell_kill"
	Read       = "read"
	Grep       = "grep"
	Glob       = "glob"
	ApplyPatch = "apply_patch"
	Skill      = "skill"
	WebFetch   = "web_fetch"
	WebSearch  = "web_search"
	TodoAdd    = "todo_add"
	TodoUpdate = "todo_update"
	AskUser    = "ask_user"

	Message = "message"

	AgentCreate = "agent_create"
	AgentCancel = "agent_cancel"
	AgentStatus = "agent_status"

	// GroupTodo is the entry in a role's tools: list that stands for both
	// todo tools.
	GroupTodo = "todo"

	// MCPPrefix starts the model-facing name of an MCP server's tool:
	// mcp__<server>__<tool>.
	MCPPrefix = "mcp__"
)

// Groups of tools offered together.
var (
	// Todo are the tools GroupTodo stands for.
	Todo = []string{TodoAdd, TodoUpdate}
	// Messaging is offered to every agent: any agent may message any other
	// in its channel, or the human, and see the tree.
	Messaging = []string{Message, AgentStatus}
	// Orchestration is implied by a non-empty spawn list.
	Orchestration = []string{AgentCreate, AgentCancel}
	// Async is offered to every agent that has shell.
	Async = []string{ShellKill}
	// Ask is offered to every agent: asking the human is never a role choice.
	Ask = []string{AskUser}
)

// Expand replaces group entries in a role's tools: list with the tools
// they stand for, keeping order and dropping duplicates.
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
		if n == GroupTodo {
			for _, t := range Todo {
				add(t)
			}
			continue
		}
		add(n)
	}
	return out
}

// User is the name that stands for the human: a message's recipient
// ("@user"), and the sender of what the human types.
const User = "user"
