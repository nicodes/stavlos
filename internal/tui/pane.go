package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// pane is what is drawn over the chat and answers the keyboard and the mouse
// while it is: the configuration editor, an overlay (a picker, a sign-in, a
// service's controls), or the dialog of a tab. There were three of them, each
// asked about by name in the key handler, the mouse handler, the view, the
// focus rules and Update, in an order every one of those had to repeat.
// Model.pane says which is on top; the rest ask it.
type pane interface {
	// key is a key press. handled false hands it to the focused section's own
	// handler, which is how a tab's dialog works: it is a section with a box
	// drawn round it.
	key(m *Model, msg tea.KeyMsg) (cmd tea.Cmd, handled bool)
	// click is a left click in screen coordinates; hit reports that it landed
	// on something of the pane's.
	click(m *Model, x, y int) (cmd tea.Cmd, hit bool)
	hover(m *Model, x, y int)
	view(m Model, width, height int) string
	// swallows reports that a click that misses the pane goes nowhere, rather
	// than to what is under it.
	swallows() bool
	// wholeScreen reports that the pane takes every key (ctrl+c included) and
	// every mouse event, and that focus rules wait until it closes.
	wholeScreen() bool
}

// pane is the pane on top, or nil. One is open at a time; the order here is
// the only place that says which wins if two ever are.
func (m *Model) pane() pane {
	switch {
	case m.cfgEditor != nil:
		return editorPane{}
	case m.ov != nil:
		return overlayPane{}
	case isTab(m.focus):
		return tabPane{}
	}
	return nil
}

// editorPane is the configuration editor (/settings).
type editorPane struct{}

func (editorPane) key(m *Model, msg tea.KeyMsg) (tea.Cmd, bool) { return m.configEditorKey(msg), true }
func (editorPane) click(m *Model, x, y int) (tea.Cmd, bool)     { return m.configEditorMouse(x, y), true }
func (editorPane) hover(*Model, int, int)                       {}
func (editorPane) view(m Model, w, h int) string                { return m.cfgEditor.view(w, h) }
func (editorPane) swallows() bool                               { return true }
func (editorPane) wholeScreen() bool                            { return true }

// overlayPane is the open overlay. Quit and "back to typing" still work from
// inside it; everything else is the overlay's.
type overlayPane struct{}

func (overlayPane) key(m *Model, msg tea.KeyMsg) (tea.Cmd, bool) {
	if key.Matches(msg, keys.Quit) {
		return m.ctrlC(), true
	}
	m.quitArmed = time.Time{} // any other key disarms the two-step quit
	if key.Matches(msg, keys.FocusInput) {
		return m.closeOverlayToInput(), true // the overlay closes with nothing picked
	}
	return m.overlayKey(msg), true
}

func (overlayPane) click(m *Model, x, y int) (tea.Cmd, bool) {
	if idx, ok := m.ov.itemAt(x, y, m.width, m.bodyHeight(), m.sp.View()); ok {
		m.ov.cursor = idx
		return m.overlaySubmit(false), true
	}
	return nil, false
}

func (overlayPane) hover(m *Model, x, y int) {
	if idx, ok := m.ov.itemAt(x, y, m.width, m.bodyHeight(), m.sp.View()); ok {
		m.ov.cursor = idx
	}
}

func (overlayPane) view(m Model, w, _ int) string {
	m.ov.hints = m.keyHints() // the dialog's own keys, shown whatever the key bar setting
	return m.ov.view(w, m.sp.View())
}
func (overlayPane) swallows() bool    { return true }
func (overlayPane) wholeScreen() bool { return false }

// tabPane is the dialog of a tab (permissions, async, todo, mcp, dirs, a usage
// chart): the focused section, drawn in a box. Its keys are the section's.
type tabPane struct{}

func (tabPane) key(*Model, tea.KeyMsg) (tea.Cmd, bool) { return nil, false }

func (tabPane) click(m *Model, x, y int) (tea.Cmd, bool) {
	if h := m.tabDialogHit(x, y); h.rowOK {
		return m.pickTabRow(h.row, true), true
	}
	return nil, false
}

func (tabPane) hover(m *Model, x, y int) {
	if h := m.tabDialogHit(x, y); h.rowOK {
		m.pickTabRow(h.row, false)
	}
}
func (tabPane) view(m Model, w, _ int) string { return m.tabDialog(w) }
func (tabPane) swallows() bool                { return false }
func (tabPane) wholeScreen() bool             { return false }
