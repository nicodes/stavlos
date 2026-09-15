package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// orchestrator implements tools.Orchestrator on top of a session (PRD §6.4).
// Messages and status reach any agent in the session: a child, a
// sibling, or the caller's parent. Lifecycle (cancel, kill) stays with the
// parent that created the agent.
type orchestrator struct{ s *Session }

// senderLabel turns an envelope source into the From shown to the
// recipient: the sender's name for "agent:<id>", "" for humans. Names are
// unique in the session and never reused, so the name alone addresses it.
func (s *Session) senderLabel(source string) string {
	id, ok := strings.CutPrefix(source, "agent:")
	if !ok {
		return ""
	}
	if a, ok := s.Agent(id); ok {
		return a.LabelNow()
	}
	return id
}

// peer resolves any other live agent in the same session.
func (o orchestrator) peer(caller, id string) (*Agent, error) {
	c, ok := o.s.resolve(id)
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", id)
	}
	if c.ID == caller {
		return nil, fmt.Errorf("agent %q is you", id)
	}
	return c, nil
}

func (o orchestrator) child(parent, id string) (*Agent, error) {
	c, ok := o.s.resolve(id)
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", id)
	}
	if c.Parent != parent {
		return nil, fmt.Errorf("agent %q is not your child", id)
	}
	return c, nil
}

func (o orchestrator) Spawn(ctx context.Context, parent, archetype, label, task, modelID string, dirs []string) (id, name string, err error) {
	a, err := o.s.spawn(ctx, parent, archetype, label, task, modelID, dirs)
	if err != nil {
		return "", "", err
	}
	return a.ID, a.LabelNow(), nil
}

// Message sends the caller's text to the human or to another agent in the
// session. To an agent waiting on the caller it is the answer: delivered
// between turns, settling the wait. To any other agent it is a new message:
// delivered at its next step (mid-turn if it is busy, a new turn if idle),
// and the caller now waits on it.
func (o orchestrator) Message(caller, to, text string) (string, error) {
	from, hasFrom := o.s.Agent(caller)
	if to == tools.User {
		post := "" // the chat post this answers, so the chat threads it under that post
		if hasFrom {
			post = from.currentPost()
		}
		if _, err := o.s.host.Append(context.Background(), event.Event{Session: o.s.ID, Agent: caller, Type: event.MessageToUser,
			Payload: event.MustPayload(event.ChatPayload{From: o.s.senderLabel("agent:" + caller), Text: text, Post: post})}); err != nil {
			return "", err
		}
		if hasFrom {
			from.settle(tools.User)
		}
		return "message delivered to the user", nil
	}
	c, err := o.peer(caller, to)
	if err != nil {
		return "", err
	}
	if !c.Alive() {
		return "", fmt.Errorf("agent %q is %s", to, c.StateOf())
	}
	if c.isAwaiting(caller) {
		if err := o.answer(caller, c, text); err != nil {
			return "", err
		}
		if hasFrom {
			from.settle(c.ID)
		}
		return "answer delivered to " + c.LabelNow(), nil
	}
	// The expectation is registered before delivery: a recipient that
	// answers (or hits its turn limit) at once must find its asker waiting.
	if hasFrom {
		from.expect(c.ID)
	}
	if err := c.Steer(context.Background(), text, "agent:"+caller); err != nil {
		if hasFrom {
			from.forget(c.ID)
		}
		return "", err
	}
	if hasFrom {
		from.settle(c.ID)
	}
	return "message delivered to " + c.LabelNow() + "; its answer wakes you between turns", nil
}

// answer delivers the caller's text to c as an answer: logged on c, then
// put in its mailbox, which wakes it between turns. The caller stays alive.
func (o orchestrator) answer(caller string, c *Agent, text string) error {
	label := o.s.senderLabel("agent:" + caller)
	if _, err := o.s.host.Append(context.Background(), event.Event{Session: o.s.ID, Agent: c.ID, Type: event.ResponseReceived,
		Payload: event.MustPayload(event.ResponsePayload{From: caller, FromLabel: label, Text: text})}); err != nil {
		return err
	}
	c.deliverResponse(caller, label, text)
	return nil
}

func (o orchestrator) Cancel(parent, id string) error {
	c, err := o.child(parent, id)
	if err != nil {
		return err
	}
	c.Cancel()
	return nil
}

// Status describes one agent (any in the session) or, with no id, the
// whole session tree in pre-order.
func (o orchestrator) Status(caller, id string) ([]tools.ChildStatus, error) {
	if _, ok := o.s.Agent(caller); !ok {
		return nil, fmt.Errorf("unknown agent %q", caller)
	}
	var agents []*Agent
	if id != "" {
		c, ok := o.s.resolve(id)
		if !ok {
			return nil, fmt.Errorf("unknown agent %q", id)
		}
		agents = []*Agent{c}
	} else {
		agents = o.s.Agents()
	}
	var out []tools.ChildStatus
	for _, c := range agents {
		in := c.Info()
		var dirs []string
		for _, d := range in.Dirs {
			dirs = append(dirs, d.Path)
		}
		out = append(out, tools.ChildStatus{ID: c.ID, Parent: c.Parent, Label: in.Label, Archetype: in.Archetype, State: string(in.State), Turn: in.Turn, CostUSD: in.CostUSD, Summary: in.Summary, You: c.ID == caller, Dirs: dirs})
	}
	return out, nil
}

func (o orchestrator) CanSpawn(agent string) (bool, string) {
	a, ok := o.s.Agent(agent)
	if !ok {
		return false, "unknown agent"
	}
	return o.s.canSpawn(a)
}

func (o orchestrator) Archetypes(agent string) []string {
	a, ok := o.s.Agent(agent)
	if !ok {
		return nil
	}
	return append([]string(nil), a.Preset().Spawn...)
}
