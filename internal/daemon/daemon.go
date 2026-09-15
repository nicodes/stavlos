// Package daemon wires the log, registry, escalation, channels, and the
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
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/eventlog"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/oauth"
	"github.com/nicodes/stavlos/internal/protocol"
)

// Daemon is the stavlosd process state.
type Daemon struct {
	Log      *eventlog.Log
	Registry *registry.Registry
	DataDir  string

	esc *escalation.Manager

	// Shutdown is called by daemon.shutdown; the main sets it to stop Serve.
	Shutdown func()

	loginMu sync.Mutex
	logins  map[string]*pendingLogin

	appendMu sync.Mutex // orders Append + broadcast across agents

	mu           sync.RWMutex
	channels     map[string]*agent.Channel
	clients      map[string]*client
	trustPrompts map[string]string // dir → prompt id
	trust        *trustStore
	lock         *os.File // the data directory's lock, held until Close
}

// New locks the data directory, opens the log and registry and recovers
// channels. A second daemon on the same directory gets ErrAlreadyRunning.
func New(ctx context.Context, dataDir string, reg *registry.Registry) (*Daemon, error) {
	lock, err := lockDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	lg, err := eventlog.Open(filepath.Join(dataDir, "events.db"))
	if err != nil {
		lock.Close()
		return nil, err
	}
	d := &Daemon{Log: lg, Registry: reg, DataDir: dataDir, lock: lock, channels: map[string]*agent.Channel{}, clients: map[string]*client{}, trustPrompts: map[string]string{}, logins: map[string]*pendingLogin{}}
	d.trust = &trustStore{log: lg}
	if err := d.trust.load(ctx); err != nil {
		return nil, err
	}
	// Escalation timers come from global config; per-channel overrides are
	// a roadmap item (PRD §7.4: single global settings in v1).
	gcfg, err := config.LoadGlobal()
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

// Close stops channels and the log and releases the data directory.
func (d *Daemon) Close() {
	d.mu.Lock()
	for _, s := range d.channels {
		s.Stop()
	}
	d.mu.Unlock()
	d.Log.Close()
	if d.lock != nil {
		d.lock.Close() // releases the flock
	}
}

func (d *Daemon) recover(ctx context.Context) error {
	rows, err := d.Log.Channels(ctx)
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
			log.Printf("channel %s: config: %v (using the global configuration)", r.ID, err)
			if cfg, err = config.LoadGlobal(); err != nil {
				return err
			}
			cfg.Dir = r.Dir
		}
		s, err := agent.Recover(ctx, d, r.ID, r.Dir, r.Created, cfg, evs)
		if err != nil {
			log.Printf("channel %s: recover: %v", r.ID, err)
			continue
		}
		d.channels[r.ID] = s
		log.Printf("recovered channel %s (%s), %d agents", r.ID, r.Dir, len(s.Agents()))
	}
	return nil
}

// --- agent.Host ---

// Append logs an event and fans it out to subscribed clients. Append and
// broadcast happen under one lock so clients see events in sequence order
// (two agents finishing at once must not deliver out of order), and so a
// subscription's handover from replay to live can take the same lock and
// miss nothing. Delivery only queues: a slow client never holds the lock.
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
	d.eachSubscribed(n.Channel, func(c *client) { c.notify(protocol.NStream, b) })
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
	for _, c := range d.clientList() {
		for _, t := range tiers {
			if c.tier == t {
				c.notify(protocol.NPrompt, b)
				break
			}
		}
	}
}

func (d *Daemon) recordPrompt(action protocol.PromptAction, info protocol.PromptInfo, answer, clientID string) {
	if info.Channel == "" {
		return
	}
	var t event.Type
	var payload any
	switch action {
	case protocol.ActionRequested:
		rp := event.PromptRequestedPayload{ID: info.ID, Kind: string(info.Kind), Tool: info.Tool, Input: info.Input, Question: info.Question, Options: info.Options}
		if len(info.Questions) > 0 {
			rp.Questions, _ = json.Marshal(info.Questions)
		}
		t, payload = event.PromptRequested, rp
	case protocol.ActionEscalated:
		t, payload = event.PromptEscalated, event.PromptRefPayload{ID: info.ID}
	case protocol.ActionClaimed:
		t, payload = event.PromptClaimed, event.PromptRefPayload{ID: info.ID, Client: clientID}
	case protocol.ActionAnswered:
		t, payload = event.PromptAnswered, event.PromptAnsweredPayload{ID: info.ID, Answer: answer, Client: clientID}
	case protocol.ActionWithdrawn:
		t, payload = event.PromptWithdrawn, event.PromptRefPayload{ID: info.ID}
	case protocol.ActionDefaulted:
		t, payload = event.PromptDefaulted, event.PromptAnsweredPayload{ID: info.ID, Answer: answer}
	default:
		return
	}
	_, _ = d.Append(context.Background(), event.Event{Channel: info.Channel, Agent: info.Agent, Type: t, Payload: event.MustPayload(payload)})
}

