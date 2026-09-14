package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// orchestrator implements tools.Orchestrator on top of a session (PRD §6.4).
// Messages, responses and status reach any agent in the session: a child, a
// sibling, or the caller's parent. Lifecycle (cancel, kill) stays with the
// parent that created the agent.
type orchestrator struct{ s *Session }

// senderLabel turns an envelope source into the From shown to the
// recipient: "scout (a1b2c3d4)" for "agent:<id>", "" for humans.
func (s *Session) senderLabel(source string) string {
	id, ok := strings.CutPrefix(source, "agent:")
	if !ok {
		return ""
	}
	// The label carries the full id: models copy it into agent_response,
	// and a shortened one would not resolve.
	if a, ok := s.Agent(id); ok {
		return fmt.Sprintf("%s (%s)", a.Label, id)
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

func (o orchestrator) Spawn(ctx context.Context, parent, archetype, label, task, modelID string, dirs []string) (string, error) {
	a, err := o.s.spawn(ctx, parent, archetype, label, task, modelID, dirs)
	if err != nil {
		return "", err
	}
	return a.ID, nil
}

// Message delivers text to another agent in the session at its next step:
// mid-turn if it is busy (a steer), as a new turn if it is idle. Every agent
// may message every agent; the caller then waits on the answer.
func (o orchestrator) Message(caller, id, text string) error {
	c, err := o.peer(caller, id)
	if err != nil {
		return err
	}
	// The expectation is registered before delivery: a recipient that
	// answers (or hits its turn limit) at once must find its asker waiting.
	from, hasFrom := o.s.Agent(caller)
	if hasFrom {
		from.expect(c.ID)
	}
	if err := c.Steer(context.Background(), text, "agent:"+caller); err != nil {
		if hasFrom {
			from.forget(c.ID)
		}
		return err
	}
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

func (o orchestrator) Kill(parent, id string) error {
	c, err := o.child(parent, id)
	if err != nil {
		return err
	}
	o.s.killTree(c)
	return nil
}

// Respond delivers the caller's answer to another agent in the session; the
// recipient is woken between turns. The caller stays alive.
func (o orchestrator) Respond(caller, to, text string) error {
	c, err := o.peer(caller, to)
	if err != nil {
		return err
	}
	if !c.Alive() {
		return fmt.Errorf("agent %q is %s", to, c.StateOf())
	}
	from, _ := o.s.Agent(caller)
	label := caller
	if from != nil {
		label = fmt.Sprintf("%s (%s)", from.Label, caller)
	}
	// Logged under the resolved id (to may be a prefix): recovery replays
	// the event onto e.Agent.
	if _, err := o.s.host.Append(context.Background(), event.Event{Session: o.s.ID, Agent: c.ID, Type: event.ResponseReceived,
		Payload: event.MustPayload(event.ResponsePayload{From: caller, FromLabel: label, Text: text})}); err != nil {
		return err
	}
	c.deliverResponse(caller, label, text)
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
		out = append(out, tools.ChildStatus{ID: c.ID, Parent: c.Parent, Label: c.Label, Archetype: c.Archetype, State: in.State, Turn: in.Turn, CostUSD: in.CostUSD, Summary: in.Summary, You: c.ID == caller, Dirs: dirs})
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
	return append([]string(nil), a.preset.Spawn...)
}
