package render

import (
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

// TestChatThreadRendersIndented: a reply draws two columns in under its post.
func TestChatThreadRendersIndented(t *testing.T) {
	c := transcript.NewChat()
	ap := func(seq int64, agent string, typ event.Type, p any) {
		c.Apply(event.Event{Seq: seq, Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)})
	}
	ap(1, "a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a", Label: "main", Archetype: "general"})
	ap(2, "", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "what's the stack?", To: []string{"main"}})
	ap(3, "a", event.MessageToUser, event.ChatPayload{From: "main", Text: "Go 1.27, SQLite event log.", Post: "p1"})
	assertSubsequence(t, renderWith(c.All(), Options{Width: 80}), []string{"› @main what's the stack?", "  @main Go 1.27, SQLite event log."})
}
