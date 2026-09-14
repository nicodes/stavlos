package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
)

func idArg(in json.RawMessage) string {
	var a struct{ ID string }
	_ = decode(in, &a)
	return a.ID
}

func needOrch(env *Env) *Result {
	if env.Orch == nil {
		r := errf("orchestration tools are not available to this agent")
		return &r
	}
	return nil
}

func jsonOut(v any) Result {
	b, _ := json.MarshalIndent(v, "", "  ")
	return Result{Output: string(b)}
}

// --- spawn ---

type spawnTool struct{}

func (spawnTool) Def() model.ToolDef {
	return model.ToolDef{Name: "agent_create", Description: "Create a child agent and give it a task. Returns its id immediately. The task is the child's first prompt; its agent_response comes back to you as a new message between turns, never mid-turn. If you have nothing else to do until then, end your turn. The child stays alive for the rest of the session: agent_message it again for follow-ups (it keeps its context). There is nothing to clean up.",
		Schema: schema(map[string]any{
			"archetype": prop("string", "Preset name of the child (see the list in your instructions)"),
			"label":     prop("string", "Short human-facing name for this child, e.g. 'auth-explorer' (required)"),
			"task":      prop("string", "The complete task description; the child has no other context"),
			"model":     prop("string", "Optional provider/model-id override for this child"),
			"dirs":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional directories to grant the child on top of the session directory and its role's own; each must be inside one of yours (agent_status lists them)"},
		}, "archetype", "label", "task")}
}
func (spawnTool) PolicyArg(in json.RawMessage) string {
	var a struct{ Archetype string }
	_ = decode(in, &a)
	return a.Archetype
}
func (spawnTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	var a struct {
		Archetype, Label, Task, Model string
		Dirs                          []string
	}
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if strings.TrimSpace(a.Label) == "" {
		return errf("label is required")
	}
	if ok, why := env.Orch.CanSpawn(env.Agent); !ok {
		return errf("cannot spawn: %s", why)
	}
	id, err := env.Orch.Spawn(ctx, env.Agent, a.Archetype, a.Label, a.Task, a.Model, a.Dirs)
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: fmt.Sprintf("spawned %s (%s) as %s", a.Label, a.Archetype, id)}
}

// --- message / cancel / kill ---

type messageTool struct{}

func (messageTool) Def() model.ToolDef {
	return model.ToolDef{Name: "agent_message", Description: "Send a message to any other agent in this session (a child, a sibling, or your parent). It reaches the agent at its next step: mid-turn if it is busy, as a new turn if it is idle. The recipient sees it as coming from you and answers with agent_response, which wakes you between turns. agent_status lists every agent and its id.",
		Schema: schema(map[string]any{"id": prop("string", "Target agent id (any agent in the session)"), "text": prop("string", "Message")}, "id", "text")}
}
func (messageTool) PolicyArg(in json.RawMessage) string { return idArg(in) }
func (messageTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	var a struct{ ID, Text string }
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if err := env.Orch.Message(env.Agent, a.ID, a.Text); err != nil {
		return errf("%v", err)
	}
	return Result{Output: "delivered"}
}

type cancelTool struct{}

func (cancelTool) Def() model.ToolDef {
	return model.ToolDef{Name: "agent_cancel", Description: "End a child's current turn immediately. The child survives and can be sent new prompts.",
		Schema: schema(map[string]any{"id": prop("string", "Child agent id")}, "id")}
}
func (cancelTool) PolicyArg(in json.RawMessage) string { return idArg(in) }
func (cancelTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	if err := env.Orch.Cancel(env.Agent, idArg(in)); err != nil {
		return errf("%v", err)
	}
	return Result{Output: "cancelled"}
}

type statusTool struct{}

func (statusTool) Def() model.ToolDef {
	return model.ToolDef{Name: "agent_status", Description: "State, turn count, and cost of one agent, or of every agent in the session (the whole tree, parents before children; your own row is marked).",
		Schema: schema(map[string]any{"id": prop("string", "Agent id; omit for the whole session")})}
}
func (statusTool) PolicyArg(in json.RawMessage) string { return idArg(in) }
func (statusTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	st, err := env.Orch.Status(env.Agent, idArg(in))
	if err != nil {
		return errf("%v", err)
	}
	return jsonOut(st)
}
