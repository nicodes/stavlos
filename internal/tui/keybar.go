package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/nicodes/stavlos/internal/tui/dialog"
	"github.com/nicodes/stavlos/internal/tui/theme"
)

// hint is one key bar entry: what to press, what it does.
func hint(key, desc string) dialog.Hint { return dialog.Hint{Key: key, Desc: desc} }

// keyHints returns the legend for the current state, most useful first.
// listDialogHints are the tab dialogs whose keys are a list's: the same
// hints but for what space does, so keyHints keeps one case each for the
// dialogs that differ.
var listDialogHints = map[focus]func() []dialog.Hint{
	focusAsync: func() []dialog.Hint { return listHints(hint("space/enter", "open chat")) },
	focusMCP:   func() []dialog.Hint { return listHints(hint("space/enter", "show/hide tools")) },
	focusTodo:  func() []dialog.Hint { return listHints() },
}

// listHints is ↑/↓ over a list, whatever select does there, and the keys
// out of the dialog.
func listHints(sel ...dialog.Hint) []dialog.Hint {
	h := append([]dialog.Hint{hint("↑/↓", "move")}, sel...)
	return append(h, hint("ctrl+space", "input"), hint("esc", "close"), hint("tab", "next section"), hint("ctrl+c", "quit"))
}

func (m Model) keyHints() []dialog.Hint {
	switch {
	case m.ov != nil && m.ov.mode == overlayLogin:
		if m.ov.login.key {
			return []dialog.Hint{hint("enter", "sign in"), hint("esc", "cancel sign-in")}
		}
		h := []dialog.Hint{hint("o", "open in browser"), hint("esc", "cancel sign-in")}
		if m.ov.login.err != "" {
			h = append([]dialog.Hint{hint("enter", "retry")}, h...)
		}
		return h
	case m.ov != nil && m.ov.kind == ovModels:
		return []dialog.Hint{hint("space/enter", "set for this agent"), hint("ctrl+s", "set channel default"), hint("↑/↓", "move"), hint("type", "filter"), hint("pgup/pgdn", "page"), hint("ctrl+space", "input"), hint("esc", "close")}
	case m.ov != nil && m.ov.mode == overlayInput:
		return []dialog.Hint{hint("enter", "create"), hint("esc", "cancel")}
	case m.ov != nil:
		return []dialog.Hint{hint("space/enter", "select"), hint("↑/↓", "move"), hint("type", "filter"), hint("pgup/pgdn", "page"), hint("ctrl+space", "input"), hint("esc", "close")}
	}
	if h, ok := listDialogHints[m.focus]; ok {
		return h()
	}
	switch m.focus {
	case focusUsage:
		return usageDialogHints()
	case focusDirs:
		if m.dirEdit != "" {
			return []dialog.Hint{hint("enter", "save"), hint("esc", "cancel"), hint("ctrl+c", "quit")}
		}
		return []dialog.Hint{hint("↑/↓", "move"), hint("a", "add directory"), hint("space/enter", "edit"), hint("ctrl+d", "remove"), hint("ctrl+space", "input"), hint("esc", "close"), hint("tab", "next section"), hint("ctrl+c", "quit")}
	case focusSidebar:
		return []dialog.Hint{hint("↑/↓", "move"), hint("space/enter", "open"), hint("n", "next agent needing you"), hint("←", "fold tree"), hint("→", "channel dirs"), hint("ctrl+space", "input"), hint("tab", "next section"), hint("esc", "back to input"), hint("ctrl+b", "close sidebar"), hint("pgup/pgdn", "scroll"), hint("ctrl+c", "quit")}
	case focusMeta:
		return []dialog.Hint{hint("←/→", "choose"), hint("space/enter", "open"), hint("ctrl+space", "input"), hint("esc", "back to input"), hint("tab", "next section"), hint("ctrl+c", "quit")}
	case focusTabs:
		return []dialog.Hint{hint("←/→", "choose"), hint("space/enter", "open"), hint("ctrl+space", "input"), hint("esc", "back to input"), hint("tab", "next section"), hint("ctrl+c", "quit")}
	case focusChat:
		return []dialog.Hint{hint("↑/↓", "item"), hint("space/enter", "expand/collapse"), hint("pgup/pgdn", "page"), hint("tab", "next section"), hint("ctrl+space", "input"), hint("esc", "input"), hint("ctrl+c", "quit")}
	case focusQuestions:
		if m.q.typing {
			return []dialog.Hint{hint("enter", "save custom answer"), hint("esc", "back to the options"), hint("ctrl+c", "quit")}
		}
		if p := m.currentQuestion(); p != nil && p.QuestionNumber > 0 {
			return []dialog.Hint{hint("↑/↓", "option"), hint("space", "toggle"), hint("enter", "submit answer"), hint("type", "something else"), hint("esc", "close"), hint("tab", "next section"), hint("ctrl+c", "quit")}
		}
		return []dialog.Hint{hint("↑/↓", "option"), hint("space", "toggle"), hint("enter", "confirm · next"), hint("←/→", "question"), hint("type", "something else"), hint("esc", "close"), hint("tab", "next section"), hint("ctrl+c", "quit")}
	case focusPermission, focusInlinePermission:
		if p := m.currentPrompt(); p != nil {
			switch m.permEdit {
			case "deny":
				return []dialog.Hint{hint("enter", "deny"), hint("esc", "cancel"), hint("ctrl+c", "quit")}
			case "dir":
				return []dialog.Hint{hint("enter", "allow + add this directory"), hint("esc", "cancel"), hint("ctrl+c", "quit")}
			}
			return []dialog.Hint{hint("↑/↓", "option"), hint("space/enter", "choose"), hint("ctrl+space", "input"), hint("tab", "next section"), hint("esc", "close"), hint("ctrl+c", "quit")}
		}
		return []dialog.Hint{hint("esc", "close"), hint("tab", "next section"), hint("ctrl+c", "quit")}
	}
	// Input focus. The tab hint appears only when there is somewhere to go;
	// a waiting permission already shows in the tab strip, so it is not
	// repeated here.
	var tab []dialog.Hint
	if len(m.focusOrder()) > 1 {
		tab = []dialog.Hint{hint("tab", "next section")}
	}
	tree := "show sidebar"
	if m.showTree {
		tree = "hide sidebar"
	}
	if m.isHome() {
		hs := []dialog.Hint{hint("enter", "send"), hint("ctrl+j", "newline"), hint("↑/↓", "history"), hint("/", "commands")}
		hs = append(hs, tab...)
		return append(hs, hint("ctrl+n/p", "agents"), hint("ctrl+b", tree), hint("ctrl+c", "quit"))
	}
	hs := []dialog.Hint{hint("enter", "send"), hint("ctrl+j", "newline"), hint("↑/↓", "history"), hint("/", "commands")}
	hs = append(hs, tab...)
	return append(hs, hint("esc esc", "cancel turn"), hint("ctrl+n/p", "agents"), hint("pgup/pgdn", "scroll"), hint("ctrl+b", tree), hint("ctrl+c", "quit"))
}

