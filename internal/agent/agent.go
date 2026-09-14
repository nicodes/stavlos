package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/project"
	"github.com/nicodes/stavlos/internal/protocol"
)

// State of an agent: the wire vocabulary (protocol.AgentState). The agent
// itself is never "waiting"; Info derives that from idle plus an
// outstanding question or job.
type State = protocol.AgentState

const (
	StateIdle    = protocol.AgentIdle
	StateRunning = protocol.AgentRunning
	StateBlocked = protocol.AgentBlocked // awaiting a permission/question answer
	StateKilled  = protocol.AgentKilled
)

type queued struct {
	text, source string
}

// response is an agent_response waiting in the mailbox: who answered (id
// and label) and the answer.
type response struct {
	from, label, text string
}

// Agent is one actor: a goroutine with a typed inbox (PRD §6.1).
type Agent struct {
	ID        string
	Parent    string
	Archetype string
	variant   string // model variant (reasoning effort); "" = provider default
	Label     string
	Depth     int

	s      *Session
	preset config.Preset

	ctx  context.Context // agent context; cancelled by Kill or parent Kill
	kill context.CancelFunc
	wake chan struct{}

	mu          sync.Mutex
	modelID     string
	state       State
	turn        int
	prompts     []queued              // Prompt inbox
	steers      []queued              // Steer inbox
	responses   []response            // answers from other agents (agent_response), not yet delivered
	awaiting    map[string]int        // agent id → questions asked of it (agent_message, a child's task); cleared by its next answer
	todos       []event.TodoItem      // the agent\'s todo list, in creation order (todo.changed snapshots)
	todoSeq     int                   // last todo id issued
	mcps        map[string]*mcpServer // MCP servers this agent has started (name → server)
	mcpIdle     *time.Timer           // stops idle MCP servers (MCPIdleAfter)
	extraDirs   []dirEntry            // working directories beyond the session\'s and the role\'s: grants and the human\'s answers (logged)
	removedDirs map[string]bool       // directories the human took out (role ones stay hidden while listed)
	events      []event.Event         // this agent's events since the last compaction (what the projection reads)
	evVer       int                   // bumps on every recorded event: the history cache is valid for one version
	hist        []model.Message       // the projected history for histVer
	histVer     int
	cancelTurn  context.CancelFunc
	armed       map[string]bool // job ids whose exit wakes this agent
	monitors    map[string]*Monitor
	monDone     []event.MonitorFiredPayload // fired monitors not yet delivered
	wakes       map[string]bool             // ids whose completion is waiting to wake this agent (unmonitor cancels)
	lastError   string                      // error that ended the most recent turn; cleared when a turn starts
	logErr      error                       // a log write that failed: the history is no longer the truth, so the turn ends at its next step
	compactNext bool                        // /compact arrived mid-turn: compact before the next model call, whatever the size
	compacting  bool                        // a compaction is running (manual or automatic): no second one starts meanwhile
	ctxTokens   int                         // estimated size of the projected history + system prompt at the last model call (or after compaction)
	ctxWindow   int                         // the model\'s context window as of the last model call
	children    []string
	done        chan struct{} // closed on kill
	usage       struct {
		tokens int
		cost   float64
	}
	started bool
}

func newAgent(s *Session, id, parent, archetype, label, modelID string, depth int, preset config.Preset) *Agent {
	return &Agent{
		ID: id, Parent: parent, Archetype: archetype, Label: label, Depth: depth,
		s: s, preset: preset, modelID: modelID, state: StateIdle,
		wake: make(chan struct{}, 1), armed: map[string]bool{}, wakes: map[string]bool{}, monitors: map[string]*Monitor{}, awaiting: map[string]int{}, done: make(chan struct{}),
	}
}

func (a *Agent) start() {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	a.started = true
	a.mu.Unlock()
	go a.run()
}

// run is the actor goroutine: it waits for work, then runs turns until the
// inbox has nothing that starts a turn.
func (a *Agent) run() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-a.wake:
		}
		for a.ctx.Err() == nil {
			inputs := a.takeInputs()
			if len(inputs) == 0 {
				break
			}
			a.runTurn(inputs)
			if !a.Alive() {
				return
			}
		}
	}
}

