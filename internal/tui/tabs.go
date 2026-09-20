package tui

import (
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

// tabRows is the strip's two rows: on top what belongs to the whole channel
// (the prompt queue every agent adds to, the working directories every agent
// shares), below what belongs to the selected
// agent.
var tabRows = [][]focus{
	{focusPermission, focusDirs},
	{focusAsync, focusTodo, focusMCP},
}

// tabFocuses is every tab in strip order: the top row, then the bottom.
// tabFocuses are the tabs of the strip under the chat, left to right. They
// are one stop in the tab cycle; ←/→ move between them.
var tabFocuses = slices.Concat(tabRows...)

// tabLayout is the tab rows as they are drawn: tabRows while the sidebar is
// hidden; with it showing, no channel row (pending permissions show on the
// channel's dot and in the chat, and dirs sits behind each channel's gear).
func (m Model) tabLayout() [][]focus {
	var rows [][]focus
	if !m.sidebarVisible() {
		rows = append(rows, tabRows[0])
	}
	if !m.superChat { // async · due · todo · mcp are an agent's: its own chat has them, the channel chat does not
		rows = append(rows, tabRows[1])
	}
	return rows
}

// tabOrder is tabLayout's tabs in order: what the strip's highlight walks.
func (m Model) tabOrder() []focus { return slices.Concat(m.tabLayout()...) }

// isTab reports whether f is one of the strip's tabs, or another dialog
// that opens and closes like one (the usage dialogs).
func isTab(f focus) bool {
	if f == focusUsage {
		return true
	}
	for _, t := range tabFocuses {
		if t == f {
			return true
		}
	}
	return false
}

// stripShown reports whether the tab strip is drawn: in a channel always,
// on the home (logo) screen never. A prompt that arrives on the home screen
// (the project trust prompt) still opens its dialog on its own; the strip
// appears with the first exchange.
func (m *Model) stripShown() bool {
	return !m.isHome()
}

// channelDirs returns the channel's working directories, which every agent
// shares.
func (m *Model) channelDirs() []protocol.DirInfo {
	return m.channel.Dirs
}

// selectedMCP returns the selected agent's MCP servers (its role's list).
func (m *Model) selectedMCP() []protocol.MCPInfo {
	if a := m.selectedAgent(); a != nil {
		return a.MCP
	}
	return nil
}

// selectedTodos returns the selected agent's todo list.
func (m *Model) selectedTodos() []event.TodoItem {
	if a := m.selectedAgent(); a != nil {
		return a.Todos
	}
	return nil
}

// activeTodo is the text of the selected agent's in-progress item, "" if none.
func (m *Model) activeTodo() string {
	for _, it := range m.selectedTodos() {
		if it.Status == event.TodoInProgress {
			return it.Text
		}
	}
	return ""
}

// awaitedAgents returns the agents the selected one is waiting on — any
// agent whose answer it expects (a child it tasked, a sibling or parent it
// messaged), in tree order.
func (m *Model) awaitedAgents() []protocol.AgentInfo {
	return awaitedOf(m.agents, m.selectedID())
}

// awaitedOf lists the live agents whose ids are in id's awaiting set, in
// the order of agents (the tree's pre-order).
func awaitedOf(agents []protocol.AgentInfo, id string) []protocol.AgentInfo {
	var self *protocol.AgentInfo
	for i := range agents {
		if agents[i].ID == id {
			self = &agents[i]
		}
	}
	if self == nil || len(self.Awaiting) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, w := range self.Awaiting {
		want[w] = true
	}
	var out []protocol.AgentInfo
	for _, a := range agents {
		if want[a.ID] && a.State != protocol.AgentKilled {
			out = append(out, a)
		}
	}
	return out
}

// dueOf is who the selected agent owes a reply: the human (you), then the
// live agents in tree order.
func (m *Model) dueOf() (human bool, agents []protocol.AgentInfo) {
	a := m.selectedAgent()
	if a == nil {
		return false, nil
	}
	want := map[string]bool{}
	for _, d := range a.Due {
		if d == "user" {
			human = true
		} else {
			want[d] = true
		}
	}
	for _, o := range m.agents {
		if want[o.ID] && o.State != protocol.AgentKilled {
			agents = append(agents, o)
		}
	}
	return human, agents
}

// asyncCount is the async tab's count: the agents and jobs the selected
// agent waits on, and the replies it owes (you, and agents).
func (m *Model) asyncCount() int {
	if m.hasReplyDetails() {
		a := m.selectedAgent()
		return len(a.PendingReplies) + len(a.AwaitingReplies) + len(m.runningJobs())
	}
	human, owed := m.dueOf()
	n := len(m.awaitedAgents()) + len(m.runningJobs()) + len(owed)
	if human {
		n++
	}
	return n
}

// runningJobs returns the selected agent's running async jobs.
func (m *Model) runningJobs() []protocol.JobInfo {
	if a := m.selectedAgent(); a != nil {
		return a.Jobs
	}
	return nil
}

// tabsKey handles keys while the strip has focus: ←/→ move the highlight
// (no wrap), ↑/↓ move it to the other row, enter opens the highlighted
// tab's dialog, esc returns to the input.
func (m *Model) tabsKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.setFocus(focusInput)
	case key.Matches(msg, keys.TabLeft):
		if m.tabSel > 0 {
			m.tabSel--
		}
	case key.Matches(msg, keys.TabRight):
		if m.tabSel < len(m.tabOrder())-1 {
			m.tabSel++
		}
	case key.Matches(msg, keys.OvUp), key.Matches(msg, keys.OvDown):
		m.tabSel = m.otherRowTab(m.tabSel, key.Matches(msg, keys.OvDown))
	case key.Matches(msg, keys.Select):
		order := m.tabOrder()
		return m.openTab(order[min(m.tabSel, len(order)-1)])
	}
	return nil
}

