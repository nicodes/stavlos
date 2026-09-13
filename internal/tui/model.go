// Package tui is the Bubble Tea terminal frontend (PRD §7.2). It is the
// reference client for the protocol: everything it shows comes from the event
// stream, and every action it takes is a protocol call issued as a tea.Cmd.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/pkg/client"
)

const (
	statusDuration = 5 * time.Second
	selectDuration = 2 * time.Second // "→ label" flash when the sidebar is hidden
)

// Run drives the TUI for one session until the user quits. c is already
// attached (tier interactive). Returns nil on a clean quit.
func Run(ctx context.Context, c *client.Client, sessionID string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	m := newModel(ctx, c, sessionID)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion())
	go forwardNotifications(ctx, c, p)

	final, err := p.Run()

	// Best-effort unsubscribe; the daemon drops it on disconnect anyway.
	select {
	case <-c.Closed():
	default:
		uctx, ucancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.Unsubscribe(uctx, sessionID)
		ucancel()
	}

	if err != nil {
		return err
	}
	if fm, ok := final.(Model); ok && fm.fatal != nil {
		return fm.fatal
	}
	return nil
}

// Model is the Bubble Tea model for one session. Update only performs state
// transitions; every daemon interaction is a tea.Cmd from commands.go.
type Model struct {
	ctx       context.Context
	c         *client.Client
	sessionID string
	spawned   map[string]time.Time // agent id → spawn time, for the agents block

	session     protocol.SessionInfo
	agents      []protocol.AgentInfo // pre-order, root first
	selected    int
	transcripts map[string]*Transcript
	seq         int64

	prompts     []protocol.PromptInfo // pending, oldest first; [0] is shown
	claimedByUs map[string]bool
	promptBusy  string // prompt id with a claim/reply in flight

	presets []protocol.PresetInfo

	vp    viewport.Model
	input textinput.Model
	sp    spinner.Model

	width, height int
	showTree      bool      // right sidebar toggle (/tree, ctrl+b)
	hideKeys      bool      // the key bar (divider + legend) at the bottom is hidden; /help shows it
	cancelArmed   time.Time // when esc was last pressed on an empty input while the agent was busy; a second esc within cancelWindow cancels
	details       bool      // expanded tool output (/details)
	follow        bool      // auto-scroll to bottom

	status      string
	statusErr   bool
	statusToken int

	// Keyboard focus (tab / shift+tab cycle the sections). The chat cursor
	// walks transcript items; expanded holds per-item tool output overrides
	// keyed by agent id; itemRows maps items to rendered viewport rows.
	focus       focus
	chatCursor  int
	expanded    map[string]map[int]bool
	itemRows    map[int]rowRange
	promptInput textinput.Model // answer field of a question prompt
	sbCursor    int
	palIdx      int               // highlighted row in the "/" command palette
	agCursor    int               // highlighted row in the agents/async tab while it has focus
	history     []string          // prompts sent from this client (and replayed human prompts)
	parentOf    map[string]string // child agent id → parent id, for the parent's agent_create line
	histIdx     int               // == len(history) when editing a new line
	histDraft   string            // unsent text saved while browsing history
	loading     bool              // replaying events up to replayTo
	replayTo    int64             // seq from reconcile
	treeTimer   bool              // a debounced tree refresh is scheduled
	reconciled  bool              // the first reconcile landed

	ov        *overlay                // open modal, or nil
	providers []protocol.ProviderInfo // last provider.list result
	login     loginFlow               // device-code sign-in in progress

	fatal error
}

// focus names the UI section that owns the keyboard. The zero value is
// the input so a bare Model starts there.
type focus int

const (
	focusInput      focus = iota // the text input (typing, enter sends)
	focusChat                    // the transcript: a cursor walks its items
	focusPermission              // the permission tab: pending prompt box (y/n/a, question field)
	focusAgents                  // the agents tab: live children
	focusAsync                   // the async tab: running bash_async jobs
	focusSidebar                 // the agent tree (↑/↓ enter)
	focusTabs                    // placeholder in focusOrder for the tab strip as a whole
)

// tabFocuses are the tabs of the strip under the chat, left to right. They
// are one stop in the tab cycle; ←/→ move between them.
var tabFocuses = []focus{focusPermission, focusAgents, focusAsync}

// isTab reports whether f is one of the strip's tabs.
func isTab(f focus) bool {
	for _, t := range tabFocuses {
		if t == f {
			return true
		}
	}
	return false
}

// chatPage is how many items pgup/pgdn move the chat cursor.
const chatPage = 5

// loginFlow tracks one device-code sign-in. cancel aborts the pending
// provider.login.wait; id is the login the wait belongs to (results for any
// other id are stale and dropped).
type loginFlow struct {
	provider, name string
	method         string // "" = provider default
	id             string
	cancel         context.CancelFunc
}

// reset cancels any pending wait and forgets the flow.
func (l *loginFlow) reset() {
	if l.cancel != nil {
		l.cancel()
	}
	*l = loginFlow{}
}

func newModel(ctx context.Context, c *client.Client, sessionID string) Model {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.Placeholder = placeholders[placeholderIndex(time.Now())]
	ti.Focus()

	vp := viewport.New(80, 20)
	vp.MouseWheelEnabled = true
	// Arrow keys are ours (selection); the viewport keeps pgup/pgdn.
	vp.KeyMap.Up = key.NewBinding()
	vp.KeyMap.Down = key.NewBinding()
	vp.KeyMap.Left = key.NewBinding()
	vp.KeyMap.Right = key.NewBinding()

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(styleRunning))

	pi := textinput.New()
	pi.Prompt = "› "
	pi.Placeholder = "answer"

	return Model{
		ctx:         ctx,
		c:           c,
		sessionID:   sessionID,
		transcripts: map[string]*Transcript{},
		claimedByUs: map[string]bool{},
		expanded:    map[string]map[int]bool{},
		vp:          vp,
		input:       ti,
		promptInput: pi,
		sp:          sp,
		hideKeys:    true, // the key bar is off until /help
		follow:      true,
		loading:     true,
	}
}

// Init starts the cursor blink, the spinner, the placeholder cycle and the
// reconcile snapshot.
func (m Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.sp.Tick, placeholderTickCmd(), reconcileCmd(m.ctx, m.c, m.sessionID))
}

