package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/protocol"
)

// Serve accepts protocol connections on the Unix socket until ctx ends.
func (d *Daemon) Serve(ctx context.Context, socket string) error {
	if len(socket) > 100 {
		return fmt.Errorf("socket path %q is too long for a Unix socket (max ~100 bytes); set STAVLOS_SOCKET to a shorter path", socket)
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	_ = os.Chmod(socket, 0o600)
	go func() {
		<-ctx.Done()
		ln.Close()
		_ = os.Remove(socket)
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.handleConn(ctx, conn)
	}
}

type conn struct {
	d   *Daemon
	c   net.Conn
	w   *bufio.Writer
	wmu sync.Mutex
	cl  *client
}

func (d *Daemon) handleConn(ctx context.Context, nc net.Conn) {
	c := &conn{d: d, c: nc, w: bufio.NewWriter(nc)}
	c.cl = &client{id: agent.NewID("c"), name: "anonymous", tier: protocol.TierInteractive, subs: map[string]int64{}, send: c.notify}
	d.addClient(c.cl)
	defer func() {
		d.removeClient(c.cl.id)
		nc.Close()
	}()
	sc := bufio.NewScanner(nc)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var req protocol.Request
		if err := json.Unmarshal(line, &req); err != nil {
			c.reply(nil, nil, &protocol.Error{Code: protocol.ErrParse, Message: err.Error()})
			continue
		}
		go func() {
			res, perr := c.dispatch(ctx, req)
			if req.ID != nil {
				c.reply(req.ID, res, perr)
			}
		}()
	}
}

func (c *conn) write(v any) {
	b, _ := json.Marshal(v)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.w.Write(append(b, '\n'))
	c.w.Flush()
}

func (c *conn) reply(id *json.RawMessage, result any, perr *protocol.Error) {
	r := protocol.Response{JSONRPC: "2.0", ID: id, Error: perr}
	if perr == nil {
		b, err := json.Marshal(result)
		if err != nil {
			r.Error = &protocol.Error{Code: protocol.ErrInternal, Message: err.Error()}
		} else {
			r.Result = b
		}
	}
	c.write(r)
}

func (c *conn) notify(method string, params json.RawMessage) {
	c.write(protocol.Response{JSONRPC: "2.0", Method: method, Params: params})
}

func perr(code int, err error) *protocol.Error {
	return &protocol.Error{Code: code, Message: err.Error()}
}

