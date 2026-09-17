package tui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"strings"
	"time"
)

func (m *Model) hasReplyDetails() bool {
	a := m.selectedAgent()
	return a != nil && (len(a.PendingReplies) > 0 || len(a.AwaitingReplies) > 0)
}

func (m *Model) replyRows(width int) (rows, targets []string, waiting int) {
	a := m.selectedAgent()
	if a == nil {
		return
	}
	add := func(r event.ReplyRequest) {
		name := r.FromName
		if name == "" {
			name = r.From
		}
		if r.From == "user" {
			name = "user"
		}
		rows = append(rows, ansi.Truncate("  "+r.ID+" @"+name+": "+strings.Join(strings.Fields(r.Text), " "), width, "…"))
		targets = append(targets, r.From)
	}
	for _, r := range a.AwaitingReplies {
		add(r)
	}
	for _, row := range jobRows(m.runningJobs(), a.Name, a.Role, time.Now(), width) {
		rows = append(rows, row)
		targets = append(targets, "")
	}
	waiting = len(rows)
	for _, r := range a.PendingReplies {
		add(r)
	}
	return
}

func (m *Model) replyBodyRows(width int) ([]string, []int) {
	rows, _, waiting := m.replyRows(width - 2)
	rows = m.cursorRows(rows)
	var lines []string
	var indices []int
	for i, row := range rows {
		if i == 0 && waiting > 0 {
			lines = append(lines, theme.StyleDim.Render("waiting for responses"))
			indices = append(indices, -1)
		}
		if i == waiting {
			lines = append(lines, theme.StyleDim.Render("requests needing responses"))
			indices = append(indices, -1)
		}
		lines = append(lines, row)
		indices = append(indices, i)
	}
	return lines, indices
}

func (m *Model) replyKey(msg tea.KeyMsg) tea.Cmd {
	_, targets, _ := m.replyRows(m.width)
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case stepCursor(msg, &m.agCursor, len(targets), true):
	case key.Matches(msg, keys.Select):
		if len(targets) == 0 {
			return nil
		}
		target := targets[m.agCursor%len(targets)]
		if target == "" {
			return nil
		}
		if target == "user" {
			m.openChat()
		} else {
			m.openAgent(m.findAgent(target))
		}
		return m.closeDialog()
	}
	return nil
}
