package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
)

// BoardService exposes shared channels without moving or duplicating agents.
type BoardService interface {
	Create(context.Context, string, string, []string) (string, error)
	List(context.Context, string) (string, error)
	Read(context.Context, string, string, int64, int) (string, error)
	Message(context.Context, string, string, []string, string, string, []string) (string, error)
}
type channelTool struct{}
type channelInput struct {
	Action  string   `json:"action" req:"true" enum:"create,list,read" desc:"Create a shared channel, list your shared channels, or read their public history"`
	Name    string   `json:"name" desc:"Required for create: unique channel name"`
	Members []string `json:"members" desc:"For create: existing child agent names/ids; self is included automatically"`
	Channel string   `json:"channel" desc:"Required for read: channel name or id"`
	From    int64    `json:"from" desc:"For read: first event sequence (default 1); returned next continues the history"`
	Limit   int      `json:"limit" desc:"For read: maximum events (default 100, max 500)"`
}

func (channelTool) Def() model.ToolDef { return ChannelDef(true) }
func ChannelDef(delegate bool) model.ToolDef {
	schema := objectSchema(reflect.TypeOf(channelInput{}))
	if !delegate {
		schema["properties"].(map[string]any)["action"].(map[string]any)["enum"] = []string{"list", "read"}
	}
	raw, _ := json.Marshal(schema)
	return model.ToolDef{Name: toolname.Channel, Description: "Create a shared board for yourself and existing descendants, list your boards, or read their durable history. Boards appear in the UI and are mirrored by the configured Discord bridge. Agents keep their own histories and parent relationships. Post with message(channel: name or id, text: ...); address members with to when delivery or a response is needed.", Schema: raw}
}
func (channelTool) Subject(in json.RawMessage) policy.Subject {
	var a channelInput
	_ = decode(in, &a)
	target := a.Channel
	if a.Action == "create" {
		target = a.Name
	}
	return policy.Text(a.Action + " " + target)
}
func (channelTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	var a channelInput
	if err := decode(in, &a); err != nil {
		return errf("bad input: %v", err)
	}
	if env.Boards == nil {
		return errf("shared channels are not available")
	}
	var out string
	var err error
	switch a.Action {
	case "create":
		if strings.TrimSpace(a.Name) == "" {
			return errf("create requires name")
		}
		out, err = env.Boards.Create(ctx, env.Agent, a.Name, a.Members)
	case "list":
		out, err = env.Boards.List(ctx, env.Agent)
	case "read":
		if strings.TrimSpace(a.Channel) == "" {
			return errf("read requires channel")
		}
		if a.Limit <= 0 {
			a.Limit = 100
		}
		if a.Limit > 500 {
			a.Limit = 500
		}
		if a.From <= 0 {
			a.From = 1
		}
		out, err = env.Boards.Read(ctx, env.Agent, a.Channel, a.From, a.Limit)
	default:
		err = fmt.Errorf("action must be create, list or read")
	}
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: out}
}
