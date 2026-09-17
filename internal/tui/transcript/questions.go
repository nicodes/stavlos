package transcript

import (
	"reflect"
	"slices"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/toolname"
	"github.com/nicodes/stavlos/internal/tui/format"
)

type questionItem struct {
	item     int
	p        protocol.PromptInfo
	resolved bool
}

// QuestionItem locates the original chat message, including after resolution.
func (t *Transcript) QuestionItem(id string) (int, bool) {
	q, ok := t.questions[id]
	return q.item, ok
}

// EnsureQuestion fills in live prompt data absent from older event logs. A
// notification may precede its event; both update the same item by prompt ID.
func (t *Transcript) EnsureQuestion(p protocol.PromptInfo) {
	if t.questions == nil {
		t.questions = map[string]questionItem{}
	}
	q, ok := t.questions[p.ID]
	if ok && q.resolved {
		return
	}
	if ok {
		if len(p.Questions) == 0 {
			p.Questions = q.p.Questions
		}
		if p.From == "" {
			p.From = q.p.From
		}
		if p.Role == "" {
			p.Role = q.p.Role
		}
		if reflect.DeepEqual(q.p, p) {
			return
		}
		q.p = p
		t.questions[p.ID] = q
		t.replaceItem(q.item, t.questionLines(p, nil))
		return
	}
	q = questionItem{item: len(t.items), p: p}
	t.questions[p.ID] = q
	t.appendItem(t.questionLines(p, nil))
}

func (t *Transcript) applyQuestion(ev event.Event) bool {
	switch ev.Type {
	case event.AskRequested:
		var p event.AskRequestedPayload
		if ev.Decode(&p) != nil || p.Kind != string(protocol.PromptQuestion) {
			return false
		}
		from := p.From
		if from == "" {
			from = t.self
			if t.chat || from == "" {
				from = t.agentName(ev.Agent)
			}
		}
		t.EnsureQuestion(protocol.PromptInfo{ID: p.ID, Agent: ev.Agent, From: from, Role: p.Role, Kind: protocol.PromptQuestion, Question: p.Question, Questions: p.Questions, QuestionNumber: p.QuestionNumber, QuestionTotal: p.QuestionTotal})
		return true
	case event.AskResolved:
		var p event.AskResolvedPayload
		if ev.Decode(&p) != nil {
			return false
		}
		q, ok := t.questions[p.ID]
		if !ok {
			return false
		}
		q.resolved = true
		t.questions[p.ID] = q
		t.replaceItem(q.item, t.questionLines(q.p, &p))
		return true
	default:
		return false
	}
}

func (t *Transcript) questionLines(p protocol.PromptInfo, result *event.AskResolvedPayload) []Line {
	lines := QuestionLines(p, result, t.chat)
	if !t.chat {
		// Agent links identify full channel-chat messages to the renderer.
		// In an agent's own transcript questions fold like its other items.
		linked(lines, "")
	}
	return lines
}

const (
	QuestionUnchecked = "□"
	QuestionChecked   = "■"
)

// QuestionHeader shares the regular message's sender/recipient styling between
// live controls and recorded results.
func QuestionHeader(p protocol.PromptInfo, index int, chat bool) Line {
	from := p.From
	if from == "" {
		from = format.ShortID(p.Agent)
	}
	if from == "" {
		from = "agent"
	}
	question := p.Question
	if index >= 0 && index < len(p.Questions) {
		question = p.Questions[index].Question
	}
	viewer := from
	if chat {
		viewer = toolname.User
	}
	address, names := messageAddress(from, []string{toolname.User}, viewer)
	return Line{Kind: LineText, Glyph: GlyphPrompt, Text: address + " " + question, Who: from, Names: names}
}

// QuestionLines draws the durable card. Live controls are supplied by the TUI;
// completed cards retain their question and checked choices in the same item.
func QuestionLines(p protocol.PromptInfo, result *event.AskResolvedPayload, chat bool) []Line {
	var lines []Line
	qs := p.Questions
	if len(qs) == 0 {
		qs = []protocol.Question{{Question: p.Question}}
	}
	for i, q := range qs {
		lines = append(lines, QuestionHeader(p, i, chat))
		for n, o := range q.Options {
			mark := QuestionUnchecked
			if result != nil && i < len(result.Details) && slices.Contains(result.Details[i].Selected, n) {
				mark = QuestionChecked
			}
			text := mark + " " + o.Label
			if o.Description != "" {
				text += " — " + o.Description
			}
			lines = append(lines, Line{Kind: LineText, Text: text, Indent: 2})
		}
		if result != nil && i < len(result.Details) && strings.TrimSpace(result.Details[i].Custom) != "" {
			lines = append(lines, Line{Kind: LineText, Text: QuestionChecked + " " + result.Details[i].Custom, Indent: 2})
		}
	}
	if result != nil {
		text := "Submitted"
		switch result.Outcome {
		case event.AskWithdrawn:
			text = "Question withdrawn"
		case event.AskDefaulted:
			text = "Defaulted: " + result.Answer
		case event.AskAnswered:
			if len(result.Details) == 0 {
				text = "Answer: " + result.Answer
			}
		}
		lines = append(lines, Line{Kind: LineDim, Text: text, Indent: 1})
		for i := range lines {
			lines[i].Note = true
		}
	} else {
		lines = append(lines, Line{Kind: LineDim, Text: "Waiting for answer", Indent: 1})
	}
	return linked(CleanLines(lines), p.Agent)
}
