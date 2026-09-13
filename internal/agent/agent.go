package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// State of an agent.
type State string

const (
	StateIdle     State = "idle"
	StateRunning  State = "running"
	StateBlocked  State = "blocked" // awaiting a permission/question answer
	StateFinished State = "finished"
	StateKilled   State = "killed"
)

type queued struct {
	text, source string
}

// Agent is one actor: a goroutine with a typed inbox (PRD §6.1).
type Agent struct {
	ID        string
	Parent    string
	Archetype string
	Label     string
	Depth     int

	s      *Session
	preset config.Preset

	ctx  context.Context // agent context; cancelled by Kill or parent Kill
	kill context.CancelFunc
	wake chan struct{}

	mu         sync.Mutex
	modelID    string
	state      State
	turn       int
	prompts    []queued            // Prompt inbox
	steers     []queued            // Steer inbox
	childDone  []tools.ChildResult // ChildFinished inbox
	events     []event.Event       // this agent's events (projection cache)
	cancelTurn context.CancelFunc
	finished   *tools.ChildResult
	finishFlag bool            // set by the finish tool during a turn
	yieldFlag  bool            // set by the monitor tool: end the turn after this batch
	armed      map[string]bool // child ids whose finish wakes this agent (monitor)
	children   []string
	results    map[string]tools.ChildResult // finished children not yet consumed by wait/result
	done       chan struct{}                // closed on finish or kill
	usage      struct {
		tokens int
		cost   float64
	}
	started bool
}

func newAgent(s *Session, id, parent, archetype, label, modelID string, depth int, preset config.Preset) *Agent {
	return &Agent{
		ID: id, Parent: parent, Archetype: archetype, Label: label, Depth: depth,
		s: s, preset: preset, modelID: modelID, state: StateIdle,
		wake: make(chan struct{}, 1), results: map[string]tools.ChildResult{}, armed: map[string]bool{}, done: make(chan struct{}),
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

// takeInputs drains the inbox into the user messages for one turn: every
// queued prompt (coalesced), any steers received while idle, and any child
// results not yet consumed (PRD §6.2 edge semantics).
func (a *Agent) takeInputs() []event.UserMessagePayload {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == StateFinished || a.state == StateKilled {
		return nil
	}
	var in []event.UserMessagePayload
	for _, q := range a.prompts {
		in = append(in, event.UserMessagePayload{Kind: "prompt", Text: q.text})
	}
	a.prompts = nil
	for _, q := range a.steers { // idle: steer behaves as prompt
		in = append(in, event.UserMessagePayload{Kind: "steer", Text: q.text})
	}
	a.steers = nil
	for _, r := range a.childDone {
		in = append(in, event.UserMessagePayload{Kind: "child_finished", Text: childText(r)})
	}
	a.childDone = nil
	return in
}

func childText(r tools.ChildResult) string {
	return fmt.Sprintf("Child agent %q (%s) finished with status %s.\n\n%s", r.Label, r.ID, r.Status, r.Summary)
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
	if a.state == StateKilled || a.state == StateFinished {
		a.mu.Unlock()
		return
	}
	a.state = StateKilled
	a.mu.Unlock()
	a.kill()
	a.closeDone()
	_, _ = a.s.host.Append(context.Background(), event.Event{Session: a.s.ID, Agent: a.ID, Type: event.AgentKilled,
		Payload: event.MustPayload(event.AgentRefPayload{ID: a.ID})})
	if p, ok := a.s.Agent(a.Parent); ok {
		p.childGone(a.ID)
	}
}

func (a *Agent) closeDone() {
	select {
	case <-a.done:
	default:
		close(a.done)
	}
}

// deliverChildFinished is the ChildFinished envelope (PRD §6.3). The result
// goes to the mailbox; the agent is woken only if it armed a wake for this
// child with monitor. Otherwise the result waits for result/status or the
// start of the next turn.
func (a *Agent) deliverChildFinished(r tools.ChildResult) {
	a.mu.Lock()
	a.results[r.ID] = r
	a.childDone = append(a.childDone, r)
	wake := a.armed[r.ID]
	delete(a.armed, r.ID)
	a.mu.Unlock()
	if wake {
		a.signal()
	}
}

func (a *Agent) childGone(id string) {
	a.mu.Lock()
	r, ok := a.results[id]
	if !ok {
		r = tools.ChildResult{ID: id, Status: "killed", Summary: "The agent was killed before finishing."}
		a.results[id] = r
		a.childDone = append(a.childDone, r)
	}
	wake := a.armed[id]
	delete(a.armed, id)
	a.mu.Unlock()
	if wake {
		a.signal()
	}
}

// IsArmed reports whether this agent will be woken when child id finishes.
func (a *Agent) IsArmed(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.armed[id]
}

func (a *Agent) addChild(id string) {
	a.mu.Lock()
	a.children = append(a.children, id)
	a.mu.Unlock()
}

// --- accessors ---

// Alive reports whether the agent can still receive work.
func (a *Agent) Alive() bool {
	st := a.StateOf()
	return st != StateFinished && st != StateKilled
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

// SetModel switches the agent's model at its next model call.
func (a *Agent) SetModel(ctx context.Context, id string) error {
	if err := a.s.host.CheckModel(id); err != nil {
		return err
	}
	a.mu.Lock()
	a.modelID = id
	a.mu.Unlock()
	_, err := a.s.host.Append(ctx, event.Event{Session: a.s.ID, Agent: a.ID, Type: event.AgentModelChanged,
		Payload: event.MustPayload(event.ModelChangedPayload{Model: id})})
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

// Done is closed when the agent finishes or is killed.
func (a *Agent) Done() <-chan struct{} { return a.done }

// Info builds the protocol view.
func (a *Agent) Info() protocol.AgentInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	info := protocol.AgentInfo{
		ID: a.ID, Session: a.s.ID, Parent: a.Parent, Archetype: a.Archetype, Label: a.Label,
		Model: a.modelID, Depth: a.Depth, State: string(a.state), Turn: a.turn,
		Queued:  len(a.prompts) + len(a.steers) + len(a.childDone),
		CostUSD: a.usage.cost, Tokens: a.usage.tokens,
	}
	if a.finished != nil {
		info.Summary = a.finished.Summary
		info.Status = a.finished.Status
	}
	a.mu.Unlock()
	if p, ok := a.s.Agent(a.Parent); ok {
		info.Monitored = p.IsArmed(a.ID)
	}
	a.mu.Lock()
	return info
}

// record appends an event for this agent and caches it for projection.
func (a *Agent) record(ctx context.Context, t event.Type, payload any) (event.Event, error) {
	e := event.Event{Session: a.s.ID, Agent: a.ID, Type: t}
	if payload != nil {
		e.Payload = event.MustPayload(payload)
	}
	e, err := a.s.host.Append(ctx, e)
	if err != nil {
		return e, err
	}
	a.mu.Lock()
	a.events = append(a.events, e)
	a.mu.Unlock()
	return e, nil
}

func (a *Agent) eventsCopy() []event.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]event.Event(nil), a.events...)
}
