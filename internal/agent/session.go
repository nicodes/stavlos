// Package agent implements sessions, the actor model, and the turn loop
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

// Host is what the daemon provides to sessions.
type Host interface {
	Append(ctx context.Context, e event.Event) (event.Event, error)
	Stream(n protocol.StreamNotification)
	Resolve(modelID string) (model.Model, model.Info, error)
	CheckModel(modelID string) error
	Variants(modelID string) []string
	Prompt(ctx context.Context, info protocol.PromptInfo) escalation.Answer
}

// Session is a root agent plus its subtree, bound to a directory (PRD §5).
type Session struct {
	ID      string
	Dir     string
	Created time.Time

	host  Host
	tools tools.Set

	mu       sync.RWMutex
	cfg      *config.Effective
	model    string // session-selected model
	rootArch string
	agents   map[string]*Agent
	order    []string // spawn order
	archived bool
	ctx      context.Context
	cancel   context.CancelFunc
	permits  permits // the human's session-scoped allows (exact calls, prefixes); they answer asks, never denies
	mode     string  // permission mode: "" or ask (every ask prompts) | auto (asks inside the agent's dirs are allowed) | yolo (every ask is allowed)
}

// Mode reports the session's permission mode (protocol.ModeAsk by default).
func (s *Session) Mode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.mode == "" {
		return protocol.ModeAsk
	}
	return s.mode
}

// Yolo reports whether every policy ask is allowed, boundary included.
func (s *Session) Yolo() bool { return s.Mode() == protocol.ModeYolo }

// SetMode switches the session's permission mode and logs it. Explicit
// deny rules, model questions and the trust prompt are unaffected in every
// mode.
func (s *Session) SetMode(ctx context.Context, mode string) error {
	switch mode {
	case protocol.ModeAsk, protocol.ModeAuto, protocol.ModeYolo:
	default:
		return fmt.Errorf("unknown mode %q: ask, auto or yolo", mode)
	}
	s.mu.Lock()
	changed := s.mode != mode && !(s.mode == "" && mode == protocol.ModeAsk)
	s.mode = mode
	s.mu.Unlock()
	if !changed {
		return nil
	}
	_, err := s.host.Append(ctx, event.Event{Session: s.ID, Type: event.SessionModeChanged, Payload: event.MustPayload(event.ModePayload{Mode: mode})})
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

// New creates a session object without logging anything; use Create or
// Recover on the daemon side.
func New(host Host, id, dir string, cfg *config.Effective, modelID, rootArch string) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	if rootArch == "" {
		rootArch = cfg.RootAgent
	}
	if modelID == "" {
		modelID = cfg.Model
	}
	return &Session{
		ID: id, Dir: dir, Created: time.Now().UTC(),
		host: host, tools: tools.Builtin(),
		cfg: cfg, model: modelID, rootArch: rootArch,
		agents: map[string]*Agent{}, ctx: ctx, cancel: cancel,
	}
}

// Start logs SessionCreated and spawns the root agent.
// A session may start with no model or an unconnected provider: the TUI
// opens regardless and the first turn reports the problem (PRD §8.4).
func (s *Session) Start(ctx context.Context) error {
	if _, ok := s.cfg.Presets[s.rootArch]; !ok {
		return fmt.Errorf("root preset %q not found", s.rootArch)
	}
	if _, err := s.host.Append(ctx, event.Event{Session: s.ID, Type: event.SessionCreated,
		Payload: event.MustPayload(event.SessionCreatedPayload{Dir: s.Dir, Model: s.model, RootAgent: s.rootArch})}); err != nil {
		return err
	}
	_, err := s.spawn(ctx, "", s.rootArch, "main", "", "", nil) // the root is always "main (role)"
	return err
}

// Config returns the effective config.
func (s *Session) Config() *config.Effective {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// SetConfig swaps the effective config (after a trust decision or edit).
func (s *Session) SetConfig(cfg *config.Effective) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// Model returns the session-selected model.
func (s *Session) Model() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model
}

// SetModel changes the session model (affects future spawns and the root's
// inherited default; running agents keep theirs — PRD §8.3).
func (s *Session) SetModel(ctx context.Context, id string) error {
	if err := s.host.CheckModel(id); err != nil {
		return err
	}
	s.mu.Lock()
	s.model = id
	s.mu.Unlock()
	_, err := s.host.Append(ctx, event.Event{Session: s.ID, Type: event.SessionModelChanged, Payload: event.MustPayload(event.ModelChangedPayload{Model: id})})
	return err
}

// Archived reports whether the session is archived.
func (s *Session) Archived() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.archived
}

// Archive kills every agent and marks the session archived.
func (s *Session) Archive(ctx context.Context) error {
	if root := s.Root(); root != nil {
		_ = s.Kill(root.ID)
	}
	s.mu.Lock()
	s.archived = true
	s.mu.Unlock()
	s.cancel()
	_, err := s.host.Append(ctx, event.Event{Session: s.ID, Type: event.SessionArchived})
	return err
}

// Stop cancels all agents without logging (daemon shutdown).
func (s *Session) Stop() { s.cancel() }

// Agent looks up an agent.
func (s *Session) Agent(id string) (*Agent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.agents[id]
	return a, ok
}

// resolve finds an agent by its id, or by a unique prefix of at least four
// characters (models sometimes copy a shortened id from a status line).
func (s *Session) resolve(id string) (*Agent, bool) {
	if a, ok := s.Agent(id); ok {
		return a, true
	}
	if len(id) < 4 {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var found *Agent
	for _, a := range s.agents {
		if strings.HasPrefix(a.ID, id) {
			if found != nil {
				return nil, false // ambiguous
			}
			found = a
		}
	}
	return found, found != nil
}

// agentsSnapshot lists the session's agents in creation order.
func (s *Session) agentsSnapshot() []*Agent {
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
func (s *Session) Root() *Agent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.order) == 0 {
		return nil
	}
	return s.agents[s.order[0]]
}

