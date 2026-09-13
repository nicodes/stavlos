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
	case m.confirmKill:
		return []keyHint{{"y", "kill agent and subtree"}, {"n", "keep it"}}
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
	case focusSidebar:
		return []keyHint{{"↑/↓", "move"}, {"enter", "select agent"}, {"tab", "next section"}, {"esc", "back to input"}, {"ctrl+b", "close sidebar"}, {"pgup/pgdn", "scroll"}, {"ctrl+c", "quit"}}
	case focusChat:
		return []keyHint{{"↑/↓", "item"}, {"enter", "expand/collapse tool"}, {"pgup/pgdn", "page"}, {"tab", "next section"}, {"esc", "input"}, {"ctrl+c", "quit"}}
	case focusPermission:
		if p := m.currentPrompt(); p != nil {
			switch p.Kind {
			case "trust":
				return []keyHint{{"y", "trust project config"}, {"n", "skip"}, {"tab", "next section"}, {"esc", "input"}, {"ctrl+c", "quit"}}
			case "question":
				return []keyHint{{"type + enter", "answer"}, {"1-9", "pick an option"}, {"tab", "next section"}, {"esc", "input"}, {"ctrl+c", "quit"}}
			default:
				return []keyHint{{"y", "allow once"}, {"a", "allow for session"}, {"n", "deny"}, {"tab", "next section"}, {"esc", "input"}, {"ctrl+c", "quit"}}
			}
		}
	}
	// Input focus. The tab hint appears only when there is somewhere to go.
	var tab []keyHint
	switch {
	case m.currentPrompt() != nil:
		tab = []keyHint{{"tab", "permission"}}
	case len(m.focusOrder()) > 1:
		tab = []keyHint{{"tab", "next section"}}
	}
	tree := "show sidebar"
	if m.showTree {
		tree = "hide sidebar"
	}
	if m.isHome() {
		hs := []keyHint{{"enter", "send"}, {"↑/↓", "history"}, {"/provider", "sign in"}, {"/models", "pick model"}, {"/spawn", "delegate"}}
		hs = append(hs, tab...)
		return append(hs, keyHint{"ctrl+n/p", "agents"}, keyHint{"ctrl+b", tree}, keyHint{"/help", "all commands"}, keyHint{"ctrl+c", "quit"})
	}
	details := "expand tool output"
	if m.details {
		details = "collapse tool output"
	}
	hs := []keyHint{{"enter", "send"}, {"↑/↓", "history"}}
	hs = append(hs, tab...)
	return append(hs, keyHint{"/steer", "redirect mid-turn"}, keyHint{"/cancel", "stop turn"}, keyHint{"/spawn", "delegate"}, keyHint{"/kill", "kill agent"}, keyHint{"ctrl+n/p", "agents"}, keyHint{"pgup/pgdn", "scroll"}, keyHint{"ctrl+b", tree}, keyHint{"/details", details}, keyHint{"/models", "model"}, keyHint{"/help", "all commands"}, keyHint{"ctrl+c", "quit"})
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
	if m.width <= 0 {
		return "", 0
	}
	rows := keyBarLines(m.keyHints(), m.width, 2)
	lines := append([]string{styleRule.Render(strings.Repeat("─", m.width))}, rows...)
	return strings.Join(lines, "\n"), len(lines)
}
