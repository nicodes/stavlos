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
}

// Asker is implemented by the agent runtime: it blocks the turn on a
// question prompt (kind "question") until the human answers or the turn
// is cancelled. Answers come back one per question, in order.
type Asker interface {
	Ask(ctx context.Context, questions []protocol.Question) ([]string, error)
}

// Monitors is implemented by the agent runtime: sources other than children
// that land a result in the agent's mailbox and wake it when armed.
type Monitors interface {
	StartCommand(command string, timeout time.Duration) (string, error)
	// AdoptCommand takes over a command the shell tool already started and
	// that outlived its wait window; it becomes a job like any other.
	AdoptCommand(command string, job Job, timeout time.Duration) (string, error)
	List() []MonitorStatus
	Stop(id string) error
	Has(id string) bool
}

// Job is a shell command already running under the shell tool.
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
	} {
		s[t.Def().Name] = t
	}
	return s
}

// OrchestrationNames are the tools implied by a non-empty spawn list.
var OrchestrationNames = []string{"agent_create", "agent_cancel"}

// MessagingNames are offered to every agent: any agent may prompt any
// other in its session and see the tree. Steering is the main agent's
// alone (it is offered separately), and lifecycle tools stay with the
// parent (see OrchestrationNames).
var MessagingNames = []string{"agent_message", "agent_response", "agent_status"}

// AsyncNames are offered to every agent that has shell.
var AsyncNames = []string{"shell_kill"}

// AskNames are offered to every agent: asking the human is never a role
// choice.
var AskNames = []string{"ask_user"}

// TodoNames are the tools implied by "todo" in a preset's tool list.
var TodoNames = []string{"todo_add", "todo_update"}

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
