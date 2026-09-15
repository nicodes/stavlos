package transcript

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/format"
)

// The session chat (docs/super-chat.md) is a Transcript of its own, fed the
// events of every agent: it keeps the human's posts and the agents'
// messages to the human in the order they happen, each message an item of
// its own linked to its agent. Tool calls, prompts and notices stay in the
// agents' own chats.

// NewChat returns an empty session chat.
func NewChat() *Transcript {
	t := NewTranscript()
	t.chat = true
	t.names, t.open = map[string]string{}, map[string]bool{}
	return t
}

// ChatEvent reports whether the session chat reads events of type typ.
func ChatEvent(typ event.Type) bool {
	switch typ {
	case event.AgentSpawned, event.AgentRoleChanged, event.ChatPosted, event.MessageToUser, event.AgentKilled:
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
			t.names[p.ID] = p.Label
		}
	case event.AgentRoleChanged:
		var p event.RoleChangedPayload
		if ev.Decode(&p) == nil && p.Label != "" {
			t.names[ev.Agent] = p.Label
		}
	case event.AgentKilled:
		delete(t.open, t.names[ev.Agent]) // no reply is coming
	case event.ChatPosted:
		var p event.ChatPayload
		if ev.Decode(&p) == nil {
			lines := CleanLines(block(BlockUser, "", addressed(p.To, p.Text)))
			for i := range lines {
				if lines[i].Lead && len(p.To) > 0 {
					// the arrow takes the (first) recipient's colour, and every
					// recipient's @name takes its own
					lines[i].Who, lines[i].Names = p.To[0], p.To
					break
				}
			}
			t.appendItem(lines)
			for _, n := range p.To {
				t.open[n] = true
			}
		}
	case event.MessageToUser:
		var p event.ChatPayload
		if ev.Decode(&p) == nil {
			t.reply(ev.Agent, p)
		}
	}
}

// reply adds an agent's message to the human: "‹ @main …", the mirror of
// a post's "› …", read like the agent's reply in its own chat, with later
// lines aligned under the text and a long one folded.
func (t *Transcript) reply(agent string, p event.ChatPayload) {
	from := p.From
	if from == "" {
		from = t.agentName(agent)
	}
	lines := markdownLines(strings.TrimRight(p.Text, "\n"))
	if len(lines) > 0 && (lines[0].Kind == LineText || lines[0].Kind == LineHeading) {
		lines[0].Text = "@" + from + " " + lines[0].Text
	} else {
		lines = append([]Line{{Kind: LineText, Text: "@" + from}}, lines...)
	}
	lines[0].Glyph, lines[0].Who = GlyphReply, from
	for i := 1; i < len(lines); i++ {
		lines[i].Indent = 1
	}
	lines = append([]Line{{Kind: LineBlank}}, collapsed(lines)...)
	t.appendItem(linked(CleanLines(append(lines, Line{Kind: LineBlank})), agent))
	delete(t.open, from)
}

// Waiting lists the agents a post of the human's is still waiting on,
// sorted: until each sends the human a message or is killed. It is empty
// outside the chat.
func (t *Transcript) Waiting() []string {
	names := make([]string, 0, len(t.open))
	for n := range t.open {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// collapsed folds a long reply the way tool output folds: its first
// MaxOutputCollapsed lines, then "… +N lines" until the item is expanded.
func collapsed(lines []Line) []Line {
	if len(lines) <= MaxOutputCollapsed {
		return lines
	}
	for i := MaxOutputCollapsed; i < len(lines); i++ {
		lines[i].Vis = VisExpanded
	}
	return append(lines, Line{Kind: LineDim, Text: fmt.Sprintf("… +%d lines", len(lines)-MaxOutputCollapsed), Vis: VisCollapsed, Indent: 1})
}

// ItemIsInput reports whether committed item i of lines is the human's own
// input, the one kind of item that never folds.
func ItemIsInput(lines []Line, item int) bool {
	for _, l := range lines {
		if l.Item == item && (l.Block == BlockUser || l.Block == BlockSteer) {
			return true
		}
	}
	return false
}

// ItemFolds reports whether committed item i of lines has lines that show
// only while collapsed or only while expanded (tool output, a long reply).
func ItemFolds(lines []Line, item int) bool {
	for _, l := range lines {
		if l.Item == item && l.Vis != VisAlways {
			return true
		}
	}
	return false
}

// addressed puts the @names a post went to before its text, leaving out
// those the text already mentions: "what's the stack?" sent to main reads
// "@main what's the stack?".
func addressed(to []string, text string) string {
	mentioned := map[string]bool{}
	for _, n := range protocol.Mentions(text) {
		mentioned[n] = true
	}
	var pre []string
	for _, n := range to {
		if !mentioned[strings.ToLower(n)] {
			pre = append(pre, "@"+n)
		}
	}
	if len(pre) == 0 {
		return text
	}
	return strings.Join(pre, " ") + " " + text
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
