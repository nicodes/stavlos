package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tui/render"
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

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func mk(seq int64, agent string, typ event.Type, payload any) event.Event {
	return event.Event{Seq: seq, Channel: "s1", Agent: agent, Type: typ, Payload: event.MustPayload(payload)}
}

// markCursorForTest swaps the (background colour) cursor highlight for a
// visible gutter mark so assertions can see which rows carry the cursor.
func markCursorForTest(t *testing.T) {
	t.Helper()
	prev := render.SwapHighlight(func(s string, _ int) string { return render.GutterMark + s })
	t.Cleanup(func() { render.SwapHighlight(prev) })
}
