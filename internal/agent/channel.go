// Package agent implements channels, the actor model, and the turn loop
// (PRD §5, §6).
//
// A channel is one state machine. Channel.mu guards its settings and every
// agent's state; state changes only by committing events (append to the
// log, then apply, both under the lock), so the live channel and a
// recovered one are the same fold of the same events. Each agent runs its
// turns on a goroutine of its own, outside the lock: it reads what a step
// needs, calls the model and runs tools unlocked, and commits what
// happened. There is no per-agent lock, so there is no lock order between
// agents to get wrong.
package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// Host is what the daemon provides to channels.
type Host interface {
	// Append logs events in one transaction and returns them numbered.
	Append(ctx context.Context, evs ...event.Event) ([]event.Event, error)
	Stream(n protocol.StreamNotification)
	Resolve(modelID string) (model.Model, model.Info, error)
	CheckModel(modelID string) error
	Variants(modelID string) []string
	Prompt(ctx context.Context, info protocol.PromptInfo) escalation.Answer
}

// ErrNoModel is the turn error when an agent has no model to call.
const ErrNoModel = "no model selected: run /models to pick one, or /providers first to connect a provider"

// errStopped is what a commit returns once the channel is stopping: nothing
// more is logged, so a daemon shutdown leaves open turns open for recovery
// to abort instead of recording them as cancelled.
var errStopped = errors.New("channel is stopped")

// Channel is a main agent plus its subtree, bound to a directory (PRD §5).
type Channel struct {
	ID      string
	Dir     string
	Created time.Time

	host  Host
	tools tools.Set

	mu      sync.Mutex
	cfg     *config.Effective
	st      *channelState
	agents  map[string]*Agent // the runtime handle of every agent in st
	stopped bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // agent goroutines and job watchers
}

// New creates a channel without logging anything; Start or Recover brings
// it to life.
func New(host Host, id, dir string, cfg *config.Effective, modelID, role string) *Channel {
	ctx, cancel := context.WithCancel(context.Background())
	if role == "" {
		role = cfg.RootAgent
	}
	if modelID == "" {
		modelID = cfg.Model
	}
	return &Channel{
		ID: id, Dir: dir, Created: time.Now().UTC(),
		host: host, tools: tools.Builtin(), cfg: cfg,
		st: newChannelState(modelID, role), agents: map[string]*Agent{},
		ctx: ctx, cancel: cancel,
	}
}

// event builds an event of the channel.
func (s *Channel) event(agent string, t event.Type, payload any) event.Event {
	e := event.Event{Channel: s.ID, Agent: agent, Type: t}
	if payload != nil {
		e.Payload = event.MustPayload(payload)
	}
	return e
}

// commitLocked logs events and applies them. The caller holds s.mu and
// signals the returned agents once it has released it.
func (s *Channel) commitLocked(ctx context.Context, evs ...event.Event) ([]*Agent, error) {
	if s.stopped {
		return nil, errStopped
	}
	if len(evs) == 0 {
		return nil, nil
	}
	out, err := s.host.Append(ctx, evs...)
	if err != nil {
		return nil, err
	}
	var fx effects
	for _, e := range out {
		s.st.apply(e, &fx)
	}
	var wake []*Agent
	for _, id := range fx.wake {
		if a, ok := s.agents[id]; ok {
			wake = append(wake, a)
		}
	}
	return wake, nil
}

// commit is commitLocked for a caller that does not hold the lock.
func (s *Channel) commit(ctx context.Context, evs ...event.Event) error {
	s.mu.Lock()
	wake, err := s.commitLocked(ctx, evs...)
	s.mu.Unlock()
	signal(wake)
	return err
}

func signal(agents []*Agent) {
	for _, a := range agents {
		a.signal()
	}
}