// Update is the single-threaded state machine.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case tea.KeyMsg:
		cmds = append(cmds, m.handleKey(msg))

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		if m.focus != focusChat {
			m.follow = m.vp.AtBottom()
		}
		cmds = append(cmds, cmd)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		cmds = append(cmds, cmd)
		// The "working…" indicator carries the spinner, so redraw mid-turn.
		if t := m.transcripts[m.selectedID()]; t != nil && (t.InTurn() || t.Running()) {
			m.refreshViewport()
		}

	case placeholderTickMsg:
		m.input.Placeholder = placeholders[placeholderIndex(time.Now())]
		cmds = append(cmds, placeholderTickCmd())

	case reconcileMsg:
		if msg.err != nil {
			m.fatal = fmt.Errorf("reconcile: %w", msg.err)
			return m, tea.Quit
		}
		m.session = msg.res.Session
		m.reconciled = true
		m.setAgents(msg.res.Agents)
		for _, p := range msg.res.Prompts {
			m.upsertPrompt(p)
		}
		m.replayTo = msg.res.Seq
		m.loading = msg.res.Seq > 0
		cmds = append(cmds, subscribeCmd(m.ctx, m.c, m.sessionID, 0))

	case subscribedMsg:
		if msg.err != nil {
			m.fatal = fmt.Errorf("subscribe: %w", msg.err)
			return m, tea.Quit
		}

	case eventMsg:
		cmds = append(cmds, m.applyEvent(msg.ev))

	case streamMsg:
		if msg.n.Session == "" || msg.n.Session == m.sessionID {
			m.transcript(msg.n.Agent).ApplyStream(msg.n)
			if msg.n.Agent == m.selectedID() {
				m.refreshViewport()
			}
		}

	case promptMsg:
		cmds = append(cmds, m.applyPromptNotification(msg.n))

	case disconnectedMsg:
		m.fatal = msg.err
		m.status, m.statusErr = "daemon disconnected", true
		return m, tea.Quit

	case treeMsg:
		if msg.err != nil {
			cmds = append(cmds, m.setStatus("tree: "+msg.err.Error(), true))
		} else {
			m.setAgents(msg.agents)
		}

	case treeTickMsg:
		m.treeTimer = false
		cmds = append(cmds, treeCmd(m.ctx, m.c, m.sessionID))

	case presetsMsg:
		if msg.err != nil {
			cmds = append(cmds, m.setStatus("presets: "+msg.err.Error(), true))
			break
		}
		m.presets = msg.presets
		lines := []string{"presets:"}
		for _, p := range msg.presets {
			s := "  " + p.Name
			if p.Model != "" {
				s += " [" + p.Model + "]"
			}
			if p.Description != "" {
				s += " — " + p.Description
			}
			if len(p.Spawn) > 0 {
				s += " (spawns: " + strings.Join(p.Spawn, ", ") + ")"
			}
			lines = append(lines, s)
		}
		if len(msg.presets) == 0 {
			lines = append(lines, "  (none)")
		}
		m.notice(lines...)
		cmds = append(cmds, m.setStatus(fmt.Sprintf("%d presets", len(msg.presets)), false))

	case resultMsg:
		if msg.err != nil {
			cmds = append(cmds, m.setStatus(msg.err.Error(), true))
		} else if msg.ok != "" {
			cmds = append(cmds, m.setStatus(msg.ok, false))
		}

	case promptReplyMsg:
		if m.promptBusy == msg.id {
			m.promptBusy = ""
		}
		switch {
		case msg.err == nil:
			m.removePrompt(msg.id)
		case isConflict(msg.err):
			delete(m.claimedByUs, msg.id)
			if i := m.findPrompt(msg.id); i >= 0 && m.prompts[i].ClaimedBy == "" {
				m.prompts[i].ClaimedBy = "?"
			}
			cmds = append(cmds, m.setStatus("claimed by another client", true))
		default:
			delete(m.claimedByUs, msg.id)
			cmds = append(cmds, m.setStatus("prompt: "+msg.err.Error(), true))
		}

	case clearStatusMsg:
		if msg.token == m.statusToken {
			m.status = ""
		}

	case providersMsg:
		cmds = append(cmds, m.onProviders(msg))

	case loginStartMsg:
		cmds = append(cmds, m.onLoginStart(msg))

	case loginDoneMsg:
		cmds = append(cmds, m.onLoginDone(msg))

	case rolesMsg:
		cmds = append(cmds, m.onRoles(msg))
	case variantsMsg:
		cmds = append(cmds, m.onVariants(msg))
	case modelsMsg:
		cmds = append(cmds, m.onModels(msg))

	default:
		// Cursor blink and other component-internal messages.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
		m.promptInput, cmd = m.promptInput.Update(msg)
		cmds = append(cmds, cmd)
		if m.ov != nil {
			cmds = append(cmds, m.ov.update(msg))
		}
	}

	cmds = append(cmds, m.ensureFocus())
	m.layout()
	return m, tea.Batch(cmds...)
}

// --- focus ---

// focusOrder lists the sections tab cycles through, top to bottom: the chat
// (once there is one), the tab strip (as one stop, see defaultTab), the
// input, and the sidebar (while visible).
func (m *Model) focusOrder() []focus {
	order := make([]focus, 0, 4)
	if !m.isHome() {
		order = append(order, focusChat)
	}
	order = append(order, focusTabs, focusInput)
	if m.sidebarVisible() {
		order = append(order, focusSidebar)
	}
	return order
}

// liveChildren returns the selected agent's live children, in tree order.
func (m *Model) liveChildren() []protocol.AgentInfo {
	sel := m.selectedID()
	if sel == "" {
		return nil
	}
	var out []protocol.AgentInfo
	for _, a := range m.agents {
		if a.Parent == sel && a.State != "finished" && a.State != "killed" {
			out = append(out, a)
		}
	}
	return out
}

// runningJobs returns the selected agent's running async jobs.
func (m *Model) runningJobs() []protocol.MonitorInfo {
	if a := m.selectedAgent(); a != nil {
		return a.Monitors
	}
	return nil
}

// cycleFocus moves focus delta steps (+1 tab, -1 shift+tab) through
// focusOrder, wrapping around. Landing on the strip opens defaultTab.
func (m *Model) cycleFocus(delta int) tea.Cmd {
	order := m.focusOrder()
	cur := m.focus
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
	next := order[((i+delta)%n+n)%n]
	if next == focusTabs {
		next = m.defaultTab()
	}
	return m.setFocus(next)
}

// defaultTab is the tab that opens when the strip gains focus: the first,
// left to right, with anything in it, or permission when all are empty.
func (m *Model) defaultTab() focus {
	switch {
	case m.currentPrompt() != nil:
		return focusPermission
	case len(m.liveChildren()) > 0:
		return focusAgents
	case len(m.runningJobs()) > 0:
		return focusAsync
	}
	return focusPermission
}

