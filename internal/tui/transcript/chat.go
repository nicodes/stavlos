package transcript

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/toolname"
	"github.com/nicodes/stavlos/internal/tui/format"
)

// The channel chat (docs/super-chat.md) is a Transcript of its own, fed the
// events of every agent: it keeps the human's posts and the agents'
// messages to the human in the order they happen, each message an item of
// its own linked to its agent. Question and permission cards also appear here;
// tool calls and notices stay in the agents' own chats.

// NewChat returns an empty channel chat.
func NewChat() *Transcript {
	t := NewTranscript()
	t.chat = true
	t.names, t.open = map[string]string{}, map[string]bool{}
	return t
}

// ChatEvent reports whether the channel chat reads events of type typ.
func ChatEvent(typ event.Type) bool {
	switch typ {
	case event.AgentSpawned, event.AgentUpdated, event.ChatPosted, event.ChatMessage, event.AgentKilled, event.ChannelUpdated, event.AskRequested, event.AskResolved:
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

// applyChat folds one channel event into the chat.
func (t *Transcript) applyChat(ev event.Event) {
	switch ev.Type {
	case event.ChannelUpdated:
		var p event.ChannelUpdatedPayload
		if ev.Decode(&p) == nil && p.Dir != nil {
			t.appendItem(CleanLines([]Line{{Kind: LineDim, Text: "Default directory → " + *p.Dir + " · mode ask · remembered approvals cleared"}}))
		}
	case event.AgentSpawned:
		var p event.AgentSpawnedPayload
		if ev.Decode(&p) == nil && p.ID != "" {
			t.names[p.ID] = p.Name
		}
	case event.AgentUpdated:
		var p event.AgentUpdatedPayload
		if ev.Decode(&p) == nil && p.Name != nil {
			old := t.names[ev.Agent]
			for _, request := range t.chatRequests {
				if request.names[old] {
					delete(request.names, old)
					request.names[*p.Name] = true
				}
			}
			t.names[ev.Agent] = *p.Name
		}
	case event.AgentKilled:
		delete(t.open, t.names[ev.Agent]) // no reply is coming
		for id := range t.chatRequests {
			t.settlePost(id, t.names[ev.Agent])
		}
	case event.ChatPosted:
		var p event.ChatPayload
		if ev.Decode(&p) == nil {
			to := p.To
			if len(to) == 1 && to[0] == "main" {
				to = nil
			}
			text := addressed(to, p.Text)
			lines := CleanLines(block(BlockUser, "", text))
			for i := range lines {
				if lines[i].Lead {
					// the glyph takes the sender's colour, and every @name its own
					lines[i].Who, lines[i].Names = toolname.User, append([]string(nil), to...)
					break
				}
			}
			t.appendItem(lines)
			t.trackPost(p.ID, p.To, p.Kind == "")
		}
	case event.ChatMessage:
		var p event.ChatPayload
		if ev.Decode(&p) == nil {
			t.reply(ev.Agent, p)
		}
	}
}

// reply adds an agent's message as "‹ @main: …"; user is implicit in channel
// chat, while any co-recipients are retained. Later lines indent under it.
func (t *Transcript) reply(agent string, p event.ChatPayload) {
	from := p.From
	if from == "" {
		from = t.agentName(agent)
	}
	lines := markdownLines(strings.TrimRight(p.Text, "\n"))
	address, names := messageAddress(from, p.To, toolname.User)
	if len(lines) > 0 && (lines[0].Kind == LineText || lines[0].Kind == LineHeading) {
		lines[0].Text = address + " " + lines[0].Text
	} else {
		lines = append([]Line{{Kind: LineText, Text: address}}, lines...)
	}
	lines[0].Glyph, lines[0].Who, lines[0].Names = GlyphReply, from, names
	if p.Kind == "info" {
		lines[0].Glyph = GlyphInfo
	}
	for i := range lines {
		lines[i].Note = true // grey like an aside: only the human's posts keep the text colour
		if i > 0 {
			lines[i].Indent = 1
		}
	}
	lines = append([]Line{{Kind: LineBlank}}, collapsed(lines)...)
	t.appendItem(linked(CleanLines(append(lines, Line{Kind: LineBlank})), agent))
	if p.Kind == "" {
		delete(t.open, from)
		for id, request := range t.chatRequests {
			if request.legacy {
				t.settlePost(id, from)
			}
		}
	} else if p.Kind == "response" {
		for _, id := range p.ReplyTo {
			t.settlePost(id, from)
		}
		for _, id := range p.Posts {
			t.settlePost(id, from)
		}
		if p.Post != "" {
			t.settlePost(p.Post, from)
		}
	}
}

// Waiting lists the agents a post of the human's is still waiting on,
// sorted: until each sends the human a message or is killed. It is empty
// outside the chat.
func (t *Transcript) Waiting() []string {
	seen := map[string]bool{}
	for n := range t.open {
		seen[n] = true
	}
	for _, request := range t.chatRequests {
		for name := range request.names {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
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

// addressed puts the @names a post went to in front of its message, the way
// it was typed: the message itself never carries them.
func addressed(to []string, text string) string {
	if len(to) == 0 {
		return text
	}
	address := "@" + strings.Join(to, " @")
	if text == "" {
		return address
	}
	return address + " " + text
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
