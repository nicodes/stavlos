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
	if got != "to scout|@scout look around|scout · permission: shell|scout (general)|Found it|the bug is in `parse`" {
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
		if l.Item == 2 && l.Text == "Found it" && (l.Kind != LineHeading || l.Block != BlockNone || l.Note) {
			t.Fatalf("message line: %+v", l)
		}
	}
}
