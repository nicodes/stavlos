package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/render"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

type permissionDraft struct {
	sel        int
	edit, text string
}

func (m *Model) permissionVisible(p protocol.PromptInfo) bool {
	return p.Kind == protocol.PromptPermission && p.Agent != "" && (p.Channel == "" || p.Channel == m.channelID) && (m.superChat || p.Agent == m.selectedID())
}

func (m *Model) inlinePermission() *protocol.PromptInfo {
	var first *protocol.PromptInfo
	for i := range m.prompts {
		p := &m.prompts[i]
		if !m.permissionVisible(*p) {
			continue
		}
		if p.ID == m.permFor {
			return p
		}
		if first == nil {
			first = p
		}
	}
	return first
}

func (m *Model) savePermissionDraft() {
	if m.permFor == "" || m.findPrompt(m.permFor) < 0 {
		return
	}
	if m.permissionDrafts == nil {
		m.permissionDrafts = map[string]permissionDraft{}
	}
	d := m.permissionDrafts[m.permFor]
	d.sel = m.permSel
	if m.permEdit != "" {
		d.edit, d.text = m.permEdit, m.dirInput.Value()
	}
	m.permissionDrafts[m.permFor] = d
}

func (m *Model) bindPermission(p *protocol.PromptInfo) {
	if p == nil || p.ID == m.permFor {
		return
	}
	m.savePermissionDraft()
	d := m.permissionDrafts[p.ID]
	m.permFor, m.permSel, m.permEdit = p.ID, d.sel, ""
	m.dirInput.SetValue(d.text)
	m.dirInput.Blur()
}

func (m *Model) permissionExpanded(id string, item int) bool {
	return m.questionExpanded(id, item) || m.focus == focusInlinePermission && m.permFor == id
}

func (m Model) permissionCard(p *protocol.PromptInfo) ([]string, int, int) {
	if p.ID != m.permFor {
		m.permFor, m.permSel, m.permEdit = "", 0, ""
	}
	info := *p
	if info.From == "" {
		if i := m.findAgent(p.Agent); i >= 0 {
			info.From = m.agents[i].Name
		}
	}
	head, _ := render.Lines([]transcript.Line{transcript.PermissionHeader(info, m.superChat)}, render.Options{Width: m.vp.Width, NoFold: true, WhoStyle: m.whoStyle})
	info.Agent = "" // the subject does not repeat the message's sender
	rows, start := m.promptBox(&info, max(1, m.vp.Width-2))
	for i := range rows {
		if rows[i] != "" {
			rows[i] = "  " + rows[i]
		}
	}
	return append(head, rows...), len(head) + start, len(permOptions(p))
}

func (m *Model) permissionCards(cards map[int][]string) {
	if m.loading {
		return
	}
	for i := range m.prompts {
		p := &m.prompts[i]
		if !m.permissionVisible(*p) {
			continue
		}
		t := m.transcript(m.viewID())
		t.EnsurePermission(*p)
		item, _ := t.PermissionItem(p.ID)
		if m.permissionExpanded(p.ID, item) {
			cards[item], _, _ = m.permissionCard(p)
		}
	}
}

func (m *Model) permissionAtItem(item int) *protocol.PromptInfo {
	t := m.transcripts[m.viewID()]
	if t == nil {
		return nil
	}
	for i := range m.prompts {
		p := &m.prompts[i]
		if n, ok := t.PermissionItem(p.ID); ok && n == item && m.permissionVisible(*p) {
			return p
		}
	}
	return nil
}

func (m *Model) openPermission(p *protocol.PromptInfo) tea.Cmd {
	if p == nil || !m.permissionVisible(*p) {
		return nil
	}
	m.bindPermission(p)
	cmd := m.setFocus(focusInlinePermission)
	m.follow = false
	m.refreshViewport()
	m.chatCursor, _ = m.transcript(m.viewID()).PermissionItem(p.ID)
	m.scrollToCursor()
	return cmd
}

func (m *Model) clickPermission(item, row int) (tea.Cmd, bool) {
	p := m.permissionAtItem(item)
	if p == nil {
		return nil, false
	}
	if !m.permissionExpanded(p.ID, item) {
		return nil, true
	}
	_, start, count := m.permissionCard(p)
	rel := row - m.itemRows[item].First - start
	if rel < 0 || rel >= count || m.permFor == p.ID && m.permEdit != "" {
		return nil, true
	}
	chat := m.focus == focusChat
	hover, from, offset := m.hoverFocus, m.hoverFrom, m.vp.YOffset
	cmd := m.openPermission(p)
	m.permSel = rel
	cmd = tea.Batch(cmd, m.permissionKey(tea.KeyMsg{Type: tea.KeySpace}))
	if chat {
		m.hoverFocus, m.hoverFrom = hover, from
		if m.permEdit == "" {
			m.focus = focusChat
		}
		m.follow = false
		m.refreshViewport()
		m.vp.SetYOffset(offset)
	}
	return cmd, true
}

func (m *Model) scrollToPermissionRow() {
	if m.focus != focusInlinePermission {
		return
	}
	p := m.inlinePermission()
	if p == nil {
		return
	}
	item, _ := m.transcript(m.viewID()).PermissionItem(p.ID)
	rows, start, _ := m.permissionCard(p)
	y := m.itemRows[item].First + start + m.permSel
	if m.permEdit != "" {
		y = m.itemRows[item].First + len(rows) - 1
	}
	if y < m.vp.YOffset {
		m.vp.SetYOffset(y)
	}
	if y >= m.vp.YOffset+m.vp.Height {
		m.vp.SetYOffset(y - m.vp.Height + 1)
	}
}
