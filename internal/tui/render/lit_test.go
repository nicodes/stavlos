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
// expanded) draws its grey text in the text colour the human's posts have: an
// agent's aside, tool output, a heading; other items keep their grey.
func TestLitItemReadsLighter(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	grey := strings.TrimSuffix(strings.TrimPrefix(sgrPrefix(theme.StyleDim), "\x1b["), "m")
	if grey == "" {
		t.Fatal("no colour for the dim style")
	}
	for _, l := range []transcript.Line{
		{Kind: transcript.LineText, Text: "an aside", Note: true},
		{Kind: transcript.LineToolOut, Text: "output"},
		{Kind: transcript.LineHeading, Text: "Plan", Note: true},
	} {
		if got := renderLine(l, Options{Width: 80}, true); strings.Contains(got, grey) {
			t.Errorf("lit %q should read in the text colour: %q", l.Text, got)
		}
		if got := renderLine(l, Options{Width: 80}, false); !strings.Contains(got, grey) {
			t.Errorf("unlit %q keeps its grey: %q", l.Text, got)
		}
	}
	aside := []transcript.Line{{Kind: transcript.LineText, Text: "an aside", Note: true}}
	if got := firstOf(linesText(aside, Options{Width: 80, NoFold: true, Expanded: map[int]bool{0: true}})); strings.Contains(got, grey) {
		t.Errorf("an expanded item reads in the text colour too: %q", got)
	}
	if got := firstOf(linesText(aside, Options{Width: 80, NoFold: true})); !strings.Contains(got, grey) {
		t.Errorf("an item neither under the cursor nor expanded keeps its grey: %q", got)
	}
}
