package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
)

func (m *Model) findAgent(id string) int {
	for i, a := range m.agents {
		if a.ID == id {
			return i
		}
	}
	return -1
}

func (m *Model) selectedID() string {
	if m.selected < 0 || m.selected >= len(m.agents) {
		return ""
	}
	return m.agents[m.selected].ID
}

func (m *Model) selectedAgent() *protocol.AgentInfo {
	if m.selected < 0 || m.selected >= len(m.agents) {
		return nil
	}
	return &m.agents[m.selected]
}

func (m *Model) agentLabel(id string) string {
	if i := m.findAgent(id); i >= 0 {
		return m.agents[i].Name
	}
	return id
}

// setAgents replaces the tree, keeping the selection on the same agent.
func (m *Model) setAgents(agents []protocol.AgentInfo) {
	agents = cleanAgents(agents)
	prev := m.selectedID()
	m.agents = agents
	if i := m.findAgent(prev); i >= 0 {
		m.selected = i
	} else {
		m.selected = 0
	}
	if prev != m.selectedID() {
		m.selectionChanged()
	}
}

// moveSelection cycles the selected agent. With the sidebar hidden the new
// label is flashed in the footer so the change is visible.
func (m *Model) moveSelection(delta int) tea.Cmd {
	n := len(m.agents)
	if n == 0 {
		return nil
	}
	m.openAgent(wrapIndex(m.selected+delta, n))
	if !m.sidebarVisible() {
		return m.setStatusFor("→ "+m.agents[m.selected].Name, false, selectDuration)
	}
	return nil
}

// toggleTree flips the sidebar; it stays hidden below sidebarMinW columns.
func (m *Model) toggleTree() tea.Cmd {
	m.showTree = !m.showTree
	if m.showTree && m.width < sidebarMinW {
		m.showTree = false
		return m.setStatus(fmt.Sprintf("sidebar needs %d columns", sidebarMinW), true)
	}
	m.layout()
	if m.showTree {
		return tea.Batch(m.setFocus(focusSidebar), channelsCmd(m.ctx, m.c, m.requestScope(), channelsNav))
	}
	return m.setFocus(focusInput)
}

// sidebarKey handles keys while the sidebar has focus: ↑/↓ (or j/k) move
// the cursor, enter selects that agent and returns to the input, esc
// returns without changing the selection (ctrl+b, handled before, closes
// the sidebar).
func (m *Model) sidebarKey(msg tea.KeyMsg) tea.Cmd {
	n := m.sidebarItems()
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.setFocus(focusInput)
	case stepCursor(msg, &m.sbCursor, n, true):
		m.followSidebarCursor()
		return nil
	case key.Matches(msg, keys.PageUp):
		m.scrollSidebar(-m.sidebarRoom())
		return nil
	case key.Matches(msg, keys.PageDown):
		m.scrollSidebar(m.sidebarRoom())
		return nil
	case key.Matches(msg, keys.Select):
		return m.sidebarSelect(m.sbCursor)
	case key.Matches(msg, keys.TabLeft): // ← folds another channel's tree, and unfolds it again
		return m.toggleChannelTree(m.sbCursor)
	case key.Matches(msg, keys.TabRight): // → on the title: its +; on a channel row: its gear, the channel's dirs
		if m.sbCursor == 0 {
			return m.newChannel()
		}
		return m.channelSettings(m.sbCursor)
	case msg.String() == "n": // the next agent that needs you, selected at once
		from := -1
		if r, ok := m.sidebarAt(m.sbCursor); ok && r.kind == sbAgent {
			from = r.k
		}
		if i := m.nextNeedy(from); i >= 0 {
			m.sbCursor = m.sidebarIndex(sidebarRow{kind: sbAgent, k: i})
			m.followSidebarCursor()
			m.openAgent(i)
		} else {
			return m.setStatus("no agent is waiting on you", false)
		}
		return nil
	}
	return nil
}

