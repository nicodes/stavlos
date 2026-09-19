package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/project"
	"github.com/nicodes/stavlos/internal/protocol"
)

// Agent is the runtime handle of one agent: its goroutine, its context and
// what the log does not hold (running jobs, MCP servers, the cached prompt
// prefix). What the log holds lives in the channel's state, under the
// channel's lock.
type Agent struct {
	ID     string
	Parent string
	Depth  int

	c    *Channel
	ctx  context.Context // cancelled by a kill (a parent's too) or the channel stopping
	kill context.CancelFunc
	wake chan struct{}

	// Guarded by c.mu.
	cancelTurn  context.CancelFunc
	jobs        map[string]*jobRun
	compactNext bool // /compact arrived mid-turn: compact before the next model call
	ctxTokens   int  // estimated size of the last model call
	ctxWindow   int
	logErr      error // a failed log write: the turn ends at its next step
	maintenance int   // manual compaction, including its preparation and cleanup
	prefix      promptPrefix
	instructed  map[string]bool // instructions files a tool result has carried since the last compaction

	mcp mcpSet // under its own lock, never held while taking c.mu
}

func newAgent(c *Channel, id, parent string, depth int, parentCtx context.Context) *Agent {
	ctx, kill := context.WithCancel(parentCtx)
	return &Agent{ID: id, Parent: parent, Depth: depth, c: c, ctx: ctx, kill: kill, wake: make(chan struct{}, 1), jobs: map[string]*jobRun{}}
}

func (a *Agent) start() {
	a.c.wg.Add(1)
	go a.run()
}

func (a *Agent) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// state is the agent's logged state; the caller holds c.mu.
func (a *Agent) state() *agentState { return a.c.st.agents[a.ID] }

// record commits one event of this agent. A failed write is kept: memory
// and the log have parted, so the turn ends at its next step rather than
// carrying on from a history nobody can replay.
func (a *Agent) record(t event.Type, payload any) error {
	return a.recordAll(a.c.event(a.ID, t, payload))
}

// recordFact is record for something that has already happened
// (Channel.commitFactLocked): the state follows the world even when the log
// refuses the event.
func (a *Agent) recordFact(t event.Type, payload any) error {
	s := a.c
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.commitFactLocked(context.Background(), s.event(a.ID, t, payload))
	if err != nil {
		err = fmt.Errorf("event log: %w", err)
	}
	return err
}

func (a *Agent) recordAll(evs ...event.Event) error {
	s := a.c
	s.mu.Lock()
	err := s.commitLocked(context.Background(), evs...)
	if err != nil {
		err = fmt.Errorf("event log: %w", err)
		if a.logErr == nil {
			a.logErr = err
		}
	}
	s.mu.Unlock()
	return err
}

// --- inbox ---

// Prompt queues the human's prompt: it runs after the current turn. A
// source "agent:<id>" makes it a request from that agent.
func (a *Agent) Prompt(ctx context.Context, text, source string) error {
	return a.queue(ctx, event.InputPrompt, text, source)
}

// Steer delivers the human's steer at the next model call; an idle agent
// starts a turn.
func (a *Agent) Steer(ctx context.Context, text, source string) error {
	return a.queue(ctx, event.InputSteer, text, source)
}

func (a *Agent) queue(ctx context.Context, kind event.InputKind, text, source string) error {
	s := a.c
	s.mu.Lock()
	st := a.state()
	if st.killed {
		s.mu.Unlock()
		return fmt.Errorf("agent %s is killed", a.ID)
	}
	in := event.Input{ID: NewID("i"), Kind: kind, Text: text}
	in.RequestID = in.ID
	if from, ok := strings.CutPrefix(source, "agent:"); ok {
		if f := s.st.agents[from]; f != nil {
			in.Kind, in.From, in.FromName = event.InputRequest, from, f.name
		}
	}
	err := s.commitLocked(ctx, s.event(a.ID, event.InputQueued, in))
	s.mu.Unlock()
	return err
}

// Cancel drops this agent's reply-debt and ends the current turn; the agent survives.
func (a *Agent) Cancel() {
	_ = a.cancel()
}