// asyncKey handles keys while the async dialog is open: ↑/↓ (or j/k) move
// over what the selected agent waits on (agents, then its jobs) and who waits
// on its reply (you, then agents); space on an agent opens that agent's chat
// and on "you" the channel chat, closing the dialog. A job row opens nothing.
func (m *Model) asyncKey(msg tea.KeyMsg) tea.Cmd {
	if m.hasReplyDetails() {
		return m.replyKey(msg)
	}
	waiting, jobs := m.awaitedAgents(), m.runningJobs()
	human, owed := m.dueOf()
	you := 0
	if human {
		you = 1
	}
	n := len(waiting) + len(jobs) + you + len(owed)
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case stepCursor(msg, &m.agCursor, n, true):
	case key.Matches(msg, keys.Select):
		if n == 0 {
			return nil
		}
		switch i := m.agCursor % n; {
		case i < len(waiting):
			m.openAgent(m.findAgent(waiting[i].ID))
		case i < len(waiting)+len(jobs):
			return nil // a job row: nothing to open
		case i < len(waiting)+len(jobs)+you:
			m.openChat()
		default:
			m.openAgent(m.findAgent(owed[i-len(waiting)-len(jobs)-you].ID))
		}
		return m.closeDialog()
	}
	return nil
}

// todoKey handles keys while the todo dialog is open: ↑/↓ (or j/k) move
// over the items (informational only), esc closes.
func (m *Model) todoKey(msg tea.KeyMsg) tea.Cmd {
	return m.listKey(msg, len(m.selectedTodos()))
}

