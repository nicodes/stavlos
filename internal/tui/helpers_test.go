package tui

import (
	"encoding/json"
	"fmt"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/tui/render"
	"github.com/nicodes/stavlos/internal/tui/tuitest"
	"strings"
	"testing"
)

// findLine is the index of the first line containing sub, or -1.
func findLine(lines []string, sub string) int {
	for i, l := range lines {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}

// inOrder reports whether each of subs appears in text on a line below the
// line of the one before it. Tests assert what follows what instead of
// pinning row numbers and line counts that any added hint or blank line
// would shift.
func inOrder(text string, subs ...string) bool {
	lines := strings.Split(text, "\n")
	at := -1
	for _, sub := range subs {
		i := findLine(lines[at+1:], sub)
		if i < 0 {
			return false
		}
		at += 1 + i
	}
	return true
}

func TestInOrder(t *testing.T) {
	text := "title\n\nrow one\nrow two"
	if !inOrder(text, "title", "row one", "row two") || !inOrder(text, "row") {
		t.Fatal("present in order")
	}
	if inOrder(text, "row two", "row one") || inOrder(text, "title", "title") || inOrder(text, "missing") {
		t.Fatal("out of order, repeated on the same line, or missing")
	}
	if findLine([]string{"a", "bc"}, "c") != 1 || findLine(nil, "x") != -1 {
		t.Fatal("findLine")
	}
}

var stripANSI = tuitest.StripANSI

var mk = tuitest.Event

// userMsg is the human's message typed into an agent's chat: queued, then
// taken by a model call (when the chat draws it).
func userMsg(seq int64, agent, text string) []event.Event {
	id := fmt.Sprintf("in%d", seq)
	return []event.Event{
		mk(seq, agent, event.InputQueued, event.Input{ID: id, Kind: event.InputPrompt, Text: text}),
		mk(seq, agent, event.InputTaken, event.InputTakenPayload{IDs: []string{id}}),
	}
}

// toolCall is an assistant message calling one tool, then its tool.started:
// the call's input travels in the assistant message.
func toolCall(seq int64, agent, id, name, input string) []event.Event {
	return []event.Event{
		mk(seq, agent, event.AssistantMessage, event.AssistantMessagePayload{Blocks: []model.Block{{Type: model.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}}}),
		mk(seq, agent, event.ToolStarted, event.ToolStartedPayload{CallID: id, Name: name}),
	}
}

// feed applies event sequences in order.
func feed(apply func(event.Event), seqs ...[]event.Event) {
	for _, evs := range seqs {
		for _, e := range evs {
			apply(e)
		}
	}
}

// markCursorForTest swaps the (background colour) cursor highlight for a
// visible gutter mark so assertions can see which rows carry the cursor.
func markCursorForTest(t *testing.T) {
	t.Helper()
	prev := render.SwapHighlight(func(s string, _ int) string { return render.GutterMark + s })
	t.Cleanup(func() { render.SwapHighlight(prev) })
}

// navAfterClients is the first header row after the Clients section and its
// blank: where the Subscriptions section starts when there is a reading, and
// where the monitors start when there is none. Tests find a row by what it is
// (navRowIndex), or relative to a section, never by a number.
func navAfterClients(m Model) int { return m.navRowIndex(navDiscord) + 2 }
