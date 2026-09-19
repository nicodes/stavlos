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
	"strings"
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
	"github.com/nicodes/stavlos/internal/web"
)

// Daemon is the stavlosd process state.
type Daemon struct {
	planUsageMu sync.Mutex                           // serialises writes of plan-usage.json
	planHistory map[string][]protocol.PlanUsagePoint // provider → observed plan usage over time, under planUsageMu
	Log         *eventlog.Log
	Registry    *registry.Registry
	DataDir     string
	Discord     DiscordService // configured before Serve, owned until Close

	esc *escalation.Manager

	// Shutdown is called by daemon.shutdown; the main sets it to stop Serve.
	Shutdown func()

	loginMu sync.Mutex
	logins  map[string]*pendingLogin

	nameMu   sync.Mutex // serialises choosing and checking channel names
	editorMu sync.Mutex // serialises config edits through validation and reload

	mu           sync.RWMutex
	channels     map[string]*agent.Channel
	clients      map[string]*client
	trustPrompts map[string]string // dir → prompt id
	trust        *trustStore
	lock         *os.File // the data directory's lock, held until Close

	streams *streams // stream deltas waiting to be sent together

	webMu  sync.Mutex
	web    *web.Server     // the browser front door (web.go); nil until Serve
	webCtx context.Context // what its listener lives under
}