// sidebarRoom is how many body rows the sidebar draws under its header.
func (m Model) sidebarRoom() int {
	return max(0, m.vp.Height-len(m.sidebarHeader(sidebarWidth-1)))
}

// scrollSidebar moves the nav's window by delta rows, kept within its body.
func (m *Model) scrollSidebar(delta int) {
	body, _ := m.sidebarBody(sidebarWidth - 1)
	m.sbTop = min(max(m.sbTop+delta, 0), max(0, len(body)-m.sidebarRoom()))
}

// sidebarWheel scrolls the nav for a wheel notch over it; a shifted wheel
// asks for sideways scrolling, which the nav does not do.
func (m *Model) sidebarWheel(msg tea.MouseMsg) {
	if msg.Action != tea.MouseActionPress || msg.Shift {
		return
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.scrollSidebar(-wheelRows)
	case tea.MouseButtonWheelDown:
		m.scrollSidebar(wheelRows)
	default: // only the wheel scrolls
	}
}

// followSidebarCursor scrolls the nav just far enough to keep the row its
// cursor is on in view; it is what a cursor move calls, so scrolling by
// hand is never undone by a redraw.
func (m *Model) followSidebarCursor() {
	body, items := m.sidebarBody(sidebarWidth - 1)
	room := m.sidebarRoom()
	if room <= 0 || len(body) <= room {
		m.sbTop = 0
		return
	}
	cur := 0
	for r, it := range items {
		if it == m.sbCursor {
			cur = r
		}
	}
	if cur == len(body)-2 {
		cur++ // the last row brings the blank row under it into view
	}
	m.sbTop, _ = listWindow(cur, m.sbTop, len(body), room)
}

// nextNeedy is the index of the next agent after from (wrapping) with a
// permission or question of its own pending, or -1.
func (m *Model) nextNeedy(from int) int {
	n := len(m.agents)
	for k := 1; k <= n; k++ {
		i := (from + k) % n
		if m.needsHuman(m.agents[i].ID) != "" {
			return i
		}
	}
	return -1
}

// sidebarItems is how many rows the sidebar cursor can rest on: + channel,
// all channels and this channel's agents. The cursor counts them
// top to bottom: 0 is + channel, then the channels alphabetically with this
// channel's agents right under its row (sidebarRows).
func (m *Model) sidebarItems() int {
	return len(m.sidebarRows())
}

// sidebarKind is what a sidebar row the cursor lands on is.
type sidebarKind int

const (
	sbNewChannel sidebarKind = iota // the "channels" title, with its ✚
	sbOther                         // another channel in the global catalog
	sbHere                          // this channel: its chat
	sbAgent                         // one of this channel's agents
	sbOtherAgent                    // an agent of another channel, under its row
	sbHereAll                       // under this channel's tree: show all agents, or hide the idle ones
	sbOtherAll                      // the same under another channel's tree
)

// sidebarRow is one row the sidebar cursor lands on; k indexes navChannels
// (sbOther) or agents (sbAgent).
type sidebarRow struct {
	kind sidebarKind
	k    int // navChannels index (sbOther, sbOtherAgent) or agents index (sbAgent)
	j    int // sbOtherAgent: the agent's place in that channel's kept tree
}

// sidebarRows is the sidebar's cursor rows top to bottom: the title, the
// channels named before this one (navChannels is in
// alphabetical order), this channel, its agents, then the channels after
// it. The sidebar cursor is an index into it; keys, clicks and drawing all
// read it, so none of them works out where a row sits.
func (m Model) sidebarRows() []sidebarRow {
	before := 0
	for _, s := range m.navChannels {
		if compareChannels(s, m.channel) < 0 {
			before++
		}
	}
	rows := make([]sidebarRow, 0, 2+len(m.agents)+len(m.navChannels))
	rows = append(rows, sidebarRow{kind: sbNewChannel})
	for k := range before {
		rows = append(rows, sidebarRow{kind: sbOther, k: k})
		rows = append(rows, m.otherTreeRows(k)...)
	}
	rows = append(rows, sidebarRow{kind: sbHere})
	if !m.treeClosed[m.channelID] {
		shown, quiet := m.shownAgents(m.channelID, m.agents, m.openAgentID())
		for _, i := range shown {
			rows = append(rows, sidebarRow{kind: sbAgent, k: i})
		}
		if quiet > 0 {
			rows = append(rows, sidebarRow{kind: sbHereAll})
		}
	}
	for k := before; k < len(m.navChannels); k++ {
		rows = append(rows, sidebarRow{kind: sbOther, k: k})
		rows = append(rows, m.otherTreeRows(k)...)
	}
	return rows
}

