package tui

import (
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
