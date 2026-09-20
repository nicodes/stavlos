// Package daemon wires the log, registry, escalation, channels, and the
// protocol server together (PRD §4.1).
package daemon

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/eventlog"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/registry"
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
	closeOnce    sync.Once

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
	// From here a failure gives back what was taken: the lock and the log used
	// to stay held by a daemon that never started, until the process ended.
	ok := false
	defer func() {
		if !ok {
			d.Close()
		}
	}()
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
	ok = true
	return d, nil
}

// Close stops what the daemon runs, in the order that lets each part finish
// with what it depends on still there: the clients' front doors (so nothing
// new arrives), then the channels (whose last events need the log), then the
// log, then the lock that says the data directory is in use. It is safe on a
// daemon that only partly started, and to call twice.
func (d *Daemon) Close() {
	d.closeOnce.Do(func() {
		for _, stop := range []func(){d.closeDiscord, d.closeWeb, d.stopChannels, d.closeLog, d.unlock} {
			stop()
		}
	})
}

func (d *Daemon) closeDiscord() {
	if d.Discord != nil {
		d.Discord.Close()
	}
}

func (d *Daemon) closeWeb() {
	d.webMu.Lock()
	defer d.webMu.Unlock()
	if d.web != nil {
		d.web.Disable() // the listener only; whether it is on stays as the human left it
	}
}

func (d *Daemon) stopChannels() {
	for _, s := range d.channelList() {
		s.Stop()
	}
}

func (d *Daemon) closeLog() {
	if d.Log != nil {
		d.Log.Close()
	}
}

func (d *Daemon) unlock() {
	if d.lock != nil {
		d.lock.Close() // releases the flock
	}
}

// recoverPage is how many events of a channel are in memory at once while it
// is folded at start.
const recoverPage = 2048

func (d *Daemon) recover(ctx context.Context) error {
	rows, err := d.Log.Channels(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Archived {
			continue
		}
		cfg, err := config.Load(r.Dir, d.trust)
		if err != nil {
			log.Printf("channel %s: config: %v (using the global configuration)", r.ID, err)
			if cfg, err = config.LoadGlobal(); err != nil {
				return err
			}
			cfg.Dir = r.Dir
		}
		s, err := agent.RecoverPaged(ctx, d, r.ID, r.Dir, r.Created, cfg, func(fold func([]event.Event)) error {
			for from := int64(1); ; {
				page, err := d.Log.Read(ctx, r.ID, from, recoverPage)
				if err != nil || len(page) == 0 {
					return err
				}
				fold(page)
				from = page[len(page)-1].Seq + 1
			}
		})
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
// in sequence; each event is encoded once for all of them (an event nobody
// is subscribed to, not at all), and delivery
// only queues, so a slow client never holds up a commit.
func (d *Daemon) committed(evs []event.Event) {
	for i, e := range evs {
		if i == 0 || e.Channel != evs[i-1].Channel {
			d.streams.flush(e.Channel) // what streamed goes out before the event that settles it
		}
	}
	clients := d.clientList()
	for _, e := range evs {
		var line []byte // encoded for the first client subscribed to it, and not at all for none
		for _, c := range clients {
			c.deliver(e, func() []byte {
				if line == nil {
					line = eventLine(e)
				}
				return line
			})
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

func (d *Daemon) PlanUsage() map[string]model.PlanUsage { return d.Registry.PlanUsage() }
func (d *Daemon) MarkLimited(provider string, until time.Time) {
	d.Registry.MarkLimited(provider, until)
	d.changed(protocol.ChangedPlan)
}

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