// sidebarAt is the row at cursor index i.
func (m Model) sidebarAt(i int) (sidebarRow, bool) {
	rows := m.sidebarRows()
	if i < 0 || i >= len(rows) {
		return sidebarRow{}, false
	}
	return rows[i], true
}

// sidebarIndex is the cursor index of row r, or 0 (the title) when it is
// not shown.
func (m Model) sidebarIndex(r sidebarRow) int {
	for i, x := range m.sidebarRows() {
		if x == r {
			return i
		}
	}
	return 0
}

// sidebarSelect acts on the item under the cursor: + channel creates a
// channel with an editable directory and opens it; this channel's row shows its chat
// and an agent row that agent's own chat (both focus the input); another
// channel's row opens that channel in place of this one.
func (m *Model) sidebarSelect(i int) tea.Cmd {
	r, ok := m.sidebarAt(i)
	if !ok {
		return nil
	}
	switch r.kind {
	case sbNewChannel:
		return m.newChannel()
	case sbOther:
		m.setTreeClosed(m.navChannels[r.k].ID, false) // an unselected channel: its tree opens if closed
		return m.openOther(r.k)
	case sbHere:
		if m.superChat { // the selected channel: the row toggles its tree
			m.toggleHereTree()
			return nil
		}
		m.setTreeClosed(m.channelID, false) // not selected (an agent's chat is open): its tree opens if closed
		chat := m.openChat()
		if cmd, ok := m.openWaiting(m.channelID, ""); ok {
			return tea.Batch(chat, cmd)
		}
		return tea.Batch(chat, m.setFocus(focusInput))
	case sbHereAll:
		m.toggleShowAll(m.channelID, r)
		return nil
	case sbOtherAll:
		m.toggleShowAll(m.navChannels[r.k].ID, r)
		return nil
	case sbAgent:
		m.openAgent(r.k)
		if cmd, ok := m.openWaiting(m.channelID, m.selectedID()); ok {
			return cmd
		}
		return m.setFocus(focusInput)
	case sbOtherAgent:
		// another channel's agent: open that channel on it
		if tree := m.trees[m.navChannels[r.k].ID]; r.j < len(tree) {
			m.selectNext = tree[r.j].ID
		}
		return m.openOther(r.k)
	}
	return nil
}

// otherTreeRows are the cursor rows of another channel's agents, drawn
// under its row while its tree is open.
func (m Model) otherTreeRows(k int) []sidebarRow {
	id := m.navChannels[k].ID
	if !m.otherTreeShown(id) {
		return nil
	}
	shown, quiet := m.shownAgents(id, m.trees[id], "")
	rows := make([]sidebarRow, 0, len(shown)+1)
	for _, j := range shown {
		rows = append(rows, sidebarRow{kind: sbOtherAgent, k: k, j: j})
	}
	if quiet > 0 {
		rows = append(rows, sidebarRow{kind: sbOtherAll, k: k})
	}
	return rows
}

