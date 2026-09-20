package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/protocol"
)

type flippingDiscord struct {
	mu sync.Mutex
	st protocol.DiscordStatus
}

func (f *flippingDiscord) Start() {}
func (f *flippingDiscord) Close() {}
func (f *flippingDiscord) Status() protocol.DiscordStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}
func (f *flippingDiscord) Connect() (protocol.DiscordStatus, error)    { return f.Status(), nil }
func (f *flippingDiscord) Disconnect() (protocol.DiscordStatus, error) { return f.Status(), nil }

// A client is told when Discord's status changes, once per change, and is
// told nothing while it stays as it was.
func TestClientsAreToldWhenDiscordChanges(t *testing.T) {
	f := &flippingDiscord{st: protocol.DiscordStatus{State: "connecting"}}
	d := &Daemon{Discord: f, clients: map[string]*client{}}
	got := make(chan string, 8)
	d.addClient(&client{id: "c", subs: map[string]int64{}, send: func(line []byte, _ bool) {
		var r protocol.Response
		var n protocol.ChangedNotification
		if json.Unmarshal(line, &r) == nil && r.Method == protocol.NChanged && json.Unmarshal(r.Params, &n) == nil {
			got <- n.What
		}
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.watchDiscord(ctx, time.Millisecond) // what it compares against is read before it returns

	f.mu.Lock()
	f.st.State = "connected"
	f.mu.Unlock()
	select {
	case what := <-got:
		if what != protocol.ChangedDiscord {
			t.Fatalf("told about %q", what)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notice of the change")
	}
	select {
	case what := <-got:
		t.Fatalf("told about %q again with nothing changed", what)
	case <-time.After(30 * time.Millisecond):
	}
}
