package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/render"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

// selectionChanged re-renders after the selected agent changed: follow the
// new transcript, or, while the chat has focus, park the cursor on its
// last item.
func (m *Model) selectionChanged() {
	if m.focus == focusQuestions || m.focus == focusInlinePermission {
		m.setFocus(focusInput)
	}
	m.collapseAll()
	if m.focus == focusChat {
		m.follow = false
		m.chatCursor = m.chatItems() - 1
		m.refreshViewport()
		m.scrollToCursor()
		return
	}
	m.follow = true
	m.refreshViewport()
}

// chatItems is the item count of the selected transcript.
func (m *Model) chatItems() int {
	if t := m.transcripts[m.viewID()]; t != nil {
		return t.Items()
	}
	return 0
}

// agentExpanded is the per-item tool output override map of agent id.
func (m *Model) agentExpanded(id string) map[int]bool {
	if m.expanded == nil {
		m.expanded = map[string]map[int]bool{}
	}
	e := m.expanded[id]
	if e == nil {
		e = map[int]bool{}
		m.expanded[id] = e
	}
	return e
}

// chatKey handles keys while the transcript has focus: ↑/↓ (j/k) move the
// cursor one item, pgup/pgdn a page of items, home/end to the ends; enter
// toggles a tool item's output; esc returns to the input.
func (m *Model) chatKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, keys.Clear):
		return m.setFocus(focusInput)
	case key.Matches(msg, keys.SelUp), msg.String() == "k":
		m.moveCursor(-1)
	case key.Matches(msg, keys.SelDown), msg.String() == "j":
		m.moveCursor(1)
	case key.Matches(msg, keys.PageUp):
		m.moveCursor(-chatPage)
	case key.Matches(msg, keys.PageDown):
		m.moveCursor(chatPage)
	case key.Matches(msg, keys.ChatTop):
		m.moveCursor(-m.chatItems())
	case key.Matches(msg, keys.ChatBottom):
		m.moveCursor(m.chatItems())
	case key.Matches(msg, keys.Select):
		if p := m.permissionAtItem(m.chatCursor); p != nil {
			return m.openPermission(p)
		}
		if p := m.questionAtItem(m.chatCursor); p != nil {
			return m.openQuestion(p)
		}
		// A long reply expands; a short one opens its agent's own chat.
		if m.chatItemFolds() || !m.followChatLink() {
			m.toggleItem()
		}
	}
	return nil
}

// moveCursor moves the chat cursor by delta items, clamped, and scrolls
// the viewport so the item is visible.
func (m *Model) moveCursor(delta int) {
	n := m.chatItems()
	if n == 0 {
		return
	}
	was := m.chatCursor
	m.chatCursor += delta
	if m.chatCursor < 0 {
		m.chatCursor = 0
	}
	if m.chatCursor >= n {
		m.chatCursor = n - 1
	}
	if m.chatCursor != was {
		m.collapseAll() // expansion is per visit: leaving an item folds it
	}
	m.refreshViewport()
	m.scrollToCursor()
}

// collapseAll drops every expand override so each item is back to its
// one-line fold (or the preview under the cursor).
func (m *Model) collapseAll() { m.expanded = nil }

// scrollToCursor sets the viewport offset so the cursor item is fully
// visible (its top when it is taller than the viewport).
func (m *Model) scrollToCursor() {
	r, ok := m.itemRows[m.chatCursor]
	if !ok {
		return
	}
	h := m.vp.Height
	if r.Last+2 == len(m.vp.rows) {
		r.Last++ // the last item brings the blank row under it into view
	}
	switch {
	case r.Last-r.First+1 > h || r.First < m.vp.YOffset:
		m.vp.SetYOffset(r.First)
	case r.Last >= m.vp.YOffset+h:
		m.vp.SetYOffset(r.Last - h + 1)
	}
}

// toggleItem flips the cursor item between expanded and collapsed (a
// per-item override of /details): in an agent's own chat any item but the
// human's input, in the channel chat a long reply.
func (m *Model) toggleItem() {
	t := m.transcripts[m.viewID()]
	if t == nil || m.superChat && !transcript.ItemFolds(t.All(), m.chatCursor) || !m.superChat && transcript.ItemIsInput(t.All(), m.chatCursor) {
		return
	}
	e := m.agentExpanded(m.viewID())
	cur, ok := e[m.chatCursor]
	if !ok {
		cur = m.details
	}
	e[m.chatCursor] = !cur
	m.refreshViewport()
	m.scrollToCursor()
}

func (m *Model) transcript(id string) *transcript.Transcript {
	t := m.transcripts[id]
	if t == nil {
		t = transcript.NewTranscript()
		if id == chatView {
			t = transcript.NewChat()
		}
		m.transcripts[id] = t
	}
	return t
}

// chatView keys the channel chat in transcripts, renders and expanded; it
// is never an agent id.
const chatView = "#chat"

// viewID is what the chat area shows: the channel chat, or the selected
// agent's own transcript.
func (m *Model) viewID() string {
	if m.superChat {
		return chatView
	}
	return m.selectedID()
}

