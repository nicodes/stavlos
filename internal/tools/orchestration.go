package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	return model.ToolDef{Name: "spawn", Description: "Create a child agent that works on a task in the background. Returns its id immediately. When it finishes you are woken with its result as a new message (never mid-turn: a result that arrives while you are working is delivered when your current turn ends). Call monitor to end your turn and wait for it; call unmonitor if you would rather check on it yourself with result or status.",
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
	return model.ToolDef{Name: "monitor", Description: "End your turn and wait to be woken: put anything else you need in the same batch, then when a listed child finishes you get its result as a new message (several finishing together wake you once). Children wake you by default, so this is mainly how you wait; it also re-arms any child you had unmonitored. Omit ids for all live children. There is no blocking wait.",
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
		return Result{Output: "nothing to monitor: no live children and no unread results"}
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return Result{Output: "wake armed; your turn ends after this batch and you will be woken with results from:\n" + string(b)}
}

// --- unmonitor ---

type unmonitorTool struct{}

func (unmonitorTool) Def() model.ToolDef {
	return model.ToolDef{Name: "unmonitor", Description: "Stop the given children or monitors (all if omitted) from waking you. Children keep running and their results stay in your mailbox for result/status or your next turn. With stop=true a general monitor is ended instead: a background command is killed, a watch or timer cancelled.",
		Schema: schema(map[string]any{
			"ids":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Child or monitor ids (default: all)"},
			"stop": prop("boolean", "Also stop the monitors themselves (kill commands, cancel watches/timers)"),
		})}
}
func (unmonitorTool) PolicyArg(in json.RawMessage) string { return "" }
func (unmonitorTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct {
		IDs  []string `json:"ids"`
		Stop bool     `json:"stop"`
	}
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	var out []string
	if a.Stop && env.Mon != nil {
		ids := a.IDs
		if len(ids) == 0 {
			for _, m := range env.Mon.List() {
				ids = append(ids, m.ID)
			}
		}
		for _, id := range ids {
			if env.Mon.Has(id) {
				if err := env.Mon.Stop(id); err == nil {
					out = append(out, "stopped "+id)
				}
			}
		}
	}
	if env.Orch != nil {
		ids, err := env.Orch.Unmonitor(env.Agent, a.IDs)
		if err != nil {
			return errf("%v", err)
		}
		if len(ids) > 0 {
			out = append(out, "wake disarmed for: "+strings.Join(ids, ", "))
		}
	}
	if len(out) == 0 {
		return Result{Output: "nothing was armed or running for those ids"}
	}
	return Result{Output: strings.Join(out, "\n")}
}

// --- general monitors: watch / timer / monitors ---

func needMon(env *Env) *Result {
	if env.Mon == nil {
		r := errf("monitors are not available to this agent")
		return &r
	}
	return nil
}

type watchTool struct{}

func (watchTool) Def() model.ToolDef {
	return model.ToolDef{Name: "watch", Description: "Watch a file or directory and be woken once when something under it changes (added, modified, removed). Returns the monitor id immediately. Useful for waiting on another process or a person to change files. One-shot: call again to keep watching.",
		Schema: schema(map[string]any{
			"path": prop("string", "File or directory, absolute or relative to the working directory"),
			"glob": prop("string", "Only files matching this glob (relative to path), e.g. **/*.go"),
		}, "path")}
}
func (watchTool) PolicyArg(in json.RawMessage) string { return pathArg(in) }
func (watchTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needMon(env); r != nil {
		return *r
	}
	var a struct{ Path, Glob string }
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	id, err := env.Mon.StartWatch(a.Path, a.Glob)
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: fmt.Sprintf("watching as monitor %s; you will be woken on the first change", id)}
}

type timerTool struct{}

func (timerTool) Def() model.ToolDef {
	return model.ToolDef{Name: "timer", Description: "Be woken after a delay, with a note to yourself. Returns the monitor id immediately. Use it to re-check something later instead of polling.",
		Schema: schema(map[string]any{
			"seconds": prop("number", "Delay in seconds (max 86400)"),
			"note":    prop("string", "What to do when it fires"),
		}, "seconds")}
}
func (timerTool) PolicyArg(in json.RawMessage) string { return "" }
func (timerTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needMon(env); r != nil {
		return *r
	}
	var a struct {
		Seconds float64 `json:"seconds"`
		Note    string  `json:"note"`
	}
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if a.Seconds <= 0 || a.Seconds > 86400 {
		return errf("seconds must be between 1 and 86400")
	}
	id, err := env.Mon.StartTimer(time.Duration(a.Seconds*float64(time.Second)), a.Note)
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: fmt.Sprintf("timer set as monitor %s", id)}
}

type monitorsTool struct{}

func (monitorsTool) Def() model.ToolDef {
	return model.ToolDef{Name: "monitors", Description: "List your running monitors (background commands, watches, timers) with their progress. Children are listed by status, not here.",
		Schema: schema(map[string]any{})}
}
func (monitorsTool) PolicyArg(in json.RawMessage) string { return "" }
func (monitorsTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needMon(env); r != nil {
		return *r
	}
	ms := env.Mon.List()
	if len(ms) == 0 {
		return Result{Output: "no monitors running"}
	}
	return jsonOut(ms)
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
