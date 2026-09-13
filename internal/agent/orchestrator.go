package agent

import (
	"context"
	"fmt"

	"github.com/nicodes/stavlos/internal/tools"
)

// orchestrator implements tools.Orchestrator on top of a session (PRD §6.4).
// Every method checks that the target is a child of the caller.
type orchestrator struct{ s *Session }

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

func (o orchestrator) Send(parent, id, text string) error {
	c, err := o.child(parent, id)
	if err != nil {
		return err
	}
	return c.Prompt(context.Background(), text, "agent:"+parent)
}

func (o orchestrator) Steer(parent, id, text string) error {
	c, err := o.child(parent, id)
	if err != nil {
		return err
	}
	return c.Steer(context.Background(), text, "agent:"+parent)
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

// Monitor marks the parent's turn to end after the current tool batch. The
// children already deliver ChildFinished envelopes; nothing else is needed.
func (o orchestrator) Monitor(parent string, ids []string) ([]tools.ChildStatus, error) {
	p, ok := o.s.Agent(parent)
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", parent)
	}
	if len(ids) == 0 {
		for _, cid := range p.Children() {
			if c, ok := o.s.Agent(cid); ok && c.Alive() {
				ids = append(ids, cid)
			}
		}
	}
	var out []tools.ChildStatus
	for _, id := range ids {
		c, err := o.child(parent, id)
		if err != nil {
			return nil, err
		}
		in := c.Info()
		out = append(out, tools.ChildStatus{ID: c.ID, Label: c.Label, Archetype: c.Archetype, State: in.State, Turn: in.Turn, CostUSD: in.CostUSD})
	}
	if len(out) > 0 {
		p.mu.Lock()
		p.yieldFlag = true
		p.mu.Unlock()
	}
	return out, nil
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
	// drop the pending inbox copy
	kept := p.childDone[:0]
	for _, x := range p.childDone {
		if x.ID != id {
			kept = append(kept, x)
		}
	}
	p.childDone = kept
	return r, true, nil
}

func (o orchestrator) Status(parent, id string) ([]tools.ChildStatus, error) {
	p, ok := o.s.Agent(parent)
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", parent)
	}
	ids := p.Children()
	if id != "" {
		ids = []string{id}
	}
	var out []tools.ChildStatus
	for _, cid := range ids {
		c, err := o.child(parent, cid)
		if err != nil {
			return nil, err
		}
		in := c.Info()
		out = append(out, tools.ChildStatus{ID: c.ID, Label: c.Label, Archetype: c.Archetype, State: in.State, Turn: in.Turn, CostUSD: in.CostUSD, Summary: in.Summary})
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
