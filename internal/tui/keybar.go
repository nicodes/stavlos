package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// keyHint is one entry of the key legend: what to press, what it does.
type keyHint struct{ key, desc string }

// keyHints returns the legend for the current state, most useful first.
func (m Model) keyHints() []keyHint {
	switch {
	case m.ov != nil && m.ov.mode == overlayLogin:
		h := []keyHint{{"o", "open in browser"}, {"esc", "cancel sign-in"}}
		if m.ov.login.err != "" {
			h = append([]keyHint{{"enter", "retry"}}, h...)
		}
		return h
	case m.ov != nil && m.ov.kind == ovModels:
		return []keyHint{{"enter", "set for this agent"}, {"ctrl+s", "set session default"}, {"↑/↓", "move"}, {"type", "filter"}, {"pgup/pgdn", "page"}, {"esc", "close"}}
	case m.ov != nil:
		return []keyHint{{"enter", "select"}, {"↑/↓", "move"}, {"type", "filter"}, {"pgup/pgdn", "page"}, {"esc", "close"}}
	}
	switch m.focus {
	case focusAgents:
		return []keyHint{{"↑/↓", "move"}, {"enter", "select agent"}, {"esc", "close"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusAsync, focusTodo, focusDirs:
		return []keyHint{{"↑/↓", "move"}, {"esc", "close"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusMCP:
		return []keyHint{{"↑/↓", "move"}, {"enter", "show/hide tools"}, {"esc", "close"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusSidebar:
		return []keyHint{{"↑/↓", "move"}, {"enter", "select agent"}, {"tab", "next section"}, {"esc", "back to input"}, {"ctrl+b", "close sidebar"}, {"pgup/pgdn", "scroll"}, {"ctrl+c", "quit"}}
	case focusMeta:
		return []keyHint{{"←/→", "choose"}, {"enter", "open (YOLO: turn off)"}, {"esc", "back to input"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusTabs:
		return []keyHint{{"←/→", "choose"}, {"enter", "open"}, {"esc", "back to input"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusChat:
		return []keyHint{{"↑/↓", "item"}, {"enter", "expand/collapse tool"}, {"pgup/pgdn", "page"}, {"tab", "next section"}, {"esc", "input"}, {"ctrl+c", "quit"}}
	case focusPermission:
		if p := m.currentPrompt(); p != nil {
			switch p.Kind {
			case "trust":
				return []keyHint{{"y", "trust project config"}, {"n", "skip"}, {"tab", "next section"}, {"esc", "close"}, {"ctrl+c", "quit"}}
			case "question":
				return []keyHint{{"type + enter", "answer"}, {"1-9", "pick an option"}, {"tab", "next section"}, {"esc", "close"}, {"ctrl+c", "quit"}}
			default:
				if p.Dir != "" {
					return []keyHint{{"y", "allow once"}, {"a", "allow + add directory"}, {"n", "deny"}, {"tab", "next section"}, {"esc", "close"}, {"ctrl+c", "quit"}}
				}
				return []keyHint{{"y", "allow once"}, {"a", "allow for session"}, {"n", "deny"}, {"tab", "next section"}, {"esc", "close"}, {"ctrl+c", "quit"}}
			}
		}
		return []keyHint{{"esc", "close"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	}
	// Input focus. The tab hint appears only when there is somewhere to go;
	// a waiting permission already shows in the tab strip, so it is not
	// repeated here.
	var tab []keyHint
	if len(m.focusOrder()) > 1 {
		tab = []keyHint{{"tab", "next section"}}
	}
	tree := "show sidebar"
	if m.showTree {
		tree = "hide sidebar"
	}
	if m.isHome() {
		hs := []keyHint{{"enter", "send"}, {"ctrl+j", "newline"}, {"↑/↓", "history"}, {"/", "commands"}}
		hs = append(hs, tab...)
		return append(hs, keyHint{"ctrl+n/p", "agents"}, keyHint{"ctrl+b", tree}, keyHint{"ctrl+c", "quit"})
	}
	hs := []keyHint{{"enter", "send"}, {"ctrl+j", "newline"}, {"↑/↓", "history"}, {"/", "commands"}}
	hs = append(hs, tab...)
	return append(hs, keyHint{"esc esc", "cancel turn"}, keyHint{"ctrl+n/p", "agents"}, keyHint{"pgup/pgdn", "scroll"}, keyHint{"ctrl+b", tree}, keyHint{"ctrl+c", "quit"})
}

// keyBarLines renders hints as "key desc" cells packed into rows of at
// most width columns, at most maxRows rows (later hints are dropped).
func keyBarLines(hints []keyHint, width, maxRows int) []string {
	const sep = "   "
	var rows []string
	var row []string
	rowW := 0
	for _, h := range hints {
		cell := styleKey.Render(h.key) + " " + styleDim.Render(h.desc)
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
	lines := append([]string{styleRule.Render(strings.Repeat("─", m.width))}, rows...)
	return strings.Join(lines, "\n"), len(lines)
}
