package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// PreparedPost is a concrete delivery plan, resolved before the agent runtime
// admits it. It contains no protocol method, HTTP state or owner capability.
type PreparedPost struct {
	ID, RequestID, Text, Kind string
	Targets, Names            []string
}

func (p PreparedPost) validate() error {
	if !strings.HasPrefix(p.ID, "post") || len(p.ID) > 64 || strings.TrimSpace(p.Text) == "" || len(p.Text) > 4<<20 {
		return fmt.Errorf("invalid prepared input")
	}
	if p.Kind != tools.KindRequest && p.Kind != tools.KindInfo {
		return fmt.Errorf("invalid prepared input kind")
	}
	if len(p.Targets) != len(p.Names) || len(p.Targets) > 1024 {
		return fmt.Errorf("invalid prepared input recipients")
	}
	return nil
}

// AcceptPreparedPost atomically admits an already-routed input into the model
// runtime's existing journal and folds. It rechecks live identities under the
// same lock as model execution; it never resolves browser mentions or RPCs.
func (c *Channel) AcceptPreparedPost(ctx context.Context, p PreparedPost) error {
	if err := p.validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.archived || c.stopped || c.st.board != nil || len(p.Targets) == 0 || p.Kind != tools.KindRequest {
		return fmt.Errorf("execution channel cannot accept this input")
	}
	if err := c.preparedTargetsLocked(p, nil); err != nil {
		return err
	}
	evs := []event.Event{c.event("", event.ChatPosted, event.ChatPayload{ID: p.ID, RequestID: p.RequestID, Kind: p.Kind, From: "human:web", Text: p.Text, To: p.Names})}
	for _, id := range p.Targets {
		evs = append(evs, c.event(id, event.InputQueued, event.Input{ID: NewID("i"), RequestID: p.RequestID, Kind: event.InputSteer, Text: p.Text, Post: p.ID, To: p.Names}))
	}
	return c.commitLocked(ctx, evs...)
}
func (c *Channel) preparedTargetsLocked(p PreparedPost, members []string) error {
	seen := map[string]bool{}
	for i, id := range p.Targets {
		if seen[id] {
			return fmt.Errorf("duplicate input recipient")
		}
		seen[id] = true
		if members != nil && id == tools.User && p.Names[i] == tools.User {
			continue
		}
		a := c.st.agents[id]
		if a == nil || a.killed || a.name != p.Names[i] {
			return fmt.Errorf("agent identity changed before input admission")
		}
		if members != nil && !slices.Contains(members, id) {
			return fmt.Errorf("recipient is no longer a member of this channel")
		}
	}
	return nil
}

// AcceptPreparedBoard uses the shared agent-runtime delivery primitive after
// Rust has chosen the board, recipients, text, kind and request identity.
func (c *Channel) AcceptPreparedBoard(ctx context.Context, board *Channel, p PreparedPost) error {
	if err := p.validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	board.mu.Lock()
	defer board.mu.Unlock()
	b := board.st.board
	if b == nil || b.Source != c.ID || board.st.archived || c.st.archived || board.stopped || c.stopped {
		return fmt.Errorf("shared channel cannot accept this input")
	}
	if err := c.preparedTargetsLocked(p, b.Members); err != nil {
		return err
	}
	if p.Kind == tools.KindRequest && len(p.Targets) == 0 {
		return fmt.Errorf("requests require explicit recipients")
	}
	payload := event.ChatPayload{ID: p.ID, RequestID: p.RequestID, Kind: p.Kind, From: "human:web", Text: p.Text, To: p.Names}
	evs := []event.Event{board.event("", event.ChatPosted, payload)}
	evs = append(evs, c.boardDeliveriesLocked(b, board.ID, board.st.name, "", p.Targets, payload, p.Kind, "human:web")...)
	return c.commitBoardEventsLocked(ctx, board, evs)
}
