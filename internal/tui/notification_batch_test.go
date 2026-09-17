package tui

import (
	"encoding/json"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestNotificationBatchPreservesOrderAndBounds(t *testing.T) {
	notify := func(seq int64) protocol.Response {
		payload, _ := json.Marshal(protocol.EventNotification{Event: event.Event{Seq: seq}})
		return protocol.Response{Method: protocol.NEvent, Params: payload}
	}
	pending := make(chan protocol.Response, 200)
	for seq := int64(2); seq <= 201; seq++ {
		pending <- notify(seq)
	}
	first := notificationBatch(notify(1), pending)
	if len(first) != 128 || len(pending) != 73 {
		t.Fatalf("batch=%d pending=%d", len(first), len(pending))
	}
	second := notificationBatch(<-pending, pending)
	for i, msg := range append(first, second...) {
		if got := msg.(eventMsg).ev.Seq; got != int64(i+1) {
			t.Fatalf("event %d: seq %d", i, got)
		}
	}
	// A single live event is forwarded without waiting for more traffic.
	if got := notificationBatch(notify(202), pending); len(got) != 1 {
		t.Fatalf("single live event: %v", got)
	}
}

func TestNotificationBatchKeepsPromptAndStreamOrder(t *testing.T) {
	e, _ := json.Marshal(protocol.EventNotification{Event: event.Event{Seq: 1}})
	p, _ := json.Marshal(protocol.PromptNotification{Action: protocol.ActionRequested})
	s, _ := json.Marshal(protocol.StreamNotification{})
	pending := make(chan protocol.Response, 2)
	pending <- protocol.Response{Method: protocol.NPrompt, Params: p}
	pending <- protocol.Response{Method: protocol.NStream, Params: s}
	batch := notificationBatch(protocol.Response{Method: protocol.NEvent, Params: e}, pending)
	if len(batch) != 3 {
		t.Fatalf("batch size: %d", len(batch))
	}
	if _, ok := batch[0].(eventMsg); !ok {
		t.Fatal("event out of order")
	}
	if _, ok := batch[1].(promptMsg); !ok {
		t.Fatal("prompt out of order")
	}
	if _, ok := batch[2].(streamMsg); !ok {
		t.Fatal("stream out of order")
	}
}
