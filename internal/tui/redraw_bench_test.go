package tui

import (
	"context"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

// BenchmarkChatRedraw is what a stream delta costs in a long chat: the tail
// changes, the chat is redrawn and the frame drawn.
func BenchmarkChatRedraw(b *testing.B) {
	m := newModel(context.Background(), nil, "s")
	m.width, m.height = 120, 40
	m.reconciled, m.loading = true, false
	m.agents = []protocol.AgentInfo{{ID: "a", Name: "main"}}
	t := m.transcript("a")
	for range 2000 {
		t.Notice("a line of the chat that is long enough to look like one, with some words in it")
	}
	m.layout()
	b.ReportAllocs()
	for b.Loop() {
		t.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Text: "token "})
		m.refreshViewport()
		_ = m.View()
	}
}
