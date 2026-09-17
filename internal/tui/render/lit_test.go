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

func TestStableTextColorKeepsCursorBackground(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })
	old := SwapHighlight(func(s string, _ int) string { return GutterMark + s })
	t.Cleanup(func() { SwapHighlight(old) })
	for _, line := range []transcript.Line{
		{Kind: transcript.LineText, Text: "agent reply", Note: true},
		{Kind: transcript.LineToolOut, Text: "tool output"},
		{Kind: transcript.LineHeading, Text: "heading", Note: true},
	} {
		base := Options{Width: 80, NoFold: true, KeepTextColor: true}
		plain := firstOf(linesText([]transcript.Line{line}, base))
		base.Focused = true
		focused := firstOf(linesText([]transcript.Line{line}, base))
		if focused != GutterMark+plain {
			t.Fatalf("focus changed text styling: %q -> %q", plain, focused)
		}
		base.Focused, base.Expanded = false, map[int]bool{0: true}
		if expanded := firstOf(linesText([]transcript.Line{line}, base)); expanded != plain {
			t.Fatalf("expansion changed text styling: %q -> %q", plain, expanded)
		}
	}
}

func TestSenderPrefixIsGreyAndRecipientsKeepTheirColors(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })
	color := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000"))
	line := transcript.Line{Kind: transcript.LineText, Text: "@scout: @main @reader @scout is literal message text", Who: "scout", Names: []string{"scout", "main", "reader"}}
	got := renderLine(line, Options{Width: 100, WhoStyle: func(string) lipgloss.Style { return color }}, false)
	if !strings.Contains(got, theme.StyleDim.Render("@scout:")) || strings.Contains(got, color.Bold(true).Render("@scout:")) {
		t.Fatalf("sender prefix is not grey: %q", got)
	}
	for _, name := range []string{"@main", "@reader"} {
		if !strings.Contains(got, color.Bold(true).Render(name)) {
			t.Fatalf("recipient color lost: %q", got)
		}
	}
	if strings.Contains(got, color.Bold(true).Render("@scout")) {
		t.Fatal("message-body mention was mistaken for another recipient")
	}
}