// Agents returns agents in pre-order (root first, children after parents).
func (s *Session) Agents() []*Agent {
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
func (s *Session) Live() int {
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
func (s *Session) Busy() int {
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
func (s *Session) Cost() float64 {
	c := 0.0
	for _, a := range s.Agents() {
		c += a.Cost()
	}
	return c
}

// Info builds the protocol view.
func (s *Session) Info() protocol.SessionInfo {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	return protocol.SessionInfo{
		ID: s.ID, Dir: s.Dir, Model: s.Model(), RootAgent: s.rootArch,
		Created: s.Created.Format(time.RFC3339), Archived: s.Archived(),
		Live: s.Live(), CostUSD: s.Cost(), TrustPending: cfg.TrustPending, Mode: s.Mode(),
		State: s.state(),
	}
}

// state sums the agents up: working while any agent runs (or is blocked
// on a prompt), waiting while any expects an answer, idle otherwise.
func (s *Session) state() string {
	out := "idle"
	for _, a := range s.Agents() {
		switch a.Info().State {
		case "running", "blocked":
			return "working"
		case "waiting":
			out = "waiting"
		}
	}
	return out
}

// Presets lists archetypes.
func (s *Session) Presets() []protocol.PresetInfo {
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
// session's) model is inherited when the role allows it, else the role's
// default (its first listed model).
func (s *Session) resolveModel(spawnArg string, preset config.Preset, parent *Agent) (string, error) {
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
func (s *Session) spawn(ctx context.Context, parentID, archetype, label, task, modelArg string, grants []string) (*Agent, error) {
	cfg := s.Config()
	preset, ok := cfg.Presets[archetype]
	if !ok {
		return nil, fmt.Errorf("unknown archetype %q", archetype)
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
		if !contains(parent.preset.Spawn, archetype) {
			return nil, fmt.Errorf("%s may not spawn %q (allowed: %v)", parent.Archetype, archetype, parent.preset.Spawn)
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
	var granted []string
	for _, g := range grants {
		d := resolveDir(s.Dir, g)
		if parent == nil {
			return nil, fmt.Errorf("the root agent takes no directory grants")
		}
		if !parent.inDirs(d) {
			return nil, fmt.Errorf("%s is not inside your directories (%s); you can only grant what you have", d, strings.Join(parent.dirPaths(), ", "))
		}
		granted = append(granted, d)
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
	for _, d := range granted {
		a.extraDirs = append(a.extraDirs, dirEntry{d, "grant"})
	}
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
	s.agents[a.ID] = a
	s.order = append(s.order, a.ID)
	s.mu.Unlock()
	if parent != nil {
		parent.addChild(a.ID)
	}
	if _, err := s.host.Append(ctx, event.Event{Session: s.ID, Agent: a.ID, Type: event.AgentSpawned,
		Payload: event.MustPayload(event.AgentSpawnedPayload{ID: a.ID, Parent: parentID, Archetype: archetype, Label: label, Model: modelID, Task: task, Depth: depth, Dirs: granted})}); err != nil {
		return nil, err
	}
	if a.variant != "" { // inherited: logged so recovery restores it
		if _, err := s.host.Append(ctx, event.Event{Session: s.ID, Agent: a.ID, Type: event.AgentVariantChanged,
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
func (s *Session) Send(ctx context.Context, agentID, text, source string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	return a.Prompt(ctx, text, source)
}

// Steer delivers a Steer.
func (s *Session) Steer(ctx context.Context, agentID, text, source string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	return a.Steer(ctx, text, source)
}

// Cancel ends the current turn.
func (s *Session) Cancel(agentID string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	a.Cancel()
	return nil
}

// Kill tears down an agent and its subtree.
func (s *Session) Kill(agentID string) error {
	a, ok := s.Agent(agentID)
	if !ok {
		return fmt.Errorf("agent %q not found", agentID)
	}
	s.killTree(a)
	return nil
}

func (s *Session) killTree(a *Agent) {
	for _, cid := range a.Children() {
		if c, ok := s.Agent(cid); ok {
			s.killTree(c)
		}
	}
	a.killNow()
}

// SpawnFromClient spawns on behalf of a human (PRD §9).
func (s *Session) SpawnFromClient(ctx context.Context, parentID, archetype, label, task, modelArg string, dirs []string) (string, error) {
	p, ok := s.Agent(parentID)
	if !ok {
		return "", fmt.Errorf("agent %q not found", parentID)
	}
	if ok, why := s.canSpawn(p); !ok {
		return "", errors.New(why)
	}
	a, err := s.spawn(ctx, parentID, archetype, label, task, modelArg, dirs)
	if err != nil {
		return "", err
	}
	return a.ID, nil
}

func (s *Session) canSpawn(p *Agent) (bool, string) {
	cfg := s.Config()
	if p.Depth+1 >= cfg.Limits.MaxDepth {
		return false, fmt.Sprintf("max depth %d reached", cfg.Limits.MaxDepth)
	}
	if s.Busy() >= cfg.Limits.MaxAgents {
		return false, fmt.Sprintf("max busy agents %d reached (idle children do not count; kill ones you no longer need)", cfg.Limits.MaxAgents)
	}
	if len(p.preset.Spawn) == 0 {
		return false, "this archetype cannot spawn"
	}
	return true, ""
}