// shownAgents is which of channel's agents its tree draws, as indexes into
// agents, and how many are quiet: idle (or finished) with nothing waiting
// on the human, and neither the channel's main agent (the root, always
// drawn) nor the selected agent. Quiet agents are left out
// unless the tree's show all row is on; with none, there is no such row.
func (m Model) shownAgents(channel string, agents []protocol.AgentInfo, selected string) (shown []int, quiet int) {
	at := make(map[string]int, len(agents))
	for i, a := range agents {
		at[a.ID] = i
	}
	keep := make([]bool, len(agents))
	for i, a := range agents {
		o := agentOutcome(a)
		keep[i] = !((o == "idle" || o == "complete") && m.needsHuman(a.ID) == "" && a.ID != selected && a.Parent != "")
	}
	// a quiet agent with a drawn descendant is drawn too: its children hang
	// under it, and hiding it would detach them from the tree
	for i, a := range agents {
		if !keep[i] {
			continue
		}
		for p := a.Parent; p != ""; {
			j, ok := at[p]
			if !ok || keep[j] {
				break
			}
			keep[j] = true
			p = agents[j].Parent
		}
	}
	all := m.treeAll[channel]
	for i := range agents {
		if !keep[i] {
			quiet++
			if !all {
				continue
			}
		}
		shown = append(shown, i)
	}
	return shown, quiet
}

// openAgentID is the agent whose own chat is open, "" in the channel chat.
func (m Model) openAgentID() string {
	if m.superChat {
		return ""
	}
	return m.selectedID()
}

// toggleShowAll flips whether channel's tree shows its quiet agents, and
// keeps the cursor on the row that did it.
func (m *Model) toggleShowAll(channel string, row sidebarRow) {
	if m.treeAll == nil {
		m.treeAll = map[string]bool{}
	}
	m.treeAll[channel] = !m.treeAll[channel]
	m.sbCursor = m.sidebarIndex(row)
	m.followSidebarCursor()
}

// otherTreeShown reports whether another channel's tree is drawn: its
// agents are known (it was open once) and the human has not closed it.
func (m Model) otherTreeShown(id string) bool {
	return len(m.trees[id]) > 0 && !m.treeClosed[id]
}

// setTreeClosed opens or closes channel id's tree in the nav; no other
// channel's tree changes.
func (m *Model) setTreeClosed(id string, closed bool) {
	if m.treeClosed == nil {
		m.treeClosed = map[string]bool{}
	}
	m.treeClosed[id] = closed
}

// toggleHereTree closes the bound channel's tree, or opens it again, with
// the cursor on the channel's row.
func (m *Model) toggleHereTree() {
	m.setTreeClosed(m.channelID, !m.treeClosed[m.channelID])
	m.sbCursor = m.sidebarIndex(sidebarRow{kind: sbHere})
	m.followSidebarCursor()
}

// toggleChannelTree folds the tree of the channel on row i, or unfolds it
// again.
func (m *Model) toggleChannelTree(i int) tea.Cmd {
	r, ok := m.sidebarAt(i)
	if ok && (r.kind == sbHere || r.kind == sbAgent || r.kind == sbHereAll) {
		m.toggleHereTree()
		return nil
	}
	if !ok || (r.kind != sbOther && r.kind != sbOtherAgent && r.kind != sbOtherAll) {
		return nil
	}
	s := m.navChannels[r.k]
	if len(m.trees[s.ID]) == 0 {
		return m.setStatus("no tree for "+channelLabel(s)+" yet: open it once", false)
	}
	m.setTreeClosed(s.ID, !m.treeClosed[s.ID])
	m.sbCursor = m.sidebarIndex(sidebarRow{kind: sbOther, k: r.k})
	m.followSidebarCursor()
	return nil
}

// channelAt is the channel a sidebar cursor index names: this channel (k
// -1) or another channel k; ok is false for any other row.
func (m Model) channelAt(i int) (k int, ok bool) {
	switch r, _ := m.sidebarAt(i); r.kind {
	case sbHere:
		return -1, true
	case sbOther:
		return r.k, true
	case sbNewChannel, sbAgent, sbOtherAgent, sbHereAll, sbOtherAll:
	}
	return 0, false
}

