// Package client is the Go protocol client (PRD §9, §14).
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/nicodes/stavlos/internal/protocol"
)

// Client is a connection to stavlosd.
type Client struct {
	conn net.Conn
	w    *bufio.Writer
	wmu  sync.Mutex

	nextID  atomic.Int64
	pending map[int64]chan protocol.Response
	pmu     sync.Mutex

	// Notifications is fed every server→client notification. It is buffered;
	// a consumer that falls far behind will block the reader.
	Notifications chan protocol.Response

	closed chan struct{}
	err    error
}

// Dial connects to the daemon socket.
func Dial(socket string) (*Client, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:          conn,
		w:             bufio.NewWriter(conn),
		pending:       map[int64]chan protocol.Response{},
		Notifications: make(chan protocol.Response, 1024),
		closed:        make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var r protocol.Response
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if r.ID == nil {
			select {
			case c.Notifications <- r:
			case <-c.closed:
				return
			}
			continue
		}
		var id int64
		_ = json.Unmarshal(*r.ID, &id)
		c.pmu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.pmu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
	c.err = sc.Err()
	if c.err == nil {
		c.err = errors.New("connection closed")
	}
	close(c.closed)
	c.pmu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.pmu.Unlock()
}

// Closed is closed when the connection drops.
func (c *Client) Closed() <-chan struct{} { return c.closed }

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Call performs one JSON-RPC request. params must be a struct with a V field
// or a map; the version is injected if missing.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	raw, err := withVersion(params)
	if err != nil {
		return err
	}
	idb, _ := json.Marshal(id)
	idr := json.RawMessage(idb)
	req := protocol.Request{JSONRPC: "2.0", ID: &idr, Method: method, Params: raw}
	b, _ := json.Marshal(req)

	ch := make(chan protocol.Response, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()

	c.wmu.Lock()
	_, werr := c.w.Write(append(b, '\n'))
	if werr == nil {
		werr = c.w.Flush()
	}
	c.wmu.Unlock()
	if werr != nil {
		return werr
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return c.err
		}
		if r.Error != nil {
			return r.Error
		}
		if result != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return c.err
	}
}

func withVersion(params any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("params must be an object: %w", err)
	}
	if v, ok := m["v"]; !ok || v == float64(0) {
		m["v"] = protocol.Version
	}
	return json.Marshal(m)
}

// --- typed helpers ---

func (c *Client) Attach(ctx context.Context, name string, tier protocol.Tier) (protocol.AttachResult, error) {
	var r protocol.AttachResult
	err := c.Call(ctx, protocol.MAttach, protocol.AttachParams{Client: name, Tier: tier}, &r)
	return r, err
}

func (c *Client) Status(ctx context.Context) (protocol.DaemonStatusResult, error) {
	var r protocol.DaemonStatusResult
	err := c.Call(ctx, protocol.MDaemonStatus, nil, &r)
	return r, err
}

// Shutdown asks the daemon to stop gracefully.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.Call(ctx, protocol.MDaemonShutdown, nil, nil)
}

func (c *Client) Sessions(ctx context.Context, dir string, archived bool) ([]protocol.SessionInfo, error) {
	var r protocol.SessionListResult
	err := c.Call(ctx, protocol.MSessionList, protocol.SessionListParams{Dir: dir, IncludeArchived: archived}, &r)
	return r.Sessions, err
}

func (c *Client) CreateSession(ctx context.Context, dir, modelID, root string) (protocol.SessionInfo, error) {
	var r protocol.SessionInfo
	err := c.Call(ctx, protocol.MSessionCreate, protocol.SessionCreateParams{Dir: dir, Model: modelID, RootAgent: root}, &r)
	return r, err
}

func (c *Client) ResumeSession(ctx context.Context, id string) (protocol.SessionInfo, error) {
	var r protocol.SessionInfo
	err := c.Call(ctx, protocol.MSessionResume, protocol.SessionRef{ID: id}, &r)
	return r, err
}

func (c *Client) ForkSession(ctx context.Context, id string, seq int64) (protocol.SessionInfo, error) {
	var r protocol.SessionInfo
	err := c.Call(ctx, protocol.MSessionFork, protocol.SessionForkParams{ID: id, Seq: seq}, &r)
	return r, err
}