func (a *Agent) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// takeInputs decides whether a turn starts and, if so, drains the inbox
// into its user messages: every queued prompt (coalesced), any steers
// received while idle, and every child result waiting in the mailbox. A
// turn starts only for a prompt, a steer, or an armed wake; results alone
// never start one (PRD §6.3).
func (a *Agent) takeInputs() []event.UserMessagePayload {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == StateKilled {
		return nil
	}
	if len(a.prompts) == 0 && len(a.steers) == 0 && len(a.wakes) == 0 {
		return nil
	}
	a.wakes = map[string]bool{}
	var in []event.UserMessagePayload
	for _, q := range a.prompts {
		in = append(in, event.UserMessagePayload{Kind: event.MsgPrompt, Text: q.text, From: a.s.senderLabel(q.source)})
	}
	a.prompts = nil
	for _, q := range a.steers { // idle: a steer is just a prompt, and reads as one
		in = append(in, event.UserMessagePayload{Kind: event.MsgPrompt, Text: q.text, From: a.s.senderLabel(q.source)})
	}
	a.steers = nil
	for _, r := range a.responses {
		in = append(in, event.UserMessagePayload{Kind: event.MsgAgentResponse, Text: r.text, From: r.label})
	}
	a.responses = nil
	for _, r := range a.monDone {
		in = append(in, event.UserMessagePayload{Kind: event.MsgMonitorFired, Text: monitorText(r)})
	}
	a.monDone = nil
	return in
}

// --- envelope delivery ---

// Prompt queues a prompt; it runs after the current turn (PRD §6.2).
func (a *Agent) Prompt(ctx context.Context, text, source string) error {
	if !a.Alive() {
		return fmt.Errorf("agent %s is %s", a.ID, a.StateOf())
	}
	if _, err := a.s.host.Append(ctx, event.Event{Session: a.s.ID, Agent: a.ID, Type: event.PromptQueued,
		Payload: event.MustPayload(event.TextPayload{Text: text, Source: source})}); err != nil {
		return err
	}
	a.mu.Lock()
	a.prompts = append(a.prompts, queued{text, source})
	a.mu.Unlock()
	a.signal()
	return nil
}

// Steer preempts at the next model-call boundary; if idle it starts a turn.
func (a *Agent) Steer(ctx context.Context, text, source string) error {
	if !a.Alive() {
		return fmt.Errorf("agent %s is %s", a.ID, a.StateOf())
	}
	if _, err := a.s.host.Append(ctx, event.Event{Session: a.s.ID, Agent: a.ID, Type: event.SteerReceived,
		Payload: event.MustPayload(event.TextPayload{Text: text, Source: source})}); err != nil {
		return err
	}
	a.mu.Lock()
	a.steers = append(a.steers, queued{text, source})
	a.mu.Unlock()
	a.signal()
	return nil
}

// Cancel ends the current turn; the agent survives.
func (a *Agent) Cancel() {
	a.mu.Lock()
	c := a.cancelTurn
	a.mu.Unlock()
	if c != nil {
		c()
	}
}

// killNow tears the agent down (children are handled by Session.killTree).
func (a *Agent) killNow() {
	a.mu.Lock()
	if a.state == StateKilled {
		a.mu.Unlock()
		return
	}
	a.state = StateKilled
	a.mu.Unlock()
	a.kill()      // cancels monitors too (their ctx derives from a.ctx)
	a.stopMCP("") // and the MCP servers it owns
	a.closeDone()
	_, _ = a.s.host.Append(context.Background(), event.Event{Session: a.s.ID, Agent: a.ID, Type: event.AgentKilled,
		Payload: event.MustPayload(event.AgentRefPayload{ID: a.ID})})
	for _, o := range a.s.Agents() { // nobody will hear back from it now
		o.forget(a.ID)
	}
}

func (a *Agent) closeDone() {
	select {
	case <-a.done:
	default:
		close(a.done)
	}
}

