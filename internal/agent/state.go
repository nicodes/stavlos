package agent

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/project"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// A channel's state is a fold of its events. apply is the only code that
// changes it: the live path commits events and applies them under
// Channel.mu, and recovery folds the same apply over the log, so the two
// can never disagree. What apply cannot do itself (wake an agent's
// goroutine) it reports as effects for the caller.

// channelState is what the log says about a channel.
type channelState struct {
	name, model, role, mode string
	archived                bool
	dirs                    []dirEntry // the working set beyond the channel directory
	permits                 permits
	agents                  map[string]*agentState
	order                   []string          // spawn order
	names                   map[string]string // agent name → id; a name is never released
}

// agentState is what the log says about one agent.
type agentState struct {
	id, parent, role, name, model, variant string
	depth                                  int
	children                               []string
	killed                                 bool

	turn      int
	inTurn    bool
	lastError string // the error that ended the latest turn

	inbox    []event.Input                      // queued, not yet taken, in order
	awaiting map[string]int                     // agent id → requests to it still unanswered
	owed     map[string]bool                    // parties owed a reply: "user" or agent ids
	nudges   int                                // reminders in a row that got no reply
	lastPost string                             // the chat post the human's latest input delivered
	jobs     map[string]event.JobStartedPayload // running background jobs
	asks     map[string]bool                    // prompts put to the human, not yet resolved

	todos      []event.TodoItem
	todoSeq    int
	compacting bool
	tokens     int
	cost       float64

	hist *project.Builder
}

// effects are what applying events asks of the runtime.
type effects struct {
	wake []string // agents whose inbox now holds something that starts a turn
}

func newChannelState(model, role string) *channelState {
	return &channelState{model: model, role: role, mode: protocol.ModeAsk, agents: map[string]*agentState{}, names: map[string]string{}}
}

// apply folds one committed event into the state.
func (cs *channelState) apply(e event.Event, fx *effects) {
	a := cs.agents[e.Agent]
	switch e.Type {
	case event.ChannelCreated, event.ChannelUpdated, event.ChannelArchived, event.ChannelDirAdded, event.ChannelDirRemoved, event.PermitGranted:
		cs.applyChannel(e)
	case event.AgentSpawned:
		cs.spawned(e)
	case event.AgentUpdated:
		if a != nil {
			cs.updated(a, e)
		}
	case event.AgentKilled:
		cs.killed(e.Agent)
	case event.InputQueued:
		if a != nil {
			cs.queued(a, e, fx)
		}
	case event.ChatMessage:
		if a != nil {
			a.settle(tools.User)
		}
	default:
		if a != nil {
			a.applyTurn(e)
		}
	}
	if a != nil {
		a.hist.Apply(e)
	}
}

func (cs *channelState) applyChannel(e event.Event) {
	switch e.Type {
	case event.ChannelCreated:
		var p event.ChannelCreatedPayload
		if e.Decode(&p) == nil {
			cs.name, cs.model, cs.role = p.Name, p.Model, p.Role
		}
	case event.ChannelUpdated:
		var p event.ChannelUpdatedPayload
		if e.Decode(&p) != nil {
			return
		}
		if p.Name != nil {
			cs.name = *p.Name
		}
		if p.Model != nil {
			cs.model = *p.Model
		}
		if p.Mode != nil {
			cs.mode = *p.Mode
		}
	case event.ChannelArchived:
		cs.archived = true
	case event.ChannelDirAdded:
		var p event.DirPayload
		if e.Decode(&p) == nil && !slices.ContainsFunc(cs.dirs, func(d dirEntry) bool { return d.path == p.Dir }) {
			cs.dirs = append(cs.dirs, dirEntry{p.Dir, p.Source})
		}
	case event.ChannelDirRemoved:
		var p event.DirPayload
		if e.Decode(&p) == nil {
			cs.dirs = slices.DeleteFunc(cs.dirs, func(d dirEntry) bool { return d.path == p.Dir })
		}
	case event.PermitGranted:
		var p event.PermitPayload
		if e.Decode(&p) == nil {
			cs.permits.apply(p)
		}
	}
}

func (cs *channelState) spawned(e event.Event) {
	var p event.AgentSpawnedPayload
	if e.Decode(&p) != nil || p.ID == "" {
		return
	}
	cs.agents[p.ID] = &agentState{
		id: p.ID, parent: p.Parent, role: p.Role, name: p.Name, model: p.Model, variant: p.Variant, depth: p.Depth,
		awaiting: map[string]int{}, owed: map[string]bool{}, jobs: map[string]event.JobStartedPayload{}, asks: map[string]bool{},
		hist: project.NewBuilder(),
	}
	cs.order = append(cs.order, p.ID)
	cs.names[p.Name] = p.ID
	if par := cs.agents[p.Parent]; par != nil {
		par.children = append(par.children, p.ID)
	}
}

func (cs *channelState) updated(a *agentState, e event.Event) {
	var p event.AgentUpdatedPayload
	if e.Decode(&p) != nil {
		return
	}
	if p.Role != nil {
		a.role = *p.Role
	}
	if p.Name != nil {
		a.name = *p.Name
		cs.names[a.name] = a.id
	}
	if p.Model != nil {
		a.model = *p.Model
	}
	if p.Variant != nil {
		a.variant = *p.Variant
	}
}

