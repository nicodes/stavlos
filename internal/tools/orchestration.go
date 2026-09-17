package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
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
	return model.ToolDef{Name: toolname.AgentCreate, Description: "Create a child agent and give it a task. Returns its id immediately. The task is the child's first prompt; its answer (a message to you) wakes you between turns, never mid-turn. If you have nothing else to do until then, end your turn. The child stays alive in the channel: message it again for follow-ups (it keeps its context). There is nothing to clean up.",
		Schema: schemaOf(spawnInput{})}
}

type spawnInput struct {
	Archetype string `json:"archetype" desc:"Preset name of the child (see the list in your instructions)" req:"true"`
	Label     string `json:"label" desc:"Short name for this child, e.g. 'auth-explorer': lowercase letters, digits, '-' and '_'. A name already taken in the channel gets a suffix (auth-explorer-2); the result says the name it got" req:"true"`
	Task      string `json:"task" desc:"The complete task description; the child has no other context" req:"true"`
	Model     string `json:"model" desc:"Optional provider/model-id override for this child"`
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
	id, name, err := env.Orch.Spawn(ctx, env.Agent, a.Archetype, a.Label, a.Task, a.Model)
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: fmt.Sprintf("created %s (%s), id %s; address it by its name", name, a.Archetype, id)}
}

// --- message / cancel / status ---

// User is the recipient that stands for the human.
const User = toolname.User

// Recipient normalises a message's to: the human is always "user" (also
// spelt "human", "@user"); anything else is an agent name or id with a
// leading @ dropped.
func Recipient(to string) string {
	return protocol.Recipient(to)
}

// Message kinds.
const (
	KindRequest  = "request"  // asks for something: the recipient owes a reply, the sender waits (the default)
	KindResponse = "response" // answers a request: settles it and wakes the agent waiting on it
	KindInfo     = "info"     // needs no reply: nobody owes or waits, and an idle recipient is not woken
)

type messageTool struct{}

func (messageTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.Message, Description: "Send text to one or more recipients in this channel using the to array. Every recipient sees the full recipient list. All targets and reply references are validated before delivery. request (the default) creates an independent request ID for the agents addressed; each owes its own explicit response. response requires reply_to containing the pending request IDs it answers, and to must contain their senders. One response may answer several requests, including repeated requests from one sender. Only those IDs are cleared; sending another request or info never clears a debt. info needs no reply and does not wake idle agents. Human-facing messages without explicit response references are updates; use ask_user for questions to the human. Human prompts and steers carry request IDs too. agent_status exposes pending_replies and awaiting_replies with IDs and excerpts.",
		Schema: schemaOf(messageInput{})}
}

type messageInput struct {
	ReplyTo []string            `json:"reply_to" desc:"Request IDs explicitly answered by this response; required for kind response, omitted for request/info" min:"1"`
	To      protocol.Recipients `json:"to" desc:"Recipient names or ids, including user for the human; everyone sees the full recipient list" req:"true" min:"1"`
	Text    string              `json:"text" desc:"The shared message body; include exact paths and results. Recipient context is attached separately" req:"true"`
	Kind    string              `json:"kind" desc:"request (the default): you want something and wait for it; response: this answers a request you received; info: no reply needed"`
}

func (messageTool) Subject(in json.RawMessage) policy.Subject {
	var a messageInput
	_ = decode(in, &a)
	return policy.Subject{Kind: policy.KindID, Values: a.To.Normalized()}
}
func (messageTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	var a messageInput
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if strings.TrimSpace(a.Text) == "" {
		return errf("text is required")
	}
	kind := strings.ToLower(strings.TrimSpace(a.Kind))
	switch kind {
	case "":
		kind = KindRequest
	case KindRequest, KindResponse, KindInfo:
	default:
		return errf("kind %q: use request, response or info", a.Kind)
	}
	if kind == KindResponse && len(a.ReplyTo) == 0 {
		return errf("responses require reply_to request IDs; use info for updates that answer no request")
	}
	if kind != KindResponse && len(a.ReplyTo) > 0 {
		return errf("reply_to is only valid for kind response")
	}
	recipients := a.To.Normalized()
	if len(recipients) == 0 {
		return errf("at least one recipient is required")
	}
	for _, to := range recipients {
		if to == "" {
			return errf("recipient must not be empty")
		}
	}
	out, err := env.Orch.Message(env.Agent, recipients, a.Text, kind, a.ReplyTo...)
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: out}
}

type cancelTool struct{}

func (cancelTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.AgentCancel, Description: "End a child's current turn immediately. The child survives and can be sent new prompts.",
		Schema: schemaOf(cancelInput{})}
}

type cancelInput struct {
	ID string `json:"id" desc:"Child agent: its name or id" req:"true"`
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
	return model.ToolDef{Name: toolname.AgentStatus, Description: "State, turn count, and cost of one agent, or of every agent in the channel (the whole tree, parents before children; your own row is marked).",
		Schema: schemaOf(statusInput{})}
}

type statusInput struct {
	ID string `json:"id" desc:"Agent name or id; omit for the whole channel"`
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
