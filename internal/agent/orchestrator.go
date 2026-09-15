package agent

import (
	"context"
	"fmt"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// orchestrator implements tools.Orchestrator on top of a channel (PRD §6.4).
// Messages and status reach any agent in the channel: a child, a sibling,
// or the caller's parent. Cancelling stays with the parent.
type orchestrator struct{ s *Channel }

func (o orchestrator) Spawn(ctx context.Context, parent, role, label, task, modelID string) (id, name string, err error) {
	a, err := o.s.spawn(ctx, parent, role, label, task, modelID)
	if err != nil {
		return "", "", err
	}
	return a.ID, a.Name(), nil
}

// Message sends the caller's text to the human or to another agent, as the
// caller says it is: a request (the recipient owes a reply and the caller
// waits; it reaches the recipient at its next model call), a response (it
// settles what the caller owed and wakes the recipient between turns) or
// info (nobody owes or waits, and it never wakes the recipient). A message
// to the human is always a response. The bookkeeping is the input's apply,
// so a request's wait exists the moment it is delivered.
func (o orchestrator) Message(caller, to, text, kind string) (string, error) {
	s := o.s
	s.mu.Lock()
	from := s.st.agents[caller]
	if from == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("unknown agent %q", caller)
	}
	if to == tools.User {
		_, err := s.commitLocked(context.Background(), s.event(caller, event.ChatMessage, event.ChatPayload{From: from.name, Text: text, Post: from.lastPost}))
		s.mu.Unlock()
		if err != nil {
			return "", err
		}
		return "message delivered to the user", nil
	}
	c, ok := s.resolveLocked(to)
	switch {
	case !ok:
		s.mu.Unlock()
		return "", fmt.Errorf("unknown agent %q", to)
	case c.id == caller:
		s.mu.Unlock()
		return "", fmt.Errorf("agent %q is you", to)
	case c.killed:
		s.mu.Unlock()
		return "", fmt.Errorf("agent %q is killed", to)
	}
	in := event.Input{ID: NewID("i"), Kind: event.InputRequest, Text: text, From: caller, FromName: from.name}
	result := "request delivered to " + c.name + "; its response wakes you between turns"
	switch kind {
	case tools.KindResponse:
		in.Kind, result = event.InputResponse, "response delivered to "+c.name
	case tools.KindInfo:
		in.Kind, result = event.InputInfo, "info delivered to "+c.name+"; it needs no reply and does not wake it"
	}
	wake, err := s.commitLocked(context.Background(), s.event(c.id, event.InputQueued, in))
	s.mu.Unlock()
	signal(wake)
	if err != nil {
		return "", err
	}
	return result, nil
}

func (o orchestrator) Cancel(parent, id string) error {
	s := o.s
	s.mu.Lock()
	c, ok := s.resolveLocked(id)
	if !ok || c.parent != parent {
		s.mu.Unlock()
		if !ok {
			return fmt.Errorf("unknown agent %q", id)
		}
		return fmt.Errorf("agent %q is not your child", id)
	}
	h := s.agents[c.id]
	s.mu.Unlock()
	h.Cancel()
	return nil
}

// Status describes one agent (any in the channel) or, with no id, the
// whole channel tree in pre-order.
func (o orchestrator) Status(caller, id string) ([]tools.ChildStatus, error) {
	s := o.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.st.agents[caller]; !ok {
		return nil, fmt.Errorf("unknown agent %q", caller)
	}
	ids := s.preorderLocked()
	if id != "" {
		c, ok := s.resolveLocked(id)
		if !ok {
			return nil, fmt.Errorf("unknown agent %q", id)
		}
		ids = []string{c.id}
	}
	out := make([]tools.ChildStatus, 0, len(ids))
	for _, aid := range ids {
		a := s.st.agents[aid]
		out = append(out, tools.ChildStatus{ID: aid, Parent: a.parent, Label: a.name, Archetype: a.role, State: string(a.status()), Turn: a.turn, CostUSD: a.cost, You: aid == caller})
	}
	return out, nil
}

func (o orchestrator) CanSpawn(agent string) (bool, string) {
	s := o.s
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.st.agents[agent]
	if !ok {
		return false, "unknown agent"
	}
	return s.canSpawnLocked(a)
}

func (o orchestrator) Archetypes(agent string) []string {
	s := o.s
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.st.agents[agent]
	if !ok {
		return nil
	}
	return append([]string(nil), s.roleLocked(a).preset.Spawn...)
}
