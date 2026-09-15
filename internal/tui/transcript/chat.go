package transcript

import (
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tui/format"
)

// The session chat (docs/super-chat.md) is a Transcript of its own, fed the
// events of every agent: it keeps the human's posts, the agents' messages
// to the human, and the permission prompts and questions, each line linked
// to its agent. Tool calls and everything else pass it by.

// NewChat returns an empty session chat.
func NewChat() *Transcript {
	t := NewTranscript()
	t.chat = true
	t.names, t.roles = map[string]string{}, map[string]string{}
	return t
}

// ChatEvent reports whether the session chat reads events of type typ.
func ChatEvent(typ event.Type) bool {
	switch typ {
	case event.AgentSpawned, event.AgentRoleChanged, event.ChatPosted, event.MessageToUser,
		event.PromptRequested, event.PromptAnswered, event.PromptWithdrawn, event.PromptDefaulted:
		return true
	}
	return false
}

// ItemAgent is the agent committed item i of lines links to, "" for none.
func ItemAgent(lines []Line, item int) string {
	for _, l := range lines {
		if l.Item == item && l.Agent != "" {
			return l.Agent
		}
	}
	return ""
}

// applyChat folds one session event into the chat.
func (t *Transcript) applyChat(ev event.Event) {
	switch ev.Type {
	case event.AgentSpawned:
		var p event.AgentSpawnedPayload
		if ev.Decode(&p) == nil && p.ID != "" {
			t.names[p.ID], t.roles[p.ID] = p.Label, p.Archetype
		}
	case event.AgentRoleChanged:
		var p event.RoleChangedPayload
		if ev.Decode(&p) == nil {
			if p.Label != "" {
				t.names[ev.Agent] = p.Label
			}
			t.roles[ev.Agent] = p.Role
		}
	case event.ChatPosted:
		var p event.ChatPayload
		if ev.Decode(&p) == nil {
			t.appendItem(CleanLines(block(BlockUser, "to "+strings.Join(p.To, ", "), p.Text)))
		}
	case event.MessageToUser:
		var p event.ChatPayload
		if ev.Decode(&p) == nil {
			// Reads like an agent's reply in its own chat, under the
			// sender's "name (role)".
			from := p.From
			if from == "" {
				from = t.agentName(ev.Agent)
			}
			if role := t.roles[ev.Agent]; role != "" {
				from += " (" + role + ")"
			}
			lines := []Line{{Kind: LineBlank}, {Kind: LineLabel, Text: from}}
			lines = append(lines, markdownLines(strings.TrimRight(p.Text, "\n"))...)
			t.appendItem(linked(CleanLines(append(lines, Line{Kind: LineBlank})), ev.Agent))
		}
	case event.PromptRequested:
		var p event.PromptRequestedPayload
		if ev.Decode(&p) != nil {
			return
		}
		lines := CleanLines(EventLines(ev))
		for i := range lines {
			if lines[i].Kind == LineNotice {
				lines[i].Text = t.agentName(ev.Agent) + " · " + lines[i].Text
			}
		}
		if r, ok := t.find(t.appendItem(linked(lines, ev.Agent)), isPromptLine); ok {
			t.promptLine[p.ID] = r
		}
	case event.PromptAnswered, event.PromptWithdrawn, event.PromptDefaulted:
		var p event.PromptRefPayload
		if ev.Decode(&p) == nil {
			t.settlePrompt(p.ID, ev.Type != event.PromptAnswered)
		}
	}
}

// agentName is how the chat names agent id: its name once spawned, a short
// id before that.
func (t *Transcript) agentName(id string) string {
	if n := t.names[id]; n != "" {
		return n
	}
	return format.ShortID(id)
}

// linked marks lines as belonging to agent id.
func linked(lines []Line, id string) []Line {
	for i := range lines {
		lines[i].Agent = id
	}
	return lines
}