// dirsKey handles keys in the dirs dialog: ↑/↓ move, a adds a directory,
// enter edits the highlighted one (replacing it), ctrl+d removes it; while
// the path field is open, enter submits and esc cancels the edit. The
// default directory is editable while idle; config-derived rows stay read-only.
func (m *Model) dirsKey(msg tea.KeyMsg) tea.Cmd {
	dirs := m.channelDirs()
	if m.dirEdit != "" {
		switch {
		case key.Matches(msg, keys.OvClose):
			m.dirEdit = ""
			m.dirInput.Blur()
			return nil
		case key.Matches(msg, keys.Submit):
			path := strings.TrimSpace(m.dirInput.Value())
			if path == "" {
				return nil
			}
			edit := m.dirEdit
			m.dirEdit = ""
			m.dirInput.Blur()
			if edit == "add" {
				return addDirCmd(m.ctx, m.c, m.channelID, path)
			}
			if edit == "default" {
				return setDirCmd(m.ctx, m.c, m.requestScope(), path)
			}
			return replaceDirCmd(m.ctx, m.c, m.channelID, edit, path)
		}
		var cmd tea.Cmd
		m.dirInput, cmd = m.dirInput.Update(msg)
		return cmd
	}
	n := len(dirs)
	cur := func() *protocol.DirInfo {
		if n == 0 {
			return nil
		}
		return &dirs[m.agCursor%n]
	}
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case stepCursor(msg, &m.agCursor, n, true):
	case msg.String() == "a":
		m.dirEdit = "add"
		m.dirInput.SetValue("")
		m.dirInput.Placeholder = "path (absolute, ~, or relative to the channel directory)"
		return m.dirInput.Focus()
	case key.Matches(msg, keys.Select):
		d := cur()
		if d == nil {
			return nil
		}
		if d.Source == "config" {
			return m.setStatus("set by dirs in stavlos.json: change it there", true)
		}
		m.dirEdit = d.Path
		if d.Source == "channel" {
			m.dirEdit = "default"
		}
		m.dirInput.SetValue(d.Path)
		m.dirInput.CursorEnd()
		return m.dirInput.Focus()
	case key.Matches(msg, keys.OvRemove):
		d := cur()
		if d == nil {
			return nil
		}
		if d.Source == "channel" {
			return m.setStatus("the channel directory cannot be removed", true)
		}
		if d.Source == "config" {
			return m.setStatus("set by dirs in stavlos.json: remove it there", true)
		}
		return removeDirCmd(m.ctx, m.c, m.channelID, d.Path)
	}
	return nil
}

// listKey is the key handling of a read-only list dialog (todo): ↑/↓ (or
// j/k) move over n rows, esc closes.
func (m *Model) listKey(msg tea.KeyMsg, n int) tea.Cmd {
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case stepCursor(msg, &m.agCursor, n, true):
	}
	return nil
}

// mcpKey handles keys while the mcp dialog is open: ↑/↓ (or j/k) move over
// the rows, enter shows or hides a server's tools, esc closes.
func (m *Model) mcpKey(msg tea.KeyMsg) tea.Cmd {
	_, owners := mcpRows(m.selectedMCP(), m.mcpOpen, clock(), 200)
	n := len(owners)
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case stepCursor(msg, &m.agCursor, n, true):
	case key.Matches(msg, keys.Select):
		if n > 0 {
			if name := owners[m.agCursor%n]; name != "" {
				if m.mcpOpen == nil {
					m.mcpOpen = map[string]bool{}
				}
				m.mcpOpen[name] = !m.mcpOpen[name]
			}
		}
	}
	return nil
}

// pickTabRow moves the open tab dialog's cursor to row, as ↑/↓ would, and
// with choose acts on it as space would: a permission answer, a question
// option toggled (or the typed answer opened), an awaited agent selected.
// While a text row is open (a denial reason, a path, a typed answer) the
// list does not follow the mouse.
func (m *Model) pickTabRow(row int, choose bool) tea.Cmd {
	space := tea.KeyMsg{Type: tea.KeySpace}
	switch m.focus {
	case focusPermission:
		p := m.currentPrompt()
		if p == nil {
			return nil
		}
		if m.permFor != p.ID {
			m.permFor, m.permSel, m.permEdit = p.ID, 0, ""
		}
		if m.permEdit != "" {
			return nil
		}
		m.permSel = row
		if choose {
			return m.permissionKey(space)
		}
	default:
		m.agCursor = row
		if choose && m.focus == focusAsync {
			return m.asyncKey(space)
		}
	}
	return nil
}