func (a *Agent) cancel() error {
	s := a.c
	s.mu.Lock()
	st := a.state()
	var err error
	if st != nil && !st.killed {
		err = s.commitLocked(context.Background(), s.event(a.ID, event.AgentCancelled, nil))
	}
	c := a.cancelTurn
	s.mu.Unlock()
	if c != nil {
		c()
	}
	return err
}

// --- reading ---

// Alive reports whether the agent can still receive work.
func (a *Agent) Alive() bool {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return !a.state().killed
}

// ModelID returns the agent's model.
func (a *Agent) ModelID() string {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.state().model
}

// variantNow returns the agent's model variant.
func (a *Agent) variantNow() string {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.state().variant
}

// Name returns the agent's name.
func (a *Agent) Name() string {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.state().name
}

// roleView is what one step needs to know about the agent's role and place,
// read once under the lock so a step sees one consistent role.
type roleView struct {
	preset     config.Preset
	missing    bool // the role is gone from config: the agent runs read-only
	role, name string
	parentName string
	turn       int
}

// roleLocked is a's role view; the caller holds c.mu. A role that no longer
// exists is replaced by a read-only stand-in: recovery and a config reload
// never widen what an agent may do.
func (c *Channel) roleLocked(a *agentState) roleView {
	p, ok := c.cfg.Presets[a.role]
	if !ok {
		p = missingRolePreset(a.role)
	}
	rv := roleView{preset: p, missing: !ok, role: a.role, name: a.name, turn: a.turn}
	if par := c.st.agents[a.parent]; par != nil {
		rv.parentName = par.name
	}
	return rv
}

func (a *Agent) role() roleView {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.c.roleLocked(a.state())
}

// Info describes the agent.
func (a *Agent) Info() protocol.AgentInfo {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.infoLocked()
}

func (a *Agent) infoLocked() protocol.AgentInfo {
	st := a.state()
	rv := a.c.roleLocked(st)
	info := protocol.AgentInfo{
		ID: a.ID, Channel: a.c.ID, Parent: a.Parent, Role: st.role, Name: st.name,
		Model: st.model, Variant: st.variant, Depth: a.Depth, State: st.status(), Turn: st.turn,
		Queued: len(st.inbox), CostUSD: st.cost, Tokens: st.tokens,
		Context: a.ctxTokens, ContextWindow: a.ctxWindow, LastError: st.lastError,
		Awaiting: st.awaitingIDs(), Due: st.due(), Todos: append([]event.TodoItem(nil), st.todos...),
		PendingReplies: st.pendingReplies(), AwaitingReplies: st.awaitingReplies(),
		Nudges: st.nudges, NudgeLimit: nudgeLimit(a.c.cfg.Reminders),
		MCP: a.mcpInfo(rv.preset.MCP), Jobs: a.jobInfosLocked(),
	}
	if rv.missing && info.LastError == "" {
		info.LastError = missingRoleError(st.role)
	}
	return info
}

// --- changing ---

// SetVariant switches the model variant at the next model call; "" is the
// provider default.
func (a *Agent) SetVariant(ctx context.Context, v string) error {
	modelID := a.ModelID()
	if v == a.variantNow() {
		return nil
	}
	if v != "" && !contains(a.c.host.Variants(modelID), v) {
		return fmt.Errorf("unknown variant %q for %s (see /variants)", v, modelID)
	}
	if p := a.role().preset; !p.AllowsVariant(modelID, v) {
		return fmt.Errorf("role %s does not allow variant %q for %s (allowed: %s)", p.Name, v, modelID, strings.Join(p.DefaultVariantList(modelID), ", "))
	}
	return a.update(ctx, event.AgentUpdatedPayload{Variant: event.Str(v)})
}