// --- channels ---

func (d *Daemon) channel(id string) (*agent.Channel, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.channels[id]
	if !ok {
		return nil, fmt.Errorf("channel %q %w", id, errNotFound)
	}
	return s, nil
}

// agentChannel finds the channel owning an agent.
func (d *Daemon) agentChannel(agentID string) (*agent.Channel, *agent.Agent, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.channels {
		if a, ok := s.Agent(agentID); ok {
			return s, a, nil
		}
	}
	return nil, nil, fmt.Errorf("agent %q %w", agentID, errNotFound)
}

// CreateChannel creates and starts a channel in dir.
func (d *Daemon) CreateChannel(ctx context.Context, dir, modelID, root string) (*agent.Channel, error) {
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
	id := agent.NewID("c")
	s := agent.New(d, id, dir, cfg, modelID, root)
	if err := s.Start(ctx); err != nil {
		return nil, err
	}
	if err := d.Log.PutChannel(ctx, eventlog.ChannelRow{ID: id, Dir: dir, Created: s.Created}); err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.channels[id] = s
	d.mu.Unlock()
	d.maybeTrustPrompt(s)
	return s, nil
}

// ArchiveChannel archives a channel.
func (d *Daemon) ArchiveChannel(ctx context.Context, id string) error {
	s, err := d.channel(id)
	if err != nil {
		return err
	}
	if err := s.Archive(ctx); err != nil {
		return err
	}
	return d.Log.PutChannel(ctx, eventlog.ChannelRow{ID: id, Dir: s.Dir, Created: s.Created, Archived: true})
}

// ChannelList returns infos.
func (d *Daemon) ChannelList(ctx context.Context, dir string, archived bool) ([]protocol.ChannelInfo, error) {
	rows, err := d.Log.Channels(ctx)
	if err != nil {
		return nil, err
	}
	var out []protocol.ChannelInfo
	for _, r := range rows {
		if dir != "" && r.Dir != dir {
			continue
		}
		if r.Archived && !archived {
			continue
		}
		d.mu.RLock()
		s, ok := d.channels[r.ID]
		d.mu.RUnlock()
		var info protocol.ChannelInfo
		if ok {
			info = s.Info()
		} else {
			info = protocol.ChannelInfo{ID: r.ID, Dir: r.Dir, Created: r.Created.Format(time.RFC3339), Archived: r.Archived}
		}
		info.Seq, info.Title = r.LastSeq, r.Title
		out = append(out, info)
	}
	return out, nil
}

// --- trust (PRD §10.6) ---

type trustStore struct {
	log *eventlog.Log
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

// maybeTrustPrompt raises a trust prompt for a channel whose project layer
// is pending. It is a channel-scoped prompt with long timeouts; the
// channel keeps running on global config meanwhile.
func (d *Daemon) maybeTrustPrompt(s *agent.Channel) {
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
			ID: id, Channel: s.ID, Kind: protocol.PromptTrust, Input: input,
			Question: fmt.Sprintf("Trust the project configuration in %s? It can define MCP servers, policy, presets, skills and AGENTS.md.", s.Dir),
			Options:  []string{"trust", "skip"},
		})
		d.mu.Lock()
		delete(d.trustPrompts, s.Dir)
		d.mu.Unlock()
		if ans.Value == protocol.AnswerAllow || ans.Value == protocol.AnswerAllowAlways {
			_ = d.Trust(context.Background(), s.Dir, cfg.TrustHash, true)
		}
	}()
}

