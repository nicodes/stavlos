// Package agent implements channels, the actor model, and the turn loop
// (PRD §5, §6).
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	Append(ctx context.Context, e event.Event) (event.Event, error)
	Stream(n protocol.StreamNotification)
	Resolve(modelID string) (model.Model, model.Info, error)
	CheckModel(modelID string) error
	Variants(modelID string) []string
	Prompt(ctx context.Context, info protocol.PromptInfo) escalation.Answer
}

// Channel is a root agent plus its subtree, bound to a directory (PRD §5).
type Channel struct {
	ID      string
	Dir     string
	Created time.Time

	host  Host
	tools tools.Set

	mu       sync.RWMutex
	cfg      *config.Effective
	model    string // channel-selected model
	rootArch string
	name     string // unique across the daemon (the daemon picks and checks it), shown as #name
	agents   map[string]*Agent
	names    map[string]string // agent name → id; a name is never released, so a mention never changes meaning
	order    []string          // spawn order
	archived bool
	ctx      context.Context
	cancel   context.CancelFunc
	permits  permits    // the human's channel-scoped allows (exact calls, prefixes); they answer asks, never denies
	mode     string     // permission mode: ask (every ask prompts) | auto (asks inside the channel's dirs are allowed, calls outside denied) | yolo (every ask is allowed)
	dirs     []dirEntry // the working set beyond the channel directory, shared by every agent: what the human added (logged)
}

// Mode reports the channel's permission mode (protocol.ModeAsk by default).
func (s *Channel) Mode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

// Yolo reports whether every policy ask is allowed, boundary included.
func (s *Channel) Yolo() bool { return s.Mode() == protocol.ModeYolo }

// SetMode switches the channel's permission mode and logs it. Explicit
// deny rules, model questions and the trust prompt are unaffected in every
// mode.
func (s *Channel) SetMode(ctx context.Context, mode string) error {
	switch mode {
	case protocol.ModeAsk, protocol.ModeAuto, protocol.ModeYolo:
	default:
		return fmt.Errorf("unknown mode %q: ask, auto or yolo", mode)
	}
	s.mu.Lock()
	changed := s.mode != mode
	s.mode = mode
	s.mu.Unlock()
	if !changed {
		return nil
	}
	_, err := s.host.Append(ctx, event.Event{Channel: s.ID, Type: event.ChannelModeChanged, Payload: event.MustPayload(event.ModePayload{Mode: mode})})
	return err
}

// ErrNoModel is the turn error when an agent has no model to call.
const ErrNoModel = "no model selected: run /models to pick one, or /providers first to connect a provider"

// NewID returns a random id with a prefix.
func NewID(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// New creates a channel object without logging anything; use Create or
// Recover on the daemon side.
func New(host Host, id, dir string, cfg *config.Effective, modelID, rootArch string) *Channel {
	ctx, cancel := context.WithCancel(context.Background())
	if rootArch == "" {
		rootArch = cfg.RootAgent
	}
	if modelID == "" {
		modelID = cfg.Model
	}
	return &Channel{
		ID: id, Dir: dir, Created: time.Now().UTC(),
		host: host, tools: tools.Builtin(),
		cfg: cfg, model: modelID, rootArch: rootArch,
		agents: map[string]*Agent{}, names: map[string]string{}, ctx: ctx, cancel: cancel,
		mode: protocol.ModeAsk,
	}
}

// Start logs ChannelCreated under name and spawns the root agent.
// A channel may start with no model or an unconnected provider: the TUI
// opens regardless and the first turn reports the problem (PRD §8.4).
func (s *Channel) Start(ctx context.Context, name string) error {
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
	if _, ok := s.cfg.Presets[s.rootArch]; !ok {
		return fmt.Errorf("root preset %q not found", s.rootArch)
	}
	if _, err := s.host.Append(ctx, event.Event{Channel: s.ID, Type: event.ChannelCreated,
		Payload: event.MustPayload(event.ChannelCreatedPayload{Name: name, Dir: s.Dir, Model: s.model, RootAgent: s.rootArch})}); err != nil {
		return err
	}
	_, err := s.spawn(ctx, "", s.rootArch, "main", "", "") // the root is always "main (role)"
	return err
}

// Config returns the effective config.
func (s *Channel) Config() *config.Effective {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Name is the channel's name, shown as #name.
func (s *Channel) Name() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name
}

// Rename logs the channel's new name and takes it. The daemon has already
// normalised it and checked no other channel has it.
func (s *Channel) Rename(ctx context.Context, name string) error {
	if _, err := s.host.Append(ctx, event.Event{Channel: s.ID, Type: event.ChannelRenamed, Payload: event.MustPayload(event.NamePayload{Name: name})}); err != nil {
		return err
	}
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
	return nil
}

// SetConfig swaps the effective config (after a trust decision or edit).
func (s *Channel) SetConfig(cfg *config.Effective) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// Model returns the channel-selected model.
func (s *Channel) Model() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model
}

