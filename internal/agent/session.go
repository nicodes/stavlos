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

	mu          sync.RWMutex
	cfg         *config.Effective
	model       string // session-selected model
	rootArch    string
	agents      map[string]*Agent
	order       []string // spawn order
	archived    bool
	ctx         context.Context
	cancel      context.CancelFunc
	allowAlways map[string]bool // "tool\x00arg" remembered allows (session-scoped)
	yolo        bool            // session-wide: policy "ask" outcomes are allowed without a prompt
}

// Yolo reports whether the session auto-approves permission prompts.
func (s *Session) Yolo() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.yolo
}

// SetYolo switches the session-wide auto-approval and logs it. Explicit
// deny rules and model questions are unaffected; only outcomes a policy
// would ask about are allowed.
func (s *Session) SetYolo(ctx context.Context, on bool) error {
	s.mu.Lock()
	changed := s.yolo != on
	s.yolo = on
	s.mu.Unlock()
	if !changed {
		return nil
	}
	_, err := s.host.Append(ctx, event.Event{Session: s.ID, Type: event.SessionYoloChanged, Payload: event.MustPayload(event.YoloPayload{On: on})})
	return err
}

// ErrNoModel is the turn error when an agent has no model to call.
const ErrNoModel = "no model selected: run /models to pick one, or /provider first to connect a provider"

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
		allowAlways: map[string]bool{},
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
	_, err := s.spawn(ctx, "", s.rootArch, s.rootArch, "", "")
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
		Live: s.Live(), CostUSD: s.Cost(), TrustPending: cfg.TrustPending, Yolo: s.Yolo(),
	}
}

// Presets lists archetypes.
func (s *Session) Presets() []protocol.PresetInfo {
	cfg := s.Config()
	var out []protocol.PresetInfo
	for _, p := range cfg.Presets {
		out = append(out, protocol.PresetInfo{Name: p.Name, Description: p.Description, Model: p.Model, Spawn: p.Spawn})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// resolveModel implements PRD §8.3.
func (s *Session) resolveModel(spawnArg string, preset config.Preset, parent *Agent) string {
	switch {
	case spawnArg != "":
		return spawnArg
	case preset.Model != "":
		return preset.Model
	case parent != nil:
		return parent.ModelID()
	}
	return s.Model()
}

// spawn creates and starts an agent. parent=="" for the root.
func (s *Session) spawn(ctx context.Context, parentID, archetype, label, task, modelArg string) (*Agent, error) {
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
	}
	modelID := s.resolveModel(modelArg, preset, parent)
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
		Payload: event.MustPayload(event.AgentSpawnedPayload{ID: a.ID, Parent: parentID, Archetype: archetype, Label: label, Model: modelID, Task: task, Depth: depth})}); err != nil {
		return nil, err
	}
	if a.variant != "" { // inherited: logged so recovery restores it
		if _, err := s.host.Append(ctx, event.Event{Session: s.ID, Agent: a.ID, Type: event.AgentVariantChanged,
			Payload: event.MustPayload(event.VariantChangedPayload{Variant: a.variant})}); err != nil {
			return nil, err
		}
	}
	if parent != nil {
		// A child wakes its parent when it finishes unless the parent
		// unmonitors it: arming is the default, not an opt-in (PRD §6.3).
		parent.mu.Lock()
		parent.armed[a.ID] = true
		parent.mu.Unlock()
		_, _ = parent.record(ctx, event.MonitorArmed, event.MonitorPayload{IDs: []string{a.ID}})
	}
	a.start()
	if task != "" {
		if err := a.Prompt(ctx, task, "agent:"+parentID); err != nil {
			return nil, err
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
func (s *Session) SpawnFromClient(ctx context.Context, parentID, archetype, label, task, modelArg string) (string, error) {
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

func (s *Session) canSpawn(p *Agent) (bool, string) {
	cfg := s.Config()
	if p.Depth+1 >= cfg.Limits.MaxDepth {
		return false, fmt.Sprintf("max depth %d reached", cfg.Limits.MaxDepth)
	}
	if s.Live() >= cfg.Limits.MaxAgents {
		return false, fmt.Sprintf("max live agents %d reached", cfg.Limits.MaxAgents)
	}
	if len(p.preset.Spawn) == 0 {
		return false, "this archetype cannot spawn"
	}
	return true, ""
}