// deliverResponse is an agent_response addressed to this agent (PRD §6.3):
// it goes to the mailbox and always wakes the agent between turns, the way
// a job's exit does. Nothing is injected into a running turn.
func (a *Agent) deliverResponse(from, label, text string) {
	a.mu.Lock()
	a.responses = append(a.responses, response{from, label, text})
	a.wakes["response:"+from] = true
	// One answer settles everything asked of that agent so far: a re-prompt
	// ("send it now") is usually covered by the same reply, and a second
	// answer never arrives for it.
	delete(a.awaiting, from)
	a.mu.Unlock()
	a.signal()
}

// expect records a question put to agent id (an agent_message, or a child's
// task): until its answer lands the agent reads as "waiting" when idle.
func (a *Agent) expect(id string) {
	a.mu.Lock()
	a.awaiting[id]++
	a.mu.Unlock()
}

// forget drops every expectation of agent id (it was killed: no answer is
// coming).
func (a *Agent) forget(id string) {
	a.mu.Lock()
	delete(a.awaiting, id)
	a.mu.Unlock()
}

// waitingOn reports whether the agent has outstanding questions or running
// jobs: idle, but expecting to be woken. Callers hold a.mu.
func (a *Agent) waitingOn() bool {
	if len(a.awaiting) > 0 {
		return true
	}
	for _, m := range a.monitors {
		if m.state == protocol.MonitorRunning {
			return true
		}
	}
	return false
}

func (a *Agent) addChild(id string) {
	a.mu.Lock()
	a.children = append(a.children, id)
	a.mu.Unlock()
}

// --- accessors ---

// Alive reports whether the agent can still receive work.
func (a *Agent) Alive() bool {
	return a.StateOf() != StateKilled
}

// StateOf returns the current state.
func (a *Agent) StateOf() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// ModelID returns the active model.
func (a *Agent) ModelID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.modelID
}

// Variant returns the model variant in force ("" = provider default).
func (a *Agent) Variant() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.variant
}

// SetVariant switches the model variant (reasoning effort and the like)
// at the agent's next model call. "" restores the provider default; any
// other name must be one the model offers.
func (a *Agent) SetVariant(ctx context.Context, v string) error {
	if v != "" {
		ok := false
		for _, name := range a.s.host.Variants(a.ModelID()) {
			if name == v {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("unknown variant %q for %s (see /variants)", v, a.ModelID())
		}
	}
	if p := a.Preset(); !p.AllowsVariant(a.ModelID(), v) {
		return fmt.Errorf("role %s does not allow variant %q for %s (allowed: %s)", p.Name, v, a.ModelID(), strings.Join(p.DefaultVariantList(a.ModelID()), ", "))
	}
	a.mu.Lock()
	a.variant = v
	a.mu.Unlock()
	_, err := a.s.host.Append(ctx, event.Event{Session: a.s.ID, Agent: a.ID, Type: event.AgentVariantChanged,
		Payload: event.MustPayload(event.VariantChangedPayload{Variant: v})})
	return err
}

// SetModel switches the agent's model at its next model call. The role's
// whitelist applies, and the variant is re-fitted to the new model.
func (a *Agent) SetModel(ctx context.Context, id string) error {
	if err := a.s.host.CheckModel(id); err != nil {
		return err
	}
	if p := a.Preset(); !p.AllowsModel(id) {
		return fmt.Errorf("role %s does not allow model %s (allowed: %s)", p.Name, id, modelList(p))
	}
	return a.setModelAndVariant(ctx, id, fitVariant(a.Preset(), id, a.Variant()))
}

// setModelAndVariant installs a model (and the variant that goes with it),
// logging each change.
func (a *Agent) setModelAndVariant(ctx context.Context, id, variant string) error {
	a.mu.Lock()
	prevModel, prevVariant := a.modelID, a.variant
	a.modelID, a.variant = id, variant
	a.mu.Unlock()
	if id != prevModel {
		if _, err := a.s.host.Append(ctx, event.Event{Session: a.s.ID, Agent: a.ID, Type: event.AgentModelChanged,
			Payload: event.MustPayload(event.ModelChangedPayload{Model: id})}); err != nil {
			return err
		}
	}
	if variant != prevVariant {
		if _, err := a.s.host.Append(ctx, event.Event{Session: a.s.ID, Agent: a.ID, Type: event.AgentVariantChanged,
			Payload: event.MustPayload(event.VariantChangedPayload{Variant: variant})}); err != nil {
			return err
		}
	}
	return nil
}

// Preset returns the role the agent runs as.
func (a *Agent) Preset() config.Preset {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.preset
}

// LabelNow is the agent's label as of now (SetRole may change it).
func (a *Agent) LabelNow() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Label
}

