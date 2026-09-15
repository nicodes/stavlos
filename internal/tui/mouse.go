package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/nicodes/stavlos/internal/tui/theme"
)

// selection is a mouse text selection: pressed while the button is down,
// active once the pointer has moved (a drag), kept highlighted after the
// release until the next press.
type selection struct {
	pressed, active bool
	ax, ay, bx, by  int // anchor (press) and pointer (latest drag) positions
}

// mouse routes mouse events: a left press anchors a possible selection, a
// drag extends and highlights it, and the release either copies the
// selection or, when nothing was dragged, counts as a click. Plain motion
// is hover.
func (m *Model) mouse(msg tea.MouseMsg) tea.Cmd {
	// Hover and clicks work in chat-column coordinates on the chat rows; the
	// sidebar, when shown, occupies the left edge beside the chat only. The
	// rows from the rule down span the window.
	cx, inMain := m.mainX(msg.X, msg.Y)
	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		m.sel = selection{pressed: true, ax: msg.X, ay: msg.Y, bx: msg.X, by: msg.Y}
		return nil
	case msg.Action == tea.MouseActionMotion && m.sel.pressed:
		if msg.X != m.sel.ax || msg.Y != m.sel.ay || m.sel.active {
			m.sel.active = true
			m.sel.bx, m.sel.by = msg.X, msg.Y
		}
		return nil
	case msg.Action == tea.MouseActionRelease && m.sel.pressed:
		m.sel.pressed = false
		if !m.sel.active {
			// A dialog (an overlay or the tab dialog) is hit-tested in
			// screen coordinates first; the tab dialog lets a miss fall
			// through to whatever is under the pointer, an overlay does not.
			if cmd, hit := m.dialogClick(msg.X, msg.Y); hit || m.ov != nil {
				return cmd
			}
			if !inMain {
				return m.sidebarClick(msg.X, msg.Y)
			}
			return m.mouseClick(cx, msg.Y)
		}
		text := m.selectedText()
		if text == "" {
			m.sel.active = false
			return nil
		}
		return tea.Batch(copyCmd(text), m.setStatus(fmt.Sprintf("copied %d characters", len([]rune(text))), false))
	case msg.Action == tea.MouseActionMotion:
		if m.ov != nil || isTab(m.focus) {
			m.dialogHover(msg.X, msg.Y) // a dialog owns hover; the chat behind it is left alone
			return nil
		}
		if !inMain {
			return m.mouseHover(-1, msg.Y) // outside the chat: hover releases, nothing else
		}
		return m.mouseHover(cx, msg.Y)
	}
	return nil
}

// dialogClick is a click while a dialog is open, in screen coordinates: on
// an overlay row it selects and submits; on a tab dialog row it moves the
// cursor (and, for agents, selects the agent like enter). hit reports
// whether the click landed on something.
func (m *Model) dialogClick(x, y int) (tea.Cmd, bool) {
	if m.ov != nil {
		if idx, ok := m.ov.itemAt(x, y, m.width, m.bodyHeight(), m.sp.View()); ok {
			m.ov.cursor = idx
			return m.overlaySubmit(false), true
		}
		return nil, false
	}
	if !isTab(m.focus) {
		return nil, false
	}
	if h := m.tabDialogHit(x, y); h.rowOK {
		return m.pickTabRow(h.row, true), true
	}
	return nil, false
}

// dialogHover moves a dialog's cursor to the row under the pointer.
func (m *Model) dialogHover(x, y int) {
	if m.ov != nil {
		if idx, ok := m.ov.itemAt(x, y, m.width, m.bodyHeight(), m.sp.View()); ok {
			m.ov.cursor = idx
		}
		return
	}
	if h := m.tabDialogHit(x, y); h.rowOK {
		m.pickTabRow(h.row, false)
	}
}

// mainX maps a screen column to the chat column for the rows the sidebar
// shares (the chat and the status line): with the sidebar shown the chat
// starts after it and its separator. inMain is false over the sidebar.
// Lower rows span the window and are returned as they are.
func (m *Model) mainX(x, y int) (int, bool) {
	if !m.sidebarVisible() || y > m.vp.Height {
		return x, true
	}
	off := sidebarWidth + 2 // separator + gap
	if x < off {
		return x, false
	}
	return x - off, true
}

// selRange is the selection in reading order: (y0,x0) before (y1,x1),
// columns inclusive.
func (s selection) selRange() (x0, y0, x1, y1 int) {
	x0, y0, x1, y1 = s.ax, s.ay, s.bx, s.by
	if y1 < y0 || (y1 == y0 && x1 < x0) {
		x0, y0, x1, y1 = x1, y1, x0, y0
	}
	return
}