func (c *Client) ArchiveSession(ctx context.Context, id string) error {
	return c.Call(ctx, protocol.MSessionArchive, protocol.SessionRef{ID: id}, nil)
}

func (c *Client) SetSessionModel(ctx context.Context, id, modelID string) error {
	return c.Call(ctx, protocol.MSessionSetModel, protocol.SessionSetModelParams{ID: id, Model: modelID}, nil)
}

// SetSessionMode sets the session's permission mode: ask, auto or yolo.
func (c *Client) SetSessionMode(ctx context.Context, id, mode string) error {
	return c.Call(ctx, protocol.MSessionSetMode, protocol.SessionSetModeParams{V: protocol.Version, ID: id, Mode: mode}, nil)
}

func (c *Client) Tree(ctx context.Context, session string) ([]protocol.AgentInfo, error) {
	var r protocol.AgentTreeResult
	err := c.Call(ctx, protocol.MAgentTree, protocol.AgentTreeParams{Session: session}, &r)
	return r.Agents, err
}

func (c *Client) Send(ctx context.Context, agent string, kind protocol.Kind, text string) error {
	return c.Call(ctx, protocol.MAgentSend, protocol.AgentSendParams{Agent: agent, Kind: kind, Text: text}, nil)
}

func (c *Client) Spawn(ctx context.Context, p protocol.AgentSpawnParams) (string, error) {
	var r protocol.AgentSpawnResult
	err := c.Call(ctx, protocol.MAgentSpawn, p, &r)
	return r.ID, err
}

func (c *Client) SetAgentModel(ctx context.Context, agent, modelID string) error {
	return c.Call(ctx, protocol.MAgentSetModel, protocol.AgentSetModelParams{Agent: agent, Model: modelID}, nil)
}

// SetAgentRole switches an agent's preset (system prompt, tools, spawn list)
// in place; it takes effect at the agent's next turn.
func (c *Client) SetAgentRole(ctx context.Context, agent, role string) error {
	return c.Call(ctx, protocol.MAgentSetRole, protocol.AgentSetRoleParams{Agent: agent, Role: role}, nil)
}

func (c *Client) Prompts(ctx context.Context, session string) ([]protocol.PromptInfo, error) {
	var r protocol.PromptListResult
	err := c.Call(ctx, protocol.MPromptList, protocol.PromptListParams{Session: session}, &r)
	return r.Prompts, err
}

func (c *Client) ClaimPrompt(ctx context.Context, id string) error {
	return c.Call(ctx, protocol.MPromptClaim, protocol.PromptClaimParams{ID: id}, nil)
}

func (c *Client) ReplyPrompt(ctx context.Context, id, answer string) error {
	return c.Call(ctx, protocol.MPromptReply, protocol.PromptReplyParams{ID: id, Answer: answer}, nil)
}

// ReplyPromptDir answers a boundary prompt with allow_always and a
// directory of the human's choosing in place of the offered one.
func (c *Client) ReplyPromptDir(ctx context.Context, id, answer, dir string) error {
	return c.Call(ctx, protocol.MPromptReply, protocol.PromptReplyParams{ID: id, Answer: answer, Dir: dir}, nil)
}

// DenyPrompt denies a permission with an optional reason the agent will read.
func (c *Client) DenyPrompt(ctx context.Context, id, reason string) error {
	return c.Call(ctx, protocol.MPromptReply, protocol.PromptReplyParams{ID: id, Answer: "deny", Reason: reason}, nil)
}

func (c *Client) TrustStatus(ctx context.Context, dir string) (protocol.TrustStatusResult, error) {
	var r protocol.TrustStatusResult
	err := c.Call(ctx, protocol.MTrustStatus, protocol.TrustStatusParams{Dir: dir}, &r)
	return r, err
}

func (c *Client) TrustReply(ctx context.Context, dir, hash string, trust bool) error {
	return c.Call(ctx, protocol.MTrustReply, protocol.TrustReplyParams{Dir: dir, Hash: hash, Trust: trust}, nil)
}

func (c *Client) Subscribe(ctx context.Context, session string, from int64) error {
	return c.Call(ctx, protocol.MSubscribe, protocol.SubscribeParams{Session: session, From: from}, nil)
}