// roleView is what one step of a turn needs to know about the agent's
// role and place, read once under the lock: SetRole and the turn loop
// run on different goroutines, and a step must see one consistent role.
type roleView struct {
	preset           config.Preset
	label, archetype string
	turn             int
}

func (a *Agent) role() roleView {
	a.mu.Lock()
	defer a.mu.Unlock()
	return roleView{preset: a.preset, label: a.Label, archetype: a.Archetype, turn: a.turn}
}

// SetRole switches the agent's preset in place. The system prompt, tool
// list, skills, and spawn list change at the next model call; the label
// follows when it was just the old role's name.
// Compact is /compact: summarise every completed turn now when the agent
// is idle ("compacted"), or, mid-turn, before its next model call
// ("queued"). Auto-compaction keeps running on its own at the threshold.
func (a *Agent) Compact(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.state == StateKilled {
		a.mu.Unlock()
		return "", fmt.Errorf("agent %s is killed", a.ID)
	}
	busy := a.state == StateRunning || a.state == StateBlocked
	if busy {
		a.compactNext = true
		a.mu.Unlock()
		return "queued", nil
	}
	if a.compacting {
		a.mu.Unlock()
		return "", errors.New("a compaction is already running")
	}
	a.compacting = true // claimed under the same lock as the state: a turn that starts now sees it
	a.mu.Unlock()
	defer a.endCompacting()
	modelID := a.ModelID()
	if modelID == "" {
		return "", errors.New(ErrNoModel)
	}
	m, _, err := a.s.host.Resolve(modelID)
	if err != nil {
		return "", err
	}
	if err := a.compact(ctx, m, len(a.eventsCopy())); err != nil {
		return "", err
	}
	system, defs := a.buildContext(a.role(), a.s.Config())
	est := project.EstimateTokens(a.history(), system, defs) // takes a.mu itself: compute before locking
	a.mu.Lock()
	a.ctxTokens = est
	a.mu.Unlock()
	return "compacted", nil
}

func (a *Agent) endCompacting() {
	a.mu.Lock()
	a.compacting = false
	a.mu.Unlock()
}

func (a *Agent) SetRole(ctx context.Context, role string) error {
	preset, ok := a.s.Config().Presets[role]
	if !ok {
		return fmt.Errorf("unknown role %q (see /roles)", role)
	}
	switch {
	case a.Parent == "" && !preset.CanBePrimary():
		return fmt.Errorf("role %q is subagent-only: the main agent cannot take it", role)
	case a.Parent != "" && !preset.CanBeSubagent():
		return fmt.Errorf("role %q is primary-only: a subagent cannot take it", role)
	}
	// The agent's model and variant must fit the new role: keep them when
	// allowed, else move to the role's defaults.
	modelID := a.ModelID()
	if !preset.AllowsModel(modelID) {
		if d := preset.DefaultModel(); d != "" {
			modelID = d
		}
	}
	if err := a.setModelAndVariant(ctx, modelID, fitVariant(preset, modelID, a.Variant())); err != nil {
		return err
	}
	a.mu.Lock()
	if a.Label == a.Archetype {
		a.Label = role
	}
	a.Archetype = role
	a.preset = preset
	label := a.Label
	a.mu.Unlock()
	_, err := a.s.host.Append(ctx, event.Event{Session: a.s.ID, Agent: a.ID, Type: event.AgentRoleChanged,
		Payload: event.MustPayload(event.RoleChangedPayload{Role: role, Label: label})})
	return err
}

