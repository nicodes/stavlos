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
	// Prompt asks the human and waits; opened runs once the prompt can be
	// listed and answered, before any answer is taken.
	Prompt(ctx context.Context, info protocol.PromptInfo, opened func()) escalation.Answer
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
func (c *Channel) event(agent string, t event.Type, payload any) event.Event {
	e := event.Event{Channel: c.ID, Agent: agent, Type: t}
	if payload != nil {
		e.Payload = event.MustPayload(payload)
	}
	return e
}

// commitLocked logs events and applies them. The caller holds c.mu and
// signals the returned agents once it has released it.
func (c *Channel) commitLocked(ctx context.Context, evs ...event.Event) ([]*Agent, error) {
	if c.stopped {
		return nil, errStopped
	}
	if len(evs) == 0 {
		return nil, nil
	}
	out, err := c.host.Append(ctx, evs...)
	if err != nil {
		return nil, err
	}
	var fx effects
	for _, e := range out {
		c.st.apply(e, &fx)
	}
	var wake []*Agent
	for _, id := range fx.wake {
		if a, ok := c.agents[id]; ok {
			wake = append(wake, a)
		}
	}
	return wake, nil
}

// commit is commitLocked for a caller that does not hold the lock.
func (c *Channel) commit(ctx context.Context, evs ...event.Event) error {
	c.mu.Lock()
	wake, err := c.commitLocked(ctx, evs...)
	c.mu.Unlock()
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
func (c *Channel) Start(ctx context.Context, name string) error {
	c.mu.Lock()
	role, modelID := c.st.role, c.st.model
	if _, ok := c.cfg.Presets[role]; !ok {
		c.mu.Unlock()
		return fmt.Errorf("root preset %q not found", role)
	}
	_, err := c.commitLocked(ctx, c.event("", event.ChannelCreated, event.ChannelCreatedPayload{Name: name, Dir: c.Dir, Model: modelID, Role: role}))
	c.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = c.spawn(ctx, "", role, "main", "", "")
	return err
}

// Stop ends every agent without logging (daemon shutdown) and waits for
// their goroutines.
func (c *Channel) Stop() {
	c.mu.Lock()
	c.stopped = true
	agents := make([]*Agent, 0, len(c.agents))
	for _, a := range c.agents {
		agents = append(agents, a)
	}
	c.mu.Unlock()
	c.cancel()
	for _, a := range agents {
		a.stopMCP("", false)
	}
	// Model calls, tools and prompts all end with their context; a call that
	// ignores it must not hold the daemon's shutdown hostage.
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
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
func (c *Channel) Config() *config.Effective {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

// SetConfig swaps the effective config (after a trust decision or edit).
func (c *Channel) SetConfig(cfg *config.Effective) {
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
}

// Name is the channel's name, shown as #name.
func (c *Channel) Name() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.name
}

// Model returns the channel-selected model.
func (c *Channel) Model() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.model
}

// Mode reports the channel's permission mode.
func (c *Channel) Mode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.mode
}

// Archived reports whether the channel is archived.
func (c *Channel) Archived() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.archived
}

