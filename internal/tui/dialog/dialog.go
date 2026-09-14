// Package dialog is the chrome every TUI dialog shares: the width, the
// title line with "esc: close", the wrapped key hints, the bordered box,
// and drawing a box over the view behind it.
package dialog

import (
	"strings"

	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/theme"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// MaxWidth is the widest a dialog box gets.
const MaxWidth = 70

// Hint is one entry of the key legend: what to press, what it does.
type Hint struct{ Key, Desc string }

// Width is the box width every dialog uses: MaxWidth, narrowed to
// fit the body with a margin, never under 24.
func Width(bodyWidth int) int {
	w := MaxWidth
	if w > bodyWidth-4 {
		w = bodyWidth - 4
	}
	if w < 24 {
		w = 24
	}
	return w
}

// Title is the first line of every dialog: the bold title on the
// left, a dim "esc: close" on the right, within width columns.
func Title(title string, width int) string {
	const hint = "esc: close"
	avail := width - len(hint) - 2
	if avail < 4 {
		return theme.StyleOvTitle.Render(format.Trunc(title, width))
	}
	t := format.Trunc(title, avail)
	pad := width - len([]rune(t)) - len(hint)
	return theme.StyleOvTitle.Render(t) + strings.Repeat(" ", pad) + theme.StyleDim.Render(hint)
}

// HintLines is the footer every dialog carries whatever the key bar
// setting: the keys that act inside it, "key desc · key desc", dim,
// wrapped onto as many lines as they need so none is cut. esc, tab and
// ctrl+c are left out (esc is on the title line; the other two are not
// the dialog's own).
func HintLines(hints []Hint, width int) []string {
	var cells []string
	for _, h := range hints {
		switch h.Key {
		case "esc", "tab", "ctrl+c":
			continue
		}
		cells = append(cells, theme.StyleKey.Render(h.Key)+" "+theme.StyleDim.Render(h.Desc))
	}
	if len(cells) == 0 {
		return nil
	}
	sep := theme.StyleDim.Render(" · ")
	var lines []string
	var line string
	for _, c := range cells {
		switch {
		case line == "":
			line = c
		case lipgloss.Width(line)+3+lipgloss.Width(c) <= width:
			line += sep + c
		default:
			lines = append(lines, line)
			line = c
		}
	}
	return append(lines, ansi.Truncate(line, width, "…"))
}

// Composite draws box centered over base (a bodyWidth×bodyHeight block).
func Composite(base string, bodyWidth, bodyHeight int, box string) string {
	baseLines := strings.Split(base, "\n")
	for len(baseLines) < bodyHeight {
		baseLines = append(baseLines, "")
	}
	boxLines := strings.Split(box, "\n")
	bw := lipgloss.Width(box)
	x := (bodyWidth - bw) / 2
	if x < 0 {
		x = 0
	}
	y := (bodyHeight - len(boxLines)) / 2
	if y < 0 {
		y = 0
	}
	for i, bl := range boxLines {
		row := y + i
		if row >= len(baseLines) {
			break
		}
		orig := baseLines[row]
		left := ansi.Truncate(orig, x, "")
		if lw := ansi.StringWidth(left); lw < x {
			left += strings.Repeat(" ", x-lw)
		}
		right := ""
		if ansi.StringWidth(orig) > x+bw {
			right = ansi.Cut(orig, x+bw, bodyWidth)
		}
		baseLines[row] = left + bl + right
	}
	return strings.Join(baseLines[:bodyHeight], "\n")
}

// Box draws lines inside the dialog border, inner columns wide.
func Box(inner int, lines []string) string {
	// Width covers padding but not the border: inner + 2 padding + 2 border.
	return theme.StyleOvBox.Width(inner + 2).Render(strings.Join(lines, "\n"))
}
