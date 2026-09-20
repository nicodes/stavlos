package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/eventlog"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
)

// The protocol's methods, one route each, keyed by the descriptors the
// client calls (protocol.Method): a handler's params and result are the
// method's, checked at compile time. A handler returns a result or a
// domain error; toProtocolError is the one place that decides the wire
// code.

type handler func(ctx context.Context, c *conn, params json.RawMessage) (any, error)

// scope is who may call a method. It is declared where the method is routed,
// so that a method's reach is read beside what it does: there used to be a
// separate list of what a browser may call, kept in step by hand.
type scope int

const (
	// scopeOwner is the default: a client on the daemon's own socket, which
	// is the user's. Everything that changes what agents may do (modes,
	// directories, trust, permission answers, providers, configuration,
	// shutdown) is the owner's alone.
	scopeOwner scope = iota
	// scopeWeb may also be called by a browser signed in to the web UI: read
	// the channels and their streams, and post to a chat.
	scopeWeb
)

// routeEntry is one method's handler and who may call it.
type routeEntry struct {
	name  string
	h     handler
	scope scope
}

// forWeb opens a method to the web UI.
func forWeb(e routeEntry) routeEntry { e.scope = scopeWeb; return e }

// route binds a handler to its method.
func route[P, R any](m protocol.Method[P, R], fn func(ctx context.Context, c *conn, p P) (R, error)) routeEntry {
	return routeEntry{name: m.Name, scope: scopeOwner, h: func(ctx context.Context, c *conn, params json.RawMessage) (any, error) {
		var p P
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, codedError{protocol.ErrInvalidParams, err}
			}
		}
		return fn(ctx, c, p)
	}}
}

// errNotFound marks a missing channel, agent or prompt; wrap it with %w so
// the message reads "channel "x" not found".
var errNotFound = errors.New("not found")

// codedError carries an explicit wire code.
type codedError struct {
	code int
	err  error
}

func (e codedError) Error() string { return e.err.Error() }
func (e codedError) Unwrap() error { return e.err }

func internal(err error) error { return codedError{protocol.ErrInternal, err} }

// toProtocolError maps a handler's error to its wire form. Unless the error
// says otherwise it is the caller's: invalid params.
func toProtocolError(err error) *protocol.Error {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe
	}
	code := protocol.ErrInvalidParams
	var ce codedError
	switch {
	case errors.As(err, &ce):
		code = ce.code
	case errors.Is(err, errNotFound):
		code = protocol.ErrNotFound
	case errors.Is(err, escalation.ErrClaimed), errors.Is(err, escalation.ErrLate), errors.Is(err, errTrustChanged):
		code = protocol.ErrConflict
	}
	return &protocol.Error{Code: code, Message: err.Error()}
}

// promptErr: a prompt that is claimed or already resolved is a conflict;
// any other failure means the prompt is not there.
func promptErr(err error) error {
	if errors.Is(err, escalation.ErrClaimed) || errors.Is(err, escalation.ErrLate) {
		return err
	}
	return codedError{protocol.ErrNotFound, err}
}

var none = protocol.None{}