// tabArrow handles ←/→ while a tab has focus: move to the neighbouring
// tab (no wrap). Reports whether the key was consumed.
func (m *Model) tabArrow(msg tea.KeyMsg) (tea.Cmd, bool) {
	delta := 0
	switch {
	case key.Matches(msg, keys.TabLeft):
		delta = -1
	case key.Matches(msg, keys.TabRight):
		delta = 1
	default:
		return nil, false
	}
	for i, t := range tabFocuses {
		if t == m.focus {
			j := i + delta
			if j >= 0 && j < len(tabFocuses) {
				return m.setFocus(tabFocuses[j]), true
			}
			return nil, true
		}
	}
	return nil, false
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
	m.input.Blur()
	m.promptInput.Blur()
	if prev == focusChat {
		m.follow = true
		m.collapseAll()
		m.refreshViewport() // drops the cursor marker and scrolls to the bottom
	}
	switch f {
	case focusInput:
		return m.input.Focus()
	case focusChat:
		m.follow = false
		m.chatCursor = m.chatItems() - 1
		m.refreshViewport()
		m.scrollToCursor()
	case focusPermission:
		if p := m.currentPrompt(); p != nil && p.Kind == "question" {
			return m.promptInput.Focus()
		}
	case focusSidebar:
		m.sbCursor = m.selected
	case focusAgents, focusAsync:
		m.agCursor = 0
	}
	return nil
}

// agentsKey handles keys while the agents tab has focus: ↑/↓ (or j/k) move
// over the live children, enter selects the child under the cursor and
// returns to the input, esc returns without changing the selection.
func (m *Model) agentsKey(msg tea.KeyMsg) tea.Cmd {
	if cmd, ok := m.tabArrow(msg); ok {
		return cmd
	}
	kids := m.liveChildren()
	n := len(kids)
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.setFocus(focusInput)
	case key.Matches(msg, keys.SelUp), msg.String() == "k":
		if n > 0 {
			m.agCursor = ((m.agCursor-1)%n + n) % n
		}
	case key.Matches(msg, keys.SelDown), msg.String() == "j":
		if n > 0 {
			m.agCursor = (m.agCursor + 1) % n
		}
	case key.Matches(msg, keys.Submit):
		if n > 0 {
			if i := m.findAgent(kids[m.agCursor%n].ID); i >= 0 && i != m.selected {
				m.selected = i
				m.follow = true
				m.refreshViewport()
			}
		}
		return m.setFocus(focusInput)
	}
	return nil
}

// asyncKey handles keys while the async tab has focus: ↑/↓ (or j/k) move
// over the running jobs (informational only), esc returns to the input.
func (m *Model) asyncKey(msg tea.KeyMsg) tea.Cmd {
	if cmd, ok := m.tabArrow(msg); ok {
		return cmd
	}
	n := len(m.runningJobs())
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.setFocus(focusInput)
	case key.Matches(msg, keys.SelUp), msg.String() == "k":
		if n > 0 {
			m.agCursor = ((m.agCursor-1)%n + n) % n
		}
	case key.Matches(msg, keys.SelDown), msg.String() == "j":
		if n > 0 {
			m.agCursor = (m.agCursor + 1) % n
		}
	}
	return nil
}

// cancelWindow is how long a first esc stays armed for the second.
const cancelWindow = 3 * time.Second

// escCancel is esc on an empty input: while the selected agent is busy, the
// first press warns and arms, the second within cancelWindow cancels the
// turn. Idle agents ignore it.
func (m *Model) escCancel() tea.Cmd {
	a := m.selectedAgent()
	if a == nil || !m.agentBusy() {
		m.cancelArmed = time.Time{}
		return nil
	}
	if !m.cancelArmed.IsZero() && time.Since(m.cancelArmed) <= cancelWindow {
		m.cancelArmed = time.Time{}
		return sendCmd(m.ctx, m.c, a.ID, protocol.KindCancel, "", "cancel sent")
	}
	m.cancelArmed = time.Now()
	return m.setStatusFor("press esc again to cancel "+a.Label+"'s turn", true, cancelWindow)
}

// agentBusy reports whether the selected agent is mid-turn.
func (m *Model) agentBusy() bool {
	if t := m.transcripts[m.selectedID()]; t != nil && t.InTurn() {
		return true
	}
	if a := m.selectedAgent(); a != nil && (a.State == "running" || a.State == "blocked") {
		return true
	}
	return false
}

// ensureFocus falls back to the input when the focused section is gone
// (prompt answered, sidebar hidden, transcript empty).
func (m *Model) ensureFocus() tea.Cmd {
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
	p := m.currentPrompt()
	want := m.focus == focusPermission && p != nil && p.Kind == "question"
	switch {
	case want && !m.promptInput.Focused():
		return m.promptInput.Focus()
	case !want && m.promptInput.Focused():
		m.promptInput.Blur()
	}
	return nil
}