// selectedText is the plain text under the selection, in terminal order:
// the first row from the anchor column, whole rows in between, the last
// row up to the pointer column. Trailing spaces are trimmed per row.
func (m Model) selectedText() string {
	if !m.sel.active {
		return ""
	}
	saved := m.sel
	m.sel = selection{} // render the frame without the highlight
	lines := strings.Split(m.View(), "\n")
	m.sel = saved
	x0, y0, x1, y1 := m.sel.selRange()
	var out []string
	for y := y0; y <= y1 && y < len(lines); y++ {
		if y < 0 {
			continue
		}
		plain := ansi.Strip(lines[y])
		from, to := 0, ansi.StringWidth(plain)
		if y == y0 {
			from = x0
		}
		if y == y1 {
			to = x1 + 1
		}
		if from < 0 {
			from = 0
		}
		if to > ansi.StringWidth(plain) {
			to = ansi.StringWidth(plain)
		}
		if to <= from {
			out = append(out, "")
			continue
		}
		out = append(out, strings.TrimRight(ansi.Cut(plain, from, to), " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// highlightSelection paints the selection onto a rendered frame: the
// selected span of each row is shown in reverse video.
func (m Model) highlightSelection(frame string) string {
	if !m.sel.active {
		return frame
	}
	x0, y0, x1, y1 := m.sel.selRange()
	lines := strings.Split(frame, "\n")
	for y := y0; y <= y1 && y < len(lines); y++ {
		if y < 0 {
			continue
		}
		line := lines[y]
		w := ansi.StringWidth(line)
		from, to := 0, w
		if y == y0 {
			from = x0
		}
		if y == y1 {
			to = x1 + 1
		}
		if from >= w || to <= from {
			continue
		}
		if to > w {
			to = w
		}
		left := ansi.Cut(line, 0, from)
		mid := ansi.Strip(ansi.Cut(line, from, to))
		right := ""
		if to < w {
			right = ansi.Cut(line, to, w)
		}
		lines[y] = left + theme.StyleSelection.Render(mid) + right
	}
	return strings.Join(lines, "\n")
}

// mouseHover is mouse movement: over a chat item it does what ↑/↓ do (the
// chat takes focus, the cursor moves to that item, it previews); leaving
// the chat area gives focus back to the input, without scrolling. Focus the
// keyboard took is left alone.
func (m *Model) mouseHover(x, y int) tea.Cmd {
	if m.isHome() {
		return nil
	}
	inChat := x >= 0 && x < m.contentWidth() && y >= 0 && y < m.vp.Height
	if !inChat {
		if m.hoverFocus && m.focus == focusChat {
			// Give focus back to wherever hover took it from, without
			// scrolling the chat.
			m.hoverFocus = false
			m.focus = m.hoverFrom
			m.collapseAll()
			m.refreshViewport()
			m.follow = m.vp.AtBottom()
			if m.focus == focusInput {
				return m.input.Focus()
			}
			return m.syncPromptInput()
		}
		return nil
	}
	item, ok := m.itemAtRow(m.vp.YOffset + y)
	if !ok {
		return nil
	}
	if m.focus == focusChat && m.chatCursor == item {
		return nil
	}
	if m.focus != focusChat {
		m.hoverFocus, m.hoverFrom = true, m.focus
		m.focus = focusChat
		m.input.Blur()
		m.promptInput.Blur()
		m.follow = false
	}
	if m.chatCursor != item {
		m.collapseAll() // per-visit expansion, as with the arrow keys
	}
	m.chatCursor = item
	m.refreshViewport()
	return nil
}

// mouseClick is a left click: on a chat item it does what enter does on
// the hovered item (the click first moves the cursor there, like hover).
func (m *Model) mouseClick(x, y int) tea.Cmd {
	if m.isHome() {
		return m.setFocus(focusInput) // the input is the only thing to click on the logo screen
	}
	if x < 0 || y < 0 {
		return nil
	}
	if y < m.vp.Height && x >= m.contentWidth() {
		return nil // right of the chat column
	}
	lay := m.rows()
	switch {
	case y < m.vp.Height: // the chat: select, and toggle like enter
		item, ok := m.itemAtRow(m.vp.YOffset + y)
		if !ok {
			return nil
		}
		cmd := m.mouseHover(x, y)
		if m.focus == focusChat && m.chatCursor == item {
			m.toggleItem()
		}
		return cmd
	case y >= lay.strip && y < lay.strip+lay.stripRows: // a tab label opens that tab's dialog
		if f, ok := m.tabAt(x, y-lay.strip); ok {
			return m.openTab(f)
		}
	case y >= lay.input && y < lay.input+m.inputRows(): // the input lines; the mode tag before the › is a button
		if y == lay.input && x < modeTagCols-1 {
			return m.metaAction(metaMode)
		}
		return m.setFocus(focusInput)
	case y == lay.rule: // the divider: the agent's role, model and variant are buttons, and so are its tabs
		if f, ok := m.metaTabAt(x, m.width); ok {
			return m.openTab(f)
		}
		if part := m.metaHit(x); part != metaNone {
			return m.metaAction(part)
		}
	}
	return nil
}

// metaHit maps a column of the divider to the role, model or variant drawn
// there.
func (m *Model) metaHit(x int) metaPart {
	d := m.divider(m.width)
	if part, ok := hitSpan(d.metaSpans, x-d.metaX); ok {
		return part
	}
	return metaNone
}

// rowLayout is where the channel view's pieces sit, in screen rows: the
// divider, then the palette (while open), the input, a blank line and the
// strip.
type rowLayout struct {
	rule      int // the divider
	input     int // first row of the input (it may span several)
	strip     int // the tab strip's first line
	stripRows int // how many lines the strip takes
}

// rows derives the row layout the same way channelView stacks its parts.
func (m *Model) rows() rowLayout {
	f := m.computeFrame()
	return rowLayout{rule: f.rule, input: f.input, strip: f.strip, stripRows: f.stripRows}
}

// itemAtRow maps a viewport content row to the chat item drawn there.
func (m *Model) itemAtRow(row int) (int, bool) {
	for item, r := range m.itemRows {
		if row >= r.First && row <= r.Last {
			return item, true
		}
	}
	return 0, false
}
