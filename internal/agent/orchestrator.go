package agent

import (
	"context"
	"fmt"

	"github.com/nicodes/stavlos/internal/event"
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

// Monitor arms a wake for the listed (or all live) children and marks the
// parent's turn to end after the current tool batch. A child that already
// finished still gets delivered: the yield starts a turn that drains it.
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
	var armed []string
	for _, id := range ids {
		c, err := o.child(parent, id)
		if err != nil {
			return nil, err
		}
		in := c.Info()
		out = append(out, tools.ChildStatus{ID: c.ID, Label: c.Label, Archetype: c.Archetype, State: in.State, Turn: in.Turn, CostUSD: in.CostUSD, Monitored: true})
		if c.Alive() {
			armed = append(armed, id)
		}
	}
	p.mu.Lock()
	for _, id := range armed {
		p.armed[id] = true
	}
	// A listed child that already finished is delivered right away: yield
	// and wake so the next turn starts with its result.
	pending := false
	for _, r := range p.childDone {
		for _, id := range ids {
			if r.ID == id {
				pending = true
			}
		}
	}
	if len(out) > 0 || pending {
		p.yieldFlag = true
	}
	if pending {
		p.wakeFlag = true
	}
	p.mu.Unlock()
	if len(armed) > 0 {
		_, _ = p.record(context.Background(), event.MonitorArmed, event.MonitorPayload{IDs: armed})
	}
	return out, nil
}

// Unmonitor disarms wakes; children and results are untouched.
func (o orchestrator) Unmonitor(parent string, ids []string) ([]string, error) {
	p, ok := o.s.Agent(parent)
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", parent)
	}
	p.mu.Lock()
	if len(ids) == 0 {
		for id := range p.armed {
			ids = append(ids, id)
		}
	}
	var disarmed []string
	for _, id := range ids {
		if p.armed[id] {
			delete(p.armed, id)
			disarmed = append(disarmed, id)
		}
	}
	p.mu.Unlock()
	if len(disarmed) > 0 {
		_, _ = p.record(context.Background(), event.MonitorDisarmed, event.MonitorPayload{IDs: disarmed})
	}
	return disarmed, nil
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
		out = append(out, tools.ChildStatus{ID: c.ID, Label: c.Label, Archetype: c.Archetype, State: in.State, Turn: in.Turn, CostUSD: in.CostUSD, Summary: in.Summary, Monitored: in.Monitored})
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