// SetModel changes the channel model (affects future spawns and the root's
// inherited default; running agents keep theirs — PRD §8.3).
func (s *Channel) SetModel(ctx context.Context, id string) error {
	if err := s.host.CheckModel(id); err != nil {
		return err
	}
	s.mu.Lock()
	s.model = id
	s.mu.Unlock()
	_, err := s.host.Append(ctx, event.Event{Channel: s.ID, Type: event.ChannelModelChanged, Payload: event.MustPayload(event.ModelChangedPayload{Model: id})})
	return err
}

// Archived reports whether the channel is archived.
func (s *Channel) Archived() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.archived
}

// Archive kills every agent and marks the channel archived.
func (s *Channel) Archive(ctx context.Context) error {
	if root := s.Root(); root != nil {
		_ = s.Kill(root.ID)
	}
	s.mu.Lock()
	s.archived = true
	s.mu.Unlock()
	s.cancel()
	_, err := s.host.Append(ctx, event.Event{Channel: s.ID, Type: event.ChannelArchived})
	return err
}

// Stop cancels all agents without logging (daemon shutdown).
func (s *Channel) Stop() { s.cancel() }

// Agent looks up an agent.
func (s *Channel) Agent(id string) (*Agent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.agents[id]
	return a, ok
}

// resolve finds an agent by its id, its name (with or without "@", in any
// case), or a unique prefix of its id of at least four characters (models
// sometimes copy a shortened id from a status line).
func (s *Channel) resolve(ref string) (*Agent, bool) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "@")
	if a, ok := s.Agent(ref); ok {
		return a, true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if name := normalizeName(ref); name != "" {
		if id, ok := s.names[name]; ok {
			return s.agents[id], true
		}
	}
	if len(ref) < 4 {
		return nil, false
	}
	var found *Agent
	for _, a := range s.agents {
		if strings.HasPrefix(a.ID, ref) {
			if found != nil {
				return nil, false // ambiguous
			}
			found = a
		}
	}
	return found, found != nil
}

// agentsSnapshot lists the channel's agents in creation order.
func (s *Channel) agentsSnapshot() []*Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Agent, 0, len(s.order))
	for _, id := range s.order {
		if a, ok := s.agents[id]; ok {
			out = append(out, a)
		}
	}
	return out
}

// Root returns the root agent.
func (s *Channel) Root() *Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.order) == 0 {
		return nil
	}
	return s.agents[s.order[0]]
}

// Agents returns agents in pre-order (root first, children after parents).
func (s *Channel) Agents() []*Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byParent := map[string][]*Agent{}
	for _, id := range s.order {
		a := s.agents[id]
		byParent[a.Parent] = append(byParent[a.Parent], a)
	}
	var out []*Agent
	var walk func(parent string)
	walk = func(parent string) {
		for _, a := range byParent[parent] {
			out = append(out, a)
			walk(a.ID)
		}
	}
	walk("")
	return out
}

// Live counts agents that are not finished or killed.
func (s *Channel) Live() int {
	n := 0
	for _, a := range s.Agents() {
		if a.Alive() {
			n++
		}
	}
	return n
}