// selectionChanged re-renders after the selected agent changed: follow the
// new transcript, or, while the chat has focus, park the cursor on its
// last item.
func (m *Model) selectionChanged() {
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
	if t := m.transcripts[m.selectedID()]; t != nil {
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

// --- keys ---

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	if key.Matches(msg, keys.Quit) {
		return tea.Quit
	}
	if m.ov != nil {
		return m.overlayKey(msg)
	}
	if !key.Matches(msg, keys.Clear) {
		m.cancelArmed = time.Time{} // any other key disarms the two-step cancel
	}

	// While the "/" palette is open in the input, tab completes the command
	// (handled below) instead of cycling focus.
	paletteOpen := m.focus == focusInput && len(paletteMatches(m.input.Value())) > 0

	// Section-independent keys.
	switch {
	case key.Matches(msg, keys.NextSection) && !paletteOpen:
		return m.cycleFocus(1)
	case key.Matches(msg, keys.PrevSection):
		return m.cycleFocus(-1)
	case key.Matches(msg, keys.NextAgent):
		return m.moveSelection(1)
	case key.Matches(msg, keys.PrevAgent):
		return m.moveSelection(-1)
	case key.Matches(msg, keys.ToggleTree):
		return m.toggleTree()
	}

	switch m.focus {
	case focusAgents:
		return m.agentsKey(msg)
	case focusAsync:
		return m.asyncKey(msg)
	case focusSidebar:
		return m.sidebarKey(msg)
	case focusChat:
		return m.chatKey(msg)
	case focusPermission:
		return m.permissionKey(msg)
	}

	switch {
	case key.Matches(msg, keys.PageUp):
		m.vp.PageUp()
		m.follow = m.vp.AtBottom()
		return nil
	case key.Matches(msg, keys.PageDown):
		m.vp.PageDown()
		m.follow = m.vp.AtBottom()
		return nil
	case key.Matches(msg, keys.Top):
		m.vp.GotoTop()
		m.follow = m.vp.AtBottom()
		return nil
	case key.Matches(msg, keys.Bottom):
		m.vp.GotoBottom()
		m.follow = true
		return nil
	case key.Matches(msg, keys.SelUp):
		if pm := paletteMatches(m.input.Value()); len(pm) > 0 {
			m.palIdx = (m.palIdx - 1 + len(pm)) % len(pm)
			return nil
		}
		m.historyMove(-1)
		return nil
	case key.Matches(msg, keys.SelDown):
		if pm := paletteMatches(m.input.Value()); len(pm) > 0 {
			m.palIdx = (m.palIdx + 1) % len(pm)
			return nil
		}
		m.historyMove(1)
		return nil
	case key.Matches(msg, keys.Clear):
		if m.input.Value() != "" {
			m.input.Reset()
			m.palIdx = 0
			m.cancelArmed = time.Time{}
			return nil
		}
		return m.escCancel()
	case msg.Type == tea.KeyTab:
		// only reached when the palette is open (tab otherwise cycles focus)
		if pm := paletteMatches(m.input.Value()); len(pm) > 0 {
			m.completeCommand(pm[m.clampPal(len(pm))])
		}
		return nil
	case key.Matches(msg, keys.Submit):
		if pm := paletteMatches(m.input.Value()); len(pm) > 0 {
			c := pm[m.clampPal(len(pm))]
			typed := strings.ToLower(m.input.Value())
			if c.Direct && (typed == c.Name || typed != c.Name && c.Args == "" || isAlias(c, typed)) {
				m.input.Reset()
				m.palIdx = 0
				m.pushHistory(c.Name)
				return m.command(c.Name)
			}
			if typed != c.Name && !isAlias(c, typed) {
				m.completeCommand(c) // enter on a partial name completes it first
				return nil
			}
		}
		return m.submit()
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if !paletteActive(m.input.Value()) {
		m.palIdx = 0
	} else if pm := paletteMatches(m.input.Value()); m.palIdx >= len(pm) {
		m.palIdx = 0
	}
	return cmd
}

func (m *Model) clampPal(n int) int {
	if n == 0 {
		return 0
	}
	if m.palIdx < 0 || m.palIdx >= n {
		m.palIdx = 0
	}
	return m.palIdx
}

// completeCommand fills the input with the command name (plus a space when
// it takes arguments) so the user can keep typing.
func (m *Model) completeCommand(c Command) {
	v := c.Name
	if c.Args != "" {
		v += " "
	}
	m.input.SetValue(v)
	m.input.CursorEnd()
	m.palIdx = 0
}

func isAlias(c Command, typed string) bool {
	for _, a := range c.Aliases {
		if a == typed {
			return true
		}
	}
	return false
}

// permissionKey handles keys while the prompt box has focus: y/n/a answer
// a permission (y/n a trust prompt); a question takes typing into its own
// field and enter submits it; esc returns to the input.
func (m *Model) permissionKey(msg tea.KeyMsg) tea.Cmd {
	if key.Matches(msg, keys.Clear) {
		return m.setFocus(focusInput)
	}
	p := m.currentPrompt()
	// ←/→ switch tabs, except while typing an answer (the field owns them).
	if p == nil || p.Kind != "question" {
		if cmd, ok := m.tabArrow(msg); ok {
			return cmd
		}
	}
	if p == nil { // empty tab: nothing to answer
		return nil
	}
	if p.Kind == "question" {
		if key.Matches(msg, keys.Submit) {
			text := strings.TrimSpace(m.promptInput.Value())
			if text == "" {
				return nil
			}
			m.promptInput.Reset()
			if n, err := strconv.Atoi(text); err == nil && n >= 1 && n <= len(p.Options) {
				text = p.Options[n-1]
			}
			return m.answerPrompt(p, text)
		}
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		return cmd
	}
	switch {
	case key.Matches(msg, keys.Yes):
		return m.answerPrompt(p, "allow")
	case key.Matches(msg, keys.No):
		return m.answerPrompt(p, "deny")
	case key.Matches(msg, keys.Always) && p.Kind != "trust":
		return m.answerPrompt(p, "allow_always")
	}
	return nil
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
	case key.Matches(msg, keys.Submit):
		m.toggleItem()
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
	switch {
	case r.last-r.first+1 > h || r.first < m.vp.YOffset:
		m.vp.SetYOffset(r.first)
	case r.last >= m.vp.YOffset+h:
		m.vp.SetYOffset(r.last - h + 1)
	}
}

// toggleItem flips the cursor item's tool output between expanded and
// collapsed (a per-item override of /details). Other items are inert.
func (m *Model) toggleItem() {
	t := m.transcripts[m.selectedID()]
	if t == nil || !itemIsTool(t.All(), m.chatCursor) {
		return
	}
	e := m.agentExpanded(m.selectedID())
	cur, ok := e[m.chatCursor]
	if !ok {
		cur = m.details
	}
	e[m.chatCursor] = !cur
	m.refreshViewport()
	m.scrollToCursor()
}

// submit handles Enter in the input: a /command or a prompt envelope.
func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	m.input.Reset()
	if text == "" {
		return nil
	}
	m.pushHistory(text)
	if strings.HasPrefix(text, "/") {
		return m.command(text)
	}
	agent := m.selectedID()
	if agent == "" {
		return m.setStatus("no agent selected", true)
	}
	// Plain text is a steer: it reaches a busy agent at its next model-call
	// boundary, and simply starts a turn when the agent is idle. /queue is
	// the way to wait for the current turn to end.
	return sendCmd(m.ctx, m.c, agent, protocol.KindSteer, text, "")
}

func (m *Model) command(text string) tea.Cmd {
	fields := strings.Fields(text)
	name := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	agent := m.selectedID()

	needAgent := func() tea.Cmd {
		if agent == "" {
			return m.setStatus("no agent selected", true)
		}
		return nil
	}

	switch name {
	case "/quit", "/q", "/exit":
		return tea.Quit
	case "/help", "/h", "/?":
		m.hideKeys = !m.hideKeys
		m.layout()
		if m.hideKeys {
			return m.setStatus("key bar hidden (/help shows it)", false)
		}
		return m.setStatus("key bar shown (/help hides it)", false)
	case "/tree":
		return m.toggleTree()
	case "/details":
		m.details = !m.details
		m.expanded = map[string]map[int]bool{} // a global toggle resets per-item overrides
		m.refreshViewport()
		if m.details {
			return m.setStatus("tool output expanded", false)
		}
		return m.setStatus("tool output collapsed", false)
	case "/role":
		if c := needAgent(); c != nil {
			return c
		}
		if rest == "" {
			return rolesCmd(m.ctx, m.c, m.sessionID)
		}
		return pickRoleCmd(m.ctx, m.c, agent, strings.ToLower(rest))
	case "/roles", "/presets":
		return presetsCmd(m.ctx, m.c, m.sessionID)
	case "/yolo":
		on := !m.session.Yolo
		switch strings.ToLower(rest) {
		case "on", "true", "1":
			on = true
		case "off", "false", "0":
			on = false
		case "":
		default:
			return m.setStatus("usage: /yolo [on|off]", true)
		}
		return setYoloCmd(m.ctx, m.c, m.sessionID, on)
	case "/variants", "/variant":
		if c := needAgent(); c != nil {
			return c
		}
		a := m.selectedAgent()
		modelID, current := m.session.Model, ""
		if a != nil {
			current = a.Variant
			if a.Model != "" {
				modelID = a.Model
			}
		}
		if modelID == "" {
			return m.setStatus("no model selected — /models first", true)
		}
		if rest == "" {
			return variantsCmd(m.ctx, m.c, modelID, current)
		}
		v := strings.ToLower(rest)
		if v == "default" || v == "none" || v == "off" {
			v = ""
		}
		return pickVariantCmd(m.ctx, m.c, agent, v)
	case "/queue":
		if c := needAgent(); c != nil {
			return c
		}
		if rest == "" {
			return m.setStatus("usage: /queue <text>", true)
		}
		return sendCmd(m.ctx, m.c, agent, protocol.KindPrompt, rest, "queued for after the current turn")
	case "/model":
		if c := needAgent(); c != nil {
			return c
		}
		if rest == "" {
			return modelsCmd(m.ctx, m.c)
		}
		if !strings.Contains(rest, "/") {
			return m.setStatus("usage: /model <provider/id> (or /models to pick)", true)
		}
		return setAgentModelCmd(m.ctx, m.c, agent, rest)
	case "/models":
		return modelsCmd(m.ctx, m.c)
	case "/provider", "/connect", "/login":
		return providersCmd(m.ctx, m.c, false, strings.ToLower(rest))
	case "/providers":
		return providersCmd(m.ctx, m.c, true, "")
	case "/disconnect":
		if rest == "" {
			return m.setStatus("usage: /disconnect <provider>", true)
		}
		return disconnectProviderCmd(m.ctx, m.c, strings.ToLower(rest))
	case "/session-model":
		if rest == "" || !strings.Contains(rest, "/") {
			return m.setStatus("usage: /session-model <provider/id>", true)
		}
		return setSessionModelCmd(m.ctx, m.c, m.sessionID, rest)
	}
	return m.setStatus("unknown command "+name+" (try /help)", true)
}

// --- events ---

// applyEvent routes one log event into the right transcript and schedules a
// tree refresh for events that change agent state or cost.
func (m *Model) applyEvent(ev event.Event) tea.Cmd {
	if ev.Session != "" && ev.Session != m.sessionID {
		return nil
	}
	if ev.Seq > m.seq {
		m.seq = ev.Seq
	}
	var cmds []tea.Cmd

	target := ev.Agent
	switch ev.Type {
	case event.AgentSpawned:
		var p event.AgentSpawnedPayload
		if ev.Decode(&p) == nil && p.ID != "" {
			target = p.ID
			if m.spawned == nil {
				m.spawned = map[string]time.Time{}
			}
			m.spawned[p.ID] = ev.Time
			if !m.loading && m.findAgent(p.ID) < 0 {
				// Placeholder until the debounced tree refresh lands.
				m.agents = append(m.agents, protocol.AgentInfo{
					ID: p.ID, Session: ev.Session, Parent: p.Parent, Archetype: p.Archetype,
					Label: p.Label, Model: p.Model, Depth: p.Depth, State: "idle",
				})
			}
			if p.Parent != "" {
				// The parent's agent_create line tracks this child's life.
				if m.parentOf == nil {
					m.parentOf = map[string]string{}
				}
				m.parentOf[p.ID] = p.Parent
				m.transcript(p.Parent).ChildSpawned(p.ID)
				if !m.loading && p.Parent == m.selectedID() {
					m.refreshViewport()
				}
			}
		}
	case event.AgentFinished, event.AgentKilled:
		status := "killed"
		if ev.Type == event.AgentFinished {
			var p event.AgentFinishedPayload
			if ev.Decode(&p) == nil {
				status = p.Status
			}
		}
		if parent := m.parentOf[ev.Agent]; parent != "" {
			m.transcript(parent).ChildDone(ev.Agent, status)
			if !m.loading && parent == m.selectedID() {
				m.refreshViewport()
			}
		}
	case event.PromptQueued:
		var p event.TextPayload
		if m.loading && ev.Decode(&p) == nil && strings.HasPrefix(p.Source, "human:") && p.Text != "" {
			if n := len(m.history); n == 0 || m.history[n-1] != p.Text {
				m.history = append(m.history, p.Text)
			}
			m.histIdx = len(m.history)
		}
	case event.SessionModelChanged:
		var p event.ModelChangedPayload
		if ev.Decode(&p) == nil {
			m.session.Model = p.Model
		}
	case event.SessionYoloChanged:
		var p event.YoloPayload
		if ev.Decode(&p) == nil {
			m.session.Yolo = p.On
		}
	case event.TurnEnded:
		var p event.TurnEndedPayload
		if !m.loading && ev.Decode(&p) == nil && p.Reason == "error" &&
			(strings.Contains(p.Error, "not connected") || strings.Contains(p.Error, "/provider")) {
			cmds = append(cmds, m.setStatus("provider not connected — run /provider", true))
		}
	}

	if target != "" {
		m.transcript(target).Apply(ev)
		if !m.loading && target == m.selectedID() {
			m.refreshViewport()
		}
	}

	switch ev.Type {
	case event.AgentSpawned, event.AgentFinished, event.AgentKilled,
		event.TurnStarted, event.TurnEnded, event.Usage,
		event.AgentModelChanged, event.AgentRoleChanged, event.AgentVariantChanged, event.SessionModelChanged,
		event.MonitorStarted, event.MonitorFired, event.MonitorStopped:
		if !m.loading {
			cmds = append(cmds, m.markTreeDirty())
		}
	}

	if m.loading && ev.Seq >= m.replayTo {
		m.loading = false
		m.refreshViewport()
		cmds = append(cmds, m.markTreeDirty())
	}
	return tea.Batch(cmds...)
}

func (m *Model) markTreeDirty() tea.Cmd {
	if m.treeTimer {
		return nil
	}
	m.treeTimer = true
	return treeDebounceCmd()
}

// --- prompts ---

// applyPromptNotification keeps the prompt queue in step with the daemon.
// The first prompt to arrive while the input is idle (focused, nothing
// typed, no overlay) opens the permission tab so it can be answered at
// once; a draft in progress is never interrupted.
func (m *Model) applyPromptNotification(n protocol.PromptNotification) tea.Cmd {
	if n.Prompt.Session != "" && n.Prompt.Session != m.sessionID {
		return nil
	}
	before := len(m.prompts)
	switch n.Action {
	case "requested", "escalated", "claimed":
		m.upsertPrompt(n.Prompt)
	case "answered", "withdrawn", "defaulted":
		m.removePrompt(n.Prompt.ID)
	}
	// The turn indicator switches between "working…" and "permission
	// requested" on prompt changes, which arrive outside the event stream.
	if n.Prompt.Agent == m.selectedID() {
		m.refreshViewport()
	}
	if before == 0 && len(m.prompts) > 0 && m.focus == focusInput && m.ov == nil && strings.TrimSpace(m.input.Value()) == "" {
		return m.setFocus(focusPermission)
	}
	return nil
}

func (m *Model) currentPrompt() *protocol.PromptInfo {
	if len(m.prompts) == 0 {
		return nil
	}
	return &m.prompts[0]
}

func (m *Model) findPrompt(id string) int {
	for i, p := range m.prompts {
		if p.ID == id {
			return i
		}
	}
	return -1
}

func (m *Model) upsertPrompt(p protocol.PromptInfo) {
	if i := m.findPrompt(p.ID); i >= 0 {
		m.prompts[i] = p
		return
	}
	m.prompts = append(m.prompts, p)
}

func (m *Model) removePrompt(id string) {
	if i := m.findPrompt(id); i >= 0 {
		m.prompts = append(m.prompts[:i], m.prompts[i+1:]...)
	}
	if len(m.prompts) == 0 && m.focus == focusPermission {
		m.setFocus(focusInput) // the last prompt was answered: back to typing
	}
	delete(m.claimedByUs, id)
	if m.promptBusy == id {
		m.promptBusy = ""
	}
}

// answerPrompt claims and replies. answer is allow | deny | allow_always or
// free text for questions; for trust prompts "allow" means trust.
func (m *Model) answerPrompt(p *protocol.PromptInfo, answer string) tea.Cmd {
	if m.promptBusy == p.ID {
		return m.setStatus("answer in flight…", false)
	}
	if p.ClaimedBy != "" && !m.claimedByUs[p.ID] {
		return m.setStatus("claimed by another client", true)
	}
	m.promptBusy = p.ID
	m.claimedByUs[p.ID] = true
	if p.Kind == "trust" {
		var t struct {
			Dir  string `json:"dir"`
			Hash string `json:"hash"`
		}
		if err := json.Unmarshal(p.Input, &t); err != nil || t.Dir == "" {
			m.promptBusy = ""
			return m.setStatus("trust prompt: bad input", true)
		}
		return trustReplyCmd(m.ctx, m.c, p.ID, t.Dir, t.Hash, answer == "allow")
	}
	return answerPromptCmd(m.ctx, m.c, p.ID, answer)
}

// --- agents / selection ---

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
		return m.agents[i].Label
	}
	return id
}

