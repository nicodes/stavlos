package transcript

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
)

// TestChatKeepsPostsMessagesAndPrompts: the session chat shows the human's
// posts, agents' messages to the human and their prompts (linked to the
// agent, named), and nothing of the agents' own work.
func TestChatKeepsPostsMessagesAndPrompts(t *testing.T) {
	c := NewChat()
	seq := int64(0)
	apply := func(agent string, typ event.Type, p any) {
		seq++
		c.Apply(event.Event{Seq: seq, Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)})
	}
	apply("a1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a1", Label: "main"})
	apply("b2", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b2", Parent: "a1", Label: "scout", Archetype: "general"})
	apply("", event.ChatPosted, event.ChatPayload{Text: "@scout look around", To: []string{"scout"}})
	apply("b2", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)})
	apply("b2", event.PromptRequested, event.PromptRequestedPayload{ID: "p1", Kind: "permission", Tool: "shell"})
	apply("b2", event.AssistantMessage, event.AssistantMessagePayload{})
	apply("b2", event.MessageToUser, event.ChatPayload{From: "scout", Text: "## Found it\nthe bug is in `parse`"})
	apply("", event.PromptAnswered, event.PromptAnsweredPayload{ID: "p1", Answer: "allow"})

	if n := c.Items(); n != 3 {
		t.Fatalf("items %d, want the post, the prompt and the message:\n%+v", n, c.All())
	}
	var texts []string
	for _, l := range c.All() {
		if l.Text != "" {
			texts = append(texts, l.Text)
		}
		if strings.Contains(l.Text, "Shell") {
			t.Fatalf("tool calls stay out of the chat: %+v", l)
		}
	}
	got := strings.Join(texts, "|")
	if got != "@scout look around|scout · permission: shell|@scout Found it|the bug is in `parse`" {
		t.Fatalf("chat lines %q", got)
	}
	lines := c.All()
	if ItemAgent(lines, 0) != "" || ItemAgent(lines, 1) != "b2" || ItemAgent(lines, 2) != "b2" {
		t.Fatalf("links: %q %q %q", ItemAgent(lines, 0), ItemAgent(lines, 1), ItemAgent(lines, 2))
	}
	for _, l := range lines {
		if l.Item == 1 && l.Kind == LineNotice && l.Tone != ToneNone {
			t.Fatalf("an answered prompt settles: %+v", l)
		}
		// the message reads like an agent's reply: markdown prose, not a
		// quoted block, and not dimmed like notes
		if l.Item == 2 && l.Text == "@scout Found it" && (l.Kind != LineHeading || l.Block != BlockNone || l.Note) {
			t.Fatalf("message line: %+v", l)
		}
	}
}

// TestChatThreadsRepliesUnderPosts: a reply joins its post's item, indented,
// even after newer posts; a message with no known post stands alone.
func TestChatThreadsRepliesUnderPosts(t *testing.T) {
	c := NewChat()
	seq := int64(0)
	apply := func(agent string, typ event.Type, p any) {
		seq++
		c.Apply(event.Event{Seq: seq, Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)})
	}
	apply("a1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a1", Label: "main", Archetype: "general"})
	apply("b2", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b2", Parent: "a1", Label: "scout", Archetype: "general"})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "@scout check the tests", To: []string{"scout"}})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p2", Text: "what's the stack?", To: []string{"main"}})
	apply("a1", event.MessageToUser, event.ChatPayload{From: "main", Text: "Go 1.27", Post: "p2"})
	apply("b2", event.MessageToUser, event.ChatPayload{From: "scout", Text: "All 42 pass.", Post: "p1"})
	apply("b2", event.MessageToUser, event.ChatPayload{From: "scout", Text: "also: one flaky test"})

	if n := c.Items(); n != 3 {
		t.Fatalf("items %d, want two threads and a standalone message", n)
	}
	item := func(i int) string {
		var out []string
		for _, l := range c.All() {
			if l.Item == i && l.Text != "" {
				out = append(out, strings.Repeat(">", l.Indent)+l.Text)
			}
		}
		return strings.Join(out, "|")
	}
	if got := item(0); got != "@scout check the tests|>@scout All 42 pass." {
		t.Fatalf("first thread %q", got)
	}
	if got := item(1); got != "@main what's the stack?|>@main Go 1.27" {
		t.Fatalf("second thread %q", got)
	}
	if got := item(2); got != "@scout also: one flaky test" {
		t.Fatalf("standalone %q", got)
	}
}

// TestChatGroupsPostsToAWaitingAgent: a post to an agent that has not
// replied yet joins its open thread; a reply closes it; a post to several
// agents groups only when they all wait in the same thread.
func TestChatGroupsPostsToAWaitingAgent(t *testing.T) {
	c := NewChat()
	seq := int64(0)
	apply := func(agent string, typ event.Type, p any) {
		seq++
		c.Apply(event.Event{Seq: seq, Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)})
	}
	apply("a1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a1", Label: "main"})
	apply("b2", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b2", Label: "scout"})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "what's the stack?", To: []string{"main"}})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p2", Text: "and the tests?", To: []string{"main"}})
	apply("a1", event.MessageToUser, event.ChatPayload{From: "main", Text: "Go; go test", Post: "p2"})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p3", Text: "one more", To: []string{"main"}})
	apply("", event.ChatPosted, event.ChatPayload{ID: "p4", Text: "@main @scout sync up", To: []string{"main", "scout"}})
	apply("a1", event.MessageToUser, event.ChatPayload{From: "main", Text: "late answer to p1", Post: "p1"})

	item := func(i int) string {
		var out []string
		for _, l := range c.All() {
			if l.Item == i && l.Text != "" {
				out = append(out, strings.Repeat(">", l.Indent)+l.Text)
			}
		}
		return strings.Join(out, "|")
	}
	if n := c.Items(); n != 3 {
		t.Fatalf("items %d", n)
	}
	if got := item(0); got != "@main what's the stack?|@main and the tests?|>@main Go; go test|>@main late answer to p1" {
		t.Fatalf("grouped thread %q", got)
	}
	if got := item(1); got != "@main one more" {
		t.Fatalf("after a reply a new thread starts: %q", got)
	}
	if got := item(2); got != "@main @scout sync up" {
		t.Fatalf("scout was not waiting, so no grouping: %q", got)
	}
}
