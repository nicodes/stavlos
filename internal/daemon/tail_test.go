package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/modelsdev"
	"github.com/nicodes/stavlos/internal/protocol"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

// A subscription with a tail is sent the channel's last events and told
// where they start; without one, or when the tail covers the channel, it is
// sent everything and told nothing.
func TestSubscribeWithATailSendsTheEndOfTheHistory(t *testing.T) {
	setupConfig(t)
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	evs := make([]event.Event, 500)
	for i := range evs {
		evs[i] = event.Event{Channel: s.ID, Type: "test.noise"}
	}
	out, err := h.d.Append(ctx, evs...)
	if err != nil {
		t.Fatal(err)
	}
	head := out[len(out)-1].Seq

	// every event of a replay is taken, so none is left for the next to find
	sent := func(first int64) {
		t.Helper()
		for want := first; want <= head; want++ {
			select {
			case e := <-h.evs:
				if e.Seq != want {
					t.Fatalf("sent seq %d, want %d (a replay from %d)", e.Seq, want, first)
				}
			case <-ctx.Done():
				t.Fatalf("the replay from %d stopped before %d", first, want)
			}
		}
	}
	res, err := rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, Tail: 100})
	if err != nil {
		t.Fatal(err)
	}
	if want := head - 99; res.First != want || res.Seq != head {
		t.Fatalf("first %d seq %d, want %d and %d", res.First, res.Seq, want, head)
	}
	sent(head - 99)
	// a tail longer than the channel: everything, and nothing said to be missing
	res, err = rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, Tail: 100_000})
	if err != nil || res.First != 0 {
		t.Fatalf("a tail that covers the channel: first %d, %v", res.First, err)
	}
	sent(1)
	// a client continuing from a seq it holds is never cut
	res, err = rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID, From: 50, Tail: 10})
	if err != nil || res.First != 0 {
		t.Fatalf("continuing from 50: first %d, %v", res.First, err)
	}
	sent(50)
}

// A daemon that fails to start gives the data directory back: the next one
// can take it without waiting for this process to end.
func TestAFailedStartReleasesTheDataDirectory(t *testing.T) {
	setupConfig(t)
	dir := t.TempDir()
	cat, _ := modelsdev.Parse([]byte(`{"fake":{"id":"fake","env":[],"models":{}}}`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // recovery's first query fails: a start that gets past the lock and the log, then stops
	if d, err := New(ctx, dir, registry.New(cat)); err == nil {
		d.Close()
		t.Skip("a cancelled context did not stop this start")
	}
	d, err := New(context.Background(), dir, registry.New(cat))
	if err != nil {
		t.Fatalf("the directory a failed start had locked: %v", err)
	}
	d.Close()
	d.Close() // twice is once
}