// Start logs the channel's creation under name and spawns its main agent.
// A channel may start with no model or an unconnected provider: the first
// turn reports the problem (PRD §8.4).
func (s *Channel) Start(ctx context.Context, name string) error {
	s.mu.Lock()
	role, modelID := s.st.role, s.st.model
	if _, ok := s.cfg.Presets[role]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("root preset %q not found", role)
	}
	_, err := s.commitLocked(ctx, s.event("", event.ChannelCreated, event.ChannelCreatedPayload{Name: name, Dir: s.Dir, Model: modelID, Role: role}))
	s.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = s.spawn(ctx, "", role, "main", "", "")
	return err
}

// Stop ends every agent without logging (daemon shutdown) and waits for
// their goroutines.
func (s *Channel) Stop() {
	s.mu.Lock()
	s.stopped = true
	agents := make([]*Agent, 0, len(s.agents))
	for _, a := range s.agents {
		agents = append(agents, a)
	}
	s.mu.Unlock()
	s.cancel()
	for _, a := range agents {
		a.stopMCP("", false)
	}
	// Model calls, tools and prompts all end with their context; a call that
	// ignores it must not hold the daemon's shutdown hostage.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopTimeout):
	}
}

// stopTimeout bounds how long Stop waits for agent goroutines.
const stopTimeout = 10 * time.Second

// --- settings ---

// Config returns the effective config.
func (s *Channel) Config() *config.Effective {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// SetConfig swaps the effective config (after a trust decision or edit).
func (s *Channel) SetConfig(cfg *config.Effective) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// Name is the channel's name, shown as #name.
func (s *Channel) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.name
}

// Model returns the channel-selected model.
func (s *Channel) Model() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.model
}

// Mode reports the channel's permission mode.
func (s *Channel) Mode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.mode
}

// Archived reports whether the channel is archived.
func (s *Channel) Archived() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.archived
}

// SetMode switches the channel's permission mode. Deny rules, questions and
// the trust prompt are unaffected in every mode.
func (s *Channel) SetMode(ctx context.Context, mode string) error {
	switch mode {
	case protocol.ModeAsk, protocol.ModeAuto, protocol.ModeYolo:
	default:
		return fmt.Errorf("unknown mode %q: ask, auto or yolo", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.mode == mode {
		return nil
	}
	_, err := s.commitLocked(ctx, s.event("", event.ChannelUpdated, event.ChannelUpdatedPayload{Mode: event.Str(mode)}))
	return err
}

// Rename takes a new name, which the daemon has normalised and checked.
func (s *Channel) Rename(ctx context.Context, name string) error {
	return s.commit(ctx, s.event("", event.ChannelUpdated, event.ChannelUpdatedPayload{Name: event.Str(name)}))
}

// SetModel changes the channel model: future spawns and the main agent's
// inherited default (running agents keep theirs, PRD §8.3).
func (s *Channel) SetModel(ctx context.Context, id string) error {
	if err := s.host.CheckModel(id); err != nil {
		return err
	}
	return s.commit(ctx, s.event("", event.ChannelUpdated, event.ChannelUpdatedPayload{Model: event.Str(id)}))
}

// Archive kills every agent and marks the channel archived.
func (s *Channel) Archive(ctx context.Context) error {
	if root := s.Root(); root != nil {
		_ = s.Kill(root.ID)
	}
	err := s.commit(ctx, s.event("", event.ChannelArchived, nil))
	s.cancel()
	return err
}

// --- agents ---

// Agent looks up an agent by id.
func (s *Channel) Agent(id string) (*Agent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[id]
	return a, ok
}

// Root returns the main agent.
func (s *Channel) Root() *Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.st.order) == 0 {
		return nil
	}
	return s.agents[s.st.order[0]]
}

// Agents returns agents in pre-order (main first, children after parents).
func (s *Channel) Agents() []*Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentsLocked()
}

func (s *Channel) agentsLocked() []*Agent {
	ids := s.preorderLocked()
	out := make([]*Agent, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.agents[id])
	}
	return out
}

