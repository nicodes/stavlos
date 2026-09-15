package transcript

import (
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tui/transcript/evtest"
)

func spawned(id, parent, name, role string) event.Event {
	return evtest.Ev(id, event.AgentSpawned, event.AgentSpawnedPayload{ID: id, Parent: parent, Name: name, Role: role})
}

func posted(id, text string, to ...string) event.Event {
	return evtest.Ev("", event.ChatPosted, event.ChatPayload{ID: id, Text: text, To: to})
}

func toUser(agent, from, text, post string) event.Event {
	return evtest.Ev(agent, event.ChatMessage, event.ChatPayload{From: from, Text: text, Post: post})
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

// TestChatKeepsOnlyPostsAndReplies: the channel chat shows the human's posts
// and agents' messages to the human, linked to the agent; tool calls,
// prompts and notices stay in the agents' own chats.
func TestChatKeepsOnlyPostsAndReplies(t *testing.T) {
	c := NewChat()
	evtest.Apply(c,
		spawned("a1", "", "main", "general"),
		spawned("b2", "a1", "scout", "general"),
		posted("p1", "look around", "scout"),
		evtest.Call("b2", "c1", "shell", `{"command":"ls"}`),
		evtest.Ev("b2", event.AskRequested, event.AskRequestedPayload{ID: "perm1", Kind: "permission", CallID: "c1", Tool: "shell"}),
		evtest.Ev("b2", event.AskResolved, event.AskResolvedPayload{ID: "perm1", Outcome: event.AskAnswered, Answer: "allow"}),
		evtest.Input("b2", event.Input{Kind: event.InputReminder, Parties: []string{"user"}, Names: []string{"user"}}),
		evtest.Ev("b2", event.AssistantMessage, event.AssistantMessagePayload{}),
		toUser("b2", "scout", "## Found it\nthe bug is in `parse`", "p1"),
	)

	if n := c.Items(); n != 2 {
		t.Fatalf("items %d, want the post and the reply:\n%+v", n, c.All())
	}
	if got := chatItem(c, 0) + " || " + chatItem(c, 1); got != "@user → @scout look around || @scout → @user Found it|>the bug is in `parse`" {
		t.Fatalf("chat %q", got)
	}
	for _, l := range c.All() {
		// the reply reads like an agent's reply: markdown prose, not a
		// quoted block, and grey like an aside (only the human's posts keep
		// the text colour)
		if l.Text == "@scout → @user Found it" && (l.Kind != LineHeading || l.Block != BlockNone || !l.Note || l.Agent != "b2" || l.Glyph != GlyphReply) {
			t.Fatalf("reply line: %+v", l)
		}
		if l.Lead && l.Note {
			t.Fatalf("the human's post keeps the text colour: %+v", l)
		}
		if l.Lead && (l.Who != "user" || strings.Join(l.Names, ",") != "user,scout") {
			t.Fatalf("the post's glyph takes its sender's colour, and each @name its own: %+v", l)
		}
	}
	if ItemAgent(c.All(), 0) != "" || ItemAgent(c.All(), 1) != "b2" {
		t.Fatalf("links: %q %q", ItemAgent(c.All(), 0), ItemAgent(c.All(), 1))
	}
}

// TestChatInArrivalOrder: posts and replies show in the order they happen,
// whichever post a reply answers.
func TestChatInArrivalOrder(t *testing.T) {
	c := NewChat()
	evtest.Apply(c,
		spawned("a1", "", "main", "general"),
		spawned("b2", "", "scout", "general"),
		posted("p1", "check the tests", "scout"),
		posted("p2", "what's the stack?", "main"),
		posted("p3", "and the setup?", "main"),
		toUser("a1", "main", "Go 1.27", "p3"),
		toUser("b2", "scout", "All 42 pass.", "p1"),
	)
	var got []string
	for i := range c.Items() {
		got = append(got, chatItem(c, i))
	}
	want := "@user → @scout check the tests / @user → @main what's the stack? / @user → @main and the setup? / @main → @user Go 1.27 / @scout → @user All 42 pass."
	if strings.Join(got, " / ") != want {
		t.Fatalf("chat:\n%s\nwant:\n%s", strings.Join(got, " / "), want)
	}
}

// TestChatNamesFollowRenames: a reply with no name of its own takes the
// agent's latest name.
func TestChatNamesFollowRenames(t *testing.T) {
	c := NewChat()
	evtest.Apply(c,
		spawned("a1", "", "main", "general"),
		evtest.Ev("a1", event.AgentUpdated, event.AgentUpdatedPayload{Role: event.Str("coder"), Name: event.Str("coder")}),
		toUser("a1", "", "hello", ""),
	)
	if got := chatItem(c, 0); got != "@coder → @user hello" {
		t.Fatalf("chat %q", got)
	}
}

// TestChatWaiting: a post waits on its agents until each sends the human a
// message or is killed.
func TestChatWaiting(t *testing.T) {
	c := NewChat()
	evtest.Apply(c,
		spawned("a1", "", "main", "general"),
		spawned("b2", "", "scout", "general"),
		spawned("c3", "", "lookout", "general"),
		posted("p1", "status?", "main", "scout", "lookout"),
	)
	if w := strings.Join(c.Waiting(), ","); w != "lookout,main,scout" {
		t.Fatalf("waiting %s", w)
	}
	evtest.Apply(c, toUser("a1", "main", "fine", "p1"), evtest.Ev("b2", event.AgentKilled, nil))
	if w := strings.Join(c.Waiting(), ","); w != "lookout" {
		t.Fatalf("after a reply and a kill: %s", w)
	}
	evtest.Apply(c, toUser("c3", "lookout", "on it", ""))
	if w := c.Waiting(); len(w) != 0 {
		t.Fatalf("any message to the human ends the wait: %v", w)
	}
}

// TestChatFoldsLongReplies: a reply longer than MaxOutputCollapsed lines
// shows its head and "… +N lines" until expanded; a short one never folds.
func TestChatFoldsLongReplies(t *testing.T) {
	c := NewChat()
	evtest.Apply(c,
		spawned("a1", "", "main", "general"),
		posted("p1", "summarise", "main"),
		toUser("a1", "main", "one\ntwo\nthree\nfour\nfive\nsix", "p1"),
		toUser("a1", "main", "short", ""),
	)

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
	if strings.Join(always, "|") != "@main → @user one|two|three" || strings.Join(expanded, "|") != "four|five|six" || strings.Join(collapsedOnly, "|") != "… +3 lines" {
		t.Fatalf("always %q expanded %q collapsed %q", always, expanded, collapsedOnly)
	}
	if !ItemFolds(c.All(), 1) || ItemFolds(c.All(), 2) || ItemFolds(c.All(), 0) {
		t.Fatal("the long reply folds; the short one and the post do not")
	}
}
