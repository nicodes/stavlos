package tui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/tui/theme"
)

// Every list the TUI draws (the sidebar, the tab dialogs, the overlay
// pickers, the palette, the prompt dialogs) moves its cursor, marks it and
// scrolls the same way, with these.

// wrapIndex is i taken around n rows, so one past either end wraps.
func wrapIndex(i, n int) int { return (i%n + n) % n }

// stepCursor applies ↑/↓ to a cursor over n rows, wrapping at both ends;
// with letters, k and j move it too (lists with no text field taking the
// keys). It reports whether the key was a move, even over no rows.
func stepCursor(msg tea.KeyMsg, cur *int, n int, letters bool) bool {
	up := key.Matches(msg, keys.SelUp) || letters && msg.String() == "k"
	down := key.Matches(msg, keys.SelDown) || letters && msg.String() == "j"
	if !up && !down {
		return false
	}
	if n > 0 {
		d := 1
		if up {
			d = -1
		}
		*cur = wrapIndex(*cur+d, n)
	}
	return true
}

// cursorMarker is what a list row starts with: the ▸ marker on the cursor's
// row, an indent as wide on the others.
func cursorMarker(on bool) string {
	if on {
		return theme.StyleOvMarker.Render("▸") + " "
	}
	return "  "
}

// listWindow is the rows [start, end) of n that a window of size rows
// shows: it stays where it was (top) unless that would leave the cursor
// out of view, then moves only as far as it must.
func listWindow(cur, top, n, size int) (start, end int) {
	start = max(0, min(max(top, cur-size+1), cur))
	return start, min(start+size, n)
}
