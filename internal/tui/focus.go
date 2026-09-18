package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// focus names the UI section that owns the keyboard. The zero value is
// the input so a bare Model starts there.
type focus int

const (
	focusInput            focus = iota // the text input (typing, enter sends)
	focusChat                          // the transcript: a cursor walks its items
	focusPermission                    // the permission tab: pending permission/trust prompts (y/n/a)
	focusQuestions                     // controls inside a question message in the chat
	focusAsync                         // the async tab: what the agent waits on, and the replies it owes (replies.go)
	focusTodo                          // the todo tab: the selected agent's todo list
	focusMCP                           // the mcp tab: the selected agent's MCP servers
	focusDirs                          // the dirs tab: the channel's working directories (every agent's)
	focusSidebar                       // the agent tree (↑/↓ enter)
	focusTabs                          // the tab strip: ←/→ highlight a tab, enter opens its dialog
	focusMeta                          // the meta row under the input: ←/→ pick yolo/role/model/variant, enter opens it
	focusInlinePermission              // permission choices embedded in the chat
	focusUsage                         // a usage dialog: tokens or cost over time (usage.go)
)

// focusOrder lists the sections tab cycles through, top to bottom: the chat
// (once there is one), the tab strip (as one stop), the
// input, and the sidebar (while visible).
func (m *Model) focusOrder() []focus {
	order := make([]focus, 0, 4)
	if !m.isHome() {
		order = append(order, focusChat)
	}
	order = append(order, focusInput)            // top to bottom: under the rule come the input, the strip, the meta row
	if m.stripShown() && len(m.tabOrder()) > 0 { // with the sidebar, the channel chat has no tabs
		order = append(order, focusTabs)
	}
	if len(m.metaParts()) > 0 { // the channel chat's meta row has nothing to pick
		order = append(order, focusMeta)
	}
	if m.sidebarVisible() {
		order = append(order, focusSidebar)
	}
	return order
}

// cycleFocus moves focus delta steps (+1 tab, -1 shift+tab) through
// focusOrder, wrapping around. An open tab dialog counts as the strip.
func (m *Model) cycleFocus(delta int) tea.Cmd {
	order := m.focusOrder()
	cur := m.focus
	if cur == focusQuestions || cur == focusInlinePermission {
		cur = focusChat
	}
	if isTab(cur) {
		cur = focusTabs
	}
	i := 0
	for k, f := range order {
		if f == cur {
			i = k
		}
	}
	n := len(order)
	return m.setFocus(order[((i+delta)%n+n)%n])
}

// closeOverlayToInput drops the overlay and puts the input in focus, wherever
// the overlay was opened from (enter's job).
func (m *Model) closeOverlayToInput() tea.Cmd {
	m.ov = nil
	if m.focus == focusInput {
		return m.input.Focus()
	}
	return m.setFocus(focusInput)
}

// closeDialog leaves an open tab dialog for whatever had focus when it was
// opened (the strip, the input, the chat…), or the input when that is no
// longer a stop. Back on the strip, the closed tab stays highlighted.
func (m *Model) closeDialog() tea.Cmd {
	if m.focus == focusInlinePermission {
		return m.setFocus(focusInput)
	}
	closed := m.focus
	from := m.dialogFrom
	if isTab(from) || !m.focusAvailable(from) {
		from = focusInput
	}
	cmd := m.setFocus(from)
	if from == focusTabs {
		for i, t := range m.tabOrder() {
			if t == closed {
				m.tabSel = i
			}
		}
	}
	return cmd
}

// focusAvailable reports whether f is a stop in the current focus order.
func (m *Model) focusAvailable(f focus) bool {
	for _, g := range m.focusOrder() {
		if g == f {
			return true
		}
	}
	return false
}