// preorderLocked lists agent ids parents first, siblings in spawn order.
func (s *Channel) preorderLocked() []string {
	var out []string
	var walk func(id string)
	walk = func(id string) {
		out = append(out, id)
		for _, c := range s.st.agents[id].children {
			walk(c)
		}
	}
	for _, id := range s.st.order {
		if s.st.agents[s.st.agents[id].parent] == nil {
			walk(id)
		}
	}
	return out
}

// resolveLocked finds an agent by id, by name (with or without "@", in any
// case), or by a unique prefix of its id of at least four characters.
func (s *Channel) resolveLocked(ref string) (*agentState, bool) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "@")
	if a, ok := s.st.agents[ref]; ok {
		return a, true
	}
	if id, ok := s.st.names[normalizeName(ref)]; ok && ref != "" {
		return s.st.agents[id], true
	}
	if len(ref) < 4 {
		return nil, false
	}
	var found *agentState
	for id, a := range s.st.agents {
		if strings.HasPrefix(id, ref) {
			if found != nil {
				return nil, false // ambiguous
			}
			found = a
		}
	}
	return found, found != nil
}

// Tree describes every agent, in pre-order.
func (s *Channel) Tree() []protocol.AgentInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []protocol.AgentInfo
	for _, a := range s.agentsLocked() {
		out = append(out, a.infoLocked())
	}
	return out
}

// Info describes the channel.
func (s *Channel) Info() protocol.ChannelInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var states []protocol.AgentState
	live, cost := 0, 0.0
	for _, id := range s.st.order {
		a := s.st.agents[id]
		states = append(states, a.status())
		cost += a.cost
		if !a.killed {
			live++
		}
	}
	return protocol.ChannelInfo{
		ID: s.ID, Name: s.st.name, Dir: s.Dir, Model: s.st.model, RootAgent: s.st.role,
		Created: s.Created.Format(time.RFC3339), Archived: s.st.archived,
		Live: live, CostUSD: cost, TrustPending: s.cfg.TrustPending, Mode: s.st.mode,
		State: protocol.RollUp(states), Dirs: s.dirInfosLocked(),
	}
}

