package daemon

import (
	"context"
	"encoding/json"
	"os"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/web"
)

type webModelCall struct {
	Sequence int64 `json:"seq"`
	Active   bool  `json:"active"`
}

// webFacts exports observations from the independent agent runtime. Rust
// applies browser scopes, filters, aggregation, replay and recipient routing.
// This feed accepts no RPC methods and exposes no credential or owner command.
func (d *Daemon) webFacts() any {
	channels := make([]any, 0)
	for _, channel := range d.channelList() {
		channels = append(channels, map[string]any{"info": channel.Info(), "agents": channel.Tree()})
	}
	subscriptions := make([]any, 0)
	for provider, usage := range d.Registry.PlanUsage() {
		connected := false
		if status, ok := d.Registry.Status(provider); ok {
			connected = status.Connected
		}
		subscriptions = append(subscriptions, map[string]any{"id": provider, "name": registry.SubscriptionName(provider), "connected": connected, "usage": usage})
	}
	epochs := map[string]webModelCall{}
	d.webModelEpoch.Range(func(key, value any) bool { epochs[key.(string)] = value.(webModelCall); return true })
	return map[string]any{"pid": os.Getpid(), "build": buildid.ID(), "data_dir": d.DataDir, "providers": d.Registry.Providers(), "channels": channels, "prompts": d.esc.Pending(""), "subscriptions": subscriptions, "model_epochs": epochs}
}
func (d *Daemon) webDeliver(ctx context.Context, input web.Delivery) error {
	channel, err := d.channel(input.Channel)
	if err != nil {
		return err
	}
	plan := agent.PreparedPost{ID: input.ID, RequestID: input.RequestID, Text: input.Text, Kind: input.MessageKind, Targets: input.Targets, Names: input.Names}
	if board := channel.Board(); board != nil {
		home, err := d.channel(board.Source)
		if err != nil {
			return err
		}
		return home.AcceptPreparedBoard(ctx, channel, plan)
	}
	return channel.AcceptPreparedPost(ctx, plan)
}
func (d *Daemon) notifyWeb(method string, payload json.RawMessage) {
	if method == "stream" {
		var stream map[string]any
		if json.Unmarshal(payload, &stream) == nil {
			if id, ok := stream["agent"].(string); ok {
				if epoch, ok := d.webModelEpoch.Load(id); ok {
					stream["_epoch"] = epoch.(webModelCall).Sequence
					payload, _ = json.Marshal(stream)
				}
			}
		}
	}
	front := d.webFeed.Load()
	if front != nil {
		front.Notify(map[string]any{"jsonrpc": "2.0", "method": method, "params": payload})
	}
}
