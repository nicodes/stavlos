package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/protocol"
)

// The protocol's methods, one handler each. A handler returns a result or
// a domain error; toProtocolError is the one place that decides the wire
// code. Params are decoded by typed, once, into the method's own struct.

type handler func(ctx context.Context, c *conn, req protocol.Request) (any, error)

// typed decodes a request's params into P and calls fn.
func typed[P any](fn func(ctx context.Context, c *conn, p P) (any, error)) handler {
	return func(ctx context.Context, c *conn, req protocol.Request) (any, error) {
		var p P
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return nil, codedError{protocol.ErrInvalidParams, err}
			}
		}
		return fn(ctx, c, p)
	}
}

// errNotFound marks a missing session, agent or prompt; wrap it with %w so
// the message reads "session "x" not found".
var errNotFound = errors.New("not found")

// codedError carries an explicit wire code for errors that are neither the
// caller's mistake nor a missing thing (an internal failure).
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

var okResult = map[string]bool{"ok": true}

type noParams struct{}

var handlers = map[string]handler{
	protocol.MDaemonStatus: typed(func(_ context.Context, c *conn, _ noParams) (any, error) {
		return c.d.Status(), nil
	}),
	protocol.MDaemonShutdown: typed(func(_ context.Context, c *conn, _ noParams) (any, error) {
		log.Printf("shutdown requested by client %s (%s)", c.cl.name, c.cl.id)
		if c.d.Shutdown != nil {
			go c.d.Shutdown()
		}
		return okResult, nil
	}),
	protocol.MAttach: typed(func(_ context.Context, c *conn, p protocol.AttachParams) (any, error) {
		if p.Tier == protocol.TierFallback {
			c.cl.tier = protocol.TierFallback
		}
		if p.Client != "" {
			c.cl.name = p.Client
		}
		return protocol.AttachResult{ClientID: c.cl.id, Version: protocol.Version}, nil
	}),

	protocol.MSessionList: typed(func(ctx context.Context, c *conn, p protocol.SessionListParams) (any, error) {
		list, err := c.d.SessionList(ctx, p.Dir, p.IncludeArchived)
		if err != nil {
			return nil, internal(err)
		}
		return protocol.SessionListResult{Sessions: list}, nil
	}),
	protocol.MSessionCreate: typed(func(ctx context.Context, c *conn, p protocol.SessionCreateParams) (any, error) {
		s, err := c.d.CreateSession(ctx, p.Dir, p.Model, p.RootAgent)
		if err != nil {
			return nil, err
		}
		return s.Info(), nil
	}),
	protocol.MSessionResume: typed(func(_ context.Context, c *conn, p protocol.SessionRef) (any, error) {
		s, err := c.d.session(p.ID)
		if err != nil {
			return nil, err
		}
		c.d.maybeTrustPrompt(s)
		return s.Info(), nil
	}),
	protocol.MSessionFork: typed(func(ctx context.Context, c *conn, p protocol.SessionForkParams) (any, error) {
		s, err := c.d.ForkSession(ctx, p.ID, p.Seq)
		if err != nil {
			return nil, err
		}
		return s.Info(), nil
	}),
	protocol.MSessionArchive: typed(func(ctx context.Context, c *conn, p protocol.SessionRef) (any, error) {
		return okResult, c.d.ArchiveSession(ctx, p.ID)
	}),
	protocol.MSessionSetModel: typed(func(ctx context.Context, c *conn, p protocol.SessionSetModelParams) (any, error) {
		s, err := c.d.session(p.ID)
		if err != nil {
			return nil, err
		}
		if err := s.SetModel(ctx, p.Model); err != nil {
			return nil, err
		}
		c.d.rememberModel(s, p.Model)
		return okResult, nil
	}),
	protocol.MSessionSetMode: typed(func(ctx context.Context, c *conn, p protocol.SessionSetModeParams) (any, error) {
		s, err := c.d.session(p.ID)
		if err != nil {
			return nil, err
		}
		if err := s.SetMode(ctx, p.Mode); err != nil {
			return nil, err
		}
		// Anything already waiting that the new mode would have allowed is
		// allowed now, so the agents move: yolo takes every permission
		// prompt, auto only the ones that stay inside the agent's directories.
		switch p.Mode {
		case protocol.ModeYolo:
			c.d.esc.AnswerAll(s.ID, protocol.PromptPermission, protocol.AnswerAllow, "yolo")
		case protocol.ModeAuto:
			c.d.esc.AnswerWhere(s.ID, protocol.PromptPermission, protocol.AnswerAllow, "auto", func(pi protocol.PromptInfo) bool { return pi.Dir == "" })
		}
		return okResult, nil
	}),

	protocol.MAgentTree: typed(func(_ context.Context, c *conn, p protocol.AgentTreeParams) (any, error) {
		s, err := c.d.session(p.Session)
		if err != nil {
			return nil, err
		}
		return protocol.AgentTreeResult{Agents: tree(s)}, nil
	}),
	protocol.MAgentSend: typed(func(ctx context.Context, c *conn, p protocol.AgentSendParams) (any, error) {
		s, _, err := c.d.agentSession(p.Agent)
		if err != nil {
			return nil, err
		}
		src := "human:" + c.cl.name
		switch p.Kind {
		case protocol.KindPrompt:
			err = s.Send(ctx, p.Agent, p.Text, src)
		case protocol.KindSteer:
			err = s.Steer(ctx, p.Agent, p.Text, src)
		case protocol.KindCancel:
			err = s.Cancel(p.Agent)
		case protocol.KindKill:
			err = s.Kill(p.Agent)
		default:
			err = fmt.Errorf("unknown envelope kind %q", p.Kind)
		}
		return okResult, err
	}),
	protocol.MAgentSpawn: typed(func(ctx context.Context, c *conn, p protocol.AgentSpawnParams) (any, error) {
		s, _, err := c.d.agentSession(p.Parent)
		if err != nil {
			return nil, err
		}
		id, err := s.SpawnFromClient(ctx, p.Parent, p.Archetype, p.Label, p.Task, p.Model, p.Dirs)
		if err != nil {
			return nil, err
		}
		return protocol.AgentSpawnResult{ID: id}, nil
	}),
	protocol.MAgentSetModel: typed(func(ctx context.Context, c *conn, p protocol.AgentSetModelParams) (any, error) {
		s, a, err := c.d.agentSession(p.Agent)
		if err != nil {
			return nil, err
		}
		if err := a.SetModel(ctx, p.Model); err != nil {
			return nil, err
		}
		if a.Parent == "" && s.Model() == "" {
			// Root picked a model in a session that had none: adopt it.
			_ = s.SetModel(ctx, p.Model)
			c.d.rememberModel(s, p.Model)
		}
		return okResult, nil
	}),
	protocol.MAgentSetRole: typed(func(ctx context.Context, c *conn, p protocol.AgentSetRoleParams) (any, error) {
		_, a, err := c.d.agentSession(p.Agent)
		if err != nil {
			return nil, err
		}
		return okResult, a.SetRole(ctx, p.Role)
	}),
	protocol.MAgentSetVariant: typed(func(ctx context.Context, c *conn, p protocol.AgentSetVariantParams) (any, error) {
		_, a, err := c.d.agentSession(p.Agent)
		if err != nil {
			return nil, err
		}
		return okResult, a.SetVariant(ctx, p.Variant)
	}),
	protocol.MAgentCompact: typed(func(ctx context.Context, c *conn, p protocol.AgentCompactParams) (any, error) {
		_, a, err := c.d.agentSession(p.Agent)
		if err != nil {
			return nil, err
		}
		status, err := a.Compact(ctx)
		if err != nil {
			return nil, err
		}
		return protocol.AgentCompactResult{Status: status}, nil
	}),
	protocol.MAgentAddDir: typed(func(ctx context.Context, c *conn, p protocol.AgentDirParams) (any, error) {
		_, a, err := c.d.agentSession(p.Agent)
		if err != nil {
			return nil, err
		}
		return okResult, a.AddDir(ctx, p.Dir)
	}),
	protocol.MAgentRemoveDir: typed(func(ctx context.Context, c *conn, p protocol.AgentDirParams) (any, error) {
		_, a, err := c.d.agentSession(p.Agent)
		if err != nil {
			return nil, err
		}
		return okResult, a.RemoveDir(ctx, p.Dir)
	}),
	protocol.MVariants: typed(func(_ context.Context, c *conn, p protocol.VariantsParams) (any, error) {
		return protocol.VariantsResult{Variants: c.d.Registry.Variants(p.Model)}, nil
	}),

	protocol.MPromptList: typed(func(_ context.Context, c *conn, p protocol.PromptListParams) (any, error) {
		return protocol.PromptListResult{Prompts: c.d.esc.Pending(p.Session)}, nil
	}),
	protocol.MPromptClaim: typed(func(_ context.Context, c *conn, p protocol.PromptClaimParams) (any, error) {
		if err := c.d.esc.Claim(p.ID, c.cl.id); err != nil {
			return nil, promptErr(err)
		}
		return okResult, nil
	}),
	protocol.MPromptReply: typed(func(_ context.Context, c *conn, p protocol.PromptReplyParams) (any, error) {
		if err := c.d.esc.ReplyAll(p.ID, c.cl.id, p.Answer, p.Dir, p.Reason, p.Answers); err != nil {
			return nil, promptErr(err)
		}
		return okResult, nil
	}),

	protocol.MTrustStatus: typed(func(_ context.Context, c *conn, p protocol.TrustStatusParams) (any, error) {
		r, err := c.d.TrustStatus(p.Dir)
		if err != nil {
			return nil, internal(err)
		}
		return r, nil
	}),
	protocol.MTrustReply: typed(func(ctx context.Context, c *conn, p protocol.TrustReplyParams) (any, error) {
		return okResult, c.d.Trust(ctx, p.Dir, p.Hash, p.Trust)
	}),

	protocol.MProviderList: typed(func(_ context.Context, c *conn, _ noParams) (any, error) {
		return c.d.ProviderList(), nil
	}),
	protocol.MProviderLoginStart: typed(func(ctx context.Context, c *conn, p protocol.LoginStartParams) (any, error) {
		return c.d.LoginStart(ctx, p.Provider, p.Method)
	}),
	protocol.MProviderLoginWait: typed(func(ctx context.Context, c *conn, p protocol.LoginWaitParams) (any, error) {
		st, err := c.d.LoginWait(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		return providerInfo(st), nil
	}),
	protocol.MProviderDisconnect: typed(func(_ context.Context, c *conn, p protocol.ProviderRef) (any, error) {
		return okResult, c.d.Registry.Disconnect(p.Provider)
	}),
	protocol.MModelList: typed(func(_ context.Context, c *conn, p protocol.ModelListParams) (any, error) {
		var out []protocol.ModelInfo
		for _, m := range c.d.Registry.Models(p.Provider, p.All) {
			out = append(out, protocol.ModelInfo{ID: m.ID, Provider: m.Provider, Name: m.Name, Context: m.Info.ContextWindow, InputPrice: m.Info.InputPrice, OutputPrice: m.Info.OutputPrice})
		}
		return protocol.ModelListResult{Models: out}, nil
	}),

	protocol.MSubscribe: typed(func(ctx context.Context, c *conn, p protocol.SubscribeParams) (any, error) {
		if _, err := c.d.session(p.Session); err != nil {
			return nil, err
		}
		last, err := c.d.subscribe(ctx, c.cl, p.Session, p.From)
		if err != nil {
			return nil, internal(err)
		}
		return map[string]any{"ok": true, "seq": last}, nil
	}),
	protocol.MUnsubscribe: typed(func(_ context.Context, c *conn, p protocol.SubscribeParams) (any, error) {
		c.cl.mu.Lock()
		delete(c.cl.subs, p.Session)
		c.cl.mu.Unlock()
		return okResult, nil
	}),
	protocol.MReconcile: typed(func(ctx context.Context, c *conn, p protocol.SessionRef) (any, error) {
		s, err := c.d.session(p.ID)
		if err != nil {
			return nil, err
		}
		seq, _ := c.d.Log.LastSeq(ctx, p.ID)
		info := s.Info()
		info.Seq = seq
		return protocol.ReconcileResult{Session: info, Agents: tree(s), Prompts: c.d.esc.Pending(p.ID), Seq: seq}, nil
	}),
	protocol.MPresets: typed(func(_ context.Context, c *conn, p protocol.PresetsParams) (any, error) {
		s, err := c.d.session(p.Session)
		if err != nil {
			return nil, err
		}
		return protocol.PresetsResult{Presets: s.Presets()}, nil
	}),
}