// Presets lists the roles available to the channel.
func (s *Channel) Presets() []protocol.PresetInfo {
	cfg := s.Config()
	var out []protocol.PresetInfo
	for _, p := range cfg.Presets {
		info := protocol.PresetInfo{Name: p.Name, Description: p.Description, Mode: p.Mode, Spawn: p.Spawn, Color: p.Color, MaxTurns: p.MaxTurns}
		for _, m := range p.Models {
			info.Models = append(info.Models, protocol.ModelSpec{ID: m.ID, Variants: m.Variants})
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// busyLocked counts agents in a turn or about to start one; idle children
// waiting for a follow-up cost nothing and do not count against fan-out.
func (s *Channel) busyLocked() int {
	n := 0
	for _, a := range s.st.agents {
		if a.busy() {
			n++
		}
	}
	return n
}

// resolveModel implements PRD §8.3 under the role's whitelist: an explicit
// spawn argument must be allowed; otherwise the parent's (or the channel's)
// model is inherited when the role allows it, else the role's default.
func (s *Channel) resolveModelLocked(spawnArg string, preset config.Preset, parent *agentState) (string, error) {
	if spawnArg != "" {
		if !preset.AllowsModel(spawnArg) {
			return "", fmt.Errorf("role %s does not allow model %s (allowed: %s)", preset.Name, spawnArg, modelList(preset))
		}
		return spawnArg, nil
	}
	inherited := s.st.model
	if parent != nil {
		inherited = parent.model
	}
	if inherited != "" && preset.AllowsModel(inherited) {
		return inherited, nil
	}
	if d := preset.DefaultModel(); d != "" {
		return d, nil
	}
	return inherited, nil
}

// modelList names a role's allowed models for error messages.
func modelList(p config.Preset) string {
	ids := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ", ")
}

// fitVariant is the variant an agent on model id runs under role p when it
// would otherwise inherit want: want when allowed, else the role's default.
func fitVariant(p config.Preset, id, want string) string {
	if p.AllowsVariant(id, want) {
		return want
	}
	return p.DefaultVariant(id)
}

// spawn creates and starts an agent; parentID is "" for the main agent. A
// task becomes the child's first input, a request from its parent, logged
// with the spawn in one transaction.
func (s *Channel) spawn(ctx context.Context, parentID, role, label, task, modelArg string) (*Agent, error) {
	s.mu.Lock()
	a, wake, err := s.spawnLocked(ctx, parentID, role, label, task, modelArg)
	s.mu.Unlock()
	signal(wake)
	return a, err
}

func (s *Channel) spawnLocked(ctx context.Context, parentID, role, label, task, modelArg string) (*Agent, []*Agent, error) {
	preset, ok := s.cfg.Presets[role]
	if !ok {
		return nil, nil, fmt.Errorf("unknown archetype %q", role)
	}
	var parent *agentState
	depth := 0
	if parentID != "" {
		if reservedNames[normalizeName(label)] {
			return nil, nil, fmt.Errorf("label %q is reserved: a child's name appears on its messages, so it may not read as the human or the system", label)
		}
		if parent = s.st.agents[parentID]; parent == nil || parent.killed {
			return nil, nil, fmt.Errorf("parent %q not found", parentID)
		}
		depth = parent.depth + 1
		if pp := s.roleLocked(parent).preset; !contains(pp.Spawn, role) {
			return nil, nil, fmt.Errorf("%s may not spawn %q (allowed: %v)", pp.Name, role, pp.Spawn)
		}
		if !preset.CanBeSubagent() {
			return nil, nil, fmt.Errorf("role %q is primary-only: it cannot be spawned", role)
		}
	} else if !preset.CanBePrimary() {
		return nil, nil, fmt.Errorf("role %q is subagent-only: it cannot be the main agent", role)
	}
	modelID, err := s.resolveModelLocked(modelArg, preset, parent)
	if err != nil {
		return nil, nil, err
	}
	variant := ""
	if parent != nil {
		if modelID == "" {
			return nil, nil, errors.New(ErrNoModel)
		}
		if err := s.host.CheckModel(modelID); err != nil {
			return nil, nil, err
		}
		if modelID == parent.model {
			variant = parent.variant // same model: same flavour
		}
	}
	variant = fitVariant(preset, modelID, variant)
	id := NewID("a")
	name, err := s.st.uniqueName(label, role, id)
	if err != nil {
		return nil, nil, err
	}
	parentCtx := s.ctx
	if p := s.agents[parentID]; p != nil {
		parentCtx = p.ctx
	}
	a := newAgent(s, id, parentID, depth, parentCtx)
	s.agents[id] = a
	evs := []event.Event{s.event(id, event.AgentSpawned, event.AgentSpawnedPayload{ID: id, Parent: parentID, Role: role, Name: name, Model: modelID, Variant: variant, Depth: depth})}
	if task != "" {
		in := event.Input{ID: NewID("i"), Kind: event.InputPrompt, Text: task}
		if parent != nil {
			in.Kind, in.From, in.FromName = event.InputRequest, parentID, parent.name
		}
		evs = append(evs, s.event(id, event.InputQueued, in))
	}
	wake, err := s.commitLocked(ctx, evs...)
	if err != nil {
		delete(s.agents, id)
		a.kill()
		return nil, nil, err
	}
	a.start()
	return a, wake, nil
}

// canSpawnLocked reports whether agent p may create a child now.
func (s *Channel) canSpawnLocked(p *agentState) (bool, string) {
	if p.depth+1 >= s.cfg.Limits.MaxDepth {
		return false, fmt.Sprintf("max depth %d reached", s.cfg.Limits.MaxDepth)
	}
	if s.busyLocked() >= s.cfg.Limits.MaxAgents {
		return false, fmt.Sprintf("max busy agents %d reached (idle children do not count)", s.cfg.Limits.MaxAgents)
	}
	if len(s.roleLocked(p).preset.Spawn) == 0 {
		return false, "this archetype cannot spawn"
	}
	return true, ""
}

// SpawnFromClient spawns on behalf of a human (PRD §9).
func (s *Channel) SpawnFromClient(ctx context.Context, parentID, role, label, task, modelArg string) (string, error) {
	s.mu.Lock()
	p := s.st.agents[parentID]
	if p == nil {
		s.mu.Unlock()
		return "", fmt.Errorf("agent %q not found", parentID)
	}
	if ok, why := s.canSpawnLocked(p); !ok {
		s.mu.Unlock()
		return "", errors.New(why)
	}
	a, wake, err := s.spawnLocked(ctx, parentID, role, label, task, modelArg)
	s.mu.Unlock()
	signal(wake)
	if err != nil {
		return "", err
	}
	return a.ID, nil
}

// --- the human's messages ---

// Send queues a prompt for an agent: it runs after the current turn.
func (s *Channel) Send(ctx context.Context, agentID, text, source string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	return a.Prompt(ctx, text, source)
}

// Steer delivers a steer: it reaches the agent at its next model call.
func (s *Channel) Steer(ctx context.Context, agentID, text, source string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	return a.Steer(ctx, text, source)
}

// Post is the human's message in the channel chat (docs/super-chat.md): the
// @names at its front say which agents it goes to, and what follows reaches
// each as a steer that is owed a reply. A post with no leading name goes to
// the main agent; a leading name that is no live agent refuses the whole
// post. The post and its deliveries are one transaction.
func (s *Channel) Post(ctx context.Context, text, _ string) ([]string, error) {
	refs, message := protocol.Addressees(text)
	if strings.TrimSpace(message) == "" {
		return nil, errors.New("empty message")
	}
	s.mu.Lock()
	var targets []*agentState
	for _, ref := range refs {
		a, ok := s.resolveLocked(ref)
		switch {
		case !ok:
			s.mu.Unlock()
			return nil, fmt.Errorf("no agent named @%s in this channel", ref)
		case a.killed:
			s.mu.Unlock()
			return nil, fmt.Errorf("@%s is killed", ref)
		case !slices.Contains(targets, a):
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		if len(s.st.order) == 0 {
			s.mu.Unlock()
			return nil, errors.New("the channel has no agents")
		}
		targets = []*agentState{s.st.agents[s.st.order[0]]}
	}
	post := NewID("post")
	names := make([]string, len(targets))
	evs := []event.Event{{}}
	for i, a := range targets {
		names[i] = a.name
		evs = append(evs, s.event(a.id, event.InputQueued, event.Input{ID: NewID("i"), Kind: event.InputSteer, Text: message, Post: post}))
	}
	evs[0] = s.event("", event.ChatPosted, event.ChatPayload{ID: post, Text: message, To: names})
	wake, err := s.commitLocked(ctx, evs...)
	s.mu.Unlock()
	signal(wake)
	return names, err
}

// Cancel ends an agent's current turn.
func (s *Channel) Cancel(agentID string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	a.Cancel()
	return nil
}

// Kill tears down an agent and its subtree, children first.
func (s *Channel) Kill(agentID string) error {
	s.mu.Lock()
	if _, ok := s.st.agents[agentID]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("agent %q not found", agentID)
	}
	var victims []*Agent
	var evs []event.Event
	var walk func(id string)
	walk = func(id string) {
		st := s.st.agents[id]
		for _, c := range st.children {
			walk(c)
		}
		if !st.killed {
			victims = append(victims, s.agents[id])
			evs = append(evs, s.event(id, event.AgentKilled, nil))
		}
	}
	walk(agentID)
	_, err := s.commitLocked(context.Background(), evs...)
	for _, a := range victims {
		a.jobs = map[string]*jobRun{} // their processes end with the agent's context
	}
	s.mu.Unlock()
	for _, a := range victims {
		a.kill()
		a.stopMCP("", false)
	}
	return err
}