func (c *Client) Unsubscribe(ctx context.Context, session string) error {
	return c.Call(ctx, protocol.MUnsubscribe, protocol.SubscribeParams{Session: session}, nil)
}

func (c *Client) Reconcile(ctx context.Context, session string) (protocol.ReconcileResult, error) {
	var r protocol.ReconcileResult
	err := c.Call(ctx, protocol.MReconcile, protocol.SessionRef{ID: session}, &r)
	return r, err
}

// SetAgentVariant switches an agent's model variant ("" = provider default).
func (c *Client) SetAgentVariant(ctx context.Context, agent, variant string) error {
	return c.Call(ctx, protocol.MAgentSetVariant, protocol.AgentSetVariantParams{V: protocol.Version, Agent: agent, Variant: variant}, nil)
}

// AddAgentDir puts a directory in an agent's working set; RemoveAgentDir
// takes one out (the session directory cannot be removed).
func (c *Client) AddAgentDir(ctx context.Context, agent, dir string) error {
	return c.Call(ctx, protocol.MAgentAddDir, protocol.AgentDirParams{V: protocol.Version, Agent: agent, Dir: dir}, nil)
}
func (c *Client) RemoveAgentDir(ctx context.Context, agent, dir string) error {
	return c.Call(ctx, protocol.MAgentRemoveDir, protocol.AgentDirParams{V: protocol.Version, Agent: agent, Dir: dir}, nil)
}

// Variants lists the variant names a model offers.
func (c *Client) Variants(ctx context.Context, modelID string) ([]string, error) {
	var r protocol.VariantsResult
	err := c.Call(ctx, protocol.MVariants, protocol.VariantsParams{V: protocol.Version, Model: modelID}, &r)
	return r.Variants, err
}

func (c *Client) Presets(ctx context.Context, session string) ([]protocol.PresetInfo, error) {
	var r protocol.PresetsResult
	err := c.Call(ctx, protocol.MPresets, protocol.PresetsParams{Session: session}, &r)
	return r.Presets, err
}

func (c *Client) Providers(ctx context.Context) (protocol.ProviderListResult, error) {
	var r protocol.ProviderListResult
	err := c.Call(ctx, protocol.MProviderList, nil, &r)
	return r, err
}

// LoginStart begins a subscription login with the given method ("" for the
// provider's default); show the URL (and Code for the device method).
func (c *Client) LoginStart(ctx context.Context, provider, method string) (protocol.LoginStartResult, error) {
	var r protocol.LoginStartResult
	err := c.Call(ctx, protocol.MProviderLoginStart, protocol.LoginStartParams{Provider: provider, Method: method}, &r)
	return r, err
}

// LoginWait blocks until the login completes (minutes); cancel ctx to abandon.
func (c *Client) LoginWait(ctx context.Context, id string) (protocol.ProviderInfo, error) {
	var r protocol.ProviderInfo
	err := c.Call(ctx, protocol.MProviderLoginWait, protocol.LoginWaitParams{ID: id}, &r)
	return r, err
}

func (c *Client) DisconnectProvider(ctx context.Context, provider string) error {
	return c.Call(ctx, protocol.MProviderDisconnect, protocol.ProviderRef{Provider: provider}, nil)
}

func (c *Client) Models(ctx context.Context, provider string, all bool) ([]protocol.ModelInfo, error) {
	var r protocol.ModelListResult
	err := c.Call(ctx, protocol.MModelList, protocol.ModelListParams{Provider: provider, All: all}, &r)
	return r.Models, err
}

// DecodeNotification unpacks a notification into the typed struct for its method.
func DecodeNotification(r protocol.Response) (any, error) {
	switch r.Method {
	case protocol.NEvent:
		var n protocol.EventNotification
		return n, json.Unmarshal(r.Params, &n)
	case protocol.NStream:
		var n protocol.StreamNotification
		return n, json.Unmarshal(r.Params, &n)
	case protocol.NPrompt:
		var n protocol.PromptNotification
		return n, json.Unmarshal(r.Params, &n)
	}
	return nil, fmt.Errorf("unknown notification %q", r.Method)
}
