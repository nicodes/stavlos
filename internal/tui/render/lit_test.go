package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

// TestLitItemReadsLighter: the chat item being read (under the cursor, or
// expanded) draws its text in the light colour whatever it is: an agent's
// grey aside, a user's prompt, tool output, a heading; other items keep their
// own colours.
func TestLitItemReadsLighter(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	code := strings.TrimSuffix(strings.TrimPrefix(sgrPrefix(theme.StyleLit), "\x1b["), "m")
	if code == "" {
		t.Fatal("no colour for the lit style")
	}
	for _, l := range []transcript.Line{
		{Kind: transcript.LineText, Text: "an aside", Note: true},
		{Kind: transcript.LineText, Text: "a prompt", Block: transcript.BlockUser, Lead: true},
		{Kind: transcript.LineToolOut, Text: "output"},
		{Kind: transcript.LineHeading, Text: "Plan", Note: true},
	} {
		if got := renderLine(l, Options{Width: 80}, true); !strings.Contains(got, code) {
			t.Errorf("lit %q should read light: %q", l.Text, got)
		}
		if got := renderLine(l, Options{Width: 80}, false); strings.Contains(got, code) {
			t.Errorf("unlit %q keeps its colour: %q", l.Text, got)
		}
	}
	aside := []transcript.Line{{Kind: transcript.LineText, Text: "an aside", Note: true}}
	if got := firstOf(Lines(aside, Options{Width: 80, NoFold: true, Expanded: map[int]bool{0: true}})); !strings.Contains(got, code) {
		t.Errorf("an expanded item reads light too: %q", got)
	}
	if got := firstOf(Lines(aside, Options{Width: 80, NoFold: true})); strings.Contains(got, code) {
		t.Errorf("an item neither under the cursor nor expanded keeps its grey: %q", got)
	}
}
