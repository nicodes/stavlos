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
	focusAsync: func() []dialog.Hint { return listHints(hint(keyLabel(keys.Select), "open chat")) },
	focusMCP:   func() []dialog.Hint { return listHints(hint(keyLabel(keys.Select), "show/hide tools")) },
	focusTodo:  func() []dialog.Hint { return listHints() },
}

// listHints is ↑/↓ over a list, whatever select does there, and the keys
// out of the dialog.
func listHints(sel ...dialog.Hint) []dialog.Hint {
	h := append([]dialog.Hint{hint(keyLabel(keys.SelUp, keys.SelDown), "move")}, sel...)
	return append(h, hint(keyLabel(keys.FocusInput), "input"), hint(keyLabel(keys.Clear), "close"), hint(keyLabel(keys.NextSection), "next section"), hint(keyLabel(keys.Quit), "quit"))
}

func (m Model) keyHints() []dialog.Hint {
	switch {
	case m.ov != nil && m.ov.mode == overlayLogin:
		if m.ov.login.key {
			return []dialog.Hint{hint(keyLabel(keys.Submit), "sign in"), hint(keyLabel(keys.Clear), "cancel sign-in")}
		}
		h := []dialog.Hint{hint(keyLabel(keys.OvOpen), "open in browser"), hint(keyLabel(keys.Clear), "cancel sign-in")}
		if m.ov.login.err != "" {
			h = append([]dialog.Hint{hint(keyLabel(keys.Submit), "retry")}, h...)
		}
		return h
	case m.ov != nil && m.ov.kind == ovModels:
		return []dialog.Hint{hint(keyLabel(keys.Select), "set for this agent"), hint(keyLabel(keys.OvAlt), "set channel default"), hint(keyLabel(keys.SelUp, keys.SelDown), "move"), hint("type", "filter"), hint(keyLabel(keys.PageUp, keys.PageDown), "page"), hint(keyLabel(keys.FocusInput), "input"), hint(keyLabel(keys.Clear), "close")}
	case m.ov != nil && m.ov.mode == overlayInput:
		return []dialog.Hint{hint(keyLabel(keys.Submit), "create"), hint(keyLabel(keys.Clear), "cancel")}
	case m.ov != nil:
		return []dialog.Hint{hint(keyLabel(keys.Select), "select"), hint(keyLabel(keys.SelUp, keys.SelDown), "move"), hint("type", "filter"), hint(keyLabel(keys.PageUp, keys.PageDown), "page"), hint(keyLabel(keys.FocusInput), "input"), hint(keyLabel(keys.Clear), "close")}
	}
	if h, ok := listDialogHints[m.focus]; ok {
		return h()
	}
	switch m.focus {
	case focusUsage:
		return usageDialogHints()
	case focusDirs:
		if m.dirEdit != "" {
			return []dialog.Hint{hint(keyLabel(keys.Submit), "save"), hint(keyLabel(keys.Clear), "cancel"), hint(keyLabel(keys.Quit), "quit")}
		}
		return []dialog.Hint{hint(keyLabel(keys.SelUp, keys.SelDown), "move"), hint(keyLabel(keys.AddDir), "add directory"), hint(keyLabel(keys.Select), "edit"), hint(keyLabel(keys.OvRemove), "remove"), hint(keyLabel(keys.FocusInput), "input"), hint(keyLabel(keys.Clear), "close"), hint(keyLabel(keys.NextSection), "next section"), hint(keyLabel(keys.Quit), "quit")}
	case focusSidebar:
		return []dialog.Hint{hint(keyLabel(keys.SelUp, keys.SelDown), "move"), hint(keyLabel(keys.Select), "open"), hint(keyLabel(keys.NextWaiting), "next agent needing you"), hint(keyLabel(keys.TabLeft), "fold tree"), hint(keyLabel(keys.TabRight), "channel dirs"), hint(keyLabel(keys.FocusInput), "input"), hint(keyLabel(keys.NextSection), "next section"), hint(keyLabel(keys.Clear), "back to input"), hint(keyLabel(keys.ToggleTree), "close sidebar"), hint(keyLabel(keys.PageUp, keys.PageDown), "scroll"), hint(keyLabel(keys.Quit), "quit")}
	case focusMeta:
		return []dialog.Hint{hint(keyLabel(keys.TabLeft, keys.TabRight), "choose"), hint(keyLabel(keys.Select), "open"), hint(keyLabel(keys.FocusInput), "input"), hint(keyLabel(keys.Clear), "back to input"), hint(keyLabel(keys.NextSection), "next section"), hint(keyLabel(keys.Quit), "quit")}
	case focusTabs:
		return []dialog.Hint{hint(keyLabel(keys.TabLeft, keys.TabRight), "choose"), hint(keyLabel(keys.Select), "open"), hint(keyLabel(keys.FocusInput), "input"), hint(keyLabel(keys.Clear), "back to input"), hint(keyLabel(keys.NextSection), "next section"), hint(keyLabel(keys.Quit), "quit")}
	case focusChat:
		return []dialog.Hint{hint(keyLabel(keys.SelUp, keys.SelDown), "item"), hint(keyLabel(keys.Select), "expand/collapse"), hint(keyLabel(keys.PageUp, keys.PageDown), "page"), hint(keyLabel(keys.NextSection), "next section"), hint(keyLabel(keys.FocusInput), "input"), hint(keyLabel(keys.Clear), "input"), hint(keyLabel(keys.Quit), "quit")}
	case focusQuestions:
		if m.q.typing {
			return []dialog.Hint{hint(keyLabel(keys.Submit), "save custom answer"), hint(keyLabel(keys.Clear), "back to the options"), hint(keyLabel(keys.Quit), "quit")}
		}
		if p := m.currentQuestion(); p != nil && p.QuestionNumber > 0 {
			return []dialog.Hint{hint(keyLabel(keys.SelUp, keys.SelDown), "option"), hint("space", "toggle"), hint(keyLabel(keys.Submit), "submit answer"), hint("type", "something else"), hint(keyLabel(keys.Clear), "close"), hint(keyLabel(keys.NextSection), "next section"), hint(keyLabel(keys.Quit), "quit")}
		}
		return []dialog.Hint{hint(keyLabel(keys.SelUp, keys.SelDown), "option"), hint("space", "toggle"), hint(keyLabel(keys.Submit), "confirm · next"), hint(keyLabel(keys.TabLeft, keys.TabRight), "question"), hint("type", "something else"), hint(keyLabel(keys.Clear), "close"), hint(keyLabel(keys.NextSection), "next section"), hint(keyLabel(keys.Quit), "quit")}
	case focusPermission, focusInlinePermission:
		if p := m.currentPrompt(); p != nil {
			switch m.permEdit {
			case "deny":
				return []dialog.Hint{hint(keyLabel(keys.Submit), "deny"), hint(keyLabel(keys.Clear), "cancel"), hint(keyLabel(keys.Quit), "quit")}
			case "dir":
				return []dialog.Hint{hint(keyLabel(keys.Submit), "allow + add this directory"), hint(keyLabel(keys.Clear), "cancel"), hint(keyLabel(keys.Quit), "quit")}
			}
			return []dialog.Hint{hint(keyLabel(keys.SelUp, keys.SelDown), "option"), hint(keyLabel(keys.Select), "choose"), hint(keyLabel(keys.FocusInput), "input"), hint(keyLabel(keys.NextSection), "next section"), hint(keyLabel(keys.Clear), "close"), hint(keyLabel(keys.Quit), "quit")}
		}
		return []dialog.Hint{hint(keyLabel(keys.Clear), "close"), hint(keyLabel(keys.NextSection), "next section"), hint(keyLabel(keys.Quit), "quit")}
	}
	// Input focus. The tab hint appears only when there is somewhere to go;
	// a waiting permission already shows in the tab strip, so it is not
	// repeated here.
	var tab []dialog.Hint
	if len(m.focusOrder()) > 1 {
		tab = []dialog.Hint{hint(keyLabel(keys.NextSection), "next section")}
	}
	tree := "show sidebar"
	if m.showTree {
		tree = "hide sidebar"
	}
	if m.isHome() {
		hs := []dialog.Hint{hint(keyLabel(keys.Submit), "send"), hint("ctrl+j", "newline"), hint(keyLabel(keys.SelUp, keys.SelDown), "history"), hint("/", "commands")}
		hs = append(hs, tab...)
		return append(hs, hint(keyLabel(keys.NextAgent, keys.PrevAgent), "agents"), hint(keyLabel(keys.ToggleTree), tree), hint(keyLabel(keys.Quit), "quit"))
	}
	hs := []dialog.Hint{hint(keyLabel(keys.Submit), "send"), hint("ctrl+j", "newline"), hint(keyLabel(keys.SelUp, keys.SelDown), "history"), hint("/", "commands")}
	hs = append(hs, tab...)
	return append(hs, hint("esc esc", "cancel turn"), hint(keyLabel(keys.NextAgent, keys.PrevAgent), "agents"), hint(keyLabel(keys.PageUp, keys.PageDown), "scroll"), hint(keyLabel(keys.ToggleTree), tree), hint(keyLabel(keys.Quit), "quit"))
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
