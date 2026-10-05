package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// BoardHost is optional on lightweight hosts; the daemon persists and routes boards.
type BoardHost interface {
	BoardCreate(context.Context, string, string, string, []string) (string, error)
	BoardList(context.Context, string, string) (string, error)
	BoardRead(context.Context, string, string, string, int64, int) (string, error)
	BoardMessage(context.Context, string, string, string, []string, string, string, []string) (string, error)
	BoardReport(context.Context, string, string, string, []string, string, []string) error
}
type boardAPI struct{ a *Agent }

func (b boardAPI) host() (BoardHost, error) {
	h, ok := b.a.c.host.(BoardHost)
	if !ok {
		return nil, fmt.Errorf("shared channels are not available")
	}
	return h, nil
}
func (b boardAPI) Create(ctx context.Context, caller, name string, members []string) (string, error) {
	h, e := b.host()
	if e != nil {
		return "", e
	}
	return h.BoardCreate(ctx, b.a.c.ID, caller, name, members)
}
func (b boardAPI) List(ctx context.Context, caller string) (string, error) {
	h, e := b.host()
	if e != nil {
		return "", e
	}
	return h.BoardList(ctx, b.a.c.ID, caller)
}
func (b boardAPI) Read(ctx context.Context, caller, ref string, from int64, limit int) (string, error) {
	h, e := b.host()
	if e != nil {
		return "", e
	}
	return h.BoardRead(ctx, b.a.c.ID, caller, ref, from, limit)
}
func (b boardAPI) Message(ctx context.Context, caller, ref string, to []string, text, kind string, reply []string) (string, error) {
	h, e := b.host()
	if e != nil {
		return "", e
	}
	return h.BoardMessage(ctx, b.a.c.ID, caller, ref, to, text, kind, reply)
}

func cloneBoard(b *event.BoardInfo) *event.BoardInfo {
	if b == nil {
		return nil
	}
	out := *b
	out.Members = slices.Clone(b.Members)
	return &out
}

// Board returns an immutable snapshot of a channel's shared membership.
func (c *Channel) Board() *event.BoardInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneBoard(c.st.board)
}

// BoardMembers resolves existing descendants; the manager always joins its board.
func (c *Channel) BoardMembers(caller string, refs []string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.st.agents[caller]
	if a == nil || a.killed {
		return nil, fmt.Errorf("unknown or killed agent %q", caller)
	}
	if !canOrchestrate(c.roleLocked(a)) {
		return nil, fmt.Errorf("this role cannot create shared channels")
	}
	ids := []string{caller}
	for _, ref := range refs {
		if ref == "self" {
			continue
		}
		member, ok := c.resolveLocked(ref)
		if !ok || member.killed {
			return nil, fmt.Errorf("unknown or killed member %q", ref)
		}
		if !c.descendantLocked(member.id, caller) {
			return nil, fmt.Errorf("member %q is outside your subtree", ref)
		}
		if !slices.Contains(ids, member.id) {
			ids = append(ids, member.id)
		}
	}
	return ids, nil
}
func (c *Channel) descendantLocked(id, owner string) bool {
	for a := c.st.agents[id]; a != nil; a = c.st.agents[a.parent] {
		if a.id == owner {
			return true
		}
	}
	return false
}

// StartBoard records a channel without spawning another root agent.
func (c *Channel) StartBoard(ctx context.Context, name string, board event.BoardInfo) error {
	return c.commit(ctx, c.event("", event.ChannelCreated, event.ChannelCreatedPayload{Name: name, Dir: c.Dir(), Board: cloneBoard(&board)}))
}

// DeliverBoard commits the public post and all inbox deliveries atomically.
// Locks always run execution channel -> board, and neither lock is acquired by
// the journal callback. Members' execution contexts stay in the source channel.
func (c *Channel) DeliverBoard(ctx context.Context, board *Channel, caller string, to []string, text, kind string, reply []string, human string, system ...bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	board.mu.Lock()
	defer board.mu.Unlock()
	b := board.st.board
	if b == nil || b.Source != c.ID {
		return "", fmt.Errorf("channel is not a board of this execution channel")
	}
	if board.st.archived || c.st.archived || board.stopped || c.stopped {
		return "", fmt.Errorf("channel is archived or stopped")
	}
	from := c.st.agents[caller]
	if human == "" && (from == nil || from.killed || !slices.Contains(b.Members, caller)) {
		return "", fmt.Errorf("you are not a live member of this channel")
	}
	targets, names, err := c.boardTargetsLocked(b, caller, to)
	if err != nil {
		return "", err
	}
	replyKind := kind
	if kind == tools.KindResponseRequest {
		replyKind = tools.KindResponse
	}
	if human == "" {
		if err := validateReplyIn(from, targets, replyKind, reply, board.ID, system...); err != nil {
			return "", err
		}
	}
	expected := kind == tools.KindRequest || kind == tools.KindResponseRequest
	if expected && len(targets) == 0 {
		return "", fmt.Errorf("requests require explicit recipients")
	}
	post := NewID("post")
	requestID := ""
	if expected {
		requestID = NewID("r")
	}
	fromName := human
	if from != nil {
		fromName = from.name
	}
	p := event.ChatPayload{ID: post, RequestID: requestID, From: fromName, Text: text, To: names, Kind: replyKind, ReplyTo: slices.Clone(reply)}
	typ := event.ChatMessage
	if human != "" {
		typ = event.ChatPosted
	}
	evs := []event.Event{board.event(caller, typ, p)}
	evs = append(evs, c.boardDeliveriesLocked(b, board.ID, board.st.name, caller, targets, p, kind, human)...)
	// Replies to a human settle the source channel's existing debt as well.
	if slices.Contains(targets, tools.User) && replyKind == tools.KindResponse {
		evs = append(evs, c.event(caller, event.ChatMessage, p))
	}
	if err := c.commitBoardEventsLocked(ctx, board, evs); err != nil {
		return "", err
	}
	result := "posted in #" + board.st.name
	if requestID != "" {
		result += "; request_id: " + requestID
	}
	return result, nil
}

