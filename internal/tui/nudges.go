package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/theme"
)

// The nudges tab: what the selected agent owes a reply to, and what the
// harness is doing about it. A turn that ends with requests unanswered and
// nothing to wait on is followed by a reminder; after nudgeLimit reminders
// in a row the harness leaves the agent alone until something new arrives
// (docs/reply-tracking.md).

// nudgeCount is the tab's count: how many replies the selected agent owes.
func (m *Model) nudgeCount() int {
	if a := m.selectedAgent(); a != nil {
		return len(a.PendingReplies)
	}
	return 0
}

// nudgeState is the line under the list: whether a reminder is coming, and
// how many have gone unanswered.
func nudgeState(a protocol.AgentInfo, waiting bool) string {
	switch {
	case len(a.PendingReplies) == 0:
		return ""
	case a.NudgeLimit == 0:
		return "reminders are off: nothing will nudge this agent"
	case a.Nudges >= a.NudgeLimit:
		return fmt.Sprintf("%d reminders went unanswered: no more until something new arrives", a.Nudges)
	case waiting:
		return fmt.Sprintf("waiting on an answer or a job: no reminder until that lands (%d of %d used)", a.Nudges, a.NudgeLimit)
	}
	return fmt.Sprintf("a reminder follows a turn that ends owing these (%d of %d used)", a.Nudges, a.NudgeLimit)
}

// nudgeRows are the tab's body: a row per request owed, oldest first, and
// the state line under them.
func (m *Model) nudgeRows(width int) ([]string, []int) {
	a := m.selectedAgent()
	if a == nil || len(a.PendingReplies) == 0 {
		return []string{theme.StyleDim.Render("  nothing owed: no reminder is coming")}, []int{-1}
	}
	var rows []string
	for _, r := range a.PendingReplies {
		name := r.FromName
		if name == "" {
			name = r.From
		}
		rows = append(rows, ansi.Truncate("  @"+name+": "+strings.Join(strings.Fields(r.Text), " "), width, "…"))
	}
	lines, indices := m.cursorRows(rows), make([]int, len(rows))
	for i := range indices {
		indices[i] = i
	}
	if state := nudgeState(*a, len(a.Awaiting) > 0 || len(m.runningJobs()) > 0); state != "" {
		lines = append(lines, "", theme.StyleDim.Render(state))
		indices = append(indices, -1, -1)
	}
	return lines, indices
}

// nudgeKey handles keys while the nudges dialog is open: ↑/↓ move over the
// requests, space or enter opens the chat of whoever is waiting, esc closes.
func (m *Model) nudgeKey(msg tea.KeyMsg) tea.Cmd {
	a := m.selectedAgent()
	n := 0
	if a != nil {
		n = len(a.PendingReplies)
	}
	if key.Matches(msg, keys.Select) && n > 0 {
		r := a.PendingReplies[m.agCursor%n]
		if r.From == "user" {
			return tea.Batch(m.openChat(), m.setFocus(focusInput))
		}
		if i := m.findAgent(r.From); i >= 0 {
			m.openAgent(i)
			return m.setFocus(focusInput)
		}
		return nil
	}
	return m.listKey(msg, n)
}
