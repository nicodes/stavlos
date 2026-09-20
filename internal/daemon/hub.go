package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"

	"github.com/nicodes/stavlos/internal/buildid"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

// The hub: the clients attached to the daemon, what each is subscribed to,
// and the handover from a channel's history to its live events.

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

// deliver sends a committed event once per subscription and in order; line
// encodes it, once for all the clients that want it.
func (c *client) deliver(e event.Event, line func() []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.subs[e.Channel]
	if !ok || e.Seq <= last {
		return
	}
	c.subs[e.Channel] = e.Seq
	c.send(line(), false)
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
	d.hubMu.RLock()
	defer d.hubMu.RUnlock()
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
	d.hubMu.Lock()
	d.clients[c.id] = c
	d.hubMu.Unlock()
}

func (d *Daemon) removeClient(id string) {
	d.hubMu.Lock()
	delete(d.clients, id)
	d.hubMu.Unlock()
}

// Status for daemon.status.
func (d *Daemon) Status() protocol.DaemonStatusResult {
	channels := d.channelList()
	n, working := 0, 0
	for _, s := range channels {
		for _, a := range s.Agents() {
			n++
			if in := a.Info(); in.State.Busy() || len(in.Jobs) > 0 {
				working++
			}
		}
	}
	provs := d.Registry.Providers()
	sort.Strings(provs)
	return protocol.DaemonStatusResult{Version: protocol.Version, Build: buildid.ID(), PID: os.Getpid(), DataDir: d.DataDir, Channels: len(channels), Agents: n, Working: working, Providers: provs}
}

// errTrustChanged: a trust reply carried a hash that no longer matches the
// project's files (mapped to ErrConflict on the wire).
var errTrustChanged = errors.New("trust: content changed")
