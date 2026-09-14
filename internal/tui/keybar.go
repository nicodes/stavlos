package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
		return []keyHint{{"space", "set for this agent"}, {"ctrl+s", "set session default"}, {"↑/↓", "move"}, {"type", "filter"}, {"pgup/pgdn", "page"}, {"enter", "input"}, {"esc", "close"}}
	case m.ov != nil:
		return []keyHint{{"space", "select"}, {"↑/↓", "move"}, {"type", "filter"}, {"pgup/pgdn", "page"}, {"enter", "input"}, {"esc", "close"}}
	}
	switch m.focus {
	case focusAgents:
		return []keyHint{{"↑/↓", "move"}, {"space", "select agent"}, {"enter", "input"}, {"esc", "close"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusAsync, focusTodo:
		return []keyHint{{"↑/↓", "move"}, {"enter", "input"}, {"esc", "close"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusDirs:
		if m.dirEdit != "" {
			return []keyHint{{"enter", "save"}, {"esc", "cancel"}, {"ctrl+c", "quit"}}
		}
		return []keyHint{{"↑/↓", "move"}, {"a", "add directory"}, {"space", "edit"}, {"ctrl+d", "remove"}, {"enter", "input"}, {"esc", "close"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusMCP:
		return []keyHint{{"↑/↓", "move"}, {"space", "show/hide tools"}, {"enter", "input"}, {"esc", "close"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusSidebar:
		return []keyHint{{"↑/↓", "move"}, {"space", "select agent"}, {"enter", "input"}, {"tab", "next section"}, {"esc", "back to input"}, {"ctrl+b", "close sidebar"}, {"pgup/pgdn", "scroll"}, {"ctrl+c", "quit"}}
	case focusMeta:
		return []keyHint{{"←/→", "choose"}, {"space", "open (mode tag: back to ask)"}, {"enter", "input"}, {"esc", "back to input"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusTabs:
		return []keyHint{{"←/→", "choose"}, {"space", "open"}, {"enter", "input"}, {"esc", "back to input"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusChat:
		return []keyHint{{"↑/↓", "item"}, {"space", "expand/collapse tool"}, {"pgup/pgdn", "page"}, {"tab", "next section"}, {"enter", "input"}, {"esc", "input"}, {"ctrl+c", "quit"}}
	case focusQuestions:
		if m.q.typing {
			return []keyHint{{"enter", "answer"}, {"esc", "back to the options"}, {"ctrl+c", "quit"}}
		}
		return []keyHint{{"↑/↓", "option"}, {"space", "toggle"}, {"enter", "confirm · next"}, {"←/→", "question"}, {"type", "something else"}, {"esc", "close"}, {"tab", "next section"}, {"ctrl+c", "quit"}}
	case focusPermission:
		if p := m.currentPrompt(); p != nil {
			switch p.Kind {
			case "trust":
				return []keyHint{{"y", "trust project config"}, {"n", "skip"}, {"enter", "input"}, {"tab", "next section"}, {"esc", "close"}, {"ctrl+c", "quit"}}
			default:
				if m.promptDeny {
					return []keyHint{{"enter", "deny"}, {"esc", "cancel"}, {"ctrl+c", "quit"}}
				}
				if p.Dir != "" {
					if m.promptDir {
						return []keyHint{{"enter", "allow + add this directory"}, {"esc", "cancel"}, {"ctrl+c", "quit"}}
					}
					return []keyHint{{"y", "allow once"}, {"a", "allow + add directory"}, {"e", "edit the directory"}, {"n", "deny"}, {"enter", "input"}, {"tab", "next section"}, {"esc", "close"}, {"ctrl+c", "quit"}}
				}
				return []keyHint{{"y", "allow once"}, {"a", "allow for session"}, {"n", "deny"}, {"enter", "input"}, {"tab", "next section"}, {"esc", "close"}, {"ctrl+c", "quit"}}
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

// dialogHintLines is the footer every dialog carries whatever the key bar
// setting: the keys that act inside it, "key desc · key desc", dim,
// wrapped onto as many lines as they need so none is cut. esc, tab and
// ctrl+c are left out (esc is on the title line; the other two are not
// the dialog's own).
func dialogHintLines(hints []keyHint, width int) []string {
	var cells []string
	for _, h := range hints {
		switch h.key {
		case "esc", "tab", "ctrl+c":
			continue
		}
		cells = append(cells, styleKey.Render(h.key)+" "+styleDim.Render(h.desc))
	}
	if len(cells) == 0 {
		return nil
	}
	sep := styleDim.Render(" · ")
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