var handlers = routes(
	webRoute(protocol.WebStatusMethod), webRoute(protocol.WebEnable), webRoute(protocol.WebDisable), webRoute(protocol.WebOpen),
	route(protocol.DiscordStatusMethod, func(_ context.Context, c *conn, _ protocol.None) (protocol.DiscordStatus, error) {
		if c.d.Discord == nil {
			return protocol.DiscordStatus{State: "disconnected", Error: "Discord service is unavailable in this daemon"}, nil
		}
		return c.d.Discord.Status(), nil
	}),
	route(protocol.DiscordConnect, func(_ context.Context, c *conn, _ protocol.None) (protocol.DiscordStatus, error) {
		if c.d.Discord == nil {
			return protocol.DiscordStatus{}, errors.New("discord service is unavailable in this daemon")
		}
		return c.d.Discord.Connect()
	}),
	route(protocol.DiscordDisconnect, func(_ context.Context, c *conn, _ protocol.None) (protocol.DiscordStatus, error) {
		if c.d.Discord == nil {
			return protocol.DiscordStatus{}, errors.New("discord service is unavailable in this daemon")
		}
		return c.d.Discord.Disconnect()
	}),
	forWeb(route(protocol.DaemonStatus, func(_ context.Context, c *conn, _ protocol.None) (protocol.DaemonStatusResult, error) {
		return c.d.Status(), nil
	})),
	route(protocol.DaemonShutdown, func(_ context.Context, c *conn, _ protocol.None) (protocol.None, error) {
		name, _ := c.cl.identity()
		log.Printf("shutdown requested by client %s (%s)", name, c.cl.id)
		if c.d.Shutdown != nil {
			go c.d.Shutdown()
		}
		return none, nil
	}),
	forWeb(route(protocol.Attach, func(_ context.Context, c *conn, p protocol.AttachParams) (protocol.AttachResult, error) {
		c.cl.mu.Lock()
		defer c.cl.mu.Unlock()
		if p.Tier == protocol.TierFallback {
			c.cl.tier = protocol.TierFallback
		}
		if p.Client != "" {
			c.cl.name = p.Client
		}
		if c.web {
			// Who a browser connection is, is the listener's to say: it could
			// attach as "discord" and have the bridge take its posts for its
			// own and drop them.
			c.cl.name = "web"
		}
		return protocol.AttachResult{ClientID: c.cl.id, Version: protocol.Version}, nil
	})),

	forWeb(route(protocol.ChannelList, func(ctx context.Context, c *conn, p protocol.ChannelListParams) (protocol.ChannelListResult, error) {
		list, err := c.d.ChannelList(ctx, p.Dir, p.IncludeArchived)
		if err != nil {
			return protocol.ChannelListResult{}, internal(err)
		}
		return protocol.ChannelListResult{Channels: list}, nil
	})),
	route(protocol.ChannelCreate, func(ctx context.Context, c *conn, p protocol.ChannelCreateParams) (protocol.ChannelInfo, error) {
		s, err := c.d.CreateChannel(ctx, p.Dir, p.Model, p.RootAgent, p.Name)
		if err != nil {
			return protocol.ChannelInfo{}, err
		}
		return s.Info(), nil
	}),
	forWeb(route(protocol.ChannelResume, func(_ context.Context, c *conn, p protocol.ChannelRef) (protocol.ChannelInfo, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return protocol.ChannelInfo{}, err
		}
		c.d.maybeTrustPrompt(s)
		return s.Info(), nil
	})),
	route(protocol.ChannelArchive, func(ctx context.Context, c *conn, p protocol.ChannelRef) (protocol.None, error) {
		return none, c.d.ArchiveChannel(ctx, p.Channel)
	}),
	route(protocol.ChannelRename, func(ctx context.Context, c *conn, p protocol.ChannelRenameParams) (protocol.None, error) {
		return none, c.d.RenameChannel(ctx, p.Channel, p.Name)
	}),
	route(protocol.ChannelSetModel, func(ctx context.Context, c *conn, p protocol.ChannelSetModelParams) (protocol.None, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return none, err
		}
		if err := s.SetModel(ctx, p.Model); err != nil {
			return none, err
		}
		c.d.rememberModel(s, p.Model)
		return none, nil
	}),
	forWeb(route(protocol.ChannelPost, func(ctx context.Context, c *conn, p protocol.ChannelPostParams) (protocol.ChannelPostResult, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return protocol.ChannelPostResult{}, err
		}
		name, _ := c.cl.identity()
		to, err := s.Post(ctx, p.Text, "human:"+name)
		return protocol.ChannelPostResult{To: to}, err
	})),
	route(protocol.ChannelSetRecap, func(ctx context.Context, c *conn, p protocol.ChannelSetRecapParams) (protocol.None, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return none, err
		}
		return none, s.SetRecap(ctx, p.Minutes)
	}),
	route(protocol.ChannelSetMode, func(ctx context.Context, c *conn, p protocol.ChannelSetModeParams) (protocol.None, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return none, err
		}
		if err := s.SetMode(ctx, p.Mode); err != nil {
			return none, err
		}
		// Anything already waiting is answered as the new mode would have
		// answered it, so the agents move, and no differently: what the mode
		// would still ask about keeps waiting. Yolo allows every permission
		// prompt but an edit to a file that steers the harness; auto allows
		// the ones inside the channel's directories that send nothing off the
		// machine, and denies the ones outside.
		for answer, verb := range map[string]policy.Verb{protocol.AnswerAllow: policy.Allow, protocol.AnswerDeny: policy.Deny} {
			c.d.esc.AnswerWhere(s.ID, protocol.PromptPermission, answer, p.Mode, func(pi protocol.PromptInfo) bool {
				return agent.ModeVerdict(p.Mode, pi.Sticky, pi.Egress, pi.Dir != "") == verb
			})
		}
		return none, nil
	}),
	route(protocol.ChannelAddDir, func(ctx context.Context, c *conn, p protocol.ChannelDirParams) (protocol.None, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return none, err
		}
		return none, s.AddDir(ctx, p.Dir)
	}),
	route(protocol.ChannelSetDir, func(ctx context.Context, c *conn, p protocol.ChannelDirParams) (protocol.ChannelInfo, error) {
		return c.d.SetChannelDir(ctx, p.Channel, p.Dir)
	}),
	route(protocol.ChannelRemoveDir, func(ctx context.Context, c *conn, p protocol.ChannelDirParams) (protocol.None, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return none, err
		}
		return none, s.RemoveDir(ctx, p.Dir)
	}),

	forWeb(route(protocol.SheetList, func(_ context.Context, c *conn, p protocol.ChannelRef) (protocol.SheetListResult, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return protocol.SheetListResult{}, err
		}
		return protocol.SheetListResult{Sheets: s.Sheets()}, nil
	})),
	forWeb(route(protocol.AgentTree, func(_ context.Context, c *conn, p protocol.AgentTreeParams) (protocol.AgentTreeResult, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return protocol.AgentTreeResult{}, err
		}
		return protocol.AgentTreeResult{Agents: s.Tree()}, nil
	})),
	route(protocol.AgentSend, func(ctx context.Context, c *conn, p protocol.AgentSendParams) (protocol.None, error) {
		s, _, err := c.d.agentChannel(p.Agent)
		if err != nil {
			return none, err
		}
		name, _ := c.cl.identity()
		src := "human:" + name
		switch p.Kind {
		case protocol.KindPrompt:
			return none, s.Send(ctx, p.Agent, p.Text, src)
		case protocol.KindSteer:
			return none, s.Steer(ctx, p.Agent, p.Text, src)
		case protocol.KindCancel:
			return none, s.Cancel(p.Agent)
		case protocol.KindKill:
			return none, s.Kill(p.Agent)
		}
		return none, fmt.Errorf("unknown envelope kind %q", p.Kind)
	}),
	route(protocol.AgentSpawn, func(ctx context.Context, c *conn, p protocol.AgentSpawnParams) (protocol.AgentSpawnResult, error) {
		s, _, err := c.d.agentChannel(p.Parent)
		if err != nil {
			return protocol.AgentSpawnResult{}, err
		}
		id, err := s.SpawnFromClient(ctx, p.Parent, p.Role, p.Name, p.Task, p.Model)
		return protocol.AgentSpawnResult{ID: id}, err
	}),
	route(protocol.AgentSetModel, func(ctx context.Context, c *conn, p protocol.AgentSetModelParams) (protocol.None, error) {
		s, a, err := c.d.agentChannel(p.Agent)
		if err != nil {
			return none, err
		}
		if err := a.SetModel(ctx, p.Model); err != nil {
			return none, err
		}
		if a.Parent == "" && s.Model() == "" {
			// The main agent picked a model in a channel that had none: adopt it.
			_ = s.SetModel(ctx, p.Model)
			c.d.rememberModel(s, p.Model)
		}
		return none, nil
	}),
	route(protocol.AgentSetRole, func(ctx context.Context, c *conn, p protocol.AgentSetRoleParams) (protocol.None, error) {
		_, a, err := c.d.agentChannel(p.Agent)
		if err != nil {
			return none, err
		}
		return none, a.SetRole(ctx, p.Role)
	}),
	route(protocol.AgentSetVariant, func(ctx context.Context, c *conn, p protocol.AgentSetVariantParams) (protocol.None, error) {
		_, a, err := c.d.agentChannel(p.Agent)
		if err != nil {
			return none, err
		}
		return none, a.SetVariant(ctx, p.Variant)
	}),
	route(protocol.AgentCompact, func(ctx context.Context, c *conn, p protocol.AgentCompactParams) (protocol.AgentCompactResult, error) {
		_, a, err := c.d.agentChannel(p.Agent)
		if err != nil {
			return protocol.AgentCompactResult{}, err
		}
		status, err := a.Compact(ctx)
		return protocol.AgentCompactResult{Status: status}, err
	}),
	route(protocol.Variants, func(_ context.Context, c *conn, p protocol.VariantsParams) (protocol.VariantsResult, error) {
		return protocol.VariantsResult{Variants: c.d.Registry.Variants(p.Model)}, nil
	}),

	forWeb(route(protocol.PromptList, func(_ context.Context, c *conn, p protocol.PromptListParams) (protocol.PromptListResult, error) {
		return protocol.PromptListResult{Prompts: c.d.esc.Pending(p.Channel)}, nil
	})),
	route(protocol.PromptClaim, func(_ context.Context, c *conn, p protocol.PromptClaimParams) (protocol.None, error) {
		if err := c.d.esc.Claim(p.ID, c.cl.id); err != nil {
			return none, promptErr(err)
		}
		return none, nil
	}),
	route(protocol.PromptReply, func(_ context.Context, c *conn, p protocol.PromptReplyParams) (protocol.None, error) {
		if err := c.d.esc.Reply(p.ID, c.cl.id, escalation.Answer{Value: p.Answer, Dir: p.Dir, Reason: p.Reason, Answers: p.Answers, Details: p.Details}); err != nil {
			return none, promptErr(err)
		}
		return none, nil
	}),

	route(protocol.TrustStatus, func(_ context.Context, c *conn, p protocol.TrustStatusParams) (protocol.TrustStatusResult, error) {
		r, err := c.d.TrustStatus(p.Dir)
		if err != nil {
			return r, internal(err)
		}
		return r, nil
	}),
	route(protocol.TrustReply, func(ctx context.Context, c *conn, p protocol.TrustReplyParams) (protocol.None, error) {
		return none, c.d.Trust(ctx, p.Dir, p.Hash, p.Trust)
	}),

	route(protocol.ProviderList, func(_ context.Context, c *conn, _ protocol.None) (protocol.ProviderListResult, error) {
		return c.d.ProviderList(), nil
	}),
	route(protocol.ProviderLoginStart, func(ctx context.Context, c *conn, p protocol.LoginStartParams) (protocol.LoginStartResult, error) {
		return c.d.LoginStart(ctx, p.Provider, p.Method)
	}),
	route(protocol.ProviderLoginWait, func(ctx context.Context, c *conn, p protocol.LoginWaitParams) (protocol.ProviderInfo, error) {
		st, err := c.d.LoginWait(ctx, p.ID)
		if err != nil {
			return protocol.ProviderInfo{}, err
		}
		return providerInfo(st), nil
	}),
	route(protocol.ProviderLoginKey, func(_ context.Context, c *conn, p protocol.LoginKeyParams) (protocol.None, error) {
		return none, c.d.LoginKey(p.ID, p.Key)
	}),
	route(protocol.ProviderDisconnect, func(_ context.Context, c *conn, p protocol.ProviderRef) (protocol.None, error) {
		return none, c.d.Registry.Disconnect(p.Provider)
	}),
	route(protocol.ModelList, func(_ context.Context, c *conn, p protocol.ModelListParams) (protocol.ModelListResult, error) {
		var out []protocol.ModelInfo
		for _, m := range c.d.Registry.Models(p.Provider, p.All) {
			out = append(out, protocol.ModelInfo{ID: m.ID, Provider: m.Provider, Name: m.Name, Context: m.Info.ContextWindow, InputPrice: m.Info.InputPrice, OutputPrice: m.Info.OutputPrice})
		}
		return protocol.ModelListResult{Models: out}, nil
	}),

	forWeb(route(protocol.Subscribe, func(ctx context.Context, c *conn, p protocol.SubscribeParams) (protocol.SubscribeResult, error) {
		if _, err := c.d.channel(p.Channel); err != nil {
			return protocol.SubscribeResult{}, err
		}
		from, first := p.From, int64(0)
		if p.Tail > 0 && from <= 1 {
			head, err := c.d.Log.LastSeq(ctx, p.Channel)
			if err != nil {
				return protocol.SubscribeResult{}, internal(err)
			}
			if start := head - int64(p.Tail) + 1; start > 1 {
				from, first = start, start
			}
		}
		last, err := c.d.subscribe(ctx, c.cl, p.Channel, from)
		if err != nil {
			return protocol.SubscribeResult{}, internal(err)
		}
		return protocol.SubscribeResult{Seq: last, First: first}, nil
	})),
	forWeb(route(protocol.Unsubscribe, func(_ context.Context, c *conn, p protocol.SubscribeParams) (protocol.None, error) {
		c.cl.mu.Lock()
		delete(c.cl.subs, p.Channel)
		c.cl.mu.Unlock()
		return none, nil
	})),
	forWeb(route(protocol.Reconcile, func(ctx context.Context, c *conn, p protocol.ChannelRef) (protocol.ReconcileResult, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return protocol.ReconcileResult{}, err
		}
		seq, _ := c.d.Log.LastSeq(ctx, p.Channel)
		info := s.Info()
		info.Seq = seq
		// Every channel's prompts: the permission and questions tabs span channels.
		return protocol.ReconcileResult{Channel: info, Agents: s.Tree(), Prompts: c.d.esc.Pending(""), Seq: seq}, nil
	})),
	route(protocol.Presets, func(_ context.Context, c *conn, p protocol.PresetsParams) (protocol.PresetsResult, error) {
		s, err := c.d.channel(p.Channel)
		if err != nil {
			return protocol.PresetsResult{}, err
		}
		return protocol.PresetsResult{Presets: s.Presets()}, nil
	}),
	forWeb(route(protocol.UsageSeries, func(ctx context.Context, c *conn, p protocol.UsageSeriesParams) (protocol.UsageSeriesResult, error) {
		switch {
		case p.Buckets < 1 || p.Buckets > 1000:
			return protocol.UsageSeriesResult{}, fmt.Errorf("buckets must be 1–1000, not %d", p.Buckets)
		case p.Agent != "" && p.Channel == "":
			return protocol.UsageSeriesResult{}, errors.New("an agent's usage needs its channel")
		case !p.From.IsZero() && !p.To.IsZero() && !p.From.Before(p.To):
			return protocol.UsageSeriesResult{}, errors.New("from must be before to")
		}
		s, err := c.d.Log.Usage(ctx, eventlog.UsageQuery{Channel: p.Channel, Agent: p.Agent, From: p.From, To: p.To, Buckets: p.Buckets})
		if err != nil {
			return protocol.UsageSeriesResult{}, internal(err)
		}
		return protocol.UsageSeriesResult{From: s.From, To: s.To, Tokens: s.Tokens, Cost: s.Cost}, nil
	})),
	forWeb(route(protocol.PlanUsage, func(_ context.Context, c *conn, _ protocol.None) (protocol.PlanUsageResult, error) {
		return c.d.planUsage(), nil
	})),
	route(protocol.PlanSeries, func(ctx context.Context, c *conn, p protocol.PlanSeriesParams) (protocol.PlanSeriesResult, error) {
		if p.Buckets < 1 || p.Buckets > 1000 {
			return protocol.PlanSeriesResult{}, fmt.Errorf("buckets must be 1–1000, not %d", p.Buckets)
		}
		// Opening a plan's chart is the human asking how it stands now.
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := c.d.Registry.PollPlanUsage(pollCtx, p.Provider, 30*time.Second); err != nil {
			log.Printf("%v", err)
		}
		cancel()
		to := cmp.Or(p.To, time.Now())
		from := p.From
		if from.IsZero() {
			from = c.d.planUsageFirst(p.Provider, to)
		}
		if !from.Before(to) {
			return protocol.PlanSeriesResult{}, errors.New("from must be before to")
		}
		return protocol.PlanSeriesResult{From: from.UTC(), To: to.UTC(), Percent: c.d.planUsageSeries(p.Provider, from, to, p.Buckets)}, nil
	}),
	forWeb(route(protocol.CacheUsage, func(ctx context.Context, c *conn, p protocol.CacheUsageParams) (protocol.CacheUsageResult, error) {
		minutes := cmp.Or(p.Minutes, 60)
		if minutes < 1 || minutes > 7*24*60 {
			return protocol.CacheUsageResult{}, fmt.Errorf("minutes must be 1–%d, not %d", 7*24*60, minutes)
		}
		fresh, cached, err := c.d.Log.CacheUsage(ctx, time.Now().Add(-time.Duration(minutes)*time.Minute))
		if err != nil {
			return protocol.CacheUsageResult{}, internal(err)
		}
		res := protocol.CacheUsageResult{Fresh: fresh, Cached: cached}
		by, err := c.d.Log.CacheUsageByProvider(ctx, time.Now().Add(-time.Duration(minutes)*time.Minute))
		if err != nil {
			return protocol.CacheUsageResult{}, internal(err)
		}
		for provider, v := range by {
			res.Providers = append(res.Providers, protocol.CacheProviderUsage{Provider: provider, Fresh: v[0], Cached: v[1]})
		}
		sort.Slice(res.Providers, func(i, j int) bool {
			a, b := res.Providers[i], res.Providers[j]
			if a.Fresh+a.Cached != b.Fresh+b.Cached {
				return a.Fresh+a.Cached > b.Fresh+b.Cached
			}
			return a.Provider < b.Provider
		})
		return res, nil
	})),
	route(protocol.CommandList, func(_ context.Context, c *conn, p protocol.ChannelRef) (protocol.CommandListResult, error) {
		return c.d.listCommands(p.Channel)
	}),
	route(protocol.ConfigList, func(_ context.Context, c *conn, p protocol.ConfigScope) (protocol.ConfigTree, error) {
		return c.d.configList(p)
	}),
	route(protocol.ConfigRead, func(_ context.Context, c *conn, p protocol.ConfigFileParams) (protocol.ConfigDocument, error) {
		return c.d.configRead(p)
	}),
	route(protocol.ConfigEdit, func(ctx context.Context, c *conn, p protocol.ConfigEditParams) (protocol.ConfigEditResult, error) {
		return c.d.configEdit(ctx, p)
	}),
	route(protocol.CommandRun, func(ctx context.Context, c *conn, p protocol.CommandRunParams) (protocol.None, error) {
		name, _ := c.cl.identity()
		return none, c.d.runCommand(ctx, p, "human:"+name)
	}),
)

// routes builds the method table; a method routed twice is a programming
// error.
func routes(entries ...routeEntry) map[string]routeEntry {
	out := map[string]routeEntry{}
	for _, e := range entries {
		if _, dup := out[e.name]; dup {
			panic("method routed twice: " + e.name)
		}
		out[e.name] = e
	}
	return out
}
