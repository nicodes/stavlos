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
	// a consumer that falls far behind blocks the reader, and the daemon then
	// drops the connection rather than wait (the client reconnects).
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
		Notifications: make(chan protocol.Response, 4096),
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

// Call performs one JSON-RPC request; params is the method's params struct
// (nil for none). The protocol version travels in the envelope.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	idb, _ := json.Marshal(id)
	idr := json.RawMessage(idb)
	req := protocol.Request{JSONRPC: "2.0", V: protocol.Version, ID: &idr, Method: method, Params: raw}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}

	ch := make(chan protocol.Response, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	defer func() { // answered, abandoned or failed: the id is done either way
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
	}()

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
	return c.Call(ctx, protocol.MSessionSetMode, protocol.SessionSetModeParams{ID: id, Mode: mode}, nil)
}

// Post sends the human's message to the session chat: every @mentioned
// agent gets it as a steer, the root agent when none is mentioned. It
// returns the names of the agents it went to.
func (c *Client) Post(ctx context.Context, session, text string) ([]string, error) {
	var r protocol.SessionPostResult
	err := c.Call(ctx, protocol.MSessionPost, protocol.SessionPostParams{ID: session, Text: text}, &r)
	return r.To, err
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

// AnswerQuestions answers a question batch, one entry per question.
func (c *Client) AnswerQuestions(ctx context.Context, id string, answers []string) error {
	return c.Call(ctx, protocol.MPromptReply, protocol.PromptReplyParams{ID: id, Answer: protocol.AnswerAnswered, Answers: answers}, nil)
}

// AllowPromptPrefix allows a permission and, for the rest of the session,
// every call the prompt's prefix covers (PromptInfo.Prefix: a command
// prefix for shell, a host for web_fetch). The daemon derives the prefix
// from the call itself; a prompt without one behaves like allow_always.
func (c *Client) AllowPromptPrefix(ctx context.Context, id string) error {
	return c.Call(ctx, protocol.MPromptReply, protocol.PromptReplyParams{ID: id, Answer: protocol.AnswerAllowPrefix}, nil)
}

// DenyPrompt denies a permission with an optional reason the agent will read.
func (c *Client) DenyPrompt(ctx context.Context, id, reason string) error {
	return c.Call(ctx, protocol.MPromptReply, protocol.PromptReplyParams{ID: id, Answer: protocol.AnswerDeny, Reason: reason}, nil)
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
	return c.Call(ctx, protocol.MAgentSetVariant, protocol.AgentSetVariantParams{Agent: agent, Variant: variant}, nil)
}

// AddAgentDir puts a directory in an agent's working set; RemoveAgentDir
// takes one out (the session directory cannot be removed).
// CompactAgent is /compact: "compacted" when the agent summarised its
// completed turns now, "queued" when it was mid-turn and will before its
// next model call.
func (c *Client) CompactAgent(ctx context.Context, agent string) (string, error) {
	var r protocol.AgentCompactResult
	err := c.Call(ctx, protocol.MAgentCompact, protocol.AgentCompactParams{Agent: agent}, &r)
	return r.Status, err
}

// AddSessionDir puts a directory in the session's working set, which every
// agent shares.
func (c *Client) AddSessionDir(ctx context.Context, id, dir string) error {
	return c.Call(ctx, protocol.MSessionAddDir, protocol.SessionDirParams{ID: id, Dir: dir}, nil)
}

// RemoveSessionDir takes a directory out of it (never the session directory).
func (c *Client) RemoveSessionDir(ctx context.Context, id, dir string) error {
	return c.Call(ctx, protocol.MSessionRemoveDir, protocol.SessionDirParams{ID: id, Dir: dir}, nil)
}

// Variants lists the variant names a model offers.
func (c *Client) Variants(ctx context.Context, modelID string) ([]string, error) {
	var r protocol.VariantsResult
	err := c.Call(ctx, protocol.MVariants, protocol.VariantsParams{Model: modelID}, &r)
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