// tabHit is the body row a screen position lands on inside a tab dialog.
type tabHit struct {
	row   int
	rowOK bool
}

// tabDialogHit maps a screen position to the open tab dialog's rows, using
// the geometry the dialog was drawn with: the box is centred in the body,
// and the row under each line inside the border comes from tabDialogBox.
func (m *Model) tabDialogHit(x, y int) tabHit {
	box, rowAt := m.tabDialogBox(m.width)
	height := strings.Count(box, "\n") + 1
	bw := lipgloss.Width(box)
	x0 := max((m.width-bw)/2, 0)
	y0 := max((m.bodyHeight()-height)/2, 0)
	if x < x0 || x >= x0+bw || y <= y0 || y >= y0+height-1 {
		return tabHit{} // outside, or on the border
	}
	if i := y - y0 - 1; i < len(rowAt) && rowAt[i] >= 0 {
		return tabHit{row: rowAt[i], rowOK: true}
	}
	return tabHit{}
}

// tabAt maps a position on the strip (x, and row within the strip) to the
// tab label drawn there.
func (m *Model) tabAt(x, row int) (focus, bool) {
	_, spans := m.tabLabels(m.currentPrompt())
	if row < 0 || row >= len(spans) {
		return 0, false
	}
	return hitSpan(spans[row], x)
}

// openTab opens a tab dialog. Permissions span channels; question navigation
// goes directly to the inline card in the current chat.
func (m *Model) openTab(f focus) tea.Cmd {
	if f == focusPermission {
		if p := m.inlinePermission(); p != nil {
			return m.openPermission(p)
		}
		for i := range m.prompts {
			p := &m.prompts[i]
			if p.Kind == protocol.PromptPermission && (p.Channel == "" || p.Channel == m.channelID) && p.Agent != "" {
				m.openChat()
				return m.openPermission(p)
			}
		}
		if p := m.currentPrompt(); p != nil && p.Kind == protocol.PromptPermission {
			return m.setStatus("no permissions waiting in this channel", false)
		}
	}
	if f == focusQuestions {
		return m.openQuestion(m.currentQuestion())
	}
	m.scope = promptScope{}
	cmd := m.setFocus(f)
	return cmd
}

// openWaiting opens the permission dialog, else the inline question card, on
// what waits in channel (only agent's when agent is set) when anything does,
// and reports whether it opened one: opening a channel or an agent that waits
// on the human goes straight to its prompts.
func (m *Model) openWaiting(channel, agent string) (tea.Cmd, bool) {
	scope := promptScope{channel, agent}
	perms, questions := m.promptCountsIn(scope)
	f := focusPermission
	switch {
	case perms > 0:
		if channel == m.channelID {
			for i := range m.prompts {
				p := &m.prompts[i]
				if m.permissionVisible(*p) && scope.holds(*p) {
					return m.openPermission(p), true
				}
			}
		}
	case questions > 0:
		if channel != m.channelID {
			return nil, false
		}
		for i := range m.prompts {
			p := &m.prompts[i]
			if p.Kind == protocol.PromptQuestion && scope.holds(*p) && m.questionVisible(*p) {
				return m.openQuestion(p), true
			}
		}
		return nil, false
	default:
		return nil, false
	}
	m.scope = scope
	cmd := m.setFocus(f)
	return cmd, true
}

// otherRowTab is the tab ↑ (down false) or ↓ (down true) moves the strip's
// highlight to from tab sel: the same place in the row above or below,
// clamped to that row's length; sel itself on the first or last row.
func (m Model) otherRowTab(sel int, down bool) int {
	rows := m.tabLayout()
	row, col, start := 0, sel, 0
	for row < len(rows)-1 && col >= len(rows[row]) {
		col -= len(rows[row])
		start += len(rows[row])
		row++
	}
	switch {
	case down && row < len(rows)-1:
		return start + len(rows[row]) + min(col, len(rows[row+1])-1)
	case !down && row > 0:
		return start - len(rows[row-1]) + min(col, len(rows[row-1])-1)
	}
	return sel
}
