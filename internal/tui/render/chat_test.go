package render

import (
	"github.com/charmbracelet/lipgloss"
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
	ap(1, "a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a", Name: "main"})
	ap(2, "b", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b", Name: "scout"})
	ap(3, "", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "what's the stack?", To: []string{"main"}})
	ap(4, "", event.ChatPosted, event.ChatPayload{ID: "p2", Text: "check the tests", To: []string{"scout"}})
	ap(5, "a", event.ChatMessage, event.ChatPayload{From: "main", Text: "Go 1.27\nSQLite event log", Post: "p1"})
	ap(6, "b", event.ChatMessage, event.ChatPayload{From: "scout", Text: "All pass", Post: "p2"})
	got := strings.Join(renderWith(c.All(), Options{Width: 80}), "\n")
	want := "› @user → @main what's the stack?\n\n› @user → @scout check the tests\n\n‹ @main → @user Go 1.27\n  SQLite event log\n\n‹ @scout → @user All pass"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestChatLongReplyExpands: collapsed, a long reply shows its head and the
// count; expanded, all of it.
func TestChatLongReplyExpands(t *testing.T) {
	c, ap := chatFeed()
	ap(1, "a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a", Name: "main"})
	ap(2, "", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "summarise", To: []string{"main"}})
	ap(3, "a", event.ChatMessage, event.ChatPayload{From: "main", Text: "one\ntwo\nthree\nfour\nfive", Post: "p1"})
	folded := strings.Join(renderWith(c.All(), Options{Width: 80}), "\n")
	if !strings.Contains(folded, "\n  … +2 lines") || strings.Contains(folded, "five") {
		t.Fatalf("collapsed:\n%s", folded)
	}
	open := strings.Join(renderWith(c.All(), Options{Width: 80, Expanded: map[int]bool{1: true}}), "\n")
	if !strings.Contains(open, "\n  five") || strings.Contains(open, "+2 lines") {
		t.Fatalf("expanded:\n%s", open)
	}
}

// TestChatPostColoursItsRecipients: a post shows its sender and recipients
// in front, "@user → @main @scout", each in its colour (the glyph takes the
// sender's), and its message as typed.
func TestChatPostColoursItsRecipients(t *testing.T) {
	c, ap := chatFeed()
	ap(1, "", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "sync up with @Scout", To: []string{"main", "scout"}})
	var asked []string
	whoStyle := func(name string) lipgloss.Style {
		asked = append(asked, name)
		return lipgloss.NewStyle()
	}
	got := strings.Join(renderWith(c.All(), Options{Width: 80, WhoStyle: whoStyle}), "\n")
	if got != "› @user → @main @scout sync up with @Scout" { // the message's own @Scout is left alone
		t.Fatalf("post: %q", got)
	}
	if strings.Join(asked, ",") != "user,user,main,scout" { // the glyph, then each leading @name
		t.Fatalf("colours asked for %v", asked)
	}
}
