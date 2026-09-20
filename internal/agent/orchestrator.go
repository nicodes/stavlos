package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// orchestrator implements tools.Orchestrator on top of a channel (PRD §6.4).
// Messages and status reach any agent in the channel: a child, a sibling,
// or the caller's parent. Cancelling stays with the parent.
type orchestrator struct{ c *Channel }

func (o orchestrator) Spawn(ctx context.Context, parent, role, label, task string) (id, name string, err error) {
	a, err := o.c.spawn(ctx, parent, role, label, task, "")
	if err != nil {
		return "", "", err
	}
	return a.ID, a.Name(), nil
}

// Message sends the caller's text to the human or to another agent, as the
// caller says it is: a request (the recipient owes a reply and the caller
// waits; it reaches the recipient at its next model call), a response (it
// settles what the caller owed and wakes the recipient between turns) or
// info (nobody owes or waits, and it never wakes the recipient). Human-facing
// updates do not settle requests: only responses with validated reply_to IDs do.
// Request waits exist as soon as the deliveries are committed.
func (o orchestrator) Message(caller string, recipients []string, text, kind string, replyTo ...string) (string, error) {
	s := o.c
	s.mu.Lock()
	from := s.st.agents[caller]
	if from == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("unknown agent %q", caller)
	}
	targets, names, err := s.messageTargetsLocked(caller, recipients)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	if err := validateReply(from, targets, kind, replyTo); err != nil {
		s.mu.Unlock()
		return "", err
	}
	requestID := ""
	if kind == tools.KindRequest || kind == "" {
		for _, target := range targets {
			if target != tools.User {
				requestID = NewID("r")
				break
			}
		}
	}
	inKind := event.InputRequest
	switch kind {
	case tools.KindResponse:
		inKind = event.InputResponse
	case tools.KindInfo:
		inKind = event.InputInfo
	}
	var evs []event.Event
	for _, target := range targets {
		if target == tools.User {
			message := event.ChatPayload{From: from.name, Text: text, To: names, Kind: tools.KindInfo}
			if kind == tools.KindResponse {
				message.Kind, message.ReplyTo = kind, slices.Clone(replyTo)
				for _, id := range replyTo {
					if debt, ok := from.owedRequest(id); ok && debt.From == tools.User && debt.Post != "" {
						message.Posts = append(message.Posts, debt.Post)
					}
				}
				if len(message.Posts) == 1 {
					message.Post = message.Posts[0]
				}
			}
			evs = append(evs, s.event(caller, event.ChatMessage, message))
			continue
		}
		in := event.Input{ID: NewID("i"), RequestID: requestID, ReplyTo: slices.Clone(replyTo), Kind: inKind, Text: text, From: caller, FromName: from.name, To: names}
		evs = append(evs, s.event(target, event.InputQueued, in))
	}
	err = s.commitLocked(context.Background(), evs...)
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	result := messageResult(names, kind)
	if requestID != "" {
		result += "; request_id: " + requestID
	}
	return result, nil
}

func validateReply(from *agentState, targets []string, kind string, ids []string) error {
	if kind != tools.KindResponse {
		if len(ids) > 0 {
			return fmt.Errorf("reply_to is only valid for a response")
		}
		return nil
	}
	if len(ids) == 0 {
		return fmt.Errorf("responses require reply_to request IDs; use info for updates")
	}
	seen, covered := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("request %q appears more than once in reply_to", id)
		}
		seen[id] = true
		request, ok := from.owedRequest(id)
		if !ok {
			return fmt.Errorf("request %q is not pending for this agent; check agent_status", id)
		}
		if !slices.Contains(targets, request.From) {
			return fmt.Errorf("request %q belongs to %s, who is not a recipient", id, request.FromName)
		}
		covered[request.From] = true
	}
	for _, target := range targets {
		if !covered[target] {
			return fmt.Errorf("recipient %q has no matching request in reply_to", target)
		}
	}
	return nil
}

// Resolve every recipient before committing any delivery. Name/id aliases
// deduplicate by identity, and an invalid recipient rejects the whole send.
func (s *Channel) messageTargetsLocked(caller string, recipients []string) (ids, names []string, err error) {
	seen := map[string]bool{}
	for _, ref := range recipients {
		ref = tools.Recipient(ref)
		id, name := tools.User, tools.User
		if ref != tools.User {
			a, ok := s.resolveLocked(ref)
			if !ok {
				return nil, nil, fmt.Errorf("unknown agent %q", ref)
			}
			if a.id == caller {
				return nil, nil, fmt.Errorf("agent %q is you", ref)
			}
			if a.killed {
				return nil, nil, fmt.Errorf("agent %q is killed", ref)
			}
			id, name = a.id, a.name
		}
		if !seen[id] {
			ids, names = append(ids, id), append(names, name)
			seen[id] = true
		}
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("at least one recipient is required")
	}
	return ids, names, nil
}

func messageResult(names []string, kind string) string {
	if len(names) == 1 && names[0] == tools.User {
		return "message delivered to the user"
	}
	to := strings.Join(names, ", ")
	switch kind {
	case tools.KindResponse:
		return "response delivered to " + to
	case tools.KindInfo:
		if len(names) > 1 {
			return "info delivered to " + to + "; no replies are needed and idle agents are not woken"
		}
		return "info delivered to " + to + "; it needs no reply and does not wake it"
	default:
		if len(names) > 1 {
			return "request delivered to " + to + "; agent responses wake you between turns"
		}
		return "request delivered to " + to + "; its response wakes you between turns"
	}
}

func (o orchestrator) Cancel(parent, id string) error {
	s := o.c
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
	s := o.c
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
		out = append(out, tools.ChildStatus{ID: aid, Parent: a.parent, Name: a.name, Role: a.role, State: string(a.status()), Turn: a.turn, CostUSD: a.cost, You: aid == caller, PendingReplies: a.pendingReplies(), AwaitingReplies: a.awaitingReplies()})
	}
	return out, nil
}

func (o orchestrator) CanSpawn(agent string) (bool, string) {
	s := o.c
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.st.agents[agent]
	if !ok {
		return false, "unknown agent"
	}
	return s.canSpawnLocked(a)
}

func (o orchestrator) Archetypes(agent string) []string {
	s := o.c
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.st.agents[agent]
	if !ok {
		return nil
	}
	return append([]string(nil), s.roleLocked(a).def.Spawn...)
}