// killed marks an agent gone: its jobs and prompts end with it, nobody
// waits on it or owes it a reply any more.
func (cs *channelState) killed(id string) {
	a := cs.agents[id]
	if a == nil {
		return
	}
	a.killed, a.inTurn = true, false
	a.jobs, a.asks = map[string]event.JobStartedPayload{}, map[string]bool{}
	for _, o := range cs.agents {
		delete(o.awaiting, id)
		if o.owed[id] {
			o.settle(id)
		}
	}
}

// queued puts an input in a's inbox and does the bookkeeping its kind
// carries: a request makes the sender wait, a response settles that wait
// and what the sender owed, a reminder counts as a nudge.
func (cs *channelState) queued(a *agentState, e event.Event, fx *effects) {
	var in event.Input
	if e.Decode(&in) != nil {
		return
	}
	a.inbox = append(a.inbox, in)
	switch in.Kind {
	case event.InputRequest:
		if from := cs.agents[in.From]; from != nil {
			from.awaiting[a.id]++
		}
	case event.InputResponse:
		delete(a.awaiting, in.From) // one answer settles everything asked of that agent
		if from := cs.agents[in.From]; from != nil {
			from.settle(a.id)
		}
	case event.InputReminder:
		a.nudges++
	case event.InputPrompt, event.InputSteer, event.InputInfo, event.InputJob:
	}
	if wakes(in.Kind) && !a.killed {
		fx.wake = append(fx.wake, a.id)
	}
}

// applyTurn folds the events of an agent's own work.
func (a *agentState) applyTurn(e event.Event) {
	switch e.Type {
	case event.InputTaken:
		var p event.InputTakenPayload
		if e.Decode(&p) == nil {
			a.taken(p.IDs)
		}
	case event.TurnStarted:
		var p event.TurnPayload
		if e.Decode(&p) == nil {
			a.turn, a.inTurn, a.lastError = p.Turn, true, ""
		}
	case event.AssistantMessage:
		var p event.AssistantMessagePayload
		if e.Decode(&p) == nil {
			a.tokens += p.Usage.InputTokens + p.Usage.OutputTokens
			a.cost += p.CostUSD
		}
	case event.TurnEnded:
		var p event.TurnEndedPayload
		if e.Decode(&p) == nil && p.Reason == event.ReasonError {
			a.lastError = p.Error
		}
		a.inTurn = false
	case event.TurnAborted:
		a.inTurn = false
	case event.AskRequested, event.AskResolved:
		var p struct {
			ID string `json:"id"`
		}
		if e.Decode(&p) == nil {
			if e.Type == event.AskRequested {
				a.asks[p.ID] = true
			} else {
				delete(a.asks, p.ID)
			}
		}
	case event.JobStarted:
		var p event.JobStartedPayload
		if e.Decode(&p) == nil {
			a.jobs[p.ID] = p
		}
	case event.JobFinished, event.JobStopped:
		var p struct {
			ID string `json:"id"`
		}
		if e.Decode(&p) == nil {
			delete(a.jobs, p.ID)
		}
	case event.TodoChanged:
		var p event.TodoPayload
		if e.Decode(&p) == nil {
			a.setTodos(p.Items)
		}
	case event.CompactionStarted:
		a.compacting = true
	case event.CompactionDone, event.CompactionFailed:
		a.compacting = false
	}
}

// taken removes consumed inputs from the inbox and records what each is
// owed.
func (a *agentState) taken(ids []string) {
	for _, id := range ids {
		i := slices.IndexFunc(a.inbox, func(in event.Input) bool { return in.ID == id })
		if i < 0 {
			continue
		}
		a.took(a.inbox[i])
		a.inbox = slices.Delete(a.inbox, i, i+1)
	}
}

func (a *agentState) setTodos(items []event.TodoItem) {
	a.todos = items
	for _, it := range items {
		if n, err := strconv.Atoi(strings.TrimPrefix(it.ID, "t")); err == nil && n > a.todoSeq {
			a.todoSeq = n
		}
	}
}

// --- derived views ---

// wakes reports whether an input of kind starts a turn (info never does).
func wakes(kind event.InputKind) bool { return kind != event.InputInfo }

// midTurn reports whether an input of kind reaches a running turn at its
// next model call; the others wait for the turn to end.
func midTurn(kind event.InputKind) bool {
	return kind == event.InputSteer || kind == event.InputRequest || kind == event.InputInfo
}

// startsTurn reports whether the inbox holds anything that starts a turn.
func (a *agentState) startsTurn() bool {
	return slices.ContainsFunc(a.inbox, func(in event.Input) bool { return wakes(in.Kind) })
}

// waiting reports whether the agent expects to be woken: an agent owes it
// an answer, or a job of its runs.
func (a *agentState) waiting() bool { return len(a.awaiting) > 0 || len(a.jobs) > 0 }

// busy reports whether the agent is in a turn or about to start one.
func (a *agentState) busy() bool { return !a.killed && (a.inTurn || a.startsTurn()) }

// status is the agent's state on the wire.
func (a *agentState) status() protocol.AgentState {
	switch {
	case a.killed:
		return protocol.AgentKilled
	case a.inTurn && len(a.asks) > 0:
		return protocol.AgentBlocked
	case a.inTurn:
		return protocol.AgentRunning
	case a.waiting():
		return protocol.AgentWaiting
	}
	return protocol.AgentIdle
}

// due lists the parties the agent owes a reply, sorted.
func (a *agentState) due() []string {
	out := make([]string, 0, len(a.owed))
	for p := range a.owed {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// awaitingIDs lists the agents this one waits on, sorted.
func (a *agentState) awaitingIDs() []string {
	out := make([]string, 0, len(a.awaiting))
	for id := range a.awaiting {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