// openChat shows the channel chat, where typing posts to the channel.
func (m *Model) openChat() tea.Cmd {
	if !m.superChat {
		m.superChat = true
		m.layout() // the footer drops the agent's tabs
		m.selectionChanged()
	}
	m.input.Placeholder = m.placeholder()
	return nil
}

// openAgent selects agent i and shows its own chat, where typing messages
// that agent alone.
func (m *Model) openAgent(i int) {
	if i < 0 || i >= len(m.agents) || i == m.selected && !m.superChat {
		return
	}
	m.selected, m.superChat = i, false
	m.input.Placeholder = m.placeholder()
	m.layout() // the footer gets the agent's tabs back
	m.selectionChanged()
}

// followChatLink opens the agent the channel chat's cursor item links to
// (its message, its prompt). It reports whether there was one.
func (m *Model) followChatLink() bool {
	t := m.transcripts[chatView]
	if !m.superChat || t == nil {
		return false
	}
	i := m.findAgent(transcript.ItemAgent(t.All(), m.chatCursor))
	if i < 0 {
		return false
	}
	m.openAgent(i)
	return true
}

// chatItemFolds reports whether the item under the chat cursor expands and
// collapses (tool output, a long reply).
func (m *Model) chatItemFolds() bool {
	t := m.transcripts[m.viewID()]
	return t != nil && transcript.ItemFolds(t.All(), m.chatCursor)
}

// refreshViewport re-renders the selected transcript into the viewport,
// marking the cursor item while the chat has focus.
func (m *Model) refreshViewport() {
	m.viewDirty = false
	cards := m.questionCards()
	m.permissionCards(cards)
	t := m.transcripts[m.viewID()]
	n := 0
	if t != nil {
		n = t.Items()
	}
	if m.chatCursor >= n {
		m.chatCursor = n - 1
	}
	if m.chatCursor < 0 {
		m.chatCursor = 0
	}
	working, waiting, verb, stats, active := false, false, "", "", ""
	if t != nil && t.InTurn() {
		working, verb = true, t.TurnVerb()
		stats = render.TurnStats(t.TurnStats(clock()))
		active = m.activeTodo()
	}
	// Between turns an agent waiting on other agents or on its jobs is still
	// busy: the indicator keeps its spinner and names what it waits on.
	if t != nil && !working && !m.superChat {
		if on, ok := m.waitingOn(); ok {
			working, verb, active = true, "Waiting", on
		}
	}
	for _, p := range m.prompts {
		if !m.superChat && p.Agent == m.selectedID() {
			waiting = true
			break
		}
	}
	// The channel chat's loader is the turn indicator at its bottom, naming
	// the agents a post is still waiting on.
	if t != nil && m.superChat {
		if names := t.Waiting(); len(names) > 0 {
			working, verb = true, transcript.TurnVerbs[t.Items()%len(transcript.TurnVerbs)]
			active = "@" + strings.Join(names, " @")
		}
	}
	opts := render.Options{
		Cards:         cards,
		Width:         m.vp.Width,
		Details:       m.details,
		Spinner:       m.sp.View(),
		Working:       working,
		Waiting:       waiting,
		Verb:          verb,
		Active:        active,
		Stats:         stats,
		Expanded:      m.expanded[m.viewID()],
		TurnGaps:      !m.superChat,
		WhoStyle:      m.whoStyle,
		Stamps:        true,
		Now:           clock(),
		WhoKey:        m.whoKey(),
		Cursor:        m.chatCursor,
		Focused:       m.focus == focusChat,
		KeepTextColor: true, // focus, hover and expansion change the background, never the text colour

		CompactFrame: render.CompactFrame(clock()),
	}
	var lines []string
	var rows map[int]render.RowRange
	if t != nil {
		lines, rows = render.Transcript(t, m.chatCache(m.viewID()), opts) // unchanged items come from the cache
	} else {
		lines, rows = render.Lines(nil, opts)
	}
	m.itemRows = rows
	if len(lines) > 0 {
		// a blank row under the last message (or the loader), so the chat
		// never rides on the divider
		lines = append(lines, "")
	}
	m.vp.SetRows(lines)
	if m.follow {
		m.vp.GotoBottom()
	}
}

// chatCache is the render cache of agent id's transcript.
func (m *Model) chatCache(id string) *render.Cache {
	c := m.renders[id]
	if c == nil {
		c = &render.Cache{}
		m.renders[id] = c
	}
	return c
}

// waitingOn is what the selected agent waits on between turns, as the
// indicator names it: the agents whose answer it expects, then its running
// jobs ("@scout · 1 job"); ok is false unless the daemon reports it waiting
// (a busy agent has its turn indicator instead).
func (m *Model) waitingOn() (string, bool) {
	if a := m.selectedAgent(); a == nil || a.State != protocol.AgentWaiting {
		return "", false
	}
	var parts []string
	var names []string
	for _, a := range m.awaitedAgents() {
		names = append(names, "@"+a.Name)
	}
	if len(names) > 0 {
		parts = append(parts, strings.Join(names, " "))
	}
	switch n := len(m.runningJobs()); {
	case n == 1:
		parts = append(parts, "1 job")
	case n > 1:
		parts = append(parts, fmt.Sprintf("%d jobs", n))
	}
	return strings.Join(parts, " · "), len(parts) > 0
}
