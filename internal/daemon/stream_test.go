package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/protocol"
)

type sent struct {
	mu sync.Mutex
	ns []protocol.StreamNotification
	ch chan struct{}
}

func newSent() *sent { return &sent{ch: make(chan struct{}, 64)} }

func (s *sent) send(n protocol.StreamNotification) {
	s.mu.Lock()
	s.ns = append(s.ns, n)
	s.mu.Unlock()
	s.ch <- struct{}{}
}

func (s *sent) all() []protocol.StreamNotification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]protocol.StreamNotification(nil), s.ns...)
}

// TestStreamsJoinWhatContinues: an agent's deltas of one kind join into one
// notification; a change of kind, a tool announcement and a reset each send
// what was pending first; flushing a channel sends only its own.
func TestStreamsJoinWhatContinues(t *testing.T) {
	out := newSent()
	s := newStreams(out.send)
	d := func(agent string, n protocol.StreamNotification) {
		n.Channel, n.Agent, n.Turn = "ch", agent, 1
		s.add(n)
	}
	d("a", protocol.StreamNotification{Thinking: "hm"})
	d("a", protocol.StreamNotification{Thinking: "m"})
	d("a", protocol.StreamNotification{Text: "hel"})
	d("a", protocol.StreamNotification{Text: "lo"})
	d("b", protocol.StreamNotification{Text: "other agent"})
	d("a", protocol.StreamNotification{ToolName: "shell"})
	d("a", protocol.StreamNotification{ToolName: "shell"})
	d("a", protocol.StreamNotification{Reset: true})
	s.add(protocol.StreamNotification{Channel: "elsewhere", Agent: "a", Text: "kept"})
	s.flush("ch")

	var got []string
	for _, n := range out.all() {
		got = append(got, n.Agent+":"+n.Thinking+"|"+n.Text+"|"+n.ToolName+map[bool]string{true: "|reset"}[n.Reset])
	}
	want := map[string]int{"a:hmm||": 1, "a:|hello|": 1, "a:||shell": 2, "a:|||reset": 1, "b:|other agent|": 1}
	counts := map[string]int{}
	for _, g := range got {
		counts[g]++
	}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("sent %q, want %v", got, want)
		}
	}
	if len(got) != 6 {
		t.Fatalf("sent %q: the other channel's delta waits", got)
	}
	// a's deltas go out in the order they streamed
	var order []string
	for _, g := range got {
		if g[0] == 'a' {
			order = append(order, g)
		}
	}
	if want := []string{"a:hmm||", "a:|hello|", "a:||shell", "a:||shell", "a:|||reset"}; len(order) != len(want) || order[0] != want[0] || order[1] != want[1] || order[4] != want[4] {
		t.Fatalf("agent a's order %q, want %q", order, want)
	}
}

// TestStreamsFlushOnTheirOwn: what nobody flushes goes out within the
// window.
func TestStreamsFlushOnTheirOwn(t *testing.T) {
	out := newSent()
	s := newStreams(out.send)
	s.add(protocol.StreamNotification{Channel: "ch", Agent: "a", Text: "x"})
	s.add(protocol.StreamNotification{Channel: "ch", Agent: "a", Text: "y"})
	select {
	case <-out.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("a pending delta was never sent")
	}
	if ns := out.all(); len(ns) != 1 || ns[0].Text != "xy" {
		t.Fatalf("sent %+v", ns)
	}
}
