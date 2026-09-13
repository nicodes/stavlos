package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/tools"
)

// orchestrator implements tools.Orchestrator on top of a session (PRD §6.4).
// Prompt and status reach any agent in the session: a child, a sibling, or
// the caller's parent. Steer is the main agent's alone. Lifecycle (cancel,
// kill, result) stays with the parent that created the agent.
type orchestrator struct{ s *Session }

// senderLabel turns an envelope source into the From shown to the
// recipient: "scout (a1b2c3d4)" for "agent:<id>", "" for humans.
func (s *Session) senderLabel(source string) string {
	id, ok := strings.CutPrefix(source, "agent:")
	if !ok {
		return ""
	}
	if a, ok := s.Agent(id); ok {
		return fmt.Sprintf("%s (%s)", a.Label, shortID(id))
	}
	return shortID(id)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// peer resolves any other live agent in the same session.
func (o orchestrator) peer(caller, id string) (*Agent, error) {
	if id == caller {
		return nil, fmt.Errorf("agent %q is you", id)
	}
	c, ok := o.s.Agent(id)
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", id)
	}
	return c, nil
}

func (o orchestrator) child(parent, id string) (*Agent, error) {
	c, ok := o.s.Agent(id)
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", id)
	}
	if c.Parent != parent {
		return nil, fmt.Errorf("agent %q is not your child", id)
	}
	return c, nil
}

func (o orchestrator) Spawn(ctx context.Context, parent, archetype, label, task, modelID string) (string, error) {
	a, err := o.s.spawn(ctx, parent, archetype, label, task, modelID)
	if err != nil {
		return "", err
	}
	return a.ID, nil
}

func (o orchestrator) Send(caller, id, text string) error {
	c, err := o.peer(caller, id)
	if err != nil {
		return err
	}
	return c.Prompt(context.Background(), text, "agent:"+caller)
}

// Steer is the main agent's alone: a steer cuts into a running turn, which
// is too invasive for a subagent to do to a peer.
func (o orchestrator) Steer(caller, id, text string) error {
	if a, ok := o.s.Agent(caller); !ok || a.Parent != "" {
		return fmt.Errorf("only the main agent can steer; use agent_prompt")
	}
	c, err := o.peer(caller, id)
	if err != nil {
		return err
	}
	return c.Steer(context.Background(), text, "agent:"+caller)
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

// Result returns a finished child's result and marks it consumed so the
// ChildFinished envelope is not delivered twice.
func (o orchestrator) Result(parent, id string) (tools.ChildResult, bool, error) {
	c, err := o.child(parent, id)
	if err != nil {
		return tools.ChildResult{}, false, err
	}
	p, _ := o.s.Agent(parent)
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.results[id]
	if !ok {
		if !c.Alive() {
			return tools.ChildResult{ID: id, Label: c.Label, Status: string(c.StateOf()), Summary: "(no result)"}, true, nil
		}
		return tools.ChildResult{}, false, nil
	}
	delete(p.results, id)
	kept := p.childDone[:0]
	for _, x := range p.childDone {
		if x.ID != id {
			kept = append(kept, x)
		}
	}
	p.childDone = kept
	return r, true, nil
}

// Status describes one agent (any in the session) or, with no id, the
// whole session tree in pre-order.
func (o orchestrator) Status(caller, id string) ([]tools.ChildStatus, error) {
	if _, ok := o.s.Agent(caller); !ok {
		return nil, fmt.Errorf("unknown agent %q", caller)
	}
	var agents []*Agent
	if id != "" {
		c, ok := o.s.Agent(id)
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
		out = append(out, tools.ChildStatus{ID: c.ID, Parent: c.Parent, Label: c.Label, Archetype: c.Archetype, State: in.State, Turn: in.Turn, CostUSD: in.CostUSD, Summary: in.Summary, Monitored: in.Monitored, You: c.ID == caller})
	}
	return out, nil
}

func (o orchestrator) Finish(agent, summary, status string, artifacts []tools.Artifact) error {
	a, ok := o.s.Agent(agent)
	if !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	return a.setFinished(summary, status, artifacts)
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
