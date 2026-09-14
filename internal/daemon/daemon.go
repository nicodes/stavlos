// Package daemon wires the log, registry, escalation, sessions, and the
// protocol server together (PRD §4.1).
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/oauth"
	"github.com/nicodes/stavlos/internal/protocol"
)

// Daemon is the stavlosd process state.
type Daemon struct {
	Log      *event.Log
	Registry *registry.Registry
	DataDir  string

	esc *escalation.Manager

	// Shutdown is called by daemon.shutdown; the main sets it to stop Serve.
	Shutdown func()

	loginMu sync.Mutex
	logins  map[string]*pendingLogin

	appendMu sync.Mutex // orders Append + broadcast across agents

	mu           sync.RWMutex
	sessions     map[string]*agent.Session
	clients      map[string]*client
	trustPrompts map[string]string // dir → prompt id
	trust        *trustStore
}

// New opens the log and registry and recovers sessions.
func New(ctx context.Context, dataDir string, reg *registry.Registry) (*Daemon, error) {
	lg, err := event.Open(filepath.Join(dataDir, "events.db"))
	if err != nil {
		return nil, err
	}
	d := &Daemon{Log: lg, Registry: reg, DataDir: dataDir, sessions: map[string]*agent.Session{}, clients: map[string]*client{}, trustPrompts: map[string]string{}, logins: map[string]*pendingLogin{}}
	d.trust = &trustStore{log: lg}
	if err := d.trust.load(ctx); err != nil {
		return nil, err
	}
	// Escalation timers come from global config; per-session overrides are
	// a roadmap item (PRD §7.4: single global settings in v1).
	gcfg, err := config.Load(os.TempDir(), d.trust)
	if err != nil {
		return nil, err
	}
	d.esc = escalation.New(escalation.Config{
		ClaimTimeout: gcfg.Escalation.ClaimTimeout, AnswerTimeout: gcfg.Escalation.AnswerTimeout, Default: string(gcfg.Escalation.Default),
	}, sinkFunc(d.notifyPrompt))
	d.esc.Record = d.recordPrompt
	if err := d.recover(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Close stops sessions and the log.
func (d *Daemon) Close() {
	d.mu.Lock()
	for _, s := range d.sessions {
		s.Stop()
	}
	d.mu.Unlock()
	d.Log.Close()
}

func (d *Daemon) recover(ctx context.Context) error {
	rows, err := d.Log.Sessions(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Archived {
			continue
		}
		evs, err := d.Log.Read(ctx, r.ID, 1, 0)
		if err != nil {
			return err
		}
		cfg, err := config.Load(r.Dir, d.trust)
		if err != nil {
			log.Printf("session %s: config: %v (using defaults)", r.ID, err)
			cfg, _ = config.Load(os.TempDir(), d.trust)
			cfg.Dir = r.Dir
		}
		s, err := agent.Recover(ctx, d, r.ID, r.Dir, r.Created, cfg, evs)
		if err != nil {
			log.Printf("session %s: recover: %v", r.ID, err)
			continue
		}
		d.sessions[r.ID] = s
		log.Printf("recovered session %s (%s), %d agents", r.ID, r.Dir, len(s.Agents()))
	}
	return nil
}

// --- agent.Host ---

// Append logs an event and fans it out to subscribed clients. Append and
// broadcast happen under one lock so clients see events in sequence order;
// otherwise two agents finishing at once could deliver out of order and the
// per-subscription dedupe would drop the earlier one as stale.
func (d *Daemon) Append(ctx context.Context, e event.Event) (event.Event, error) {
	d.appendMu.Lock()
	defer d.appendMu.Unlock()
	e, err := d.Log.Append(ctx, e)
	if err != nil {
		return e, err
	}
	d.broadcastEvent(e)
	return e, nil
}

func (d *Daemon) Stream(n protocol.StreamNotification) {
	b, _ := json.Marshal(n)
	d.eachSubscribed(n.Session, func(c *client) { c.notify(protocol.NStream, b) })
}

func (d *Daemon) Resolve(id string) (model.Model, model.Info, error) { return d.Registry.Resolve(id) }
func (d *Daemon) CheckModel(id string) error                         { return d.Registry.Check(id) }
func (d *Daemon) Variants(id string) []string                        { return d.Registry.Variants(id) }

func (d *Daemon) Prompt(ctx context.Context, info protocol.PromptInfo) escalation.Answer {
	return d.esc.Request(ctx, info)
}

// --- prompts ---

type sinkFunc func(protocol.PromptNotification, []protocol.Tier)

func (f sinkFunc) Notify(n protocol.PromptNotification, tiers []protocol.Tier) { f(n, tiers) }

func (d *Daemon) notifyPrompt(n protocol.PromptNotification, tiers []protocol.Tier) {
	b, _ := json.Marshal(n)
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, c := range d.clients {
		for _, t := range tiers {
			if c.tier == t {
				c.notify(protocol.NPrompt, b)
				break
			}
		}
	}
}

func (d *Daemon) recordPrompt(action string, info protocol.PromptInfo, answer, clientID string) {
	if info.Session == "" {
		return
	}
	var t event.Type
	var payload any
	switch action {
	case "requested":
		rp := event.PromptRequestedPayload{ID: info.ID, Kind: info.Kind, Tool: info.Tool, Input: info.Input, Question: info.Question, Options: info.Options}
		if len(info.Questions) > 0 {
			rp.Questions, _ = json.Marshal(info.Questions)
		}
		t, payload = event.PromptRequested, rp
	case "claimed":
		t, payload = event.PromptClaimed, event.PromptRefPayload{ID: info.ID, Client: clientID}
	case "answered":
		t, payload = event.PromptAnswered, event.PromptAnsweredPayload{ID: info.ID, Answer: answer, Client: clientID}
	case "withdrawn":
		t, payload = event.PromptWithdrawn, event.PromptRefPayload{ID: info.ID}
	case "defaulted":
		t, payload = event.PromptDefaulted, event.PromptAnsweredPayload{ID: info.ID, Answer: answer}
	default:
		return
	}
	_, _ = d.Append(context.Background(), event.Event{Session: info.Session, Agent: info.Agent, Type: t, Payload: event.MustPayload(payload)})
}

// --- sessions ---

func (d *Daemon) session(id string) (*agent.Session, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return s, nil
}

// agentSession finds the session owning an agent.
func (d *Daemon) agentSession(agentID string) (*agent.Session, *agent.Agent, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.sessions {
		if a, ok := s.Agent(agentID); ok {
			return s, a, nil
		}
	}
	return nil, nil, fmt.Errorf("agent %q not found", agentID)
}

// CreateSession creates and starts a session in dir.
func (d *Daemon) CreateSession(ctx context.Context, dir, modelID, root string) (*agent.Session, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	cfg, err := config.Load(dir, d.trust)
	if err != nil {
		return nil, err
	}
	id := agent.NewID("s")
	s := agent.New(d, id, dir, cfg, modelID, root)
	if err := s.Start(ctx); err != nil {
		return nil, err
	}
	if err := d.Log.PutSession(ctx, event.SessionRow{ID: id, Dir: dir, Created: s.Created}); err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.sessions[id] = s
	d.mu.Unlock()
	d.maybeTrustPrompt(s)
	return s, nil
}

// ForkSession copies events up to seq into a new session (PRD §5).
func (d *Daemon) ForkSession(ctx context.Context, id string, seq int64) (*agent.Session, error) {
	src, err := d.session(id)
	if err != nil {
		return nil, err
	}
	if seq <= 0 {
		seq, _ = d.Log.LastSeq(ctx, id)
	}
	evs, err := d.Log.ReadRange(ctx, id, 1, seq)
	if err != nil {
		return nil, err
	}
	nid := agent.NewID("s")
	idmap := map[string]string{}
	remap := func(a string) string {
		if a == "" {
			return ""
		}
		if n, ok := idmap[a]; ok {
			return n
		}
		n := agent.NewID("a")
		idmap[a] = n
		return n
	}
	var copied []event.Event
	for _, e := range evs {
		ne := event.Event{Session: nid, Agent: remap(e.Agent), Type: e.Type, Time: e.Time, Payload: e.Payload}
		switch e.Type {
		case event.SessionCreated:
			var p event.SessionCreatedPayload
			_ = e.Decode(&p)
			p.ForkedFrom, p.ForkSeq = id, seq
			ne.Payload = event.MustPayload(p)
		case event.AgentSpawned:
			var p event.AgentSpawnedPayload
			_ = e.Decode(&p)
			p.ID, p.Parent = remap(p.ID), remap(p.Parent)
			ne.Payload = event.MustPayload(p)
		case event.AgentKilled:
			var p event.AgentRefPayload
			_ = e.Decode(&p)
			p.ID = remap(p.ID)
			ne.Payload = event.MustPayload(p)
		}
		ne, err = d.Log.Append(ctx, ne)
		if err != nil {
			return nil, err
		}
		copied = append(copied, ne)
	}
	if err := d.Log.PutSession(ctx, event.SessionRow{ID: nid, Dir: src.Dir, Created: time.Now().UTC()}); err != nil {
		return nil, err
	}
	cfg, err := config.Load(src.Dir, d.trust)
	if err != nil {
		return nil, err
	}
	s, err := agent.Recover(ctx, d, nid, src.Dir, time.Now().UTC(), cfg, copied)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.sessions[nid] = s
	d.mu.Unlock()
	return s, nil
}

// ArchiveSession archives a session.
func (d *Daemon) ArchiveSession(ctx context.Context, id string) error {
	s, err := d.session(id)
	if err != nil {
		return err
	}
	if err := s.Archive(ctx); err != nil {
		return err
	}
	return d.Log.PutSession(ctx, event.SessionRow{ID: id, Dir: s.Dir, Created: s.Created, Archived: true})
}

// SessionList returns infos.
func (d *Daemon) SessionList(ctx context.Context, dir string, archived bool) ([]protocol.SessionInfo, error) {
	rows, err := d.Log.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	var out []protocol.SessionInfo
	for _, r := range rows {
		if dir != "" && r.Dir != dir {
			continue
		}
		if r.Archived && !archived {
			continue
		}
		d.mu.RLock()
		s, ok := d.sessions[r.ID]
		d.mu.RUnlock()
		var info protocol.SessionInfo
		if ok {
			info = s.Info()
		} else {
			info = protocol.SessionInfo{ID: r.ID, Dir: r.Dir, Created: r.Created.Format(time.RFC3339), Archived: r.Archived}
		}
		info.Seq, _ = d.Log.LastSeq(ctx, r.ID)
		info.Title = d.sessionTitle(ctx, r.ID)
		out = append(out, info)
	}
	return out, nil
}

// sessionTitle is the first human prompt of a session (its opening line,
// trimmed), or "" for a session nobody has spoken to yet.
func (d *Daemon) sessionTitle(ctx context.Context, id string) string {
	evs, err := d.Log.Read(ctx, id, 1, 400)
	if err != nil {
		return ""
	}
	for _, e := range evs {
		if e.Type != event.PromptQueued && e.Type != event.SteerReceived {
			continue
		}
		var p event.TextPayload
		if e.Decode(&p) != nil || !strings.HasPrefix(p.Source, "human:") {
			continue
		}
		t := strings.TrimSpace(p.Text)
		if i := strings.IndexByte(t, '\n'); i >= 0 {
			t = t[:i]
		}
		if t != "" {
			return t
		}
	}
	return ""
}

// --- trust (PRD §10.6) ---

type trustStore struct {
	log *event.Log
	mu  sync.RWMutex
	m   map[string]string // dir → hash
}

func (t *trustStore) load(ctx context.Context) error {
	t.m = map[string]string{}
	_, err := t.log.Get(ctx, "trust", &t.m)
	return err
}

func (t *trustStore) Trusted(dir, hash string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.m[dir] == hash
}

func (t *trustStore) set(ctx context.Context, dir, hash string) error {
	t.mu.Lock()
	t.m[dir] = hash
	m := map[string]string{}
	for k, v := range t.m {
		m[k] = v
	}
	t.mu.Unlock()
	return t.log.Put(ctx, "trust", m)
}

// maybeTrustPrompt raises a trust prompt for a session whose project layer
// is pending. It is a session-scoped prompt with long timeouts; the
// session keeps running on global config meanwhile.
func (d *Daemon) maybeTrustPrompt(s *agent.Session) {
	cfg := s.Config()
	if !cfg.TrustPending {
		return
	}
	d.mu.Lock()
	if _, busy := d.trustPrompts[s.Dir]; busy {
		d.mu.Unlock()
		return
	}
	id := agent.NewID("t")
	d.trustPrompts[s.Dir] = id
	d.mu.Unlock()
	go func() {
		input, _ := json.Marshal(map[string]any{"dir": s.Dir, "hash": cfg.TrustHash, "files": cfg.TrustFiles})
		ans := d.esc.Request(context.Background(), protocol.PromptInfo{
			ID: id, Session: s.ID, Kind: "trust", Input: input,
			Question: fmt.Sprintf("Trust the project configuration in %s? It can define MCP servers, policy, presets, skills and AGENTS.md.", s.Dir),
			Options:  []string{"trust", "skip"},
		})
		d.mu.Lock()
		delete(d.trustPrompts, s.Dir)
		d.mu.Unlock()
		if ans.Value == "allow" || ans.Value == "trust" || ans.Value == "allow_always" {
			_ = d.Trust(context.Background(), s.Dir, cfg.TrustHash, true)
		}
	}()
}

// Trust records a decision and reloads config for sessions in dir.
func (d *Daemon) Trust(ctx context.Context, dir, hash string, trust bool) error {
	if trust {
		if err := d.trust.set(ctx, dir, hash); err != nil {
			return err
		}
	}
	// resolve any open trust prompt for this dir
	d.mu.RLock()
	pid := d.trustPrompts[dir]
	d.mu.RUnlock()
	if pid != "" {
		ans := "deny"
		if trust {
			ans = "allow"
		}
		_ = d.esc.Reply(pid, "trust.reply", ans)
	}
	if !trust {
		return nil
	}
	d.mu.RLock()
	var ss []*agent.Session
	for _, s := range d.sessions {
		if s.Dir == dir {
			ss = append(ss, s)
		}
	}
	d.mu.RUnlock()
	for _, s := range ss {
		cfg, err := config.Load(dir, d.trust)
		if err != nil {
			return err
		}
		s.SetConfig(cfg)
	}
	return nil
}

// TrustStatus reports whether dir's project layer is pending.
func (d *Daemon) TrustStatus(dir string) (protocol.TrustStatusResult, error) {
	dir, _ = filepath.Abs(dir)
	files, hash, err := config.ProjectHash(dir)
	if err != nil {
		return protocol.TrustStatusResult{}, err
	}
	return protocol.TrustStatusResult{Dir: dir, Pending: len(files) > 0 && !d.trust.Trusted(dir, hash), Hash: hash, Files: files}, nil
}

// --- subscription logins (PRD §8.4) ---

type pendingLogin struct {
	provider string
	pending  *oauth.Pending
	started  time.Time
}

// LoginStart begins a device-code login and returns what to show the user.
func (d *Daemon) LoginStart(ctx context.Context, provider, method string) (protocol.LoginStartResult, error) {
	f, err := d.Registry.Flow(provider)
	if err != nil {
		return protocol.LoginStartResult{}, err
	}
	p, err := f.Start(context.Background(), method) // outlives the request; Wait owns cancellation
	if err != nil {
		return protocol.LoginStartResult{}, err
	}
	id := agent.NewID("l")
	d.loginMu.Lock()
	for k, v := range d.logins { // drop stale ones
		if time.Since(v.started) > 30*time.Minute {
			delete(d.logins, k)
		}
	}
	d.logins[id] = &pendingLogin{provider: provider, pending: p, started: time.Now()}
	d.loginMu.Unlock()
	return protocol.LoginStartResult{ID: id, Provider: provider, Method: p.Method, URL: p.URL, Code: p.Code, Instructions: p.Instructions, ExpiresIn: int(p.ExpiresIn.Seconds())}, nil
}

// LoginWait polls until the login completes, then stores the tokens.
func (d *Daemon) LoginWait(ctx context.Context, id string) (registry.Status, error) {
	d.loginMu.Lock()
	pl, ok := d.logins[id]
	d.loginMu.Unlock()
	if !ok {
		return registry.Status{}, fmt.Errorf("no login in progress with id %q", id)
	}
	f, err := d.Registry.Flow(pl.provider)
	if err != nil {
		return registry.Status{}, err
	}
	tok, err := f.Wait(ctx, pl.pending)
	if err != nil {
		if ctx.Err() == nil { // terminal failure: forget it
			d.loginMu.Lock()
			delete(d.logins, id)
			d.loginMu.Unlock()
		}
		return registry.Status{}, err
	}
	d.loginMu.Lock()
	delete(d.logins, id)
	d.loginMu.Unlock()
	if err := d.Registry.SaveLogin(pl.provider, tok); err != nil {
		return registry.Status{}, err
	}
	log.Printf("%s login completed (%s)", pl.provider, tok.Email)
	st, _ := d.Registry.Status(pl.provider)
	return st, nil
}

// --- clients ---

type client struct {
	id   string
	name string
	tier protocol.Tier
	send func(method string, params json.RawMessage)

	mu   sync.Mutex
	subs map[string]int64 // session id → last seq delivered
}

func (c *client) notify(method string, params json.RawMessage) { c.send(method, params) }

// deliver sends an event once per subscription, in order.
func (c *client) deliver(e event.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.subs[e.Session]
	if !ok || e.Seq <= last {
		return
	}
	c.subs[e.Session] = e.Seq
	b, _ := json.Marshal(protocol.EventNotification{Event: e})
	c.send(protocol.NEvent, b)
}

func (d *Daemon) eachSubscribed(session string, fn func(*client)) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, c := range d.clients {
		c.mu.Lock()
		_, ok := c.subs[session]
		c.mu.Unlock()
		if ok {
			fn(c)
		}
	}
}

func (d *Daemon) broadcastEvent(e event.Event) {
	d.eachSubscribed(e.Session, func(c *client) { c.deliver(e) })
}

func (d *Daemon) addClient(c *client) {
	d.mu.Lock()
	d.clients[c.id] = c
	d.mu.Unlock()
}

func (d *Daemon) removeClient(id string) {
	d.mu.Lock()
	delete(d.clients, id)
	d.mu.Unlock()
}

// Status for daemon.status.
func (d *Daemon) Status() protocol.DaemonStatusResult {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n := 0
	for _, s := range d.sessions {
		n += len(s.Agents())
	}
	provs := d.Registry.Providers()
	sort.Strings(provs)
	return protocol.DaemonStatusResult{Version: protocol.Version, Build: buildid.ID(), PID: os.Getpid(), DataDir: d.DataDir, Sessions: len(d.sessions), Agents: n, Providers: provs}
}

var errNoSession = errors.New("session not found")
