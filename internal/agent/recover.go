package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
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
	openMonitors := map[string]event.MonitorStartedPayload{} // id → spec, still running at shutdown
	monitorOwner := map[string]string{}
	pendingPrompts := map[string][]queued{}
	pendingResponses := map[string][]response{}
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
		case event.SessionYoloChanged:
			var p event.YoloPayload
			_ = e.Decode(&p)
			s.yolo = p.On
		case event.AgentSpawned:
			var p event.AgentSpawnedPayload
			_ = e.Decode(&p)
			arch := p.Archetype
			preset, ok := cfg.Presets[arch]
			if !ok {
				// The preset this agent was created with is gone (removed,
				// renamed, or a former built-in): it carries on as the
				// configured root preset rather than a crippled read-only one.
				arch = cfg.RootAgent
				preset, ok = cfg.Presets[arch]
				if !ok {
					preset = config.Preset{Name: p.Archetype, Description: "(preset no longer exists)", Tools: []string{"read", "bash"}, Loop: "default"}
					arch = p.Archetype
				}
			}
			a := newAgent(s, p.ID, p.Parent, arch, p.Label, p.Model, p.Depth, preset)
			if par, ok := s.agents[p.Parent]; ok {
				a.ctx, a.kill = context.WithCancel(par.ctx)
				par.children = append(par.children, a.ID)
			} else {
				a.ctx, a.kill = context.WithCancel(s.ctx)
			}
			s.agents[a.ID] = a
			s.order = append(s.order, a.ID)
		case event.AgentRoleChanged:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.RoleChangedPayload
				_ = e.Decode(&p)
				if preset, ok := cfg.Presets[p.Role]; ok {
					a.preset = preset
					a.Archetype = p.Role
				} else if preset, ok := cfg.Presets[cfg.RootAgent]; ok {
					a.preset = preset // the role it switched to is gone: same fallback as above
					a.Archetype = cfg.RootAgent
				}
				if p.Label != "" {
					a.Label = p.Label
				}
			}
		case event.AgentModelChanged:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.ModelChangedPayload
				_ = e.Decode(&p)
				a.modelID = p.Model
			}
		case event.AgentVariantChanged:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.VariantChangedPayload
				_ = e.Decode(&p)
				a.variant = p.Variant
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
				// A consumed prompt came from the prompt queue, or from a
				// steer that arrived while idle (logged as a prompt).
				if q := pendingPrompts[e.Agent]; len(q) > 0 && q[0].text == p.Text {
					pendingPrompts[e.Agent] = q[1:]
				} else if q := pendingSteers[e.Agent]; len(q) > 0 && q[0].text == p.Text {
					pendingSteers[e.Agent] = q[1:]
				} else if q := pendingPrompts[e.Agent]; len(q) > 0 {
					pendingPrompts[e.Agent] = q[1:]
				}
			case "steer":
				if q := pendingSteers[e.Agent]; len(q) > 0 {
					pendingSteers[e.Agent] = q[1:]
				}
			case "agent_response":
				if q := pendingResponses[e.Agent]; len(q) > 0 {
					pendingResponses[e.Agent] = q[1:]
				}
			}
		case event.MonitorStarted:
			var p event.MonitorStartedPayload
			_ = e.Decode(&p)
			openMonitors[p.ID] = p
			monitorOwner[p.ID] = e.Agent
		case event.MonitorFired, event.MonitorStopped:
			var p struct {
				ID string `json:"id"`
			}
			_ = e.Decode(&p)
			delete(openMonitors, p.ID)
		case event.MonitorArmed, event.MonitorDisarmed:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.MonitorPayload
				_ = e.Decode(&p)
				for _, id := range p.IDs {
					if e.Type == event.MonitorArmed {
						a.armed[id] = true
					} else {
						delete(a.armed, id)
					}
				}
			}
		case event.TurnStarted:
			var p event.TurnPayload
			_ = e.Decode(&p)
			openTurns[e.Agent] = &open{turn: p.Turn}
			if a, ok := s.agents[e.Agent]; ok {
				a.turn = p.Turn
				a.lastError = ""
			}
		case event.TurnEnded, event.TurnAborted:
			delete(openTurns, e.Agent)
			if a, ok := s.agents[e.Agent]; ok && e.Type == event.TurnEnded {
				var p event.TurnEndedPayload
				_ = e.Decode(&p)
				if p.Reason == "error" {
					a.lastError = p.Error
				}
			}
		case event.Usage:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.UsagePayload
				_ = e.Decode(&p)
				a.usage.tokens += p.Usage.InputTokens + p.Usage.OutputTokens
				a.usage.cost += p.CostUSD
			}
		case event.ResponseReceived:
			var p event.ResponsePayload
			_ = e.Decode(&p)
			pendingResponses[e.Agent] = append(pendingResponses[e.Agent], response{p.From, p.FromLabel, p.Text})
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

	// Background jobs that were running at shutdown: the process died with
	// the daemon, so the owner is told the job was lost.
	for id, p := range openMonitors {
		a, ok := s.agents[monitorOwner[id]]
		if !ok || finished[a.ID] || s.archived {
			continue
		}
		{
			res := event.MonitorFiredPayload{ID: id, Kind: p.Kind, Label: p.Label, Summary: "background job lost in a daemon restart; rerun it if you still need the result", IsError: true, ExitCode: -1}
			e, err := host.Append(ctx, event.Event{Session: s.ID, Agent: a.ID, Type: event.MonitorFired, Payload: event.MustPayload(res)})
			if err == nil {
				a.events = append(a.events, e)
			}
			a.monDone = append(a.monDone, res)
			if a.armed[id] {
				delete(a.armed, id)
				a.wakes[id] = true
			}
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
		a.responses = pendingResponses[id]
		for _, r := range a.responses {
			a.wakes["response:"+r.from] = true
		}
		for cid := range a.armed {
			if c, ok := s.agents[cid]; ok && !c.Alive() {
				a.wakes[cid] = true
			}
		}
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