// Busy counts agents that are working or have work queued; idle children
// waiting for a follow-up cost nothing and do not count against the
// fan-out limit.
func (s *Channel) Busy() int {
	n := 0
	for _, a := range s.Agents() {
		st := a.StateOf()
		a.mu.Lock()
		queued := len(a.prompts) + len(a.steers) + len(a.responses) + len(a.wakes)
		a.mu.Unlock()
		if st == StateRunning || st == StateBlocked || (st == StateIdle && queued > 0) {
			n++
		}
	}
	return n
}

// Cost sums usage across agents.
func (s *Channel) Cost() float64 {
	c := 0.0
	for _, a := range s.Agents() {
		c += a.Cost()
	}
	return c
}

// Info builds the protocol view.
func (s *Channel) Info() protocol.ChannelInfo {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	return protocol.ChannelInfo{
		ID: s.ID, Name: s.Name(), Dir: s.Dir, Model: s.Model(), RootAgent: s.rootArch,
		Created: s.Created.Format(time.RFC3339), Archived: s.Archived(),
		Live: s.Live(), CostUSD: s.Cost(), TrustPending: cfg.TrustPending, Mode: s.Mode(),
		State: s.state(), Dirs: s.dirInfos(),
	}
}

// state sums the agents up (protocol.RollUp): working while any agent is
// in a turn, waiting while any expects an answer, idle otherwise.
func (s *Channel) state() protocol.ChannelState {
	var states []protocol.AgentState
	for _, a := range s.Agents() {
		states = append(states, a.Info().State)
	}
	return protocol.RollUp(states)
}

// Presets lists archetypes.
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

