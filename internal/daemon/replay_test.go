package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

func TestReplayBackpressureAndLiveHandover(t *testing.T) {
	setupConfig(t)
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	evs := make([]event.Event, 1200) // spans several replay pages
	for i := range evs {
		evs[i] = event.Event{Channel: s.ID, Type: "test.noise"}
	}
	if _, err := h.d.Append(ctx, evs...); err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer b.Close()
	c := &conn{c: a, out: make(chan outMsg, 2), done: make(chan struct{})}
	defer c.close()
	full := make(chan struct{})
	calls := 0
	c.cl = &client{id: "replay-test", subs: map[string]int64{}, send: c.enqueue, replay: func(ctx context.Context, line []byte) error {
		calls++
		if calls == 3 {
			close(full)
		}
		return c.enqueueReplay(ctx, line)
	}}
	h.d.addClient(c.cl)
	defer h.d.removeClient(c.cl.id)
	done := make(chan error, 1)
	go func() {
		_, err := h.d.subscribe(ctx, c.cl, s.ID, 1)
		done <- err
	}()
	select {
	case <-full:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-c.done:
		t.Fatal("history burst disconnected a healthy reader")
	case err := <-done:
		t.Fatalf("replay did not wait for the reader: %v", err)
	default:
	}
	// Appends must proceed while replay waits for queue space. The new event
	// must then be included in the replay tail, before the live subscription.
	appended := make(chan []event.Event, 1)
	go func() {
		out, _ := h.d.Append(ctx, event.Event{Channel: s.ID, Type: "test.tail"})
		appended <- out
	}()
	var tail []event.Event
	select {
	case tail = <-appended:
		if len(tail) != 1 {
			t.Fatal("append failed")
		}
	case <-ctx.Done():
		t.Fatal("replay held the log barrier while waiting for the reader")
	}
	read := func(want int64) {
		t.Helper()
		select {
		case m := <-c.out:
			var r protocol.Response
			var n protocol.EventNotification
			if err := json.Unmarshal(m.b, &r); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(r.Params, &n); err != nil || n.Event.Seq != want {
				t.Fatalf("want seq %d, got %d: %v", want, n.Event.Seq, err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for seq := int64(1); seq <= tail[0].Seq; seq++ {
		read(seq)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Append(ctx, event.Event{Channel: s.ID, Type: "test.live"}); err != nil {
		t.Fatal(err)
	}
	read(tail[0].Seq + 1)
}

func TestReplayWaitEndsOnCancellationOrDisconnect(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		c := &conn{out: make(chan outMsg, 1), done: make(chan struct{})}
		c.out <- outMsg{}
		want := error(context.Canceled)
		if disconnect {
			close(c.done)
			want = net.ErrClosed
		} else {
			cancel()
		}
		if err := c.enqueueReplay(ctx, nil); !errors.Is(err, want) {
			t.Fatalf("want %v, got %v", want, err)
		}
		cancel()
	}
}