// setAgents replaces the tree, keeping the selection on the same agent.
func (m *Model) setAgents(agents []protocol.AgentInfo) {
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
	m.selected = ((m.selected+delta)%n + n) % n
	m.selectionChanged()
	if !m.sidebarVisible() {
		return m.setStatusFor("→ "+m.agents[m.selected].Label, false, selectDuration)
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
		return m.setFocus(focusSidebar)
	}
	return m.setFocus(focusInput)
}

// sidebarKey handles keys while the sidebar has focus: ↑/↓ (or j/k) move
// the cursor, enter selects that agent and returns to the input, esc
// returns without changing the selection (ctrl+b, handled before, closes
// the sidebar).
func (m *Model) sidebarKey(msg tea.KeyMsg) tea.Cmd {
	n := len(m.agents)
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.setFocus(focusInput)
	case key.Matches(msg, keys.SelUp), msg.String() == "k":
		if n > 0 {
			m.sbCursor = ((m.sbCursor-1)%n + n) % n
		}
		return nil
	case key.Matches(msg, keys.SelDown), msg.String() == "j":
		if n > 0 {
			m.sbCursor = (m.sbCursor + 1) % n
		}
		return nil
	case key.Matches(msg, keys.PageUp):
		m.vp.PageUp()
		m.follow = m.vp.AtBottom()
		return nil
	case key.Matches(msg, keys.PageDown):
		m.vp.PageDown()
		m.follow = m.vp.AtBottom()
		return nil
	case key.Matches(msg, keys.Submit):
		if n > 0 && m.sbCursor != m.selected {
			m.selected = m.sbCursor
			m.follow = true
			m.refreshViewport()
		}
		return m.setFocus(focusInput)
	}
	return nil
}

