// Package tools implements the built-in tool set (PRD §14) and the
// orchestration tools (PRD §6.4).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/toolname"
)

// Result is a tool's output.
type Result struct {
	Output  string
	IsError bool
}

// Env is what a tool gets from the calling agent.
type Env struct {
	Dir       string                  // session working directory
	Skills    map[string]config.Skill // skills this agent may load
	Agent     string                  // caller agent id
	Orch      Orchestrator            // nil if the agent cannot orchestrate
	Partial   func(string)            // receives streamed partial output (shell); may be nil
	MaxOutput int                     // truncate tool output beyond this many bytes (0 = 32k)
	Mon       Monitors                // general monitors (background commands, watches, timers); nil if unavailable
	Todo      Todos                   // the agent's todo list; nil if the preset does not include "todo"
	Ask       Asker                   // raises a question batch to the human and waits; nil in tests without a runtime
	Search    SearchConfig            // web_search backend; zero → the tool explains how to configure it
	PassEnv   []string                // environment variables kept for child processes although their names look like secrets (config env.pass)
}

// Asker is implemented by the agent runtime: it blocks the turn on a
// question prompt (kind "question") until the human answers or the turn
// is cancelled. Answers come back one per question, in order.
type Asker interface {
	Ask(ctx context.Context, questions []protocol.Question) ([]string, error)
}

// Monitors is implemented by the agent runtime: background jobs, whose
// exit lands a result in the agent's mailbox and wakes it.
type Monitors interface {
	// AdoptCommand takes over a command the shell tool started (one that
	// outlived its wait window, or was started in the background) and
	// kills, reaps and reports it like any other job.
	AdoptCommand(command string, job Job, timeout time.Duration) (string, error)
	List() []MonitorStatus
	Stop(id string) error
	Has(id string) bool
}

// Job is a shell command running under the shell tool (a *proc.Job).
type Job interface {
	Done() <-chan struct{} // closed once the process has exited
	Err() error            // the exit error, valid after Done
	Kill()                 // ends the process group
	Output() string        // output so far (tail-capped)
	Lines() int
	Started() time.Time
}

// MonitorStatus is a running general monitor.
type MonitorStatus struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Label    string    `json:"label"`
	Spec     string    `json:"spec"`
	State    string    `json:"state"`
	Progress string    `json:"progress,omitempty"`
	Started  time.Time `json:"started"`
}

// MultiArg is implemented by tools that touch several paths in one call
// (apply_patch); policy evaluates every path and the most restrictive
// decision wins.
type MultiArg interface {
	PolicyArgs(input json.RawMessage) []string
}

// Tool is one callable tool.
type Tool interface {
	Def() model.ToolDef
	// PolicyArg extracts the string that policy patterns match against.
	PolicyArg(input json.RawMessage) string
	Run(ctx context.Context, input json.RawMessage, env *Env) Result
}

// Orchestrator is implemented by the agent runtime (PRD §6.4).
type Orchestrator interface {
	// Spawn creates a child; dirs are directories to grant it, each of which
	// must be inside the parent's own working directories.
	Spawn(ctx context.Context, parent, archetype, label, task, modelID string, dirs []string) (string, error)
	// Message delivers text to any agent in the session at its next step:
	// mid-turn if it is busy, as a new turn if it is idle.
	Message(caller, id, text string) error
	Cancel(parent, id string) error
	Status(parent, id string) ([]ChildStatus, error)
	// Respond delivers the caller's answer to an agent that prompted it; the
	// recipient is woken between turns. The caller stays alive.
	Respond(caller, to, text string) error
	// CanSpawn reports whether depth/fan-out limits currently permit a spawn.
	CanSpawn(agent string) (bool, string)
	// Archetypes the caller may spawn.
	Archetypes(agent string) []string
}

type ChildStatus struct {
	ID        string   `json:"id"`
	Parent    string   `json:"parent,omitempty"`
	You       bool     `json:"you,omitempty"` // this row is the caller
	Label     string   `json:"label"`
	Archetype string   `json:"archetype"`
	State     string   `json:"state"`
	Turn      int      `json:"turn"`
	CostUSD   float64  `json:"cost_usd"`
	Summary   string   `json:"summary,omitempty"`
	Dirs      []string `json:"dirs,omitempty"` // working directories (what a parent may grant on)
}

type Artifact struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

// Set is a named collection.
type Set map[string]Tool

// Builtin returns every built-in tool.
func Builtin() Set {
	s := Set{}
	for _, t := range []Tool{
		shellTool{}, readTool{}, patchTool{}, skillTool{}, responseTool{},
		spawnTool{}, messageTool{}, cancelTool{}, statusTool{},
		shellKillTool{},
		todoAddTool{}, todoUpdateTool{}, askTool{},
		webFetchTool{}, webSearchTool{},
	} {
		s[t.Def().Name] = t
	}
	return s
}

// The tool groups a preset's list implies live in toolname; these names
// stay for the agent package's prompt assembly.
var (
	OrchestrationNames = toolname.Orchestration // implied by a non-empty spawn list
	MessagingNames     = toolname.Messaging     // every agent may message any other and see the tree
	AsyncNames         = toolname.Async         // every agent that has shell
	AskNames           = toolname.Ask           // every agent: asking the human is never a role choice
	TodoNames          = toolname.Todo          // implied by "todo" in a preset's tool list
)

func schema(props map[string]any, required ...string) json.RawMessage {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	b, _ := json.Marshal(m)
	return b
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

func errf(format string, a ...any) Result {
	return Result{Output: fmt.Sprintf(format, a...), IsError: true}
}

// Clip truncates tool output to max bytes (0 = 32k), keeping the head and
// the tail.
func Clip(s string, max int) string { return clip(s, max) }

func clip(s string, max int) string {
	if max <= 0 {
		max = 32 * 1024
	}
	if len(s) <= max {
		return s
	}
	head := max * 2 / 3
	tail := max - head
	return s[:head] + fmt.Sprintf("\n\n… [%d bytes truncated] …\n\n", len(s)-max) + s[len(s)-tail:]
}

func decode(input json.RawMessage, v any) error {
	if len(input) == 0 {
		return nil
	}
	return json.Unmarshal(input, v)
}
