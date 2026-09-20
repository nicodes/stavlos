package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/tui/render"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

// tail is how much history to ask for: a first replay takes the recent part,
// one that continues from a seq already held (a channel visited before, a
// reconnect) or that /history asked for takes everything after it.
func (m *Model) tail() int {
	if m.wholeLog || m.seq > 0 {
		return 0
	}
	return historyTail
}

// viewCommand runs the commands that change what is shown and nothing else.
func (m *Model) viewCommand(name string) tea.Cmd {
	if name == "/history" {
		return m.loadHistory()
	}
	return m.toggleTree()
}

// onSubscribed is the reply to subscribe, which arrives once the replay has.
func (m *Model) onSubscribed(msg subscribedMsg) ([]tea.Cmd, bool) {
	if !m.accepts(msg.scope) {
		return nil, false
	}
	if msg.err != nil {
		m.fatal = fmt.Errorf("subscribe: %w", msg.err)
		return nil, true
	}
	if m.historyFrom = msg.first; msg.first > 1 {
		return []tea.Cmd{m.setStatus("showing recent history · /history loads all of it", false)}, false
	}
	return nil, false
}

// loadHistory is /history: what the replay built is dropped and the channel
// is replayed from its first event.
func (m *Model) loadHistory() tea.Cmd {
	if m.historyFrom <= 1 {
		return m.setStatus("all of this channel's history is loaded", false)
	}
	m.generation++ // what is still arriving from the tail's subscription is for a state that is gone
	m.wholeLog, m.historyFrom, m.awaitFirst = true, 0, true
	m.seq, m.loading, m.reconciled = 0, true, false
	m.spawned, m.parentOf = map[string]time.Time{}, map[string]string{}
	m.transcripts, m.renders = map[string]*transcript.Transcript{}, map[string]*render.Cache{}
	m.refreshViewport()
	return tea.Batch(reconcileCmd(m.ctx, m.c, m.requestScope()), m.setStatus("loading history…", false))
}