// --- prompt history ---

// pushHistory records a submitted line; consecutive duplicates collapse.
func (m *Model) pushHistory(text string) {
	if n := len(m.history); n == 0 || m.history[n-1] != text {
		m.history = append(m.history, text)
	}
	m.histIdx = len(m.history)
	m.histDraft = ""
}

// historyMove walks the history: -1 older, +1 newer. Moving past the newest
// entry restores whatever was being typed before browsing began.
func (m *Model) historyMove(delta int) {
	n := len(m.history)
	if n == 0 {
		return
	}
	if m.histIdx == n {
		m.histDraft = m.input.Value()
	}
	idx := m.histIdx + delta
	if idx < 0 {
		idx = 0
	}
	if idx > n {
		idx = n
	}
	if idx == m.histIdx {
		return
	}
	m.histIdx = idx
	if idx == n {
		m.input.SetValue(m.histDraft)
	} else {
		m.input.SetValue(m.history[idx])
	}
	m.input.CursorEnd()
}

func (m *Model) transcript(id string) *Transcript {
	t := m.transcripts[id]
	if t == nil {
		t = NewTranscript()
		m.transcripts[id] = t
	}
	return t
}

// notice appends local lines to the selected transcript (help, presets).
func (m *Model) notice(lines ...string) {
	m.transcript(m.selectedID()).Notice(lines...)
	m.follow = true
	m.refreshViewport()
}

func (m *Model) totalCost() float64 {
	var c float64
	for _, a := range m.agents {
		c += a.CostUSD
	}
	return c
}

// --- layout / status ---

func (m *Model) setStatus(text string, isErr bool) tea.Cmd {
	return m.setStatusFor(text, isErr, statusDuration)
}

func (m *Model) setStatusFor(text string, isErr bool, d time.Duration) tea.Cmd {
	m.status, m.statusErr = text, isErr
	m.statusToken++
	return clearStatusCmd(m.statusToken, d)
}