// SetMode switches the channel's permission mode. Deny rules, questions and
// the trust prompt are unaffected in every mode.
func (c *Channel) SetMode(ctx context.Context, mode string) error {
	switch mode {
	case protocol.ModeAsk, protocol.ModeAuto, protocol.ModeYolo:
	default:
		return fmt.Errorf("unknown mode %q: ask, auto or yolo", mode)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.mode == mode {
		return nil
	}
	_, err := c.commitLocked(ctx, c.event("", event.ChannelUpdated, event.ChannelUpdatedPayload{Mode: event.Str(mode)}))
	return err
}

// Rename takes a new name, which the daemon has normalised and checked.
func (c *Channel) Rename(ctx context.Context, name string) error {
	return c.commit(ctx, c.event("", event.ChannelUpdated, event.ChannelUpdatedPayload{Name: event.Str(name)}))
}

// SetModel changes the channel model: future spawns and the main agent's
// inherited default (running agents keep theirs, PRD §8.3).
func (c *Channel) SetModel(ctx context.Context, id string) error {
	if err := c.host.CheckModel(id); err != nil {
		return err
	}
	return c.commit(ctx, c.event("", event.ChannelUpdated, event.ChannelUpdatedPayload{Model: event.Str(id)}))
}

// Archive kills every agent and marks the channel archived.
func (c *Channel) Archive(ctx context.Context) error {
	if root := c.Root(); root != nil {
		_ = c.Kill(root.ID)
	}
	err := c.commit(ctx, c.event("", event.ChannelArchived, nil))
	c.cancel()
	return err
}

// --- agents ---

// Agent looks up an agent by id.
func (c *Channel) Agent(id string) (*Agent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.agents[id]
	return a, ok
}

// Root returns the main agent.
func (c *Channel) Root() *Agent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.st.order) == 0 {
		return nil
	}
	return c.agents[c.st.order[0]]
}

// Agents returns agents in pre-order (main first, children after parents).
func (c *Channel) Agents() []*Agent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.agentsLocked()
}

func (c *Channel) agentsLocked() []*Agent {
	ids := c.preorderLocked()
	out := make([]*Agent, 0, len(ids))
	for _, id := range ids {
		out = append(out, c.agents[id])
	}
	return out
}

// preorderLocked lists agent ids parents first, siblings in spawn order.
func (c *Channel) preorderLocked() []string {
	var out []string
	var walk func(id string)
	walk = func(id string) {
		out = append(out, id)
		for _, child := range c.st.agents[id].children {
			walk(child)
		}
	}
	for _, id := range c.st.order {
		if c.st.agents[c.st.agents[id].parent] == nil {
			walk(id)
		}
	}
	return out
}

// resolveLocked finds an agent by id, by name (with or without "@", in any
// case), or by a unique prefix of its id of at least four characters.
func (c *Channel) resolveLocked(ref string) (*agentState, bool) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "@")
	if a, ok := c.st.agents[ref]; ok {
		return a, true
	}
	if id, ok := c.st.names[normalizeName(ref)]; ok && ref != "" {
		return c.st.agents[id], true
	}
	if len(ref) < 4 {
		return nil, false
	}
	var found *agentState
	for id, a := range c.st.agents {
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
func (c *Channel) Tree() []protocol.AgentInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []protocol.AgentInfo
	for _, a := range c.agentsLocked() {
		out = append(out, a.infoLocked())
	}
	return out
}

// Info describes the channel.
func (c *Channel) Info() protocol.ChannelInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	var states []protocol.AgentState
	live, cost := 0, 0.0
	for _, id := range c.st.order {
		a := c.st.agents[id]
		states = append(states, a.status())
		cost += a.cost
		if !a.killed {
			live++
		}
	}
	return protocol.ChannelInfo{
		ID: c.ID, Name: c.st.name, Dir: c.Dir, Model: c.st.model, RootAgent: c.st.role,
		Created: c.Created.Format(time.RFC3339), Archived: c.st.archived,
		Live: live, CostUSD: cost, TrustPending: c.cfg.TrustPending, Mode: c.st.mode,
		State: protocol.RollUp(states), Dirs: c.dirInfosLocked(),
	}
}

