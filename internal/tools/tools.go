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
	Partial   func(string)            // receives streamed partial output (bash); may be nil
	MaxOutput int                     // truncate tool output beyond this many bytes (0 = 32k)
	Mon       Monitors                // general monitors (background commands, watches, timers); nil if unavailable
}

// Monitors is implemented by the agent runtime: sources other than children
// that land a result in the agent's mailbox and wake it when armed.
type Monitors interface {
	StartCommand(command string, timeout time.Duration) (string, error)
	List() []MonitorStatus
	Stop(id string) error
	Has(id string) bool
}

// MonitorStatus is a running general monitor.
type MonitorStatus struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Label     string    `json:"label"`
	Spec      string    `json:"spec"`
	State     string    `json:"state"`
	Progress  string    `json:"progress,omitempty"`
	Monitored bool      `json:"wakes_you"`
	Started   time.Time `json:"started"`
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
	Spawn(ctx context.Context, parent, archetype, label, task, modelID string) (string, error)
	Send(parent, id, text string) error
	Steer(parent, id, text string) error
	Cancel(parent, id string) error
	Kill(parent, id string) error
	Result(parent, id string) (ChildResult, bool, error)
	Status(parent, id string) ([]ChildStatus, error)
	// Finish records the caller's completion; the loop ends the turn after it.
	Finish(agent, summary, status string, artifacts []Artifact) error
	// CanSpawn reports whether depth/fan-out limits currently permit a spawn.
	CanSpawn(agent string) (bool, string)
	// Archetypes the caller may spawn.
	Archetypes(agent string) []string
}

type ChildResult struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Status    string     `json:"status"`
	Summary   string     `json:"summary"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

type ChildStatus struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Archetype string  `json:"archetype"`
	State     string  `json:"state"`
	Turn      int     `json:"turn"`
	CostUSD   float64 `json:"cost_usd"`
	Summary   string  `json:"summary,omitempty"`
	Monitored bool    `json:"monitored"` // a wake is armed for this child
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
		bashTool{}, readTool{}, patchTool{}, skillTool{}, finishTool{},
		spawnTool{}, sendTool{}, steerTool{}, cancelTool{}, killTool{}, resultTool{}, statusTool{},
		bashAsyncTool{}, bashKillTool{},
	} {
		s[t.Def().Name] = t
	}
	return s
}

// OrchestrationNames are the tools implied by a non-empty spawn list.
var OrchestrationNames = []string{"agent_create", "agent_prompt", "agent_steer", "agent_cancel", "agent_kill", "agent_result", "agent_status"}

// AsyncNames are offered to every agent that has bash.
var AsyncNames = []string{"bash_async", "bash_kill"}

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
