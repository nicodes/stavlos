package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
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
	return model.ToolDef{Name: toolname.AgentCreate, Description: "Create a child agent and give it a task. Returns its id immediately. The task is the child's first prompt; its agent_response comes back to you as a new message between turns, never mid-turn. If you have nothing else to do until then, end your turn. The child stays alive for the rest of the session: agent_message it again for follow-ups (it keeps its context). There is nothing to clean up.",
		Schema: schemaOf(spawnInput{})}
}

type spawnInput struct {
	Archetype string   `json:"archetype" desc:"Preset name of the child (see the list in your instructions)" req:"true"`
	Label     string   `json:"label" desc:"Short human-facing name for this child, e.g. 'auth-explorer' (required)" req:"true"`
	Task      string   `json:"task" desc:"The complete task description; the child has no other context" req:"true"`
	Model     string   `json:"model" desc:"Optional provider/model-id override for this child"`
	Dirs      []string `json:"dirs" desc:"Optional directories to grant the child on top of the session directory and its role's own; each must be inside one of yours (agent_status lists them)"`
}

func (spawnTool) Subject(in json.RawMessage) policy.Subject {
	var a spawnInput
	_ = decode(in, &a)
	return policy.Text(a.Archetype)
}
func (spawnTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	var a spawnInput
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
	return model.ToolDef{Name: toolname.AgentMessage, Description: "Send a message to any other agent in this session (a child, a sibling, or your parent). It reaches the agent at its next step: mid-turn if it is busy, as a new turn if it is idle. The recipient sees it as coming from you and answers with agent_response, which wakes you between turns. agent_status lists every agent and its id.",
		Schema: schemaOf(messageInput{})}
}

type messageInput struct {
	ID   string `json:"id" desc:"Target agent id (any agent in the session)" req:"true"`
	Text string `json:"text" desc:"Message" req:"true"`
}

func (messageTool) Subject(in json.RawMessage) policy.Subject { return policy.ID(idArg(in)) }
func (messageTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	var a messageInput
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
	return model.ToolDef{Name: toolname.AgentCancel, Description: "End a child's current turn immediately. The child survives and can be sent new prompts.",
		Schema: schemaOf(cancelInput{})}
}

type cancelInput struct {
	ID string `json:"id" desc:"Child agent id" req:"true"`
}

func (cancelTool) Subject(in json.RawMessage) policy.Subject { return policy.ID(idArg(in)) }
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
	return model.ToolDef{Name: toolname.AgentStatus, Description: "State, turn count, and cost of one agent, or of every agent in the session (the whole tree, parents before children; your own row is marked).",
		Schema: schemaOf(statusInput{})}
}

type statusInput struct {
	ID string `json:"id" desc:"Agent id; omit for the whole session"`
}

func (statusTool) Subject(in json.RawMessage) policy.Subject { return policy.ID(idArg(in)) }
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
