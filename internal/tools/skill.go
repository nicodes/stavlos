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
