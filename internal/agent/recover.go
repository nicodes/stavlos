package agent

import (
	"context"
	"fmt"
	"github.com/nicodes/stavlos/internal/tools"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/toolname"
)

// Recover rebuilds a channel from its log (PRD §4.3, §5). Any turn that was
// open when the daemon stopped gets a TurnAborted event; agents come back
// idle with their unconsumed prompts and steers re-queued. Nothing restarts
// automatically.
func Recover(ctx context.Context, host Host, id, dir string, created time.Time, cfg *config.Effective, events []event.Event) (*Channel, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("channel %s has no events", id)
	}
	s := New(host, id, dir, cfg, "", "")
	s.Created = created
	r := &recovery{
		s: s, cfg: cfg,
		openTurns: map[string]int{}, openMonitors: map[string]event.MonitorStartedPayload{}, monitorOwner: map[string]string{},
		prompts: map[string][]queued{}, steers: map[string][]queued{}, notes: map[string][]queued{}, responses: map[string][]response{},
		finished: map[string]bool{}, missingRole: map[string]string{},
	}
	for _, e := range events {
		r.apply(e)
		if a, ok := s.agents[e.Agent]; ok && e.Agent != "" {
			a.events = append(a.events, e)
		}
	}
	r.reportLostJobs(ctx, host)
	if err := r.resume(ctx, host); err != nil {
		return nil, err
	}
	if s.archived {
		s.cancel()
	}
	return s, nil
}

// recovery is what Recover gathers while it walks the log.
type recovery struct {
	s   *Channel
	cfg *config.Effective

	openTurns    map[string]int                         // agent → turn still open at shutdown
	openMonitors map[string]event.MonitorStartedPayload // job id → spec, still running at shutdown
	monitorOwner map[string]string
	prompts      map[string][]queued
	steers       map[string][]queued
	notes        map[string][]queued
	responses    map[string][]response
	finished     map[string]bool
	missingRole  map[string]string // agent id → why it runs read-only (set after replay: turns clear lastError)
}

// apply folds one logged event into the channel being rebuilt.
func (r *recovery) apply(e event.Event) {
	switch e.Type {
	case event.ChannelCreated, event.ChannelArchived, event.ChannelModelChanged, event.ChannelRenamed, event.ChannelModeChanged,
		event.ChannelDirAdded, event.ChannelDirRemoved:
		r.channel(e)
	case event.AgentSpawned:
		r.spawned(e)
	case event.AgentRoleChanged, event.AgentModelChanged, event.AgentVariantChanged, event.TodoChanged:
		r.agentSetting(e)
	case event.PromptQueued, event.SteerReceived, event.NoteQueued, event.UserMessage, event.ResponseReceived:
		r.inbox(e)
	case event.MessageToUser, event.ReminderQueued:
		r.replies(e)
	case event.MonitorStarted, event.MonitorFired, event.MonitorStopped, event.MonitorArmed, event.MonitorDisarmed:
		r.monitor(e)
	case event.TurnStarted, event.TurnEnded, event.TurnAborted, event.Usage:
		r.turn(e)
	case event.PermitGranted:
		var p event.PermitPayload
		if e.Decode(&p) == nil {
			r.s.permits.apply(p)
		}
	case event.AgentKilled:
		if a, ok := r.s.agents[e.Agent]; ok {
			a.state = StateKilled
			r.finished[a.ID] = true
		}
		for _, o := range r.s.agents {
			delete(o.awaiting, e.Agent)
			o.settle(e.Agent)
		}
	}
}

func (r *recovery) channel(e event.Event) {
	s := r.s
	switch e.Type {
	case event.ChannelCreated:
		var p event.ChannelCreatedPayload
		_ = e.Decode(&p)
		s.name, s.model, s.rootArch = p.Name, p.Model, p.RootAgent
		if s.Dir == "" {
			s.Dir = p.Dir
		}
	case event.ChannelArchived:
		s.archived = true
	case event.ChannelRenamed:
		var p event.NamePayload
		_ = e.Decode(&p)
		s.name = p.Name
	case event.ChannelModelChanged:
		var p event.ModelChangedPayload
		_ = e.Decode(&p)
		s.model = p.Model
	case event.ChannelModeChanged:
		var p event.ModePayload
		_ = e.Decode(&p)
		s.mode = p.Mode
	case event.ChannelDirAdded:
		var p event.DirAddedPayload
		_ = e.Decode(&p)
		s.applyDirAdded(p.Dir, p.Source)
	case event.ChannelDirRemoved:
		var p event.DirRefPayload
		_ = e.Decode(&p)
		s.applyDirRemoved(p.Dir)
	}
}

