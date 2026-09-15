package transcript

import (
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
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
	t.names, t.posts, t.open = map[string]string{}, map[string]int{}, map[string]int{}
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
			t.names[p.ID] = p.Label
		}
	case event.AgentRoleChanged:
		var p event.RoleChangedPayload
		if ev.Decode(&p) == nil && p.Label != "" {
			t.names[ev.Agent] = p.Label
		}
	case event.ChatPosted:
		var p event.ChatPayload
		if ev.Decode(&p) == nil {
			t.post(p)
		}
	case event.MessageToUser:
		var p event.ChatPayload
		if ev.Decode(&p) == nil {
			// Reads like an agent's reply in its own chat, with the
			// sender's @name before it.
			from := p.From
			if from == "" {
				from = t.agentName(ev.Agent)
			}
			lines := markdownLines(strings.TrimRight(p.Text, "\n"))
			if len(lines) > 0 && (lines[0].Kind == LineText || lines[0].Kind == LineHeading) {
				lines[0].Text = "@" + from + " " + lines[0].Text
			} else {
				lines = append([]Line{{Kind: LineText, Text: "@" + from}}, lines...)
			}
			lines = collapsed(lines)
			lines = linked(CleanLines(append(lines, Line{Kind: LineBlank})), ev.Agent)
			// A reply joins the thread of the post it answers, indented under
			// it, wherever that post is; a message with no known post stands
			// on its own at the end.
			if item, ok := t.posts[p.Post]; ok {
				for i := range lines {
					lines[i].Indent = 1
				}
				t.insertIntoItem(item, lines)
				if t.open[from] == item {
					delete(t.open, from) // answered: the next post to it starts a new thread
				}
				return
			}
			t.appendItem(append([]Line{{Kind: LineBlank, Agent: ev.Agent}}, lines...))
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

// post adds the human's post to the chat. A post to agents that are all
// still waiting to reply in the same thread joins that thread, since one
// reply will cover both; otherwise it starts a thread of its own. Either
// way its agents now wait to reply in its thread.
func (t *Transcript) post(p event.ChatPayload) {
	lines := CleanLines(block(BlockUser, "", addressed(p.To, p.Text)))
	item, grouped := -1, len(p.To) > 0
	for _, n := range p.To {
		open, ok := t.open[n]
		if !ok || item >= 0 && open != item {
			grouped = false
			break
		}
		item = open
	}
	if grouped {
		t.insertIntoItem(item, lines[1:]) // no blank line between grouped posts
	} else {
		refs := t.appendItem(lines)
		if len(refs) == 0 {
			return
		}
		item = refs[0].item
	}
	if p.ID != "" {
		t.posts[p.ID] = item
	}
	for _, n := range p.To {
		t.open[n] = item
	}
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
	return append(lines, Line{Kind: LineDim, Text: fmt.Sprintf("… +%d lines", len(lines)-MaxOutputCollapsed), Vis: VisCollapsed})
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