// layout recomputes component sizes from the window and the optional rows
// (prompt box). Called at the end of every Update.
func (m *Model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	boxW := m.boxWidth()
	m.input.Width = boxW - len([]rune(m.input.Prompt)) - 1 // prompt + cursor
	// The prompt chevron carries the focus colour (there is no box border).
	m.input.PromptStyle = styleBorderMuted
	if m.focus == focusInput {
		m.input.PromptStyle = styleBorderUser
	}
	m.promptInput.Width = boxW - 4 - len([]rune(m.promptInput.Prompt)) - 1

	_, kb := m.keyBarView()
	bodyH := m.height - kb - 2 - inputBoxLines // key bar, blank + chat rule, input + meta row
	if sv := m.sectionsView(m.contentWidth()); sv != "" {
		bodyH -= strings.Count(sv, "\n") + 1 + 1 // plus the blank line below
	}
	if pv := m.paletteViewFor(m.contentWidth()); pv != "" {
		bodyH -= strings.Count(pv, "\n") + 1
	}
	if bodyH < 1 {
		bodyH = 1
	}
	vw := m.contentWidth()
	widthChanged := vw != m.vp.Width
	m.vp.Width, m.vp.Height = vw, bodyH
	if widthChanged {
		m.refreshViewport()
	} else if m.follow {
		m.vp.GotoBottom()
	}
}

// refreshViewport re-renders the selected transcript into the viewport,
// marking the cursor item while the chat has focus.
func (m *Model) refreshViewport() {
	var lines []Line
	if t := m.transcripts[m.selectedID()]; t != nil {
		lines = t.All()
	}
	if n := itemCount(lines); m.chatCursor >= n {
		m.chatCursor = n - 1
	}
	if m.chatCursor < 0 {
		m.chatCursor = 0
	}
	working, waiting, verb, stats := false, false, "", ""
	if t := m.transcripts[m.selectedID()]; t != nil && t.InTurn() {
		working, verb = true, t.TurnVerb()
		stats = turnStats(t.TurnStats(time.Now()))
	}
	for _, p := range m.prompts {
		if p.Agent == m.selectedID() {
			waiting = true
			break
		}
	}
	content, rows := renderAll(lines, RenderOpts{
		Width:    m.vp.Width,
		Details:  m.details,
		Spinner:  m.sp.View(),
		Working:  working,
		Waiting:  waiting,
		Verb:     verb,
		Stats:    stats,
		Expanded: m.expanded[m.selectedID()],
		Cursor:   m.chatCursor,
		Focused:  m.focus == focusChat,
	})
	m.itemRows = rows
	m.vp.SetContent(content)
	if m.follow {
		m.vp.GotoBottom()
	}
}

// --- overlay (provider / model pickers) ---

// openOverlay replaces any open overlay and blurs the main input. A sign-in
// shown by the replaced overlay is abandoned.
func (m *Model) openOverlay(o *overlay) tea.Cmd {
	if m.ov != nil && m.ov.mode == overlayLogin && o.mode != overlayLogin {
		m.login.reset()
	}
	m.ov = o
	m.input.Blur()
	return o.input.Focus()
}

func (m *Model) closeOverlay() tea.Cmd {
	m.ov = nil
	return m.input.Focus()
}

// overlayKey routes a key while an overlay is open.
func (m *Model) overlayKey(msg tea.KeyMsg) tea.Cmd {
	o := m.ov
	if o.mode == overlayLogin {
		return m.loginKey(msg)
	}
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeOverlay()
	case key.Matches(msg, keys.OvSelect):
		return m.overlaySubmit(false)
	case key.Matches(msg, keys.OvAlt):
		return m.overlaySubmit(true)
	case o.handleNav(msg):
		return nil
	}
	return o.update(msg)
}

// loginKey handles keys while the overlay shows a sign-in: esc cancels,
// o re-opens the browser, enter retries after an error.
func (m *Model) loginKey(msg tea.KeyMsg) tea.Cmd {
	o := m.ov
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.cancelLogin()
	case key.Matches(msg, keys.OvOpen):
		return openBrowserCmd(o.login.url)
	case key.Matches(msg, keys.OvSelect):
		if o.login.err == "" {
			return nil
		}
		for _, p := range m.providers {
			if p.ID == m.login.provider {
				return m.startLogin(p, m.login.method)
			}
		}
		return m.startLogin(protocol.ProviderInfo{ID: m.login.provider, Name: m.login.name}, m.login.method)
	}
	return nil
}

// cancelLogin abandons the pending wait and closes the overlay.
func (m *Model) cancelLogin() tea.Cmd {
	m.login.reset()
	return tea.Batch(m.closeOverlay(), m.setStatus("login cancelled", false))
}

// overlaySubmit is Enter (alt=false) or ctrl+s (alt=true) in the overlay.
func (m *Model) overlaySubmit(alt bool) tea.Cmd {
	o := m.ov
	switch o.kind {
	case ovProviders:
		it := o.selected()
		if it == nil {
			return nil
		}
		for _, p := range m.providers {
			if p.ID == it.id {
				if len(p.Methods) > 1 {
					return m.openMethodMenu(p)
				}
				return m.startLogin(p, "")
			}
		}
		return nil

	case ovMethods:
		it := o.selected()
		if it == nil {
			return nil
		}
		for _, p := range m.providers {
			if p.ID == m.login.provider {
				return m.startLogin(p, it.id)
			}
		}
		return nil

	case ovRoles:
		it := o.selected()
		if it == nil {
			return nil
		}
		agent := m.selectedID()
		if agent == "" {
			return m.setStatus("no agent selected", true)
		}
		return tea.Batch(m.closeOverlay(), pickRoleCmd(m.ctx, m.c, agent, it.id))
	case ovVariants:
		it := o.selected()
		if it == nil {
			return nil
		}
		agent := m.selectedID()
		if agent == "" {
			return m.setStatus("no agent selected", true)
		}
		return tea.Batch(m.closeOverlay(), pickVariantCmd(m.ctx, m.c, agent, it.id))
	case ovModels:
		it := o.selected()
		if it == nil {
			return nil
		}
		if alt {
			return tea.Batch(m.closeOverlay(), pickSessionModelCmd(m.ctx, m.c, m.sessionID, it.id))
		}
		agent := m.selectedID()
		if agent == "" {
			return m.setStatus("no agent selected (ctrl+s sets the session default)", true)
		}
		return tea.Batch(m.closeOverlay(), pickAgentModelCmd(m.ctx, m.c, agent, it.id))
	}
	return nil
}

// startLogin switches the overlay to "Sign in to <Name>" and asks the
// daemon for a device code. Any earlier sign-in is abandoned.
// openMethodMenu shows the provider's sign-in methods (opencode's "Login
// method" step), default first.
func (m *Model) openMethodMenu(p protocol.ProviderInfo) tea.Cmd {
	name := p.Name
	if name == "" {
		name = p.ID
	}
	m.login.reset()
	m.login.provider, m.login.name = p.ID, name
	items := make([]overlayItem, 0, len(p.Methods))
	for _, me := range p.Methods {
		items = append(items, overlayItem{id: me.ID, label: me.Label})
	}
	ov := newOverlay(ovMethods, overlayList, "Login method", "enter to select · esc to close")
	ov.setItems(items)
	return m.openOverlay(ov)
}