func (r *recovery) spawned(e event.Event) {
	s := r.s
	var p event.AgentSpawnedPayload
	_ = e.Decode(&p)
	preset, ok := r.cfg.Presets[p.Archetype]
	if !ok {
		// The role this agent was created with is gone (removed or
		// renamed). Recovery never widens what an agent may do: it comes
		// back read-only, says so, and waits for /role.
		preset = missingRolePreset(p.Archetype)
	}
	a := newAgent(s, p.ID, p.Parent, p.Archetype, p.Label, p.Model, p.Depth, preset)
	// Names are claimed in creation order, so a replay gives every agent the
	// name the live path gave it.
	s.mu.Lock()
	name, _ := s.claimNameLocked(p.Label, p.Archetype, a.ID)
	s.mu.Unlock()
	a.Label = name
	if !ok {
		r.missingRole[a.ID] = missingRoleError(p.Archetype)
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
}

func (r *recovery) agentSetting(e event.Event) {
	a, ok := r.s.agents[e.Agent]
	if !ok {
		return
	}
	switch e.Type {
	case event.AgentRoleChanged:
		var p event.RoleChangedPayload
		_ = e.Decode(&p)
		if preset, ok := r.cfg.Presets[p.Role]; ok {
			a.preset = preset
			delete(r.missingRole, a.ID)
		} else {
			a.preset = missingRolePreset(p.Role) // the role it switched to is gone: the same fallback as at spawn
			r.missingRole[a.ID] = missingRoleError(p.Role)
		}
		a.Archetype = p.Role
		if p.Label != "" {
			r.s.mu.Lock()
			if name, err := r.s.claimNameLocked(p.Label, p.Role, a.ID); err == nil {
				a.Label = name
			}
			r.s.mu.Unlock()
		}
	case event.AgentModelChanged:
		var p event.ModelChangedPayload
		_ = e.Decode(&p)
		a.modelID = p.Model
	case event.AgentVariantChanged:
		var p event.VariantChangedPayload
		_ = e.Decode(&p)
		a.variant = p.Variant
	case event.TodoChanged:
		var p event.TodoPayload
		_ = e.Decode(&p)
		a.restoreTodos(p.Items)
	}
}

func (r *recovery) inbox(e event.Event) {
	switch e.Type {
	case event.PromptQueued:
		var p event.TextPayload
		_ = e.Decode(&p)
		r.prompts[e.Agent] = append(r.prompts[e.Agent], queued{p.Text, p.Source, p.Post})
	case event.SteerReceived:
		var p event.TextPayload
		_ = e.Decode(&p)
		r.steers[e.Agent] = append(r.steers[e.Agent], queued{p.Text, p.Source, p.Post})
		// A steer from an agent is a request it now waits on (a response is
		// ResponseReceived, info is NoteQueued).
		if caller, ok := strings.CutPrefix(p.Source, "agent:"); ok {
			if a, ok := r.s.agents[caller]; ok {
				a.awaiting[e.Agent]++
			}
		}
	case event.NoteQueued:
		var p event.TextPayload
		_ = e.Decode(&p)
		r.notes[e.Agent] = append(r.notes[e.Agent], queued{text: p.Text, source: p.Source})
	case event.UserMessage:
		r.consumed(e)
	case event.ResponseReceived:
		var p event.ResponsePayload
		_ = e.Decode(&p)
		r.responses[e.Agent] = append(r.responses[e.Agent], response{p.From, p.FromLabel, p.Text})
		if from, ok := r.s.agents[p.From]; ok {
			from.settle(e.Agent)
		}
		if a, ok := r.s.agents[e.Agent]; ok {
			delete(a.awaiting, p.From) // an answer settles every question asked of that agent
		}
	}
}

// consumed unqueues what a logged user message took from the agent's inbox.
func (r *recovery) consumed(e event.Event) {
	var p event.UserMessagePayload
	_ = e.Decode(&p)
	a, known := r.s.agents[e.Agent]
	if known {
		a.took(p)
	}
	switch p.Kind {
	case event.MsgPrompt:
		// A consumed prompt came from the prompt queue, or from a steer that
		// arrived while idle (logged as a prompt).
		if q := r.prompts[e.Agent]; len(q) > 0 && q[0].text == p.Text {
			r.prompts[e.Agent] = q[1:]
		} else if q := r.steers[e.Agent]; len(q) > 0 && q[0].text == p.Text {
			r.steers[e.Agent] = q[1:]
		} else if q := r.prompts[e.Agent]; len(q) > 0 {
			r.prompts[e.Agent] = q[1:]
		}
	case event.MsgSteer:
		if q := r.steers[e.Agent]; len(q) > 0 {
			r.steers[e.Agent] = q[1:]
		}
	case event.MsgAgentResponse:
		if q := r.responses[e.Agent]; len(q) > 0 {
			r.responses[e.Agent] = q[1:]
		}
	case event.MsgNote:
		if q := r.notes[e.Agent]; len(q) > 0 {
			r.notes[e.Agent] = q[1:]
		}
	case event.MsgMonitorFired:
		// A job result the turn consumed; open jobs are found from the
		// monitor events, so nothing to unqueue here.
	case event.MsgReminder:
		if known {
			a.remind = nil
		}
	}
}

// replies folds the reply bookkeeping of replies.go back in: a message to
// the human settles what is owed to it, and a queued reminder counts as a
// nudge and waits to start a turn.
func (r *recovery) replies(e event.Event) {
	a, ok := r.s.agents[e.Agent]
	if !ok {
		return
	}
	switch e.Type {
	case event.MessageToUser:
		a.settle(tools.User)
	case event.ReminderQueued:
		var p event.RepliesPayload
		_ = e.Decode(&p)
		a.nudges++
		a.remind = p.Parties
	}
}

func (r *recovery) monitor(e event.Event) {
	switch e.Type {
	case event.MonitorStarted:
		var p event.MonitorStartedPayload
		_ = e.Decode(&p)
		r.openMonitors[p.ID] = p
		r.monitorOwner[p.ID] = e.Agent
	case event.MonitorFired, event.MonitorStopped:
		var p struct {
			ID string `json:"id"`
		}
		_ = e.Decode(&p)
		delete(r.openMonitors, p.ID)
		// The live path disarms a job when it fires or stops without logging
		// MonitorDisarmed; replay must not leave it armed.
		if a, ok := r.s.agents[r.monitorOwner[p.ID]]; ok {
			delete(a.armed, p.ID)
		}
	case event.MonitorArmed, event.MonitorDisarmed:
		a, ok := r.s.agents[e.Agent]
		if !ok {
			return
		}
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
}

func (r *recovery) turn(e event.Event) {
	a, known := r.s.agents[e.Agent]
	switch e.Type {
	case event.TurnStarted:
		var p event.TurnPayload
		_ = e.Decode(&p)
		r.openTurns[e.Agent] = p.Turn
		if known {
			a.turn = p.Turn
			a.lastError = ""
		}
	case event.TurnEnded, event.TurnAborted:
		delete(r.openTurns, e.Agent)
		if known && e.Type == event.TurnEnded {
			var p event.TurnEndedPayload
			_ = e.Decode(&p)
			if p.Reason == event.ReasonError {
				a.lastError = p.Error
			}
		}
	case event.Usage:
		if known {
			var p event.UsagePayload
			_ = e.Decode(&p)
			a.usage.tokens += p.Usage.InputTokens + p.Usage.OutputTokens
			a.usage.cost += p.CostUSD
		}
	}
}

// reportLostJobs tells each owner that a background job running at
// shutdown was lost: its process died with the daemon.
func (r *recovery) reportLostJobs(ctx context.Context, host Host) {
	s := r.s
	for id, p := range r.openMonitors {
		a, ok := s.agents[r.monitorOwner[id]]
		if !ok || r.finished[a.ID] || s.archived {
			continue
		}
		res := event.MonitorFiredPayload{ID: id, Kind: p.Kind, Label: p.Label, Summary: "background job lost in a daemon restart; rerun it if you still need the result", IsError: true, ExitCode: -1}
		e, err := host.Append(ctx, event.Event{Channel: s.ID, Agent: a.ID, Type: event.MonitorFired, Payload: event.MustPayload(res)})
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

// resume closes the turns left open, re-queues each survivor's inbox and
// starts it; killed agents, and every agent of an archived channel, stay
// down.
func (r *recovery) resume(ctx context.Context, host Host) error {
	s := r.s
	for _, id := range s.order {
		a := s.agents[id]
		if turn, ok := r.openTurns[id]; ok && !r.finished[id] {
			e, err := host.Append(ctx, event.Event{Channel: s.ID, Agent: id, Type: event.TurnAborted, Payload: event.MustPayload(event.TurnPayload{Turn: turn})})
			if err != nil {
				return err
			}
			a.events = append(a.events, e)
		}
		if r.finished[id] {
			a.closeDone()
			a.kill()
			continue
		}
		if why, ok := r.missingRole[id]; ok {
			a.lastError = why
		}
		a.prompts, a.steers, a.notes, a.responses = r.prompts[id], r.steers[id], r.notes[id], r.responses[id]
		for _, resp := range a.responses {
			a.wakes["response:"+resp.from] = true
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
		// Anything that starts a turn wakes it: prompts, steers, a queued
		// reminder, and wakes (a response or a lost job in the mailbox).
		if len(a.prompts)+len(a.steers)+len(a.wakes)+len(a.remind) > 0 {
			a.signal()
		}
	}
	return nil
}

// missingRolePreset stands in for a role that no longer exists in config:
// read-only, no delegation, no MCP, so a restart can never hand an agent
// more than its role gave it.
func missingRolePreset(name string) config.Preset {
	return config.Preset{
		Name: name, Description: "(role no longer exists)", Mode: config.ModeAll, Layer: "builtin", Loop: "default",
		Tools: []string{toolname.Read},
		Body:  "Your role's definition is gone from the configuration. You can only read files until the human picks a role with /role; say so if asked to do more.",
	}
}

func missingRoleError(name string) string {
	return fmt.Sprintf("role %q no longer exists: running read-only until /role picks another", name)
}
