package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// wheelRows is how far one notch of the mouse wheel scrolls the chat.
const wheelRows = 3

// chatViewport is the scrolled window onto the chat's rendered rows. It
// keeps the rows as render returns them and draws only those in view, so a
// redraw of a long chat costs its window, not its length: nothing is joined
// into one string, split again or measured.
type chatViewport struct {
	Width, Height int
	YOffset       int // the first row in view
	rows          []string
}

// SetRows replaces the content; an offset past the new end goes to the
// bottom.
func (v *chatViewport) SetRows(rows []string) {
	v.rows = rows
	if v.YOffset > len(rows)-1 {
		v.GotoBottom()
	}
}

func (v chatViewport) maxYOffset() int { return max(0, len(v.rows)-v.Height) }

// AtBottom reports whether the last row is in view.
func (v chatViewport) AtBottom() bool { return v.YOffset >= v.maxYOffset() }

// SetYOffset scrolls to row n, kept within the content.
func (v *chatViewport) SetYOffset(n int) { v.YOffset = min(max(n, 0), v.maxYOffset()) }

func (v *chatViewport) GotoTop()    { v.YOffset = 0 }
func (v *chatViewport) GotoBottom() { v.YOffset = v.maxYOffset() }
func (v *chatViewport) PageUp()     { v.SetYOffset(v.YOffset - v.Height) }
func (v *chatViewport) PageDown()   { v.SetYOffset(v.YOffset + v.Height) }

// Wheel scrolls for a mouse wheel press; other mouse input is ignored (and
// a shifted wheel, which asks for sideways scrolling the chat never does).
func (v *chatViewport) Wheel(msg tea.MouseMsg) {
	if msg.Action != tea.MouseActionPress || msg.Shift {
		return
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		v.SetYOffset(v.YOffset - wheelRows)
	case tea.MouseButtonWheelDown:
		v.SetYOffset(v.YOffset + wheelRows)
	default: // only the wheel scrolls
	}
}

// View is the rows in view, each cut to Width and padded to it, padded with
// blank rows to Height.
func (v chatViewport) View() string {
	if v.Height <= 0 {
		return ""
	}
	blank := strings.Repeat(" ", max(v.Width, 0))
	out := make([]string, v.Height)
	for i := range out {
		r := v.YOffset + i
		if r >= len(v.rows) {
			out[i] = blank
			continue
		}
		row := v.rows[r]
		if w := ansi.StringWidth(row); w > v.Width {
			row = ansi.Truncate(row, v.Width, "")
		} else if w < v.Width {
			row += blank[:v.Width-w]
		}
		out[i] = row
	}
	return strings.Join(out, "\n")
}
