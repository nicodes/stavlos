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
	return model.ToolDef{Name: "agent_create", Description: "Create a child agent and give it a task. Returns its id immediately. The task is the child's first prompt; its agent_response comes back to you as a new message between turns, never mid-turn. If you have nothing else to do until then, end your turn. The child stays alive afterwards: agent_prompt it again for follow-ups (it keeps its context), and agent_kill it when you are done with it.",
		Schema: schema(map[string]any{
			"archetype": prop("string", "Preset name of the child (see the list in your instructions)"),
			"label":     prop("string", "Short human-facing name for this child, e.g. 'auth-explorer' (required)"),
			"task":      prop("string", "The complete task description; the child has no other context"),
			"model":     prop("string", "Optional provider/model-id override for this child"),
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
	var a struct{ Archetype, Label, Task, Model string }
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if strings.TrimSpace(a.Label) == "" {
		return errf("label is required")
	}
	if ok, why := env.Orch.CanSpawn(env.Agent); !ok {
		return errf("cannot spawn: %s", why)
	}
	id, err := env.Orch.Spawn(ctx, env.Agent, a.Archetype, a.Label, a.Task, a.Model)
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: fmt.Sprintf("spawned %s (%s) as %s", a.Label, a.Archetype, id)}
}

// --- send / steer / cancel / kill ---

type sendTool struct{}

func (sendTool) Def() model.ToolDef {
	return model.ToolDef{Name: "agent_prompt", Description: "Send a message to any other agent in this session (a child, a sibling, or your parent); it runs after that agent's current turn ends. The recipient sees it as coming from you and answers with agent_response, which wakes you between turns. agent_status lists every agent and its id.",
		Schema: schema(map[string]any{"id": prop("string", "Target agent id (any agent in the session)"), "text": prop("string", "Message")}, "id", "text")}
}
func (sendTool) PolicyArg(in json.RawMessage) string { return idArg(in) }
func (sendTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	var a struct{ ID, Text string }
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if err := env.Orch.Send(env.Agent, a.ID, a.Text); err != nil {
		return errf("%v", err)
	}
	return Result{Output: "queued"}
}

type steerTool struct{}

func (steerTool) Def() model.ToolDef {
	return model.ToolDef{Name: "agent_steer", Description: "Main agent only: redirect any other agent in this session at its next model-call boundary without discarding its work. If the agent is idle this behaves like agent_prompt.",
		Schema: schema(map[string]any{"id": prop("string", "Target agent id (any agent in the session)"), "text": prop("string", "Instruction")}, "id", "text")}
}
func (steerTool) PolicyArg(in json.RawMessage) string { return idArg(in) }
func (steerTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	var a struct{ ID, Text string }
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if err := env.Orch.Steer(env.Agent, a.ID, a.Text); err != nil {
		return errf("%v", err)
	}
	return Result{Output: "steered"}
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

type killTool struct{}

func (killTool) Def() model.ToolDef {
	return model.ToolDef{Name: "agent_kill", Description: "Tear down a child agent and its subtree when you no longer need it. Its history is preserved but it cannot be prompted again.",
		Schema: schema(map[string]any{"id": prop("string", "Child agent id")}, "id")}
}
func (killTool) PolicyArg(in json.RawMessage) string { return idArg(in) }
func (killTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	if err := env.Orch.Kill(env.Agent, idArg(in)); err != nil {
		return errf("%v", err)
	}
	return Result{Output: "killed"}
}

// --- monitor / result / status ---

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