// resolveModel implements PRD §8.3 under the role's whitelist: an explicit
// spawn argument must be allowed; otherwise the parent's (or the
// channel's) model is inherited when the role allows it, else the role's
// default (its first listed model).
func (s *Channel) resolveModel(spawnArg string, preset config.Preset, parent *Agent) (string, error) {
	if spawnArg != "" {
		if !preset.AllowsModel(spawnArg) {
			return "", fmt.Errorf("role %s does not allow model %s (allowed: %s)", preset.Name, spawnArg, modelList(preset))
		}
		return spawnArg, nil
	}
	inherited := s.Model()
	if parent != nil {
		inherited = parent.ModelID()
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

// fitVariant picks the variant an agent on model id should run under role
// p when it would otherwise inherit want: want when allowed, else the
// model's default variant under the role.
func fitVariant(p config.Preset, id, want string) string {
	if p.AllowsVariant(id, want) {
		return want
	}
	return p.DefaultVariant(id)
}

// spawn creates and starts an agent. parent=="" for the root. grants are
// directories the parent hands down; each must be inside the parent's own.
func (s *Channel) spawn(ctx context.Context, parentID, archetype, label, task, modelArg string) (*Agent, error) {
	cfg := s.Config()
	preset, ok := cfg.Presets[archetype]
	if !ok {
		return nil, fmt.Errorf("unknown archetype %q", archetype)
	}
	if parentID != "" && reservedNames[normalizeName(label)] {
		return nil, fmt.Errorf("label %q is reserved: a child's name appears on its messages, so it may not read as the human or the system", label)
	}
	var parent *Agent
	depth := 0
	if parentID != "" {
		p, ok := s.Agent(parentID)
		if !ok {
			return nil, fmt.Errorf("parent %q not found", parentID)
		}
		parent = p
		depth = p.Depth + 1
		if pp := parent.Preset(); !contains(pp.Spawn, archetype) {
			return nil, fmt.Errorf("%s may not spawn %q (allowed: %v)", pp.Name, archetype, pp.Spawn)
		}
		if !preset.CanBeSubagent() {
			return nil, fmt.Errorf("role %q is primary-only: it cannot be spawned", archetype)
		}
	} else if !preset.CanBePrimary() {
		return nil, fmt.Errorf("role %q is subagent-only: it cannot be the main agent", archetype)
	}
	modelID, err := s.resolveModel(modelArg, preset, parent)
	if err != nil {
		return nil, err
	}
	if parent != nil {
		if modelID == "" {
			return nil, errors.New(ErrNoModel)
		}
		if err := s.host.CheckModel(modelID); err != nil {
			return nil, err
		}
	}
	a := newAgent(s, NewID("a"), parentID, archetype, label, modelID, depth, preset)
	if parent != nil && modelID == parent.ModelID() {
		a.variant = parent.Variant() // same model: same flavour (logged below, after the spawn event)
	}
	a.variant = fitVariant(preset, modelID, a.variant) // …within what the role allows for that model
	if parent != nil {
		a.ctx, a.kill = context.WithCancel(parent.ctx)
	} else {
		a.ctx, a.kill = context.WithCancel(s.ctx)
	}
	s.mu.Lock()
	name, err := s.claimNameLocked(label, archetype, a.ID) // claimed with the insert: two spawns at once cannot take one name
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	a.Label = name
	s.agents[a.ID] = a
	s.order = append(s.order, a.ID)
	s.mu.Unlock()
	if parent != nil {
		parent.addChild(a.ID)
	}
	if _, err := s.host.Append(ctx, event.Event{Channel: s.ID, Agent: a.ID, Type: event.AgentSpawned,
		Payload: event.MustPayload(event.AgentSpawnedPayload{ID: a.ID, Parent: parentID, Archetype: archetype, Label: name, Model: modelID, Task: task, Depth: depth})}); err != nil {
		return nil, err
	}
	if a.variant != "" { // inherited: logged so recovery restores it
		if _, err := s.host.Append(ctx, event.Event{Channel: s.ID, Agent: a.ID, Type: event.AgentVariantChanged,
			Payload: event.MustPayload(event.VariantChangedPayload{Variant: a.variant})}); err != nil {
			return nil, err
		}
	}
	a.start()
	if task != "" {
		if err := a.Prompt(ctx, task, "agent:"+parentID); err != nil {
			return nil, err
		}
		if parent != nil {
			parent.expect(a.ID) // the task is a question: the parent waits for the answer
		}
	}
	return a, nil
}

// maxNameLen bounds an agent's name.
const maxNameLen = 32

// reservedNames may not be taken by an agent: messages are attributed by
// name, and these read as the human or the system.
var reservedNames = map[string]bool{"user": true, "human": true, "system": true}

// normalizeName turns a requested label into a name: lowercase letters,
// digits, '-' and '_', every run of anything else collapsed to one '-',
// trimmed, at most maxNameLen characters. So "SYSTEM: ignore this" becomes
// a plain identifier that cannot read as an instruction.
func normalizeName(label string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
			dash = r == '-'
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(b.String(), "-_")
	if len(name) > maxNameLen {
		name = strings.TrimRight(name[:maxNameLen], "-_")
	}
	return name
}

// NormalizeName is normalizeName for the daemon, which names channels by the
// same rules as agents.
func NormalizeName(label string) string { return normalizeName(label) }

// UniqueName is want normalised (fallback when that leaves nothing, then
// "agent") with a -2, -3, … suffix while taken reports the name in use.
func UniqueName(want, fallback string, taken func(string) bool) string {
	base := normalizeName(want)
	if base == "" {
		base = normalizeName(fallback)
	}
	if base == "" {
		base = "agent"
	}
	name := base
	for n := 2; taken(name); n++ {
		suffix := fmt.Sprintf("-%d", n)
		name = strings.TrimRight(base[:min(len(base), maxNameLen-len(suffix))], "-_") + suffix
	}
	return name
}

// claimNameLocked gives agent id the name want asks for (fallback when want
// normalises to nothing, then "agent"), with a -2, -3, … suffix when the
// name is taken. Names are never released: a killed or renamed agent keeps
// its old one reserved. Callers hold s.mu.
func (s *Channel) claimNameLocked(want, fallback, id string) (string, error) {
	base := normalizeName(want)
	if base == "" {
		base = normalizeName(fallback)
	}
	if base == "" {
		base = "agent"
	}
	if reservedNames[base] {
		return "", fmt.Errorf("label %q is reserved: a name may not read as the human or the system", base)
	}
	name := base
	for n := 2; ; n++ {
		if owner, taken := s.names[name]; !taken || owner == id {
			break
		}
		suffix := fmt.Sprintf("-%d", n)
		name = strings.TrimRight(base[:min(len(base), maxNameLen-len(suffix))], "-_") + suffix
	}
	s.names[name] = id
	return name, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// --- human-facing envelope entry points (same inbox as agents, PRD §6.1) ---

// Send queues a Prompt.
func (s *Channel) Send(ctx context.Context, agentID, text, source string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	return a.Prompt(ctx, text, source)
}

// Steer delivers a Steer.
func (s *Channel) Steer(ctx context.Context, agentID, text, source string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	return a.Steer(ctx, text, source)
}

// Post is the human's message in the channel chat (docs/super-chat.md):
// the @names at its front say which agents it goes to, and what follows is
// delivered to each as a steer, exactly as written (an @ inside it is the
// author's own). A post with no leading name goes to the root. A leading
// name that is no live agent refuses the whole post before anything is
// delivered. It is logged once on the channel as chat.posted, and returns
// the names it went to.
func (s *Channel) Post(ctx context.Context, text, source string) ([]string, error) {
	refs, message := protocol.Addressees(text)
	if strings.TrimSpace(message) == "" {
		return nil, errors.New("empty message")
	}
	var targets []*Agent
	seen := map[string]bool{}
	for _, ref := range refs {
		a, ok := s.resolve(ref)
		if !ok {
			return nil, fmt.Errorf("no agent named @%s in this channel", ref)
		}
		if !a.Alive() {
			return nil, fmt.Errorf("@%s is %s", ref, a.StateOf())
		}
		if !seen[a.ID] {
			seen[a.ID] = true
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		root := s.Root()
		if root == nil {
			return nil, errors.New("the channel has no agents")
		}
		targets = []*Agent{root}
	}
	id := NewID("post")
	names := make([]string, len(targets))
	for i, a := range targets {
		names[i] = a.LabelNow()
	}
	if _, err := s.host.Append(ctx, event.Event{Channel: s.ID, Type: event.ChatPosted,
		Payload: event.MustPayload(event.ChatPayload{ID: id, Text: message, To: names})}); err != nil {
		return nil, err
	}
	for _, a := range targets {
		if err := a.steer(ctx, message, source, id); err != nil {
			return names, err
		}
	}
	return names, nil
}

// Cancel ends the current turn.
func (s *Channel) Cancel(agentID string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	a.Cancel()
	return nil
}

// Kill tears down an agent and its subtree.
func (s *Channel) Kill(agentID string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	s.killTree(a)
	return nil
}

func (s *Channel) killTree(a *Agent) {
	for _, cid := range a.Children() {
		if c, ok := s.Agent(cid); ok {
			s.killTree(c)
		}
	}
	a.killNow()
}

// SpawnFromClient spawns on behalf of a human (PRD §9).
func (s *Channel) SpawnFromClient(ctx context.Context, parentID, archetype, label, task, modelArg string) (string, error) {
	p, ok := s.Agent(parentID)
	if !ok {
		return "", fmt.Errorf("agent %q not found", parentID)
	}
	if ok, why := s.canSpawn(p); !ok {
		return "", errors.New(why)
	}
	a, err := s.spawn(ctx, parentID, archetype, label, task, modelArg)
	if err != nil {
		return "", err
	}
	return a.ID, nil
}

func (s *Channel) canSpawn(p *Agent) (bool, string) {
	cfg := s.Config()
	if p.Depth+1 >= cfg.Limits.MaxDepth {
		return false, fmt.Sprintf("max depth %d reached", cfg.Limits.MaxDepth)
	}
	if s.Busy() >= cfg.Limits.MaxAgents {
		return false, fmt.Sprintf("max busy agents %d reached (idle children do not count; kill ones you no longer need)", cfg.Limits.MaxAgents)
	}
	if len(p.Preset().Spawn) == 0 {
		return false, "this archetype cannot spawn"
	}
	return true, ""
}