// keyBarLines renders hints as "key desc" cells packed into rows of at
// most width columns, at most maxRows rows (later hints are dropped).
func keyBarLines(hints []dialog.Hint, width, maxRows int) []string {
	const sep = "   "
	var rows []string
	var row []string
	rowW := 0
	for _, h := range hints {
		cell := theme.StyleKey.Render(h.Key) + " " + theme.StyleDim.Render(h.Desc)
		cw := lipgloss.Width(cell)
		if rowW > 0 && rowW+len(sep)+cw > width {
			rows = append(rows, strings.Join(row, sep))
			if len(rows) == maxRows {
				return rows
			}
			row, rowW = nil, 0
		}
		row = append(row, cell)
		if rowW > 0 {
			rowW += len(sep)
		}
		rowW += cw
	}
	if len(row) > 0 && len(rows) < maxRows {
		rows = append(rows, strings.Join(row, sep))
	}
	return rows
}

// keyBarView is the divider plus legend rows, and how many lines it takes.
func (m Model) keyBarView() (string, int) {
	if m.width <= 0 || m.hideKeys {
		return "", 0
	}
	rows := keyBarLines(m.keyHints(), m.width, 2)
	lines := append([]string{theme.StyleRule.Render(strings.Repeat("─", m.width))}, rows...)
	return strings.Join(lines, "\n"), len(lines)
}