// Trust records a decision and reloads config for channels in dir. The
// directory is normalised and the hash recomputed from what is on disk:
// a client says which directory it means and whether it trusts what it
// was shown; the daemon decides what that content is.
func (d *Daemon) Trust(ctx context.Context, dir, hash string, trust bool) error {
	dir, _ = filepath.Abs(dir)
	dir = filepath.Clean(dir)
	if trust {
		_, current, err := config.ProjectHash(dir)
		if err != nil {
			return err
		}
		if current == "" {
			return fmt.Errorf("%s has no project configuration to trust", dir)
		}
		if hash != current {
			return fmt.Errorf("%w: the project configuration in %s changed since it was shown; look again before trusting it", errTrustChanged, dir)
		}
		if err := d.trust.set(ctx, dir, current); err != nil {
			return err
		}
	}
	// resolve any open trust prompt for this dir
	d.mu.RLock()
	pid := d.trustPrompts[dir]
	d.mu.RUnlock()
	if pid != "" {
		ans := protocol.AnswerDeny
		if trust {
			ans = protocol.AnswerAllow
		}
		// Settle the open prompt even if a client had claimed it: the
		// decision has been made.
		_ = d.esc.Resolve(pid, "trust.reply", escalation.Answer{Value: ans})
	}
	if !trust {
		return nil
	}
	d.mu.RLock()
	var ss []*agent.Channel
	for _, s := range d.channels {
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
	for k, v := range d.logins { // drop stale ones, releasing their loopback port
		if time.Since(v.started) > 30*time.Minute {
			v.pending.Close()
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
			pl.pending.Close()
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
	subs map[string]int64 // channel id → last seq delivered
}

func (c *client) notify(method string, params json.RawMessage) { c.send(method, params) }

// deliver sends a live event once per subscription, in order.
func (c *client) deliver(e event.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.subs[e.Channel]
	if !ok || e.Seq <= last {
		return
	}
	c.subs[e.Channel] = e.Seq
	c.sendEvent(e)
}

func (c *client) sendEvent(e event.Event) {
	b, _ := json.Marshal(protocol.EventNotification{Event: e})
	c.send(protocol.NEvent, b)
}

// subscribe replays a channel's events from seq from, then hands the
// client over to live delivery with nothing missed and nothing twice. The
// bulk of the replay runs without the append lock; the tail written
// meanwhile is read and sent under it, and the subscription is registered
// before the lock is released, so the next live event is the next seq.
func (d *Daemon) subscribe(ctx context.Context, cl *client, channel string, from int64) (int64, error) {
	if from <= 0 {
		from = 1
	}
	cl.mu.Lock()
	delete(cl.subs, channel) // a re-subscription starts over: no live delivery during the replay
	cl.mu.Unlock()
	last := from - 1
	evs, err := d.Log.Read(ctx, channel, from, 0)
	if err != nil {
		return 0, err
	}
	for _, e := range evs {
		cl.sendEvent(e)
		last = e.Seq
	}
	d.appendMu.Lock()
	defer d.appendMu.Unlock()
	tail, err := d.Log.Read(ctx, channel, last+1, 0)
	if err != nil {
		return 0, err
	}
	for _, e := range tail {
		cl.sendEvent(e)
		last = e.Seq
	}
	cl.mu.Lock()
	cl.subs[channel] = last
	cl.mu.Unlock()
	return last, nil
}

// clientList snapshots the attached clients, so nothing is sent while the
// daemon's lock is held.
func (d *Daemon) clientList() []*client {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*client, 0, len(d.clients))
	for _, c := range d.clients {
		out = append(out, c)
	}
	return out
}

func (d *Daemon) eachSubscribed(channel string, fn func(*client)) {
	for _, c := range d.clientList() {
		c.mu.Lock()
		_, ok := c.subs[channel]
		c.mu.Unlock()
		if ok {
			fn(c)
		}
	}
}

func (d *Daemon) broadcastEvent(e event.Event) {
	d.eachSubscribed(e.Channel, func(c *client) { c.deliver(e) })
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
	for _, s := range d.channels {
		n += len(s.Agents())
	}
	provs := d.Registry.Providers()
	sort.Strings(provs)
	return protocol.DaemonStatusResult{Version: protocol.Version, Build: buildid.ID(), PID: os.Getpid(), DataDir: d.DataDir, Channels: len(d.channels), Agents: n, Providers: provs}
}

// errTrustChanged: a trust reply carried a hash that no longer matches the
// project's files (mapped to ErrConflict on the wire).
var errTrustChanged = errors.New("trust: content changed")
