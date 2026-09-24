package agent

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/project"
	"github.com/nicodes/stavlos/internal/toolname"
)

// Recover rebuilds a channel from its log (PRD §4.3, §5): the same apply the
// live channel runs, folded over every event, then resume. Nothing restarts
// that was not waiting to: an agent with inputs in its inbox is woken.
func Recover(ctx context.Context, host Host, id, dir string, created time.Time, cfg *config.Effective, events []event.Event) (*Channel, error) {
	return RecoverPaged(ctx, host, id, dir, created, cfg, func(fold func([]event.Event)) error {
		fold(events)
		return nil
	})
}

// RecoverPaged is Recover for a log read a page at a time: read calls fold
// with each page in order. The largest channel is tens of megabytes of
// events, and nothing needs them once they are folded.
func RecoverPaged(ctx context.Context, host Host, id, dir string, created time.Time, cfg *config.Effective, read func(fold func([]event.Event)) error) (*Channel, error) {
	s := New(host, id, dir, cfg, "", "")
	s.Created = created
	var fx effects
	folded := 0
	err := read(func(page []event.Event) {
		for _, e := range page {
			s.st.apply(e, &fx)
		}
		folded += len(page)
	})
	if err != nil {
		s.cancel()
		return nil, err
	}
	if folded == 0 {
		s.cancel()
		return nil, fmt.Errorf("channel %s has no events", id)
	}
	for _, aid := range s.st.order {
		st := s.st.agents[aid]
		parentCtx := s.ctx
		if p := s.agents[st.parent]; p != nil {
			parentCtx = p.ctx
		}
		s.agents[aid] = newAgent(s, aid, st.parent, st.depth, parentCtx)
	}
	if err := s.resume(ctx); err != nil {
		return nil, err
	}
	s.restoreGauges()
	return s, nil
}

// restoreGauges works out again how full each agent's context is. The
// figure is measured at a model call and kept in memory, so after a restart
// every agent read "0%" until it next spoke, which for an idle agent with a
// full window is exactly when the figure matters. The history is the log's;
// the estimate leaves out the system prompt and the tools, which the next
// call adds back.
func (c *Channel) restoreGauges() {
	type reading struct {
		a       *Agent
		modelID string
		history []model.Message
	}
	c.mu.Lock()
	var rs []reading
	for id, a := range c.agents {
		if st := c.st.agents[id]; st != nil && !st.killed && st.model != "" {
			rs = append(rs, reading{a, st.model, st.hist.History()})
		}
	}
	c.mu.Unlock()
	for _, r := range rs {
		_, info, err := c.host.Resolve(r.modelID) // outside the lock: the catalogue is not the channel's
		if err != nil || len(r.history) == 0 {
			continue
		}
		est := project.EstimateTokens(r.history, "", nil)
		c.mu.Lock()
		if r.a.ctxTokens == 0 {
			r.a.ctxTokens, r.a.ctxWindow = est, info.ContextWindow
		}
		c.mu.Unlock()
	}
}

// lostJob is what an agent is told about a job that was running when the
// daemon stopped: its process died with it.
const lostJob = "background job lost in a daemon restart; rerun it if you still need the result"

// resume settles what the daemon's stop left open and starts the agents:
// open turns are aborted, prompts nobody answered withdrawn, a compaction in
// flight failed, and each lost job's result queued for its agent. Killed
// agents, and every agent of an archived channel, stay down.
func (c *Channel) resume(ctx context.Context) error {
	c.mu.Lock()
	var evs []event.Event
	for _, id := range c.st.order {
		st := c.st.agents[id]
		if st.inTurn {
			evs = append(evs, c.event(id, event.TurnAborted, event.TurnPayload{Turn: st.turn}))
		}
		for _, ask := range sortedKeys(st.asks) {
			evs = append(evs, c.event(id, event.AskResolved, event.AskResolvedPayload{ID: ask, Outcome: event.AskWithdrawn}))
		}
		if st.compacting {
			evs = append(evs, c.event(id, event.CompactionFailed, event.CompactionPayload{Error: "interrupted by a daemon restart"}))
		}
		if st.killed || c.st.archived {
			continue
		}
		for _, job := range sortedKeys(st.jobs) {
			evs = append(evs,
				c.event(id, event.JobFinished, event.JobFinishedPayload{ID: job, Summary: lostJob, IsError: true, ExitCode: -1}),
				c.event(id, event.InputQueued, event.Input{ID: NewID("i"), Kind: event.InputJob, Job: job}))
		}
	}
	err := c.commitLocked(ctx, evs...)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	for _, id := range c.st.order {
		a, st := c.agents[id], c.st.agents[id]
		if st.killed || c.st.archived {
			a.kill()
			continue
		}
		a.start()
		if st.startsTurn() {
			a.signal() // what was waiting in its inbox when the daemon stopped
		}
	}
	if c.st.archived {
		c.cancel()
	}
	c.mu.Unlock()
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// missingRoleRole stands in for a role that no longer exists in config:
// read-only, no delegation, no MCP, so a restart or a config reload never
// hands an agent more than its role gave it.
func missingRole(name string) config.Role {
	return config.Role{
		Name: name, Description: "(role no longer exists)", Type: config.TypeAll, Layer: "builtin",
		Tools: []string{toolname.Read},
		Body:  "Your role's definition is gone from the configuration. You can only read files until the human picks a role with /role; say so if asked to do more.",
	}
}

func missingRoleError(name string) string {
	return fmt.Sprintf("role %q no longer exists: running read-only until /role picks another", name)
}
