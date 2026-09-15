package daemon

import (
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/protocol"
)

// streamWindow is how long a stream delta may wait for more to join it.
const streamWindow = 30 * time.Millisecond

// streams coalesces stream deltas. A model streams a token at a time, and
// encoding and queueing each one for every subscriber costs more than the
// screen can show: an agent's deltas that continue the same text join into
// one notification, sent at most streamWindow later. A channel's pending
// deltas are sent before any event of the channel goes out, since an event
// (assistant.message, tool.finished) settles what streamed before it.
type streams struct {
	send func(protocol.StreamNotification)

	mu      sync.Mutex
	pending map[streamKey]*protocol.StreamNotification // at most one per agent
	timer   *time.Timer                                // armed while anything is pending
}

type streamKey struct{ channel, agent string }

func newStreams(send func(protocol.StreamNotification)) *streams {
	return &streams{send: send, pending: map[streamKey]*protocol.StreamNotification{}}
}

// add queues n, joined to the agent's pending delta when it continues it;
// otherwise the pending one is sent first.
func (s *streams) add(n protocol.StreamNotification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := streamKey{n.Channel, n.Agent}
	if p := s.pending[k]; p != nil {
		if joins(*p, n) {
			p.Text += n.Text
			p.Thinking += n.Thinking
			return
		}
		s.send(*p)
	}
	s.pending[k] = &n
	if s.timer == nil {
		s.timer = time.AfterFunc(streamWindow, s.flushAll)
	}
}

// joins reports whether n continues p: the same turn and tool and the same
// kind of text. A reset and a tool announcement (a name with no text) each
// stand alone.
func joins(p, n protocol.StreamNotification) bool {
	return p.Turn == n.Turn && p.ToolName == n.ToolName && !p.Reset && !n.Reset &&
		(p.Text == "") == (n.Text == "") && (p.Thinking == "") == (n.Thinking == "") &&
		(n.Text != "" || n.Thinking != "")
}

// flush sends the channel's pending deltas.
func (s *streams) flush(channel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendLocked(func(k streamKey) bool { return k.channel == channel })
}

// flushAll sends every pending delta.
func (s *streams) flushAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendLocked(func(streamKey) bool { return true })
}

func (s *streams) sendLocked(match func(streamKey) bool) {
	for k, p := range s.pending {
		if match(k) {
			s.send(*p)
			delete(s.pending, k)
		}
	}
	if len(s.pending) == 0 && s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}
