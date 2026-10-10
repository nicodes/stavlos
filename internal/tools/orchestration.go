package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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

// --- spawn ---

type spawnTool struct{}

type spawnInput struct {
	Directories []string `json:"directories,omitempty" desc:"Task directories already granted to you; defaults to your first directory. Children cannot widen this scope."`
	Archetype   string   `json:"archetype" desc:"Role name of the child (see the list in your instructions)" req:"true"`
	Label       string   `json:"label" desc:"Short name for this child, e.g. 'auth-explorer': lowercase letters, digits, '-' and '_'. A name already taken in the channel gets a suffix (auth-explorer-2); the result says the name it got" req:"true"`
	Task        string   `json:"task" desc:"The complete task description; the child has no other context" req:"true"`
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
	var id, name string
	var err error
	if scoped, ok := env.Orch.(interface {
		SpawnScoped(context.Context, string, string, string, string, []string) (string, string, error)
	}); ok {
		id, name, err = scoped.SpawnScoped(ctx, env.Agent, a.Archetype, a.Label, a.Task, a.Directories)
	} else if len(a.Directories) > 0 {
		return errf("this runtime does not support task directory scopes")
	} else {
		id, name, err = env.Orch.Spawn(ctx, env.Agent, a.Archetype, a.Label, a.Task)
	}
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
	KindRequest         = "request"  // asks for something: the recipient owes a reply, the sender waits (the default)
	KindResponse        = "response" // answers a request: settles it and wakes the agent waiting on it
	KindInfo            = "info"     // needs no reply: nobody owes or waits, and an idle recipient is not woken
	KindSteer           = "steer"
	KindResponseRequest = "response_request" // settles reply_to and opens a new request
	KindNoReply         = "no_reply"         // parse alias for KindInfo; stored events stay "info"
)

type messageTool struct{}

func (messageTool) Def() model.ToolDef {
	schema := objectSchema(reflect.TypeOf(messageInput{}))
	delete(schema["properties"].(map[string]any), "kind") // decode old calls, advertise the new contract
	raw, _ := json.Marshal(schema)
	return model.ToolDef{Name: toolname.Message, Description: "Send a private message to agents/user, or post publicly in a shared channel. Addressed messages default to expect_response true: each recipient owes a reply. Set false to steer agents without requiring an answer. Unaddressed channel posts and answers with reply_to default false. reply_to settles specific request IDs; public answers must use their original channel. Ask the human questions with ask.", Schema: raw}
}

type messageInput struct {
	Channel        string              `json:"channel" desc:"Shared channel name or id; omit for private delivery"`
	To             protocol.Recipients `json:"to" desc:"Recipient names/ids, or user. Omit for an unaddressed channel post" min:"1"`
	Text           string              `json:"text" req:"true" desc:"Message body"`
	ExpectResponse *bool               `json:"expect_response" desc:"Addressed messages default true; false steers recipients without a reply obligation. Unaddressed posts and answers default false"`
	ReplyTo        []string            `json:"reply_to" desc:"Request IDs this message answers; their senders must be recipients. Include the original channel for public requests" min:"1"`
	Kind           string              `json:"kind"` // legacy input only
}

func (messageTool) Subject(in json.RawMessage) policy.Subject {
	var a messageInput
	_ = decode(in, &a)
	values := a.To.Normalized()
	if a.Channel != "" {
		values = append(values, "#"+a.Channel)
	}
	return policy.Subject{Kind: policy.KindID, Values: values}
}
func (messageTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a messageInput
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if strings.TrimSpace(a.Text) == "" {
		return errf("text is required")
	}
	recipients := a.To.Normalized()
	kind, err := messageKind(a, recipients)
	if err != nil {
		return errf("%v", err)
	}
	if len(recipients) == 0 && a.Channel == "" {
		return errf("at least one recipient is required")
	}
	for _, to := range recipients {
		if to == "" {
			return errf("recipient must not be empty")
		}
	}
	var out string
	if a.Channel != "" {
		if env.Boards == nil {
			return errf("shared channels are not available")
		}
		out, err = env.Boards.Message(ctx, env.Agent, a.Channel, recipients, a.Text, kind, a.ReplyTo)
	} else {
		if r := needOrch(env); r != nil {
			return *r
		}
		out, err = env.Orch.Message(env.Agent, recipients, a.Text, kind, a.ReplyTo...)
	}
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: out}
}
func messageKind(a messageInput, recipients []string) (string, error) {
	if a.Kind != "" {
		if a.ExpectResponse != nil {
			return "", fmt.Errorf("use expect_response instead of kind, not both")
		}
		kind := strings.ToLower(strings.TrimSpace(a.Kind))
		if kind == KindNoReply {
			kind = KindInfo
		}
		switch kind {
		case KindRequest, KindResponse, KindInfo:
		default:
			return "", fmt.Errorf("kind %q: use request, response, info or no_reply", a.Kind)
		}
		if kind == KindResponse && len(a.ReplyTo) == 0 {
			return "", fmt.Errorf("responses require reply_to request IDs")
		}
		if kind != KindResponse && len(a.ReplyTo) > 0 {
			return "", fmt.Errorf("reply_to is only valid for kind response")
		}
		return kind, nil
	}
	expect := len(recipients) > 0 && !(len(recipients) == 1 && recipients[0] == User) && len(a.ReplyTo) == 0
	if a.ExpectResponse != nil {
		expect = *a.ExpectResponse
	}
	if expect && len(recipients) == 0 {
		return "", fmt.Errorf("expect_response requires explicit recipients")
	}
	if len(a.ReplyTo) > 0 {
		if expect {
			return KindResponseRequest, nil
		}
		return KindResponse, nil
	}
	if expect {
		return KindRequest, nil
	}
	if len(recipients) > 0 {
		return KindSteer, nil
	}
	return KindInfo, nil
}

