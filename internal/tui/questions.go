package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/render"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

func (m *Model) questionVisible(p protocol.PromptInfo) bool {
	return p.Kind == protocol.PromptQuestion && (p.Channel == m.channelID || p.Channel == "") && (m.superChat || p.Agent == m.selectedID())
}

func (m *Model) saveQuestionDraft() {
	if m.q.id == "" {
		return
	}
	if m.q.typing {
		m.q.custom = m.promptInput.Value()
	}
	if m.questionDrafts == nil {
		m.questionDrafts = map[string]questionState{}
	}
	q := m.q
	q.typing = false
	m.questionDrafts[q.id] = q
}

func (m *Model) bindQuestion(p *protocol.PromptInfo) {
	if p != nil && p.ID == m.q.id {
		return
	}
	m.saveQuestionDraft()
	m.q.bind(p)
	if p != nil {
		if q, ok := m.questionDrafts[p.ID]; ok {
			m.q = q
		}
	}
	m.promptInput.Blur()
}

// questionCard owns the geometry used both for rendering and hit testing.
// Options and controls occupy one row each; the question itself wraps in full.
func (m Model) questionCard(p *protocol.PromptInfo) ([]string, int, int) {
	if p.ID != m.q.id {
		m.q = m.questionDrafts[p.ID]
		m.q.bind(p)
		m.focus = focusInput
	}
	width := max(1, m.vp.Width)
	rows, start, count := m.questionLines(p, max(1, width-2))
	for i := range rows {
		if rows[i] != "" {
			rows[i] = ansi.Truncate("  "+rows[i], width, "…")
		}
	}
	info := *p
	if p.From == "" {
		if i := m.findAgent(p.Agent); i >= 0 {
			info.From = m.agents[i].Name
		}
	}
	head, _ := render.Lines([]transcript.Line{transcript.QuestionHeader(info, m.q.idx, m.superChat)}, render.Options{Width: width, NoFold: true, WhoStyle: m.whoStyle})
	rows = append(head, rows...)
	if m.promptBusy == p.ID {
		rows = append(rows, ansi.Truncate("  Submitting answer…", width, "…"))
	} else if p.ClaimedBy != "" && !m.claimedByUs[p.ID] {
		rows = append(rows, ansi.Truncate("  Claimed by another client", width, "…"))
	}
	return rows, start + len(head), count
}

func (m *Model) questionCards() map[int][]string {
	cards := map[int][]string{}
	if m.loading {
		return cards
	} // replay establishes chronological positions first
	for i := range m.prompts {
		p := &m.prompts[i]
		if !m.questionVisible(*p) {
			continue
		}
		t := m.transcript(m.viewID())
		t.EnsureQuestion(*p)
		item, _ := t.QuestionItem(p.ID)
		if !m.questionExpanded(p.ID, item) {
			continue
		}
		cards[item], _, _ = m.questionCard(p)
	}
	return cards
}

func (m *Model) questionExpanded(id string, item int) bool {
	return m.superChat || m.details || m.expanded[m.viewID()][item] || m.focus == focusChat && m.chatCursor == item || m.focus == focusQuestions && m.q.id == id
}

func (m *Model) questionAtItem(item int) *protocol.PromptInfo {
	t := m.transcripts[m.viewID()]
	if t == nil {
		return nil
	}
	for i := range m.prompts {
		p := &m.prompts[i]
		if n, ok := t.QuestionItem(p.ID); ok && n == item && m.questionVisible(*p) {
			return p
		}
	}
	return nil
}

func (m *Model) openQuestion(p *protocol.PromptInfo) tea.Cmd {
	if p == nil || !m.questionVisible(*p) {
		return nil
	}
	m.bindQuestion(p)
	cmd := m.setFocus(focusQuestions)
	m.follow = false
	m.refreshViewport()
	m.chatCursor, _ = m.transcript(m.viewID()).QuestionItem(p.ID)
	m.scrollToCursor()
	return cmd
}

func (m *Model) clickQuestion(item, row int) (tea.Cmd, bool) {
	p := m.questionAtItem(item)
	if p == nil {
		return nil, false
	}
	if !m.questionExpanded(p.ID, item) {
		return nil, true // highlighting exposes the controls; the header has no click action
	}
	_, start, count := m.questionCard(p)
	rel := row - m.itemRows[item].First - start
	if rel < 0 || rel >= count {
		return nil, true
	}
	chatFocus := m.focus == focusChat || m.focus == focusQuestions && m.hoverFocus
	hover, from, offset := m.hoverFocus, m.hoverFrom, m.vp.YOffset
	cmd := m.openQuestion(p)
	if m.q.typing {
		m.q.custom = m.promptInput.Value()
		m.q.typing = false
		m.promptInput.Blur()
	}
	m.q.sel = rel
	cmd = tea.Batch(cmd, m.questionsKey(tea.KeyMsg{Type: tea.KeySpace}))
	if chatFocus {
		// Clicking a selector must not pin the card open or jump the viewport.
		// Only the custom text field temporarily takes keyboard focus.
		m.hoverFocus, m.hoverFrom = hover, from
		if !m.q.typing {
			m.focus = focusChat
		}
		m.follow = false
		m.refreshViewport()
		m.vp.SetYOffset(offset)
	}
	return cmd, true
}

// Keep the active control visible even for a card taller than the viewport.
func (m *Model) scrollToQuestionRow() {
	if m.focus != focusQuestions {
		return
	}
	p := m.currentQuestion()
	if p == nil {
		return
	}
	item, ok := m.transcript(m.viewID()).QuestionItem(p.ID)
	if !ok {
		return
	}
	_, start, _ := m.questionCard(p)
	y := m.itemRows[item].First + start + m.q.sel
	if y < m.vp.YOffset {
		m.vp.SetYOffset(y)
	}
	if y >= m.vp.YOffset+m.vp.Height {
		m.vp.SetYOffset(y - m.vp.Height + 1)
	}
}
