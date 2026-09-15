package render

import (
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

func chatFeed() (*transcript.Transcript, func(seq int64, agent string, typ event.Type, p any)) {
	c := transcript.NewChat()
	return c, func(seq int64, agent string, typ event.Type, p any) {
		c.Apply(event.Event{Seq: seq, Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)})
	}
}

// TestChatRendersInOrder: posts and replies render in the order they came,
// spaced alike, a reply as "‹ @name …" with its later lines past the glyph.
func TestChatRendersInOrder(t *testing.T) {
	c, ap := chatFeed()
	ap(1, "a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a", Label: "main"})
	ap(2, "b", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b", Label: "scout"})
	ap(3, "", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "what's the stack?", To: []string{"main"}})
	ap(4, "", event.ChatPosted, event.ChatPayload{ID: "p2", Text: "@scout check the tests", To: []string{"scout"}})
	ap(5, "a", event.MessageToUser, event.ChatPayload{From: "main", Text: "Go 1.27\nSQLite event log", Post: "p1"})
	ap(6, "b", event.MessageToUser, event.ChatPayload{From: "scout", Text: "All pass", Post: "p2"})
	got := strings.Join(renderWith(c.All(), Options{Width: 80}), "\n")
	want := "› @main what's the stack?\n\n› @scout check the tests\n\n‹ @main Go 1.27\n  SQLite event log\n\n‹ @scout All pass"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestChatLongReplyExpands: collapsed, a long reply shows its head and the
// count; expanded, all of it.
func TestChatLongReplyExpands(t *testing.T) {
	c, ap := chatFeed()
	ap(1, "a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a", Label: "main"})
	ap(2, "", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "summarise", To: []string{"main"}})
	ap(3, "a", event.MessageToUser, event.ChatPayload{From: "main", Text: "one\ntwo\nthree\nfour\nfive", Post: "p1"})
	folded := strings.Join(renderWith(c.All(), Options{Width: 80}), "\n")
	if !strings.Contains(folded, "\n  … +2 lines") || strings.Contains(folded, "five") {
		t.Fatalf("collapsed:\n%s", folded)
	}
	open := strings.Join(renderWith(c.All(), Options{Width: 80, Expanded: map[int]bool{1: true}}), "\n")
	if !strings.Contains(open, "\n  five") || strings.Contains(open, "+2 lines") {
		t.Fatalf("expanded:\n%s", open)
	}
}
