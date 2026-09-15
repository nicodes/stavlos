package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
)

type skillTool struct{}

func (skillTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.Skill, Description: "Load the full instructions of a skill by name. Skill descriptions are listed in your system prompt; load one when its description matches the task.",
		Schema: schemaOf(skillInput{})}
}

type skillInput struct {
	Name string `json:"name" desc:"Skill name" req:"true"`
}

func (skillTool) Subject(in json.RawMessage) policy.Subject {
	var a skillInput
	_ = decode(in, &a)
	return policy.Text(a.Name)
}
func (skillTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a skillInput
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
		Schema: schemaOf(responseInput{})}
}

type responseInput struct {
	To   string `json:"to" desc:"The asking agent's name or id (the message you are answering names it)" req:"true"`
	Text string `json:"text" desc:"Your answer: what you did or found, with exact paths and results" req:"true"`
}

func (responseTool) Subject(in json.RawMessage) policy.Subject { return policy.ID(idArg(in)) }
func (responseTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a responseInput
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