// New locks the data directory, opens the log and registry and recovers
// channels. A second daemon on the same directory gets ErrAlreadyRunning.
func New(ctx context.Context, dataDir string, reg *registry.Registry) (*Daemon, error) {
	lock, err := lockDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	d := &Daemon{Registry: reg, DataDir: dataDir, lock: lock, channels: map[string]*agent.Channel{}, clients: map[string]*client{}, trustPrompts: map[string]string{}, logins: map[string]*pendingLogin{}}
	d.streams = newStreams(d.sendStream)
	d.loadPlanUsage()
	lg, err := eventlog.Open(filepath.Join(dataDir, "events.db"), d.committed)
	if err != nil {
		lock.Close()
		return nil, err
	}
	d.Log = lg
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
	if err := d.recover(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// Close stops channels and the log and releases the data directory.
func (d *Daemon) Close() {
	if d.Discord != nil {
		d.Discord.Close()
	}
	d.webMu.Lock()
	if d.web != nil {
		d.web.Disable() // the listener only; whether it is on stays as the human left it
	}
	d.webMu.Unlock()
	for _, s := range d.channelList() {
		s.Stop()
	}
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

// Append logs events in one transaction. Delivery to clients happens in
// committed, as the log commits them.
func (d *Daemon) Append(ctx context.Context, evs ...event.Event) ([]event.Event, error) {
	return d.Log.Append(ctx, evs...)
}

// committed fans committed events out to subscribed clients. The log calls
// it on its writer, in commit order, so clients see every channel's events
// in sequence; each event is encoded once for all of them, and delivery
// only queues, so a slow client never holds up a commit.
func (d *Daemon) committed(evs []event.Event) {
	for i, e := range evs {
		if i == 0 || e.Channel != evs[i-1].Channel {
			d.streams.flush(e.Channel) // what streamed goes out before the event that settles it
		}
	}
	clients := d.clientList()
	for _, e := range evs {
		line := eventLine(e)
		for _, c := range clients {
			c.deliver(e, line)
		}
	}
}

func (d *Daemon) Stream(n protocol.StreamNotification) { d.streams.add(n) }

// sendStream sends one (coalesced) stream delta to the channel's
// subscribers.
func (d *Daemon) sendStream(n protocol.StreamNotification) {
	b, _ := json.Marshal(n)
	line := notification(protocol.NStream, b)
	d.eachSubscribed(n.Channel, func(c *client) { c.send(line, true) })
}

func (d *Daemon) Resolve(id string) (model.Model, model.Info, error) { return d.Registry.Resolve(id) }
func (d *Daemon) CheckModel(id string) error                         { return d.Registry.Check(id) }

// ProjectChanged loads the configuration of dir's channels again after one
// of its instructions files changed, and asks for trust again when the hash
// no longer matches.
func (d *Daemon) ProjectChanged(dir string) {
	for _, s := range d.channelList() {
		if s.Dir() != dir {
			continue
		}
		cfg, err := config.Load(dir, d.trust)
		if err != nil {
			log.Printf("reloading %s after its instructions changed: %v", dir, err)
			continue
		}
		s.SetConfig(cfg)
		d.maybeTrustPrompt(s)
	}
}
func (d *Daemon) Variants(id string) []string { return d.Registry.Variants(id) }

func (d *Daemon) Prompt(ctx context.Context, info protocol.PromptInfo, opened func()) escalation.Answer {
	return d.esc.Request(ctx, info, opened)
}

// --- prompts ---

type sinkFunc func(protocol.PromptNotification, []protocol.Tier)

func (f sinkFunc) Notify(n protocol.PromptNotification, tiers []protocol.Tier) { f(n, tiers) }

func (d *Daemon) notifyPrompt(n protocol.PromptNotification, tiers []protocol.Tier) {
	b, _ := json.Marshal(n)
	line := notification(protocol.NPrompt, b)
	for _, c := range d.clientList() {
		_, tier := c.identity()
		for _, t := range tiers {
			if tier == t {
				c.send(line, false)
				break
			}
		}
	}
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

// channelList snapshots the channels. d.mu is never held while a channel is
// called: a channel holds its own lock while it appends, the log's writer
// delivers the commit to clients under d.mu, and holding d.mu across a
// channel call would close that loop into a deadlock.
func (d *Daemon) channelList() []*agent.Channel {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*agent.Channel, 0, len(d.channels))
	for _, s := range d.channels {
		out = append(out, s)
	}
	return out
}

// agentChannel finds the channel owning an agent.
func (d *Daemon) agentChannel(agentID string) (*agent.Channel, *agent.Agent, error) {
	for _, s := range d.channelList() {
		if a, ok := s.Agent(agentID); ok {
			return s, a, nil
		}
	}
	return nil, nil, fmt.Errorf("agent %q %w", agentID, errNotFound)
}

// CreateChannel creates and starts a channel in dir.
func (d *Daemon) CreateChannel(ctx context.Context, dir, modelID, root, want string) (*agent.Channel, error) {
	dir, err := config.WorkingDirectory("", dir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(dir, d.trust)
	if err != nil {
		return nil, err
	}
	id := agent.NewID("c")
	s := agent.New(d, id, dir, cfg, modelID, root)
	d.nameMu.Lock()
	defer d.nameMu.Unlock()
	taken, err := d.channelNames(ctx, "")
	if err != nil {
		return nil, err
	}
	name := agent.UniqueName(filepath.Base(dir), "channel", func(n string) bool { return taken[n] })
	if strings.TrimSpace(want) != "" {
		if name, err = checkName(want, taken); err != nil {
			return nil, err
		}
	}
	if err := s.Start(ctx, name); err != nil { // channel.created indexes it
		return nil, err
	}
	d.mu.Lock()
	d.channels[id] = s
	d.mu.Unlock()
	d.maybeTrustPrompt(s)
	return s, nil
}

// SetChannelDir changes an idle channel's default directory and rebinds its
// project configuration. Old trust prompts are dismissed, never transferred.
func (d *Daemon) SetChannelDir(ctx context.Context, id, dir string) (protocol.ChannelInfo, error) {
	s, err := d.channel(id)
	if err != nil {
		return protocol.ChannelInfo{}, err
	}
	old := s.Dir()
	if err := s.SetDir(ctx, dir, func(dir string) (*config.Effective, error) { return config.Load(dir, d.trust) }); err != nil {
		return protocol.ChannelInfo{}, err
	}
	if old != s.Dir() {
		for _, p := range d.esc.Pending(id) {
			if p.Kind == protocol.PromptTrust {
				_ = d.esc.Resolve(p.ID, "directory-change", escalation.Answer{Value: protocol.AnswerDeny})
			}
		}
	}
	d.maybeTrustPrompt(s)
	return s.Info(), nil
}

// ArchiveChannel archives a channel.
func (d *Daemon) ArchiveChannel(ctx context.Context, id string) error {
	s, err := d.channel(id)
	if err != nil {
		return err
	}
	return s.Archive(ctx)
}

// RenameChannel gives a channel another name: normalised like an agent's
// ("#Docs Site" becomes docs-site), refused when it leaves nothing or another
// channel, archived or not, has it.
func (d *Daemon) RenameChannel(ctx context.Context, id, want string) error {
	s, err := d.channel(id)
	if err != nil {
		return err
	}
	d.nameMu.Lock()
	defer d.nameMu.Unlock()
	taken, err := d.channelNames(ctx, id)
	if err != nil {
		return err
	}
	name, err := checkName(want, taken)
	if err != nil {
		return err
	}
	return s.Rename(ctx, name)
}

// checkName normalises a name the human chose ("#Docs Site" becomes
// docs-site) and refuses one that leaves nothing or another channel has.
func checkName(want string, taken map[string]bool) (string, error) {
	name := agent.NormalizeName(strings.TrimPrefix(strings.TrimSpace(want), "#"))
	if name == "" {
		return "", fmt.Errorf("%q has no letters or digits to name a channel with", want)
	}
	if taken[name] {
		return "", fmt.Errorf("#%s is taken by another channel", name)
	}
	return name, nil
}

// channelNames is every channel's name but except's. Callers hold d.nameMu.
func (d *Daemon) channelNames(ctx context.Context, except string) (map[string]bool, error) {
	rows, err := d.Log.Channels(ctx)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, r := range rows {
		if r.ID != except {
			taken[r.Name] = true
		}
	}
	return taken, nil
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
			info = protocol.ChannelInfo{ID: r.ID, Name: r.Name, Dir: r.Dir, Created: r.Created.Format(time.RFC3339), Archived: r.Archived}
		}
		info.Seq, info.Title = r.LastSeq, r.Title
		info.DirError = config.DirectoryError(info.Dir)
		for _, p := range d.esc.Pending(r.ID) { // what the channel waits on the human for
			if p.Kind == "question" {
				info.Questions++
			} else {
				info.Permissions++
			}
		}
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
	m, err := t.log.Trust(ctx)
	t.m = m
	return err
}

func (t *trustStore) Trusted(dir, hash string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.m[dir] == hash
}

func (t *trustStore) set(ctx context.Context, dir, hash string) error {
	if err := t.log.SetTrust(ctx, dir, hash); err != nil {
		return err
	}
	t.mu.Lock()
	t.m[dir] = hash
	t.mu.Unlock()
	return nil
}

// maybeTrustPrompt raises a trust prompt for a channel whose project layer
// is pending. It is a channel-scoped prompt with long timeouts; the
// channel keeps running on global config meanwhile.
func (d *Daemon) maybeTrustPrompt(s *agent.Channel) {
	cfg := s.Config()
	dir := cfg.Dir
	if dir == "" {
		dir = s.Dir()
	}
	if !cfg.TrustPending {
		return
	}
	d.mu.Lock()
	if _, busy := d.trustPrompts[dir]; busy {
		d.mu.Unlock()
		return
	}
	id := agent.NewID("t")
	d.trustPrompts[dir] = id
	d.mu.Unlock()
	opened := make(chan struct{})
	go func() {
		input, _ := json.Marshal(map[string]any{"dir": dir, "hash": cfg.TrustHash, "files": cfg.TrustFiles})
		ans := d.esc.Request(context.Background(), protocol.PromptInfo{
			ID: id, Channel: s.ID, ChannelName: s.Name(), Kind: protocol.PromptTrust, Input: input,
			Question: fmt.Sprintf("Trust the project configuration in %s? It can define MCP servers, policy, presets, skills and AGENTS.md.", dir),
			Options:  []string{"trust", "skip"},
		}, func() { close(opened) }) // published before a caller can change the channel directory
		d.mu.Lock()
		delete(d.trustPrompts, dir)
		d.mu.Unlock()
		if ans.Value == protocol.AnswerAllow || ans.Value == protocol.AnswerAllowAlways {
			_ = d.Trust(context.Background(), dir, cfg.TrustHash, true)
		}
	}()
	<-opened
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
		if s.Dir() == dir {
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

// LoginKey hands a pasted key to a login waiting for one, which is what
// completes an "apikey" sign-in: the waiting LoginWait then stores it like
// any other credential.
func (d *Daemon) LoginKey(id, key string) error {
	d.loginMu.Lock()
	pl, ok := d.logins[id]
	d.loginMu.Unlock()
	if !ok {
		return fmt.Errorf("no login in progress with id %q", id)
	}
	return pl.pending.Deliver(key)
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
	id     string
	name   string
	tier   protocol.Tier
	send   func(line []byte, droppable bool)   // queues one encoded line; droppable for stream deltas
	replay func(context.Context, []byte) error // waits for queue space during history replay

	mu   sync.Mutex
	subs map[string]int64 // channel id → last seq delivered
}

func (c *client) identity() (string, protocol.Tier) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.name, c.tier
}

// notification encodes a notification line once, for any number of clients.
func notification(method string, params json.RawMessage) []byte {
	b, _ := json.Marshal(protocol.Response{JSONRPC: "2.0", Method: method, Params: params})
	return append(b, '\n')
}

func eventLine(e event.Event) []byte {
	b, _ := json.Marshal(protocol.EventNotification{Event: e})
	return notification(protocol.NEvent, b)
}

// deliver sends a committed event, already encoded, once per subscription
// and in order.
func (c *client) deliver(e event.Event, line []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.subs[e.Channel]
	if !ok || e.Seq <= last {
		return
	}
	c.subs[e.Channel] = e.Seq
	c.send(line, false)
}

// subscribe replays a channel's events from seq from, then hands the
// client over to live delivery with nothing missed and nothing twice. The
// replay waits for the reader outside the log barrier. Once caught up, a
// barrier checks for a final tail and registers live delivery only if empty.
func (d *Daemon) subscribe(ctx context.Context, cl *client, channel string, from int64) (int64, error) {
	if from <= 0 {
		from = 1
	}
	cl.mu.Lock()
	delete(cl.subs, channel) // a re-subscription starts over: no live delivery during the replay
	cl.mu.Unlock()
	last := from - 1
	for {
		evs, err := d.Log.Read(ctx, channel, last+1, 512)
		if err != nil {
			return 0, err
		}
		if len(evs) == 0 {
			var readErr error
			if err := d.Log.Barrier(func() {
				evs, readErr = d.Log.Read(ctx, channel, last+1, 512)
				if readErr == nil && len(evs) == 0 {
					cl.mu.Lock()
					cl.subs[channel] = last
					cl.mu.Unlock()
				}
			}); err != nil {
				return 0, err
			}
			if readErr != nil || len(evs) == 0 {
				return last, readErr
			}
		}
		for _, e := range evs {
			if err := cl.replay(ctx, eventLine(e)); err != nil {
				return 0, err
			}
			last = e.Seq
		}
	}
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
	channels := d.channelList()
	n := 0
	for _, s := range channels {
		n += len(s.Agents())
	}
	provs := d.Registry.Providers()
	sort.Strings(provs)
	return protocol.DaemonStatusResult{Version: protocol.Version, Build: buildid.ID(), PID: os.Getpid(), DataDir: d.DataDir, Channels: len(channels), Agents: n, Providers: provs}
}

// errTrustChanged: a trust reply carried a hash that no longer matches the
// project's files (mapped to ErrConflict on the wire).
var errTrustChanged = errors.New("trust: content changed")
