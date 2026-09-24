package dialog

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestWidthFitsTheBodyWithAFloor: MaxWidth when the body has room, the
// body less a margin otherwise, never under 24.
func TestWidthFitsTheBodyWithAFloor(t *testing.T) {
	for body, want := range map[int]int{200: MaxWidth, MaxWidth + 4: MaxWidth, 50: 46, 28: 24, 10: 24, 0: 24} {
		if got := Width(body); got != want {
			t.Errorf("Width(%d) = %d, want %d", body, got, want)
		}
	}
}

// TestTitleFillsTheWidth: the title on the left, "esc: close" at the right
// edge, exactly width columns; a long title is cut to leave the hint room,
// and a width with no room for both keeps only the title.
func TestTitleFillsTheWidth(t *testing.T) {
	got := ansi.Strip(Title("Models", 40))
	if ansi.StringWidth(got) != 40 || !strings.HasPrefix(got, "Models ") || !strings.HasSuffix(got, "esc: close") {
		t.Errorf("Title: %q", got)
	}
	long := ansi.Strip(Title(strings.Repeat("x", 60), 40))
	if ansi.StringWidth(long) != 40 || !strings.HasSuffix(long, "esc: close") || !strings.Contains(long, "…") {
		t.Errorf("a long title: %q", long)
	}
	// Too narrow for both: the hint goes, and the title is cut to the width
	// (format.Trunc keeps width characters and adds the ellipsis after).
	narrow := ansi.Strip(Title("Models and more", 12))
	if strings.Contains(narrow, "esc") || narrow != "Models and m…" {
		t.Errorf("a narrow title: %q", narrow)
	}
}

// TestHintLinesWrapAndSkipTheSharedKeys: cells go on one line while they
// fit and wrap whole otherwise; esc, tab and ctrl+c are not the dialog's
// to list; no cells means no lines.
func TestHintLinesWrapAndSkipTheSharedKeys(t *testing.T) {
	hints := []Hint{{"esc", "close"}, {"a", "one"}, {"tab", "next"}, {"b", "two"}, {"ctrl+c", "quit"}, {"c", "three"}}
	lines := HintLines(hints, 20)
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	if a, b := ansi.Strip(lines[0]), ansi.Strip(lines[1]); a != "a one · b two" || b != "c three" {
		t.Errorf("wrapped: %q %q", a, b)
	}
	if one := HintLines(hints, 80); len(one) != 1 || ansi.Strip(one[0]) != "a one · b two · c three" {
		t.Errorf("one line: %q", one)
	}
	if got := HintLines([]Hint{{"esc", "close"}, {"tab", "next"}}, 80); got != nil {
		t.Errorf("only shared keys: %q", got)
	}
	// A cell wider than the dialog is cut, not overflowed.
	if got := HintLines([]Hint{{"k", strings.Repeat("d", 40)}}, 20); len(got) != 1 || ansi.StringWidth(got[0]) > 20 {
		t.Errorf("a wide cell: %q", got)
	}
}

// TestCompositeCentersTheBox: the box is laid over the middle of the base,
// the base showing on both sides; a box bigger than the body starts at the
// top-left corner and is cut to the body's height.
func TestCompositeCentersTheBox(t *testing.T) {
	base := strings.Join([]string{"abcdefghij", "0123456789", "ABCDEFGHIJ", "klmnopqrst"}, "\n")
	got := strings.Split(Composite(base, 10, 4, "XX\nYY"), "\n")
	if want := []string{"abcdefghij", "0123XX6789", "ABCDYYGHIJ", "klmnopqrst"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("centered: %q", got)
	}
	// A short base is padded to the body's height before drawing.
	got = strings.Split(Composite("ab", 10, 3, "XX"), "\n")
	if want := []string{"ab", "    XX", ""}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("padded: %q", got)
	}
	// Bigger than the body: from the corner, cut to the height.
	got = strings.Split(Composite("abcd\n0123\nABCD", 4, 2, "XXXXXX\nYYYYYY\nZZZZZZ"), "\n")
	if want := []string{"XXXXXX", "YYYYYY"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("oversized: %q", got)
	}
}