func (c *conn) dispatch(ctx context.Context, req protocol.Request) (any, *protocol.Error) {
	var ver struct {
		V int `json:"v"`
	}
	_ = json.Unmarshal(req.Params, &ver)
	if ver.V != protocol.Version {
		return nil, &protocol.Error{Code: protocol.ErrVersion, Message: fmt.Sprintf("protocol version %d not served; this daemon serves %d", ver.V, protocol.Version)}
	}
	decode := func(v any) *protocol.Error {
		if len(req.Params) == 0 {
			return nil
		}
		if err := json.Unmarshal(req.Params, v); err != nil {
			return perr(protocol.ErrInvalidParams, err)
		}
		return nil
	}
	d := c.d
	switch req.Method {
	case protocol.MDaemonStatus:
		return d.Status(), nil

	case protocol.MDaemonShutdown:
		log.Printf("shutdown requested by client %s (%s)", c.cl.name, c.cl.id)
		if d.Shutdown != nil {
			go d.Shutdown()
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MAttach:
		var p protocol.AttachParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		if p.Tier == protocol.TierFallback {
			c.cl.tier = protocol.TierFallback
		}
		if p.Client != "" {
			c.cl.name = p.Client
		}
		return protocol.AttachResult{ClientID: c.cl.id, Version: protocol.Version}, nil

	case protocol.MSessionList:
		var p protocol.SessionListParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		list, err := d.SessionList(ctx, p.Dir, p.IncludeArchived)
		if err != nil {
			return nil, perr(protocol.ErrInternal, err)
		}
		return protocol.SessionListResult{Sessions: list}, nil

	case protocol.MSessionCreate:
		var p protocol.SessionCreateParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		s, err := d.CreateSession(ctx, p.Dir, p.Model, p.RootAgent)
		if err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		return s.Info(), nil

	case protocol.MSessionResume:
		var p protocol.SessionRef
		if e := decode(&p); e != nil {
			return nil, e
		}
		s, err := d.session(p.ID)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		d.maybeTrustPrompt(s)
		return s.Info(), nil

	case protocol.MSessionFork:
		var p protocol.SessionForkParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		s, err := d.ForkSession(ctx, p.ID, p.Seq)
		if err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		return s.Info(), nil

	case protocol.MSessionArchive:
		var p protocol.SessionRef
		if e := decode(&p); e != nil {
			return nil, e
		}
		if err := d.ArchiveSession(ctx, p.ID); err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MSessionSetModel:
		var p protocol.SessionSetModelParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		s, err := d.session(p.ID)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		if err := s.SetModel(ctx, p.Model); err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		d.rememberModel(s, p.Model)
		return map[string]bool{"ok": true}, nil

	case protocol.MSessionSetYolo:
		var p protocol.SessionSetYoloParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		s, err := d.session(p.ID)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		if err := s.SetYolo(ctx, p.On); err != nil {
			return nil, perr(protocol.ErrInternal, err)
		}
		if p.On {
			// Anything already waiting is allowed too, so the agents move.
			d.esc.AnswerAll(s.ID, "permission", "allow", "yolo")
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MAgentTree:
		var p protocol.AgentTreeParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		s, err := d.session(p.Session)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		return protocol.AgentTreeResult{Agents: tree(s)}, nil

	case protocol.MAgentSend:
		var p protocol.AgentSendParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		s, _, err := d.agentSession(p.Agent)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
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
		if err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MAgentSpawn:
		var p protocol.AgentSpawnParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		s, _, err := d.agentSession(p.Parent)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		id, err := s.SpawnFromClient(ctx, p.Parent, p.Archetype, p.Label, p.Task, p.Model)
		if err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		return protocol.AgentSpawnResult{ID: id}, nil

	case protocol.MAgentSetModel:
		var p protocol.AgentSetModelParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		_, a, err := d.agentSession(p.Agent)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		if err := a.SetModel(ctx, p.Model); err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		s, _, _ := d.agentSession(p.Agent)
		if s != nil && a.Parent == "" && s.Model() == "" {
			// Root picked a model in a session that had none: adopt it.
			_ = s.SetModel(ctx, p.Model)
			d.rememberModel(s, p.Model)
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MAgentSetRole:
		var p protocol.AgentSetRoleParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		_, a, err := d.agentSession(p.Agent)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		if err := a.SetRole(ctx, p.Role); err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MAgentSetVariant:
		var p protocol.AgentSetVariantParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		_, a, err := d.agentSession(p.Agent)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		if err := a.SetVariant(ctx, p.Variant); err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MVariants:
		var p protocol.VariantsParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		return protocol.VariantsResult{Variants: d.Registry.Variants(p.Model)}, nil

	case protocol.MPromptList:
		var p protocol.PromptListParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		return protocol.PromptListResult{Prompts: d.esc.Pending(p.Session)}, nil

	case protocol.MPromptClaim:
		var p protocol.PromptClaimParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		if err := d.esc.Claim(p.ID, c.cl.id); err != nil {
			return nil, perr(promptErrCode(err), err)
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MPromptReply:
		var p protocol.PromptReplyParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		if err := d.esc.Reply(p.ID, c.cl.id, p.Answer); err != nil {
			return nil, perr(promptErrCode(err), err)
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MTrustStatus:
		var p protocol.TrustStatusParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		r, err := d.TrustStatus(p.Dir)
		if err != nil {
			return nil, perr(protocol.ErrInternal, err)
		}
		return r, nil

	case protocol.MTrustReply:
		var p protocol.TrustReplyParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		if err := d.Trust(ctx, p.Dir, p.Hash, p.Trust); err != nil {
			return nil, perr(protocol.ErrInternal, err)
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MProviderList:
		return d.ProviderList(), nil

	case protocol.MProviderLoginStart:
		var p protocol.LoginStartParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		r, err := d.LoginStart(ctx, p.Provider, p.Method)
		if err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		return r, nil

	case protocol.MProviderLoginWait:
		var p protocol.LoginWaitParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		st, err := d.LoginWait(ctx, p.ID)
		if err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		return providerInfo(st), nil

	case protocol.MProviderDisconnect:
		var p protocol.ProviderRef
		if e := decode(&p); e != nil {
			return nil, e
		}
		if err := d.Registry.Disconnect(p.Provider); err != nil {
			return nil, perr(protocol.ErrInvalidParams, err)
		}
		return map[string]bool{"ok": true}, nil

	case protocol.MModelList:
		var p protocol.ModelListParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		var out []protocol.ModelInfo
		for _, m := range d.Registry.Models(p.Provider, p.All) {
			out = append(out, protocol.ModelInfo{ID: m.ID, Provider: m.Provider, Name: m.Name, Context: m.Info.ContextWindow, InputPrice: m.Info.InputPrice, OutputPrice: m.Info.OutputPrice})
		}
		return protocol.ModelListResult{Models: out}, nil

	case protocol.MSubscribe:
		var p protocol.SubscribeParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		if _, err := d.session(p.Session); err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		return c.subscribe(ctx, p)

	case protocol.MUnsubscribe:
		var p protocol.SubscribeParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		c.cl.mu.Lock()
		delete(c.cl.subs, p.Session)
		c.cl.mu.Unlock()
		return map[string]bool{"ok": true}, nil

	case protocol.MReconcile:
		var p protocol.SessionRef
		if e := decode(&p); e != nil {
			return nil, e
		}
		s, err := d.session(p.ID)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		seq, _ := d.Log.LastSeq(ctx, p.ID)
		info := s.Info()
		info.Seq = seq
		return protocol.ReconcileResult{Session: info, Agents: tree(s), Prompts: d.esc.Pending(p.ID), Seq: seq}, nil

	case protocol.MPresets:
		var p protocol.PresetsParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		s, err := d.session(p.Session)
		if err != nil {
			return nil, perr(protocol.ErrNotFound, err)
		}
		return protocol.PresetsResult{Presets: s.Presets()}, nil
	}
	return nil, &protocol.Error{Code: protocol.ErrMethodNotFound, Message: "unknown method " + req.Method}
}

func promptErrCode(err error) int {
	switch {
	case errors.Is(err, escalation.ErrClaimed), errors.Is(err, escalation.ErrLate):
		return protocol.ErrConflict
	}
	return protocol.ErrNotFound
}

func tree(s *agent.Session) []protocol.AgentInfo {
	var out []protocol.AgentInfo
	for _, a := range s.Agents() {
		out = append(out, a.Info())
	}
	return out
}

// subscribe replays from the requested offset, then goes live. Delivery is
// deduplicated per subscription by seq, so the handover cannot double-send.
func (c *conn) subscribe(ctx context.Context, p protocol.SubscribeParams) (any, *protocol.Error) {
	from := p.From
	if from <= 0 {
		from = 1
	}
	c.cl.mu.Lock()
	c.cl.subs[p.Session] = from - 1
	c.cl.mu.Unlock()
	live, cancel := c.d.Log.Subscribe(p.Session)
	defer cancel()
	evs, err := c.d.Log.Read(ctx, p.Session, from, 0)
	if err != nil {
		return nil, perr(protocol.ErrInternal, err)
	}
	for _, e := range evs {
		c.cl.deliver(e)
	}
	for {
		select {
		case e := <-live:
			c.cl.deliver(e)
		default:
			c.cl.mu.Lock()
			last := c.cl.subs[p.Session]
			c.cl.mu.Unlock()
			return map[string]any{"ok": true, "seq": last}, nil
		}
	}
}

// rememberModel makes the first model a user picks the global default when
// no config layer has set one, so the next session does not start empty.
func (d *Daemon) rememberModel(s *agent.Session, modelID string) {
	if s.Config().Model != "" {
		return
	}
	if err := config.SetGlobalModel(modelID); err != nil {
		log.Printf("could not save default model: %v", err)
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, ss := range d.sessions {
		if cfg, err := config.Load(ss.Dir, d.trust); err == nil {
			ss.SetConfig(cfg)
		}
	}
}

// ProviderList builds the provider.list result.
func (d *Daemon) ProviderList() protocol.ProviderListResult {
	r := protocol.ProviderListResult{}
	if st := d.Registry.Store(); st != nil {
		r.AuthPath = st.Path()
	}
	for _, s := range d.Registry.List() {
		r.Providers = append(r.Providers, providerInfo(s))
	}
	return r
}

func providerInfo(s registry.Status) protocol.ProviderInfo {
	info := protocol.ProviderInfo{ID: s.ID, Name: s.Name, Connected: s.Connected, Kind: string(s.Kind), Priority: s.Priority,
		Models: s.Models, Label: s.Label, Account: s.Account}
	for _, m := range s.Methods {
		info.Methods = append(info.Methods, protocol.LoginMethod{ID: m.ID, Label: m.Label})
	}
	return info
}

var _ = log.Printf