func (m *Model) startLogin(p protocol.ProviderInfo, method string) tea.Cmd {
	name := p.Name
	if name == "" {
		name = p.ID
	}
	m.login.reset()
	m.login.provider, m.login.name, m.login.method = p.ID, name, method
	var cmd tea.Cmd
	if m.ov == nil {
		cmd = m.openOverlay(newOverlay(ovProviders, overlayLogin, "", ""))
	}
	m.ov.switchLogin(name)
	return tea.Batch(cmd, loginStartCmd(m.ctx, m.c, p.ID, method))
}

// onLoginStart shows the URL and code, opens the browser once and starts
// the cancellable wait. A result for a sign-in that was cancelled is dropped.
func (m *Model) onLoginStart(msg loginStartMsg) tea.Cmd {
	if m.ov == nil || m.ov.mode != overlayLogin || msg.provider != m.login.provider {
		return nil
	}
	if msg.err != nil {
		m.ov.setLoginError(msg.err.Error())
		return nil
	}
	m.ov.setLogin(msg.res.URL, msg.res.Code, msg.res.Instructions)
	ctx, cancel := context.WithCancel(m.ctx)
	m.login.id, m.login.cancel = msg.res.ID, cancel
	return tea.Batch(openBrowserCmd(msg.res.URL), loginWaitCmd(ctx, m.c, msg.res.ID))
}

// onLoginDone finishes the sign-in: close the overlay, refresh providers and
// offer a model when none is set; on failure show the error for a retry.
func (m *Model) onLoginDone(msg loginDoneMsg) tea.Cmd {
	if msg.id == "" || msg.id != m.login.id {
		return nil // cancelled or superseded
	}
	name := m.login.name
	if msg.err != nil {
		if errors.Is(msg.err, context.Canceled) {
			return nil
		}
		if m.login.cancel != nil {
			m.login.cancel()
			m.login.cancel = nil
		}
		m.login.id = ""
		if m.ov != nil && m.ov.mode == overlayLogin {
			m.ov.setLoginError(msg.err.Error())
			return nil
		}
		return m.setStatus("sign in to "+name+": "+msg.err.Error(), true)
	}
	if msg.info.Name != "" {
		name = msg.info.Name
	}
	m.login.reset()
	for i, p := range m.providers {
		if p.ID == msg.info.ID {
			m.providers[i] = msg.info
		}
	}
	var cmds []tea.Cmd
	if m.ov != nil && m.ov.mode == overlayLogin {
		cmds = append(cmds, m.closeOverlay())
	}
	cmds = append(cmds, m.setStatus("connected "+name+" ✓", false), refreshProvidersCmd(m.ctx, m.c))
	if m.session.Model == "" && (len(m.agents) == 0 || m.agents[0].Model == "") {
		cmds = append(cmds, modelsCmd(m.ctx, m.c))
	}
	return tea.Batch(cmds...)
}

func (m *Model) onProviders(msg providersMsg) tea.Cmd {
	if msg.err != nil {
		if msg.refresh {
			return nil
		}
		return m.setStatus("providers: "+msg.err.Error(), true)
	}
	m.providers = msg.res.Providers
	if msg.refresh {
		return nil
	}
	if msg.notice {
		lines := []string{"providers:"}
		n := 0
		for _, p := range msg.res.Providers {
			name := p.Name
			if name == "" {
				name = p.ID
			}
			state := "not connected"
			if p.Connected {
				n++
				state = connectedHint(p)
			}
			lines = append(lines, "  "+name+" ("+p.ID+") · "+state)
		}
		if len(msg.res.Providers) == 0 {
			lines = append(lines, "  (none)")
		} else if n == 0 {
			lines = append(lines, "  run /provider to sign in")
		}
		m.notice(lines...)
		return m.setStatus(fmt.Sprintf("%d connected", n), false)
	}

	if msg.jump != "" {
		for _, p := range msg.res.Providers {
			if p.ID == msg.jump || strings.ToLower(p.Name) == msg.jump {
				if len(p.Methods) > 1 {
					return m.openMethodMenu(p)
				}
				return m.startLogin(p, "")
			}
		}
	}
	o := newOverlay(ovProviders, overlayList, "Connect a provider", "type to search · enter to select · esc to close")
	o.setItems(providerItems(msg.res.Providers))
	cmd := m.openOverlay(o)
	if msg.jump != "" {
		return tea.Batch(cmd, m.setStatus("unknown provider "+msg.jump, true))
	}
	return cmd
}

func (m *Model) onRoles(msg rolesMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("roles: "+msg.err.Error(), true)
	}
	label := m.agentLabel(m.selectedID())
	o := newOverlay(ovRoles, overlayList, "Change role of "+label, "enter: switch this agent's preset · takes effect at its next turn")
	items := make([]overlayItem, 0, len(msg.roles))
	for _, r := range msg.roles {
		hint := r.Description
		if len(r.Spawn) > 0 {
			hint += "  · spawns " + strings.Join(r.Spawn, ", ")
		}
		items = append(items, overlayItem{id: r.Name, label: r.Name, hint: hint})
	}
	o.setItems(items)
	return m.openOverlay(o)
}

// onVariants opens the /variants picker: the provider default plus every
// variant the model offers, the one in force marked.
func (m *Model) onVariants(msg variantsMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("variants: "+msg.err.Error(), true)
	}
	label := m.agentLabel(m.selectedID())
	o := newOverlay(ovVariants, overlayList, "Variant for "+label+" · "+msg.model, "enter: use this variant at the agent's next model call")
	if len(msg.variants) == 0 {
		o.setInfo("this model has no variants", false)
	}
	mark := func(id string) string {
		if id == msg.current {
			return "  · current"
		}
		return ""
	}
	items := []overlayItem{{id: "", label: "default", hint: "provider default" + mark("")}}
	for _, v := range msg.variants {
		items = append(items, overlayItem{id: v, label: v, hint: "reasoning effort" + mark(v)})
	}
	o.setItems(items)
	return m.openOverlay(o)
}

func (m *Model) onModels(msg modelsMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("models: "+msg.err.Error(), true)
	}
	o := newOverlay(ovModels, overlayList, "Select a model", "enter: set for the selected agent · ctrl+s: set session default")
	if len(msg.models) == 0 {
		o.setInfo("no providers connected — run /provider", true)
	}
	o.setItems(modelItems(msg.models))
	return m.openOverlay(o)
}

// shortHome abbreviates the home directory prefix.
func shortHome(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}
