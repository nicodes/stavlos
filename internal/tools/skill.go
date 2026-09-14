package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/toolname"
)

type skillTool struct{}

func (skillTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.Skill, Description: "Load the full instructions of a skill by name. Skill descriptions are listed in your system prompt; load one when its description matches the task.",
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

type responseTool struct{}

func (responseTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.AgentResponse, Description: "Answer an agent that prompted you. The text lands in that agent's mailbox and wakes it between turns; you stay alive and can be prompted again. Use it once per asker; answer the human in your normal reply instead.",
		Schema: schema(map[string]any{
			"to":   prop("string", "The asking agent's id (the message you are answering names it)"),
			"text": prop("string", "Your answer: what you did or found, with exact paths and results"),
		}, "to", "text")}
}
func (responseTool) PolicyArg(in json.RawMessage) string { return idArg(in) }
func (responseTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a struct{ To, Text string }
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if env.Orch == nil {
		return errf("agent_response is not available to this agent")
	}
	if err := env.Orch.Respond(env.Agent, a.To, a.Text); err != nil {
		return errf("%v", err)
	}
	return Result{Output: "response delivered to " + a.To}
}