// setFocus moves keyboard focus to f. Entering the chat suspends
// auto-scroll and parks the cursor on the last item; leaving it resumes
// following and scrolls to the bottom.
func (m *Model) setFocus(f focus) tea.Cmd {
	if f == m.focus {
		return nil
	}
	prev := m.focus
	m.focus = f
	if prev == focusInlinePermission {
		m.savePermissionDraft()
		m.permEdit = ""
		m.dirInput.Blur()
		m.viewDirty = true
	}
	if prev == focusDirs && f != focusDirs {
		m.dirEdit = "" // leaving the dirs dialog drops a half-typed edit
		m.dirInput.Blur()
	}
	if prev == focusPermission && f != focusPermission {
		m.permEdit = ""
		m.dirInput.Blur()
	}
	if prev == focusQuestions && f != focusQuestions {
		m.saveQuestionDraft()
		m.q.typing = false
		m.promptInput.Blur()
		m.viewDirty = true
	}
	if (prev == focusPermission || prev == focusQuestions) && f != focusPermission && f != focusQuestions {
		m.scope = promptScope{} // the dialog closed: one opened from a tab next shows every channel's
	}
	if isTab(f) && !isTab(prev) {
		m.dialogFrom = prev // a tab dialog opens: remember where to return on close
	}
	m.hoverFocus = false // keyboard focus changes always win over hover
	m.input.Blur()
	m.promptInput.Blur()
	if prev == focusChat {
		m.follow = true
		m.collapseAll()
		m.refreshViewport() // drops the cursor marker and scrolls to the bottom
	}
	switch f {
	case focusInput:
		if prev == focusQuestions || prev == focusInlinePermission {
			m.follow = true
		}
		return m.input.Focus()
	case focusChat:
		m.follow = false
		m.chatCursor = m.chatItems() - 1
		m.refreshViewport()
		m.scrollToCursor()
	case focusQuestions:
		m.bindQuestion(m.currentQuestion())
		m.follow = false
		m.viewDirty = true
	case focusInlinePermission:
		m.bindPermission(m.inlinePermission())
		m.follow = false
		m.viewDirty = true
		if m.permEdit != "" {
			return m.dirInput.Focus()
		}
	case focusSidebar:
		m.sbCursor = m.sidebarIndex(sidebarRow{kind: sbHere})
		if !m.superChat {
			m.sbCursor = m.sidebarIndex(sidebarRow{kind: sbAgent, k: m.selected})
		}
		m.followSidebarCursor()
	case focusAsync, focusTodo, focusMCP, focusDirs:
		m.agCursor = 0
	case focusMeta:
		if parts := m.metaParts(); len(parts) > 0 {
			m.metaSel = parts[0] // always the leftmost part: the role
		}
	case focusTabs:
		m.tabSel = 0 // always the leftmost tab: permission
	}
	return nil
}

// ensureFocus falls back to the input when the focused section is gone
// (prompt answered, sidebar hidden, transcript empty).
func (m *Model) ensureFocus() tea.Cmd {
	if m.cfgEditor != nil {
		return nil
	}
	if m.focus == focusInlinePermission && m.inlinePermission() != nil {
		if m.permEdit != "" && !m.dirInput.Focused() {
			return m.dirInput.Focus()
		}
		return nil
	}
	if m.focus == focusQuestions && m.currentQuestion() != nil {
		return m.syncPromptInput()
	}
	if isTab(m.focus) {
		return m.syncPromptInput() // the strip is always in the order
	}
	for _, f := range m.focusOrder() {
		if f == m.focus {
			return m.syncPromptInput()
		}
	}
	return m.setFocus(focusInput)
}

// syncPromptInput keeps the question field focused only while a question
// is the prompt at the head of the queue and the box has focus (the queue
// may advance onto a question while the box already has focus).
func (m *Model) syncPromptInput() tea.Cmd {
	want := m.focus == focusQuestions && m.currentQuestion() != nil && m.q.typing
	switch {
	case want && !m.promptInput.Focused():
		return m.promptInput.Focus()
	case !want && m.promptInput.Focused():
		m.promptInput.Blur()
	}
	return nil
}