// SetModel switches the agent's model at its next model call, within the
// role's whitelist, re-fitting the variant.
func (a *Agent) SetModel(ctx context.Context, id string) error {
	if err := a.c.host.CheckModel(id); err != nil {
		return err
	}
	mk := a.c.readMarket(a.c.Config(), id) // before the lock
	a.c.mu.Lock()
	st := a.state()
	p := a.c.roleLocked(st).preset
	up := mk.retarget(st, p, "", id, "")
	a.c.mu.Unlock()
	if !p.AllowsModel(id) {
		return fmt.Errorf("role %s does not allow model %s (allowed: %s)", p.Name, id, modelList(p))
	}
	return a.update(ctx, up)
}

// SetRole switches the agent's role in place: the system prompt, tools,
// skills and spawn list change at the next model call; the model and
// variant move to what the role allows; the name follows when it was just
// the old role's name.
func (a *Agent) SetRole(ctx context.Context, role string) error {
	s := a.c
	mk := s.readMarket(s.Config(), a.ModelID()) // before the lock
	s.mu.Lock()
	defer s.mu.Unlock()
	preset, ok := s.cfg.Presets[role]
	switch {
	case !ok:
		return fmt.Errorf("unknown role %q (see /roles)", role)
	case a.Parent == "" && !preset.CanBePrimary():
		return fmt.Errorf("role %q is subagent-only: the main agent cannot take it", role)
	case a.Parent != "" && !preset.CanBeSubagent():
		return fmt.Errorf("role %q is primary-only: a subagent cannot take it", role)
	}
	st := a.state()
	modelID := st.model
	if !preset.AllowsModel(modelID) {
		if d := preset.DefaultModel(); d != "" {
			modelID = d
		}
	}
	// (a role's default model is one of its listed models, which the market looked at)
	p := mk.retarget(st, preset, role, modelID, "")
	if st.name == st.role && role != st.role {
		if name, err := s.st.uniqueName(role, role, a.ID); err == nil {
			p.Name = event.Str(name)
		}
	}
	if p == (event.AgentUpdatedPayload{}) {
		return nil
	}
	err := s.commitLocked(ctx, s.event(a.ID, event.AgentUpdated, p))
	return err
}

func (a *Agent) update(ctx context.Context, p event.AgentUpdatedPayload) error {
	if p == (event.AgentUpdatedPayload{}) {
		return nil
	}
	return a.c.commit(ctx, a.c.event(a.ID, event.AgentUpdated, p))
}

// changed is the update that moves st to role, model and variant, carrying
// only what differs ("" role means unchanged).
func changed(st *agentState, role, model, variant string) event.AgentUpdatedPayload {
	var p event.AgentUpdatedPayload
	if role != "" && role != st.role {
		p.Role = event.Str(role)
	}
	if model != st.model {
		p.Model = event.Str(model)
	}
	if variant != st.variant {
		p.Variant = event.Str(variant)
	}
	return p
}

// Compact is /compact: summarise every completed turn now when the agent
// is idle ("compacted"), or before its next model call when it is in a
// turn ("queued").
func (a *Agent) Compact(ctx context.Context) (string, error) {
	s := a.c
	s.mu.Lock()
	st := a.state()
	switch {
	case s.reconfiguring:
		s.mu.Unlock()
		return "", errors.New("a directory change is in progress")
	case st.killed:
		s.mu.Unlock()
		return "", fmt.Errorf("agent %s is killed", a.ID)
	case st.inTurn:
		a.compactNext = true
		s.mu.Unlock()
		return "queued", nil
	case st.compacting:
		s.mu.Unlock()
		return "", errors.New("a compaction is already running")
	}
	modelID := st.model
	a.maintenance++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); a.maintenance--; s.mu.Unlock() }()
	if modelID == "" {
		return "", errors.New(ErrNoModel)
	}
	m, info, err := s.host.Resolve(modelID)
	if err != nil {
		return "", err
	}
	if err := a.compact(ctx, m, info, true); err != nil {
		return "", err
	}
	rv, cfg := a.role(), s.Config()
	system, defs := a.buildContext(rv, cfg)
	s.mu.Lock()
	a.ctxTokens = project.EstimateTokens(a.state().hist.History(), system, defs)
	s.mu.Unlock()
	return "compacted", nil
}

// history is a copy of the agent's model-visible conversation.
func (a *Agent) history() []model.Message {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	return a.state().hist.History()
}
