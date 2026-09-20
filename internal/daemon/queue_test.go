package daemon

import (
	"net"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
)

// A client's queue is bounded by what it holds, not only by how many: a few
// large messages nobody reads drop the client long before 16,384 of them
// could pile up, and a delta is shed before anything is dropped.
func TestAClientsQueueIsBoundedInBytes(t *testing.T) {
	old := maxQueueBytes
	maxQueueBytes = 1000
	defer func() { maxQueueBytes = old }()
	here, there := net.Pipe()
	defer there.Close()
	c := &conn{c: here, out: make(chan outMsg, outQueue), room: make(chan struct{}, 1), done: make(chan struct{}), cl: &client{id: "t"}}
	big := make([]byte, 400)
	c.enqueue(big, false)
	c.enqueue(big, false)
	c.enqueue(big, true) // a delta, past half the budget: shed, and the client kept
	select {
	case <-c.done:
		t.Fatal("a shed delta cost the client its connection")
	default:
	}
	if got := c.queued.Load(); got != 800 {
		t.Fatalf("queued = %d bytes, want 800", got)
	}
	c.enqueue(big, false) // 1200 > 1000
	select {
	case <-c.done:
	default:
		t.Fatal("a queue over its byte budget kept its client")
	}
	if len(c.out) != 2 {
		t.Fatalf("%d messages queued, want 2", len(c.out))
	}
}

// An event nobody is subscribed to is never encoded.
func TestAnEventNobodyWatchesIsNotEncoded(t *testing.T) {
	encoded := 0
	line := func() []byte { encoded++; return []byte("x\n") }
	idle := &client{id: "idle", subs: map[string]int64{}}
	idle.deliver(event.Event{Channel: "c", Seq: 1}, line)
	if encoded != 0 {
		t.Fatalf("encoded %d times for no subscriber", encoded)
	}
}
