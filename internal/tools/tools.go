// Package tools implements the built-in tool set (PRD §14) and the
// orchestration tools (PRD §6.4).
package tools

import (
	"context"
	"encoding/json"
	"fmt"

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
	// Monitor is the only way to await children: the caller's turn ends
	// after this tool batch and each child's result wakes it as a message.
	// A child's finish always wakes its parent; monitor just yields now.
	Monitor(parent string, ids []string) ([]ChildStatus, error)
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
		bashTool{}, readTool{}, writeTool{}, editTool{}, grepTool{}, globTool{}, skillTool{}, finishTool{},
		spawnTool{}, sendTool{}, steerTool{}, cancelTool{}, killTool{}, monitorTool{}, resultTool{}, statusTool{},
	} {
		s[t.Def().Name] = t
	}
	return s
}

// OrchestrationNames are the tools implied by a non-empty spawn list.
var OrchestrationNames = []string{"spawn", "send", "steer", "cancel", "kill", "monitor", "result", "status"}

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
