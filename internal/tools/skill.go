package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
)

type skillTool struct{}

func (skillTool) Def() model.ToolDef {
	return model.ToolDef{Name: "skill", Description: "Load the full instructions of a skill by name. Skill descriptions are listed in your system prompt; load one when its description matches the task.",
		Schema: schema(map[string]any{"name": prop("string", "Skill name")}, "name")}
}
func (skillTool) PolicyArg(in json.RawMessage) string {
	var a struct{ Name string }
	_ = decode(in, &a)
	return a.Name
}
func (skillTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct{ Name string }
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	s, ok := env.Skills[a.Name]
	if !ok {
		names := make([]string, 0, len(env.Skills))
		for n := range env.Skills {
			names = append(names, n)
		}
		sort.Strings(names)
		return errf("unknown skill %q; available: %s", a.Name, strings.Join(names, ", "))
	}
	return Result{Output: fmt.Sprintf("# Skill: %s\nDirectory: %s\n\n%s", s.Name, s.Dir, s.Body)}
}

type finishTool struct{}

func (finishTool) Def() model.ToolDef {
	return model.ToolDef{Name: "agent_finish", Description: "Declare your task complete and report the result to your parent. Call this exactly once, when the work is done or cannot be done.",
		Schema: schema(map[string]any{
			"summary": prop("string", "What you did and found; include exact paths and results"),
			"status":  map[string]any{"type": "string", "enum": []string{"success", "failure", "partial"}, "description": "Outcome"},
			"artifacts": map[string]any{"type": "array", "description": "Files produced or changed",
				"items": map[string]any{"type": "object", "properties": map[string]any{
					"path": prop("string", "Path"), "description": prop("string", "What it is")}, "required": []string{"path"}}},
		}, "summary", "status")}
}
func (finishTool) PolicyArg(json.RawMessage) string { return "" }
func (finishTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct {
		Summary   string     `json:"summary"`
		Status    string     `json:"status"`
		Artifacts []Artifact `json:"artifacts"`
	}
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if env.Orch == nil {
		return errf("agent_finish is not available to this agent")
	}
	if a.Status == "" {
		a.Status = "success"
	}
	if err := env.Orch.Finish(env.Agent, a.Summary, a.Status, a.Artifacts); err != nil {
		return errf("%v", err)
	}
	return Result{Output: "finished; result delivered to parent"}
}
