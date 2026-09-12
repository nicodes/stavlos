package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// Recover rebuilds a session from its log (PRD §4.3, §5). Any turn that was
// open when the daemon stopped gets a TurnAborted event; agents come back
// idle with their unconsumed prompts and steers re-queued. Nothing restarts
// automatically.
func Recover(ctx context.Context, host Host, id, dir string, created time.Time, cfg *config.Effective, events []event.Event) (*Session, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("session %s has no events", id)
	}
	s := New(host, id, dir, cfg, "", "")
	s.Created = created
	type open struct{ turn int }
	openTurns := map[string]*open{}
	pendingPrompts := map[string][]queued{}
	pendingSteers := map[string][]queued{}
	finished := map[string]bool{}

	for _, e := range events {
		switch e.Type {
		case event.SessionCreated:
			var p event.SessionCreatedPayload
			_ = e.Decode(&p)
			s.model, s.rootArch = p.Model, p.RootAgent
			if s.Dir == "" {
				s.Dir = p.Dir
			}
		case event.SessionArchived:
			s.archived = true
		case event.SessionModelChanged:
			var p event.ModelChangedPayload
			_ = e.Decode(&p)
			s.model = p.Model
		case event.AgentSpawned:
			var p event.AgentSpawnedPayload
			_ = e.Decode(&p)
			preset, ok := cfg.Presets[p.Archetype]
			if !ok {
				preset = config.Preset{Name: p.Archetype, Description: "(preset no longer exists)", Tools: []string{"read", "grep", "glob"}, Loop: "default"}
			}
			a := newAgent(s, p.ID, p.Parent, p.Archetype, p.Label, p.Model, p.Depth, preset)
			if par, ok := s.agents[p.Parent]; ok {
				a.ctx, a.kill = context.WithCancel(par.ctx)
				par.children = append(par.children, a.ID)
			} else {
				a.ctx, a.kill = context.WithCancel(s.ctx)
			}
			s.agents[a.ID] = a
			s.order = append(s.order, a.ID)
		case event.AgentModelChanged:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.ModelChangedPayload
				_ = e.Decode(&p)
				a.modelID = p.Model
			}
		case event.PromptQueued:
			var p event.TextPayload
			_ = e.Decode(&p)
			pendingPrompts[e.Agent] = append(pendingPrompts[e.Agent], queued{p.Text, p.Source})
		case event.SteerReceived:
			var p event.TextPayload
			_ = e.Decode(&p)
			pendingSteers[e.Agent] = append(pendingSteers[e.Agent], queued{p.Text, p.Source})
		case event.UserMessage:
			var p event.UserMessagePayload
			_ = e.Decode(&p)
			switch p.Kind {
			case "prompt":
				if q := pendingPrompts[e.Agent]; len(q) > 0 {
					pendingPrompts[e.Agent] = q[1:]
				}
			case "steer":
				if q := pendingSteers[e.Agent]; len(q) > 0 {
					pendingSteers[e.Agent] = q[1:]
				}
			}
		case event.TurnStarted:
			var p event.TurnPayload
			_ = e.Decode(&p)
			openTurns[e.Agent] = &open{turn: p.Turn}
			if a, ok := s.agents[e.Agent]; ok {
				a.turn = p.Turn
			}
		case event.TurnEnded, event.TurnAborted:
			delete(openTurns, e.Agent)
		case event.Usage:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.UsagePayload
				_ = e.Decode(&p)
				a.usage.tokens += p.Usage.InputTokens + p.Usage.OutputTokens
				a.usage.cost += p.CostUSD
			}
		case event.AgentFinished:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.AgentFinishedPayload
				_ = e.Decode(&p)
				a.state = StateFinished
				a.finished = &tools.ChildResult{ID: a.ID, Label: a.Label, Status: p.Status, Summary: p.Summary}
				finished[a.ID] = true
				if par, ok := s.agents[a.Parent]; ok {
					par.results[a.ID] = *a.finished
				}
			}
		case event.AgentKilled:
			if a, ok := s.agents[e.Agent]; ok {
				a.state = StateKilled
				finished[a.ID] = true
			}
		}
		if a, ok := s.agents[e.Agent]; ok && e.Agent != "" {
			a.events = append(a.events, e)
		}
	}

	// Close open turns and start survivors.
	for _, id := range s.order {
		a := s.agents[id]
		if ot, ok := openTurns[id]; ok && !finished[id] {
			e, err := host.Append(ctx, event.Event{Session: s.ID, Agent: id, Type: event.TurnAborted, Payload: event.MustPayload(event.TurnPayload{Turn: ot.turn})})
			if err != nil {
				return nil, err
			}
			a.events = append(a.events, e)
		}
		if finished[id] {
			a.closeDone()
			a.kill()
			continue
		}
		a.prompts = pendingPrompts[id]
		a.steers = pendingSteers[id]
		if s.archived {
			a.state = StateKilled
			a.closeDone()
			a.kill()
			continue
		}
		a.start()
		if len(a.prompts)+len(a.steers) > 0 {
			a.signal()
		}
	}
	if s.archived {
		s.cancel()
	}
	return s, nil
}
