package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
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
	askTargets := map[string]string{} // agent_message call id → asked agent, while the call is open
	pendingSteers := map[string][]queued{}
	finished := map[string]bool{}
	missingRole := map[string]string{} // agent id → why it runs read-only (set after replay: turns clear lastError)

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
		case event.SessionYoloChanged: // legacy logs
			var p event.YoloPayload
			_ = e.Decode(&p)
			s.mode = protocol.ModeAsk
			if p.On {
				s.mode = protocol.ModeYolo
			}
		case event.SessionModeChanged:
			var p event.ModePayload
			_ = e.Decode(&p)
			s.mode = p.Mode
		case event.AgentSpawned:
			var p event.AgentSpawnedPayload
			_ = e.Decode(&p)
			preset, ok := cfg.Presets[p.Archetype]
			if !ok {
				// The role this agent was created with is gone (removed or
				// renamed). Recovery never widens what an agent may do: it
				// comes back read-only, says so, and waits for /role.
				preset = missingRolePreset(p.Archetype)
			}
			a := newAgent(s, p.ID, p.Parent, p.Archetype, p.Label, p.Model, p.Depth, preset)
			if !ok {
				missingRole[a.ID] = missingRoleError(p.Archetype)
			}
			for _, d := range p.Dirs {
				a.extraDirs = append(a.extraDirs, dirEntry{d, "grant"})
			}
			if par, ok := s.agents[p.Parent]; ok {
				a.ctx, a.kill = context.WithCancel(par.ctx)
				par.children = append(par.children, a.ID)
				if p.Task != "" {
					par.awaiting[a.ID]++
				}
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
					delete(missingRole, a.ID)
				} else {
					a.preset = missingRolePreset(p.Role) // the role it switched to is gone: same fallback as above
					missingRole[a.ID] = missingRoleError(p.Role)
				}
				a.Archetype = p.Role
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
		case event.AgentDirAdded:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.DirAddedPayload
				_ = e.Decode(&p)
				a.applyDirAdded(p.Dir, p.Source)
			}
		case event.AgentDirRemoved:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.DirRefPayload
				_ = e.Decode(&p)
				a.applyDirRemoved(p.Dir)
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
			case event.MsgPrompt:
				// A consumed prompt came from the prompt queue, or from a
				// steer that arrived while idle (logged as a prompt).
				if q := pendingPrompts[e.Agent]; len(q) > 0 && q[0].text == p.Text {
					pendingPrompts[e.Agent] = q[1:]
				} else if q := pendingSteers[e.Agent]; len(q) > 0 && q[0].text == p.Text {
					pendingSteers[e.Agent] = q[1:]
				} else if q := pendingPrompts[e.Agent]; len(q) > 0 {
					pendingPrompts[e.Agent] = q[1:]
				}
			case event.MsgSteer:
				if q := pendingSteers[e.Agent]; len(q) > 0 {
					pendingSteers[e.Agent] = q[1:]
				}
			case event.MsgAgentResponse:
				if q := pendingResponses[e.Agent]; len(q) > 0 {
					pendingResponses[e.Agent] = q[1:]
				}
			}
		case event.TodoChanged:
			if a, ok := s.agents[e.Agent]; ok {
				var p event.TodoPayload
				_ = e.Decode(&p)
				a.restoreTodos(p.Items)
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
				if p.Reason == event.ReasonError {
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
			if a, ok := s.agents[e.Agent]; ok {
				delete(a.awaiting, p.From) // an answer settles every question asked of that agent
			}
		case event.ToolCallStarted:
			var p event.ToolStartedPayload
			if _ = e.Decode(&p); p.Name == "agent_message" || p.Name == "agent_prompt" {
				var in struct{ ID string }
				if json.Unmarshal(p.Input, &in) == nil && in.ID != "" {
					askTargets[p.CallID] = in.ID
				}
			}
		case event.ToolCallFinished:
			var p event.ToolFinishedPayload
			_ = e.Decode(&p)
			if target, ok := askTargets[p.CallID]; ok {
				delete(askTargets, p.CallID)
				if a, ok := s.agents[e.Agent]; ok && !p.IsError && !p.Cancelled && !p.Denied {
					a.awaiting[target]++
				}
			}
		case event.AgentKilled:
			if a, ok := s.agents[e.Agent]; ok {
				a.state = StateKilled
				finished[a.ID] = true
			}
			for _, o := range s.agents {
				delete(o.awaiting, e.Agent)
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
		if why, ok := missingRole[id]; ok {
			a.lastError = why
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
		// Anything that starts a turn wakes it: prompts, steers, and wakes
		// (a response or a lost job waiting in the mailbox).
		if len(a.prompts)+len(a.steers)+len(a.wakes) > 0 {
			a.signal()
		}
	}
	if s.archived {
		s.cancel()
	}
	return s, nil
}

// missingRolePreset stands in for a role that no longer exists in config:
// read-only, no delegation, no MCP, so a restart can never hand an agent
// more than its role gave it.
func missingRolePreset(name string) config.Preset {
	return config.Preset{
		Name: name, Description: "(role no longer exists)", Mode: config.ModeAll, Layer: "builtin", Loop: "default",
		Tools: []string{"read"},
		Body:  "Your role's definition is gone from the configuration. You can only read files until the human picks a role with /role; say so if asked to do more.",
	}
}

func missingRoleError(name string) string {
	return fmt.Sprintf("role %q no longer exists: running read-only until /role picks another", name)
}