// Presets lists the roles available to the channel.
func (c *Channel) Presets() []protocol.PresetInfo {
	cfg := c.Config()
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
func (c *Channel) busyLocked() int {
	n := 0
	for _, a := range c.st.agents {
		if a.busy() {
			n++
		}
	}
	return n
}

// resolveModelLocked implements PRD §8.3 under the role's whitelist: an explicit
// spawn argument must be allowed; otherwise the parent's (or the channel's)
// model is inherited when the role allows it, else the role's default.
func (c *Channel) resolveModelLocked(spawnArg string, preset config.Preset, parent *agentState) (string, error) {
	if spawnArg != "" {
		if !preset.AllowsModel(spawnArg) {
			return "", fmt.Errorf("role %s does not allow model %s (allowed: %s)", preset.Name, spawnArg, modelList(preset))
		}
		return spawnArg, nil
	}
	inherited := c.st.model
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
func (c *Channel) spawn(ctx context.Context, parentID, role, label, task, modelArg string) (*Agent, error) {
	c.mu.Lock()
	a, wake, err := c.spawnLocked(ctx, parentID, role, label, task, modelArg)
	c.mu.Unlock()
	signal(wake)
	return a, err
}

func (c *Channel) spawnLocked(ctx context.Context, parentID, role, label, task, modelArg string) (*Agent, []*Agent, error) {
	preset, ok := c.cfg.Presets[role]
	if !ok {
		return nil, nil, fmt.Errorf("unknown archetype %q", role)
	}
	var parent *agentState
	depth := 0
	if parentID != "" {
		if reservedNames[normalizeName(label)] {
			return nil, nil, fmt.Errorf("label %q is reserved: a child's name appears on its messages, so it may not read as the human or the system", label)
		}
		if parent = c.st.agents[parentID]; parent == nil || parent.killed {
			return nil, nil, fmt.Errorf("parent %q not found", parentID)
		}
		depth = parent.depth + 1
		if pp := c.roleLocked(parent).preset; !contains(pp.Spawn, role) {
			return nil, nil, fmt.Errorf("%s may not spawn %q (allowed: %v)", pp.Name, role, pp.Spawn)
		}
		if !preset.CanBeSubagent() {
			return nil, nil, fmt.Errorf("role %q is primary-only: it cannot be spawned", role)
		}
	} else if !preset.CanBePrimary() {
		return nil, nil, fmt.Errorf("role %q is subagent-only: it cannot be the main agent", role)
	}
	modelID, err := c.resolveModelLocked(modelArg, preset, parent)
	if err != nil {
		return nil, nil, err
	}
	variant := ""
	if parent != nil {
		if modelID == "" {
			return nil, nil, errors.New(ErrNoModel)
		}
		if err := c.host.CheckModel(modelID); err != nil {
			return nil, nil, err
		}
		if modelID == parent.model {
			variant = parent.variant // same model: same flavour
		}
	}
	variant = fitVariant(preset, modelID, variant)
	id := NewID("a")
	name, err := c.st.uniqueName(label, role, id)
	if err != nil {
		return nil, nil, err
	}
	parentCtx := c.ctx
	if p := c.agents[parentID]; p != nil {
		parentCtx = p.ctx
	}
	a := newAgent(c, id, parentID, depth, parentCtx)
	c.agents[id] = a
	evs := []event.Event{c.event(id, event.AgentSpawned, event.AgentSpawnedPayload{ID: id, Parent: parentID, Role: role, Name: name, Model: modelID, Variant: variant, Depth: depth})}
	if task != "" {
		in := event.Input{ID: NewID("i"), Kind: event.InputPrompt, Text: task}
		if parent != nil {
			in.Kind, in.From, in.FromName = event.InputRequest, parentID, parent.name
		}
		evs = append(evs, c.event(id, event.InputQueued, in))
	}
	wake, err := c.commitLocked(ctx, evs...)
	if err != nil {
		delete(c.agents, id)
		a.kill()
		return nil, nil, err
	}
	a.start()
	return a, wake, nil
}

// canSpawnLocked reports whether agent p may create a child now.
func (c *Channel) canSpawnLocked(p *agentState) (bool, string) {
	if p.depth+1 >= c.cfg.Limits.MaxDepth {
		return false, fmt.Sprintf("max depth %d reached", c.cfg.Limits.MaxDepth)
	}
	if c.busyLocked() >= c.cfg.Limits.MaxAgents {
		return false, fmt.Sprintf("max busy agents %d reached (idle children do not count)", c.cfg.Limits.MaxAgents)
	}
	if len(c.roleLocked(p).preset.Spawn) == 0 {
		return false, "this archetype cannot spawn"
	}
	return true, ""
}

// SpawnFromClient spawns on behalf of a human (PRD §9).
func (c *Channel) SpawnFromClient(ctx context.Context, parentID, role, label, task, modelArg string) (string, error) {
	c.mu.Lock()
	p := c.st.agents[parentID]
	if p == nil {
		c.mu.Unlock()
		return "", fmt.Errorf("agent %q not found", parentID)
	}
	if ok, why := c.canSpawnLocked(p); !ok {
		c.mu.Unlock()
		return "", errors.New(why)
	}
	a, wake, err := c.spawnLocked(ctx, parentID, role, label, task, modelArg)
	c.mu.Unlock()
	signal(wake)
	if err != nil {
		return "", err
	}
	return a.ID, nil
}

// --- the human's messages ---

// Send queues a prompt for an agent: it runs after the current turn.
func (c *Channel) Send(ctx context.Context, agentID, text, source string) error {
	a, ok := c.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	return a.Prompt(ctx, text, source)
}

// Steer delivers a steer: it reaches the agent at its next model call.
func (c *Channel) Steer(ctx context.Context, agentID, text, source string) error {
	a, ok := c.Agent(agentID)
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
func (c *Channel) Post(ctx context.Context, text, _ string) ([]string, error) {
	refs, message := protocol.Addressees(text)
	if strings.TrimSpace(message) == "" {
		return nil, errors.New("empty message")
	}
	c.mu.Lock()
	var targets []*agentState
	for _, ref := range refs {
		a, ok := c.resolveLocked(ref)
		switch {
		case !ok:
			c.mu.Unlock()
			return nil, fmt.Errorf("no agent named @%s in this channel", ref)
		case a.killed:
			c.mu.Unlock()
			return nil, fmt.Errorf("@%s is killed", ref)
		case !slices.Contains(targets, a):
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		if len(c.st.order) == 0 {
			c.mu.Unlock()
			return nil, errors.New("the channel has no agents")
		}
		targets = []*agentState{c.st.agents[c.st.order[0]]}
	}
	post := NewID("post")
	names := make([]string, len(targets))
	evs := []event.Event{{}}
	for i, a := range targets {
		names[i] = a.name
		evs = append(evs, c.event(a.id, event.InputQueued, event.Input{ID: NewID("i"), Kind: event.InputSteer, Text: message, Post: post}))
	}
	evs[0] = c.event("", event.ChatPosted, event.ChatPayload{ID: post, Text: message, To: names})
	wake, err := c.commitLocked(ctx, evs...)
	c.mu.Unlock()
	signal(wake)
	return names, err
}

// Cancel ends an agent's current turn.
func (c *Channel) Cancel(agentID string) error {
	a, ok := c.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	a.Cancel()
	return nil
}

// Kill tears down an agent and its subtree, children first.
func (c *Channel) Kill(agentID string) error {
	c.mu.Lock()
	if _, ok := c.st.agents[agentID]; !ok {
		c.mu.Unlock()
		return fmt.Errorf("agent %q not found", agentID)
	}
	var victims []*Agent
	var evs []event.Event
	var walk func(id string)
	walk = func(id string) {
		st := c.st.agents[id]
		for _, child := range st.children {
			walk(child)
		}
		if !st.killed {
			victims = append(victims, c.agents[id])
			evs = append(evs, c.event(id, event.AgentKilled, nil))
		}
	}
	walk(agentID)
	_, err := c.commitLocked(context.Background(), evs...)
	for _, a := range victims {
		a.jobs = map[string]*jobRun{} // their processes end with the agent's context
	}
	c.mu.Unlock()
	for _, a := range victims {
		a.kill()
		a.stopMCP("", false)
	}
	return err
}