// commitBoardEventsLocked applies the one transaction to both channel folds.
// The caller holds the source lock followed by the board lock.
func (c *Channel) commitBoardEventsLocked(ctx context.Context, board *Channel, evs []event.Event) error {
	out, err := c.host.Append(ctx, evs...)
	if err != nil {
		return err
	}
	var fx effects
	for _, e := range out {
		if e.Channel == c.ID {
			c.st.apply(e, &fx)
		} else {
			board.st.apply(e, &effects{})
		}
	}
	c.dispatchLocked(fx)
	return nil
}
func (c *Channel) boardDeliveriesLocked(b *event.BoardInfo, channel, name, caller string, targets []string, p event.ChatPayload, kind, human string) []event.Event {
	var evs []event.Event
	expected := kind == tools.KindRequest || kind == tools.KindResponseRequest
	for _, id := range b.Members {
		a := c.st.agents[id]
		if a == nil || a.killed || id == caller {
			continue
		}
		addressed := slices.Contains(targets, id)
		in := event.Input{ID: NewID("i"), Kind: event.InputInfo, Text: p.Text, From: caller, FromName: p.From, To: p.To, Channel: channel, ChannelName: name}
		if human != "" {
			in.FromName = "user"
		}
		if addressed {
			in.RequestID = p.RequestID
			switch kind {
			case tools.KindRequest:
				in.Kind = event.InputRequest
			case tools.KindResponse, tools.KindResponseRequest:
				in.Kind = event.InputResponse
				in.ReplyTo = slices.Clone(p.ReplyTo)
				in.ExpectResponse = kind == tools.KindResponseRequest
			case tools.KindSteer:
				in.Kind = event.InputAgentSteer
			}
			if human != "" {
				in.From = ""
				in.FromName = "user"
				in.Kind = event.InputSteer
				if expected {
					in.Post = p.ID
				} else {
					in.RequestID = ""
				}
			}
		}
		evs = append(evs, c.event(id, event.InputQueued, in))
	}
	return evs
}
func (c *Channel) boardTargetsLocked(b *event.BoardInfo, caller string, refs []string) ([]string, []string, error) {
	var ids, names []string
	for _, ref := range refs {
		if tools.Recipient(ref) == tools.User {
			if !slices.Contains(ids, tools.User) {
				ids = append(ids, tools.User)
				names = append(names, tools.User)
			}
			continue
		}
		a, ok := c.resolveLocked(ref)
		if !ok || a.killed || !slices.Contains(b.Members, a.id) {
			return nil, nil, fmt.Errorf("%q is not a live member of this channel", ref)
		}
		if a.id == caller {
			return nil, nil, fmt.Errorf("agent %q is you", ref)
		}
		if !slices.Contains(ids, a.id) {
			ids = append(ids, a.id)
			names = append(names, a.name)
		}
	}
	return ids, names, nil
}

// BoardTree returns live member identities from their source execution context.
func (c *Channel) BoardTree(b *event.BoardInfo) []protocol.AgentInfo {
	var out []protocol.AgentInfo
	for _, a := range c.Tree() {
		if slices.Contains(b.Members, a.ID) {
			out = append(out, a)
		}
	}
	return out
}

// BoardHumanPost uses explicit @recipients for requests; unaddressed posts are passive.
func (c *Channel) BoardHumanPost(ctx context.Context, board *Channel, text, source string) ([]string, error) {
	refs, body := protocol.Addressees(text)
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("empty message")
	}
	kind := tools.KindInfo
	if len(refs) > 0 {
		kind = tools.KindRequest
	}
	_, err := c.DeliverBoard(ctx, board, "", refs, body, kind, nil, source)
	return refs, err
}
