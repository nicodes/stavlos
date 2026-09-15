package render

import (
	"strings"
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

// TestChatLongReplyExpands: collapsed, a long reply shows its head and the
// count; expanded, all of it.
func TestChatLongReplyExpands(t *testing.T) {
	c := transcript.NewChat()
	ap := func(seq int64, agent string, typ event.Type, p any) {
		c.Apply(event.Event{Seq: seq, Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)})
	}
	ap(1, "a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a", Label: "main"})
	ap(2, "", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "summarise", To: []string{"main"}})
	ap(3, "a", event.MessageToUser, event.ChatPayload{From: "main", Text: "one\ntwo\nthree\nfour\nfive", Post: "p1"})
	folded := strings.Join(renderWith(c.All(), Options{Width: 80}), "\n")
	if !strings.Contains(folded, "  … +2 lines") || strings.Contains(folded, "five") {
		t.Fatalf("collapsed:\n%s", folded)
	}
	open := strings.Join(renderWith(c.All(), Options{Width: 80, Expanded: map[int]bool{0: true}}), "\n")
	if !strings.Contains(open, "  five") || strings.Contains(open, "+2 lines") {
		t.Fatalf("expanded:\n%s", open)
	}
}

// TestChatGapAndLoader: a blank row separates a post from its reply, and a
// thread still waiting shows a loader naming who it waits on.
func TestChatGapAndLoader(t *testing.T) {
	c := transcript.NewChat()
	ap := func(seq int64, agent string, typ event.Type, p any) {
		c.Apply(event.Event{Seq: seq, Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)})
	}
	ap(1, "a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a", Label: "main"})
	ap(2, "", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "what's the stack?", To: []string{"main"}})
	ap(3, "a", event.MessageToUser, event.ChatPayload{From: "main", Text: "Go", Post: "p1"})
	ap(4, "", event.ChatPosted, event.ChatPayload{ID: "p2", Text: "and the tests?", To: []string{"main"}})
	rows := renderWith(c.All(), Options{Width: 80, Spinner: "◐", Pending: c.Waiting()})
	got := strings.Join(rows, "\n")
	want := "› @main what's the stack?\n\n  @main Go\n\n› @main and the tests?\n\n  ◐ " + transcript.TurnVerbs[1] + "… · @main"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
