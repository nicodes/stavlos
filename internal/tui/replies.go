package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/theme"
)

// The async tab is both halves of the selected agent's waiting: what it
// waits on (agents it asked, running jobs) and what it owes a reply to,
// with the line under them saying what the harness will do about the
// second half (docs/reply-tracking.md).

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
	for _, row := range jobRows(m.runningJobs(), a.Name, a.Role, clock(), width) {
		rows = append(rows, row)
		targets = append(targets, "")
	}
	waiting = len(rows)
	for _, r := range a.PendingReplies {
		add(r)
	}
	return
}

// owedCount is how many replies the selected agent owes.
func (m *Model) owedCount() int {
	if a := m.selectedAgent(); a != nil {
		return len(a.PendingReplies)
	}
	return 0
}

// nudgeState is the line under the list: whether a reminder is coming, and
// how many have gone unanswered.
func nudgeState(a protocol.AgentInfo, jobWaiting bool) string {
	switch {
	case len(a.PendingReplies) == 0:
		return ""
	case a.NudgeLimit == 0:
		return "reminders are off: nothing will nudge this agent"
	case a.Nudges >= a.NudgeLimit:
		return fmt.Sprintf("%d empty reminder turns: no more until a tool, a reply, or a new request", a.Nudges)
	case jobWaiting:
		return fmt.Sprintf("waiting on a job: no reminder until that lands (%d of %d used)", a.Nudges, a.NudgeLimit)
	}
	return fmt.Sprintf("a reminder follows a turn that ends owing these (%d of %d used)", a.Nudges, a.NudgeLimit)
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
	if a := m.selectedAgent(); a != nil {
		if state := nudgeState(*a, len(m.runningJobs()) > 0); state != "" {
			lines = append(lines, "", theme.StyleDim.Render(state))
			indices = append(indices, -1, -1)
		}
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