// channelSettings is the gear of the channel on sidebar row i (→ on the row,
// or a click on the gear): the channel's file-backed project configuration.
func (m *Model) channelSettings(i int) tea.Cmd {
	k, ok := m.channelAt(i)
	switch {
	case !ok:
		return nil
	case k < 0:
		return m.openConfigEditor(false)
	}
	m.dirsNext = true
	return m.openOther(k)
}

// openOther opens another channel k (navChannels order) in
// place of this one.
func (m *Model) openOther(k int) tea.Cmd {
	s := m.navChannels[k]
	if m.switching {
		return m.setStatus("a channel is already opening", false)
	}
	return tea.Batch(m.setStatus("opening #"+s.Name, false), m.switchTo(s.ID))
}

func (m *Model) switchTo(id string) tea.Cmd {
	if m.switching {
		return m.setStatus("a channel is already opening", false)
	}
	m.switching = true
	return switchChannelCmd(m.ctx, m.c, m.channelID, id)
}

// newChannel names a channel, then offers the current directory as an editable
// default. The new channel opens in place of this one.
func (m *Model) newChannel() tea.Cmd {
	if m.switching {
		return m.setStatus("a channel is already opening", false)
	}
	o := newOverlay(ovNewChannel, overlayInput, "New channel")
	o.input.Placeholder = "name, shown as #name"
	return m.openOverlay(o)
}

// sidebarClick focuses the sidebar and acts on the row under the pointer
// like space: the chat row or an agent row opens that chat (the sidebar
// keeps focus), + channel or another channel's row acts like space.
func (m *Model) sidebarClick(x, y int) tea.Cmd {
	if y == 0 && x >= sidebarWidth-3 {
		return m.openConfigEditor(true)
	}
	cmd := m.setFocus(focusSidebar)
	if p, ok := m.planAt(y); ok { // a plan's row: its usage over time
		return tea.Batch(cmd, m.openPlanUsage(p.Provider, p.Name))
	}
	if y == m.sidebarTrustRow() { // the project row: ask for trust, or open the config
		return tea.Batch(cmd, m.trustClick())
	}
	if y == m.sidebarDiscordRow() {
		return tea.Batch(cmd, m.openDiscord("status"))
	}
	if _, system, tokens, cost, ok := m.usageRowFigures(y); ok { // the usage rows: their figures chart tokens or cost over time
		if kind, on := usageFigureAt(tokens, cost, sidebarWidth-1, x); on {
			return tea.Batch(cmd, m.openUsage(kind, system))
		}
		return cmd
	}
	_, items := m.sidebarLines(m.vp.Height)
	if y < 0 || y >= len(items) || items[y] < 0 {
		return cmd
	}
	i := items[y]
	m.sbCursor = i
	if i == 0 { // the channels title: only its + acts
		if x >= sidebarWidth-3 {
			return tea.Batch(cmd, m.newChannel())
		}
		return cmd
	}
	if _, ok := m.channelAt(i); ok && x >= sidebarWidth-3 { // the gear at the row's right edge
		return tea.Batch(cmd, m.channelSettings(i))
	}
	switch r, _ := m.sidebarAt(i); r.kind {
	case sbHere:
		if m.superChat { // the selected channel: a click toggles its tree
			m.toggleHereTree()
			return cmd
		}
		m.setTreeClosed(m.channelID, false) // not selected (an agent's chat is open): its tree opens if closed
		chat := m.openChat()
		if open, ok := m.openWaiting(m.channelID, ""); ok {
			return tea.Batch(cmd, chat, open)
		}
		return tea.Batch(cmd, chat)
	case sbAgent:
		m.openAgent(r.k)
		if open, ok := m.openWaiting(m.channelID, m.selectedID()); ok {
			return tea.Batch(cmd, open)
		}
		return cmd
	case sbNewChannel, sbOther, sbOtherAgent, sbHereAll, sbOtherAll:
	}
	return tea.Batch(cmd, m.sidebarSelect(i))
}
