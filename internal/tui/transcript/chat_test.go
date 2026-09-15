package transcript

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
)

func newChatFeed() (*Transcript, func(agent string, typ event.Type, p any)) {
	c := NewChat()
	seq := int64(0)
	return c, func(agent string, typ event.Type, p any) {
		seq++
		c.Apply(event.Event{Seq: seq, Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)})
	}
}

// chatItem is item i's non-empty texts, each prefixed by ">" per indent.
func chatItem(c *Transcript, i int) string {
	var out []string
	for _, l := range c.All() {
		if l.Item == i && l.Text != "" {
			out = append(out, strings.Repeat(">", l.Indent)+l.Text)
		}
	}
	return strings.Join(out, "|")
}

// TestChatKeepsOnlyPostsAndReplies: the session chat shows the human's posts
// and agents' messages to the human, linked to the agent; tool calls,
// prompts and notices stay in the agents' own chats.
func TestChatKeepsOnlyPostsAndReplies(t *testing.T) {
	c, apply := newChatFeed()
	apply("a1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a1", Label: "main"})
	apply("b2", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b2", Parent: "a1", Label: "scout", Archetype: "general"})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "look around", To: []string{"scout"}})
	apply("b2", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)})
	apply("b2", event.PromptRequested, event.PromptRequestedPayload{ID: "perm1", Kind: "permission", Tool: "shell"})
	apply("", event.PromptAnswered, event.PromptAnsweredPayload{ID: "perm1", Answer: "allow"})
	apply("b2", event.ReminderQueued, event.RepliesPayload{Parties: []string{"user"}, Names: []string{"user"}})
	apply("b2", event.AssistantMessage, event.AssistantMessagePayload{})
	apply("b2", event.MessageToUser, event.ChatPayload{From: "scout", Text: "## Found it\nthe bug is in `parse`", Post: "p1"})

	if n := c.Items(); n != 2 {
		t.Fatalf("items %d, want the post and the reply:\n%+v", n, c.All())
	}
	if got := chatItem(c, 0) + " || " + chatItem(c, 1); got != "@scout look around || @scout Found it|>the bug is in `parse`" {
		t.Fatalf("chat %q", got)
	}
	for _, l := range c.All() {
		// the reply reads like an agent's reply: markdown prose, not a
		// quoted block, and not dimmed like notes
		if l.Text == "@scout Found it" && (l.Kind != LineHeading || l.Block != BlockNone || l.Note || l.Agent != "b2" || l.Glyph != GlyphReply) {
			t.Fatalf("reply line: %+v", l)
		}
	}
	for _, l := range c.All() {
		if l.Lead && (l.Who != "scout" || len(l.Names) != 1 || l.Names[0] != "scout") {
			t.Fatalf("the post's arrow and @name take its recipient's colour: %+v", l)
		}
	}
	if ItemAgent(c.All(), 0) != "" || ItemAgent(c.All(), 1) != "b2" {
		t.Fatalf("links: %q %q", ItemAgent(c.All(), 0), ItemAgent(c.All(), 1))
	}
}

// TestChatInArrivalOrder: posts and replies show in the order they happen,
// whichever post a reply answers.
func TestChatInArrivalOrder(t *testing.T) {
	c, apply := newChatFeed()
	apply("a1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a1", Label: "main"})
	apply("b2", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b2", Label: "scout"})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "check the tests", To: []string{"scout"}})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p2", Text: "what's the stack?", To: []string{"main"}})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p3", Text: "and the setup?", To: []string{"main"}})
	apply("a1", event.MessageToUser, event.ChatPayload{From: "main", Text: "Go 1.27", Post: "p3"})
	apply("b2", event.MessageToUser, event.ChatPayload{From: "scout", Text: "All 42 pass.", Post: "p1"})
	var got []string
	for i := range c.Items() {
		got = append(got, chatItem(c, i))
	}
	want := "@scout check the tests / @main what's the stack? / @main and the setup? / @main Go 1.27 / @scout All 42 pass."
	if strings.Join(got, " / ") != want {
		t.Fatalf("chat:\n%s\nwant:\n%s", strings.Join(got, " / "), want)
	}
}

// TestChatWaiting: a post waits on its agents until each sends the human a
// message or is killed.
func TestChatWaiting(t *testing.T) {
	c, apply := newChatFeed()
	apply("a1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a1", Label: "main"})
	apply("b2", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b2", Label: "scout"})
	apply("c3", event.AgentSpawned, event.AgentSpawnedPayload{ID: "c3", Label: "lookout"})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "status?", To: []string{"main", "scout", "lookout"}})
	if w := strings.Join(c.Waiting(), ","); w != "lookout,main,scout" {
		t.Fatalf("waiting %s", w)
	}
	apply("a1", event.MessageToUser, event.ChatPayload{From: "main", Text: "fine", Post: "p1"})
	apply("b2", event.AgentKilled, event.AgentRefPayload{ID: "b2"})
	if w := strings.Join(c.Waiting(), ","); w != "lookout" {
		t.Fatalf("after a reply and a kill: %s", w)
	}
	apply("c3", event.MessageToUser, event.ChatPayload{From: "lookout", Text: "on it"})
	if w := c.Waiting(); len(w) != 0 {
		t.Fatalf("any message to the human ends the wait: %v", w)
	}
}

// TestChatFoldsLongReplies: a reply longer than MaxOutputCollapsed lines
// shows its head and "… +N lines" until expanded; a short one never folds.
func TestChatFoldsLongReplies(t *testing.T) {
	c, apply := newChatFeed()
	apply("a1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a1", Label: "main"})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "summarise", To: []string{"main"}})
	apply("a1", event.MessageToUser, event.ChatPayload{From: "main", Text: "one\ntwo\nthree\nfour\nfive\nsix", Post: "p1"})
	apply("a1", event.MessageToUser, event.ChatPayload{From: "main", Text: "short"})

	var always, expanded, collapsedOnly []string
	for _, l := range c.All() {
		if l.Item != 1 || l.Text == "" {
			continue
		}
		switch l.Vis {
		case VisAlways:
			always = append(always, l.Text)
		case VisExpanded:
			expanded = append(expanded, l.Text)
		case VisCollapsed:
			collapsedOnly = append(collapsedOnly, l.Text)
		}
	}
	if strings.Join(always, "|") != "@main one|two|three" || strings.Join(expanded, "|") != "four|five|six" || strings.Join(collapsedOnly, "|") != "… +3 lines" {
		t.Fatalf("always %q expanded %q collapsed %q", always, expanded, collapsedOnly)
	}
	if !ItemFolds(c.All(), 1) || ItemFolds(c.All(), 2) || ItemFolds(c.All(), 0) {
		t.Fatal("the long reply folds; the short one and the post do not")
	}
}