// Children returns child ids.
func (a *Agent) Children() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.children...)
}

// Cost returns accumulated cost in USD.
func (a *Agent) Cost() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage.cost
}

// Done is closed when the agent is killed.
func (a *Agent) Done() <-chan struct{} { return a.done }

// Info builds the protocol view.
func (a *Agent) Info() protocol.AgentInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.state
	if a.state == StateIdle && a.waitingOn() {
		state = protocol.AgentWaiting // idle, but a question or a job is outstanding
	}
	info := protocol.AgentInfo{
		ID: a.ID, Session: a.s.ID, Parent: a.Parent, Archetype: a.Archetype, Label: a.Label,
		Model: a.modelID, Variant: a.variant, Depth: a.Depth, State: state, Turn: a.turn,
		Queued:  len(a.prompts) + len(a.steers) + len(a.responses),
		CostUSD: a.usage.cost, Tokens: a.usage.tokens,
		Context: a.ctxTokens, ContextWindow: a.ctxWindow,
	}
	info.LastError = a.lastError
	for id := range a.awaiting {
		info.Awaiting = append(info.Awaiting, id)
	}
	sort.Strings(info.Awaiting)
	info.Todos = append([]event.TodoItem(nil), a.todos...)
	info.MCP = a.mcpInfoLocked()
	for _, d := range a.dirListLocked() {
		info.Dirs = append(info.Dirs, protocol.DirInfo{Path: d.path, Source: d.source})
	}
	mons := make([]*Monitor, 0, len(a.monitors))
	for _, m := range a.monitors {
		mons = append(mons, m)
	}
	a.mu.Unlock()
	sort.Slice(mons, func(i, j int) bool { return mons[i].Started.Before(mons[j].Started) })
	for _, m := range mons {
		mi := m.Info()
		mi.Agent = a.ID
		info.Monitors = append(info.Monitors, mi)
	}
	a.mu.Lock()
	return info
}

// record appends an event for this agent and caches it for projection.
// The log is the source of truth: when a write fails the in-memory history
// and the log have parted, so the failure is kept and the turn ends at its
// next step rather than carrying on from a history nobody can replay.
func (a *Agent) record(ctx context.Context, t event.Type, payload any) (event.Event, error) {
	e := event.Event{Session: a.s.ID, Agent: a.ID, Type: t}
	if payload != nil {
		e.Payload = event.MustPayload(payload)
	}
	e, err := a.s.host.Append(ctx, e)
	if err != nil {
		a.mu.Lock()
		if a.logErr == nil {
			a.logErr = fmt.Errorf("event log: %s: %w", t, err)
		}
		a.mu.Unlock()
		return e, err
	}
	a.mu.Lock()
	if cp, ok := payload.(event.CompactedPayload); ok {
		// Everything the summary covers is gone from the projection for
		// good; dropping it here keeps memory and every later projection
		// proportional to what the model still sees.
		kept := a.events[:0]
		for _, old := range a.events {
			if old.Seq > cp.ToSeq {
				kept = append(kept, old)
			}
		}
		a.events = kept
	}
	a.events = append(a.events, e)
	a.evVer++
	a.mu.Unlock()
	return e, nil
}

// history is the model-visible conversation, projected once per change to
// the events and cached. The result is the caller's to extend but not to
// mutate in place.
func (a *Agent) history() []model.Message {
	a.mu.Lock()
	if a.hist != nil && a.histVer == a.evVer {
		h := append([]model.Message(nil), a.hist...)
		a.mu.Unlock()
		return h
	}
	evs := append([]event.Event(nil), a.events...)
	ver := a.evVer
	a.mu.Unlock()
	h := project.Project(evs)
	a.mu.Lock()
	if ver == a.evVer {
		a.hist, a.histVer = h, ver
	}
	a.mu.Unlock()
	return append([]model.Message(nil), h...)
}

// takeLogErr returns and clears the first log failure since the last call.
func (a *Agent) takeLogErr() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.logErr
	a.logErr = nil
	return err
}

func (a *Agent) eventsCopy() []event.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]event.Event(nil), a.events...)
}