type cancelTool struct{}

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

func (statusTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if r := needOrch(env); r != nil {
		return *r
	}
	st, err := env.Orch.Status(env.Agent, idArg(in))
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: statusLines(st)}
}

// statusLines is a status as lines, one per agent: what a parent needs to
// decide something, and not the full text of every request, which is what
// message delivered and the log holds. One channel's parents called this
// 3,080 times, 2.4 MB of JSON, mostly to ask whether a child was done yet.
func statusLines(st []ChildStatus) string {
	var b strings.Builder
	for _, s := range st {
		you := ""
		if s.You {
			you = " (you)"
		}
		fmt.Fprintf(&b, "%s%s [%s] %s · turn %d · $%.2f", s.Name, you, s.ID, s.State, s.Turn, s.CostUSD)
		if len(s.PendingReplies) > 0 {
			fmt.Fprintf(&b, " · owes %d reply", len(s.PendingReplies))
			if len(s.PendingReplies) > 1 {
				b.WriteString("s")
			}
			for _, r := range s.PendingReplies {
				fmt.Fprintf(&b, " (%s from %s: %s)", r.ID, r.FromName, excerpt(r.Text, 80))
			}
		}
		if len(s.AwaitingReplies) > 0 {
			var from []string
			for _, r := range s.AwaitingReplies {
				from = append(from, r.FromName)
			}
			fmt.Fprintf(&b, " · awaits %s", strings.Join(from, ", "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// agentTool dispatches lifecycle actions without adding separate model tools.
type agentTool struct{}
type agentInput struct {
	Directories []string `json:"directories,omitempty" desc:"For create: task directories already granted to you; defaults to your first directory"`
	Action      string   `json:"action" req:"true" enum:"create,cancel,status" desc:"create a child, cancel its current turn, or inspect status"`
	Archetype   string   `json:"archetype" desc:"Required for create: role from your delegation instructions"`
	Label       string   `json:"label" desc:"Required for create: short child name"`
	Task        string   `json:"task" desc:"Required for create: complete task; the child has no other context"`
	ID          string   `json:"id" desc:"Required for cancel: child name or id; optional for status (omit for the whole channel)"`
}

func (agentTool) Def() model.ToolDef { return AgentDef(true) }

// AgentDef restricts the advertised actions for roles without delegation.
func AgentDef(delegate bool) model.ToolDef {
	schema := objectSchema(reflect.TypeOf(agentInput{}))
	if !delegate {
		schema["properties"].(map[string]any)["action"].(map[string]any)["enum"] = []string{"status"}
	}
	raw, _ := json.Marshal(schema)
	return model.ToolDef{Name: toolname.Agent, Description: "Manage agents. create returns a child id immediately; its response wakes you between turns. Children keep their context for follow-up messages. cancel ends a child's current turn; it survives. status reports state, turns, cost and reply obligations for one agent or the channel. Do not poll status to wait: end your turn when you have nothing else to do.", Schema: raw}
}
func (agentTool) Subject(in json.RawMessage) policy.Subject {
	if toolname.Operation(toolname.Agent, in) == toolname.AgentCreate {
		return (spawnTool{}).Subject(in)
	}
	return policy.ID(idArg(in))
}
func (agentTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a agentInput
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	switch a.Action {
	case "create":
		if strings.TrimSpace(a.Archetype) == "" || strings.TrimSpace(a.Task) == "" {
			return errf("create requires archetype, label and task")
		}
		return (spawnTool{}).Run(ctx, in, env)
	case "cancel":
		if strings.TrimSpace(a.ID) == "" {
			return errf("cancel requires id")
		}
		if r := needOrch(env); r != nil {
			return *r
		}
		if len(env.Orch.Archetypes(env.Agent)) == 0 {
			return errf("this role cannot cancel agents")
		}
		return (cancelTool{}).Run(ctx, in, env)
	case "status":
		return (statusTool{}).Run(ctx, in, env)
	default:
		return errf("action must be create, cancel or status")
	}
}
