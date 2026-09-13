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
	return model.ToolDef{Name: "spawn", Description: "Create a child agent that works on a task in the background. Returns its id immediately. Then call monitor: your turn ends and the child's finish result wakes you as a new message, so you stay responsive meanwhile.",
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
	return model.ToolDef{Name: "send", Description: "Queue a prompt for a child agent; it runs after the child's current turn ends.",
		Schema: schema(map[string]any{"id": prop("string", "Child agent id"), "text": prop("string", "Message")}, "id", "text")}
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
	return model.ToolDef{Name: "steer", Description: "Redirect a running child at its next model-call boundary without discarding its work. If the child is idle this behaves like send.",
		Schema: schema(map[string]any{"id": prop("string", "Child agent id"), "text": prop("string", "Instruction")}, "id", "text")}
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
	return model.ToolDef{Name: "cancel", Description: "End a child's current turn immediately. The child survives and can be sent new prompts.",
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
	return model.ToolDef{Name: "kill", Description: "Tear down a child agent and its subtree. Its history is preserved but it cannot be resumed.",
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

// --- monitor ---

type monitorTool struct{}

func (monitorTool) Def() model.ToolDef {
	return model.ToolDef{Name: "monitor", Description: "Hand control back now and be woken when your children finish. Your turn ends after this tool call (put anything else you need in the same batch); each child's result then arrives as a message that starts a new turn. Omit ids for all live children. There is no blocking wait: this is how subagents are awaited.",
		Schema: schema(map[string]any{"ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Child ids (default: all live children)"}})}
}
func (monitorTool) PolicyArg(in json.RawMessage) string { return "" }
func (monitorTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	var a struct{ IDs []string }
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	st, err := env.Orch.Monitor(env.Agent, a.IDs)
	if err != nil {
		return errf("%v", err)
	}
	if len(st) == 0 {
		return Result{Output: "no live children; any finished results arrive as your next message"}
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return Result{Output: "monitoring; your turn ends after this batch and each result will arrive as a message:\n" + string(b)}
}

type resultTool struct{}

func (resultTool) Def() model.ToolDef {
	return model.ToolDef{Name: "result", Description: "Fetch a finished child's result without blocking.",
		Schema: schema(map[string]any{"id": prop("string", "Child agent id")}, "id")}
}
func (resultTool) PolicyArg(in json.RawMessage) string { return idArg(in) }
func (resultTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	res, done, err := env.Orch.Result(env.Agent, idArg(in))
	if err != nil {
		return errf("%v", err)
	}
	if !done {
		return Result{Output: "not finished yet"}
	}
	return jsonOut(res)
}

type statusTool struct{}

func (statusTool) Def() model.ToolDef {
	return model.ToolDef{Name: "status", Description: "State, turn count, and cost of one or all children.",
		Schema: schema(map[string]any{"id": prop("string", "Child id; omit for all")})}
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
	if len(st) == 0 {
		return Result{Output: "no children"}
	}
	return jsonOut(st)
}
