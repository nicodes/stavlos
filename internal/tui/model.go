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
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseAllMotion())
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
	input textarea.Model // grows with the text, up to inputMaxLines
	sp    spinner.Model

	width, height int
	showTree      bool            // right sidebar toggle (/tree, ctrl+b)
	hideKeys      bool            // the key bar (divider + legend) at the bottom is hidden; /help shows it
	cancelArmed   time.Time       // when esc was last pressed on an empty input while the agent was busy; a second esc within cancelWindow cancels
	quitArmed     time.Time       // when ctrl+c was last pressed; a second within cancelWindow quits
	hoverFocus    bool            // the chat has focus because the mouse is over it (released when the mouse leaves)
	hoverFrom     focus           // where focus was before hover took it, restored when the mouse leaves the chat
	sel           selection       // mouse text selection (drag to select, release to copy)
	metaSel       metaPart        // the highlighted part of the meta row while it has focus
	tabSel        int             // the highlighted tab (index into tabFocuses) while the strip has focus
	dialogFrom    focus           // what had focus when the open dialog (a tab's or an overlay) was opened; closing returns there
	mcpOpen       map[string]bool // MCP servers whose tool list is expanded in the mcp dialog
	details       bool            // expanded tool output (/details)
	follow        bool            // auto-scroll to bottom

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
	dirInput    textinput.Model // path field of the dirs dialog while adding or editing
	dirEdit     string          // "" | "add" | the path being replaced
	permSel     int             // highlighted option of the permission dialog
	permFor     string          // the prompt id permSel belongs to (a new prompt starts at the top)
	permEdit    string          // "" | "deny" (reason row open) | "dir" (path row open) in the permission dialog
	q           questionState   // the questions dialog: where the human is in the current batch
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
	focusPermission              // the permission tab: pending permission/trust prompts (y/n/a)
	focusQuestions               // the questions tab: an ask_user batch, answered one question at a time
	focusAsync                   // the async tab: running shell jobs
	focusTodo                    // the todo tab: the selected agent's todo list
	focusMCP                     // the mcp tab: the selected agent's MCP servers
	focusDirs                    // the dirs tab: the selected agent's working directories
	focusSidebar                 // the agent tree (↑/↓ enter)
	focusTabs                    // the tab strip: ←/→ highlight a tab, enter opens its dialog
	focusMeta                    // the meta row under the input: ←/→ pick yolo/role/model/variant, enter opens it
)

// tabFocuses are the tabs of the strip under the chat, left to right. They
// are one stop in the tab cycle; ←/→ move between them.
var tabFocuses = []focus{focusPermission, focusQuestions, focusAsync, focusTodo, focusMCP, focusDirs}

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

// inputMaxLines is the least the input may grow to before it scrolls
// inside; inputHardMax the most. Between them the cap follows the window
// (inputCap), so a big paste on a tall terminal is shown whole.
const (
	inputMaxLines = 8
	inputHardMax  = 40
)

// inputCap is how tall the input may grow on this window: half its height,
// never under inputMaxLines nor over inputHardMax.
func (m Model) inputCap() int {
	c := m.height / 2
	if c < inputMaxLines {
		c = inputMaxLines
	}
	if c > inputHardMax {
		c = inputHardMax
	}
	return c
}

// newInputArea builds the message input: a textarea that starts one line
// tall and grows with the text (see inputRows), no line numbers, no cursor
// line highlight, enter sends and ctrl+j (or alt+enter) breaks a line.
func newInputArea() textarea.Model {
	ta := textarea.New()
	ta.Prompt = "› "
	// One chevron: the first line carries ›, continuation lines are
	// indented under it.
	ta.SetPromptFunc(2, func(lineIdx int) string {
		if lineIdx == 0 {
			return "› "
		}
		return "  "
	})
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// The textarea itself is always inputMaxLines tall so it never scrolls
	// until a message really is that long; the view shows only the rows
	// the text needs (see inputRows).
	ta.MaxHeight = inputHardMax
	ta.SetHeight(inputMaxLines) // layout raises it to inputCap for the window
	ta.EndOfBufferCharacter = ' '
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "alt+enter"))
	// A subtle background makes the input stand out from the chat above
	// and the meta row below; every part of the block shares it.
	bg := lipgloss.NewStyle().Background(colInputBg)
	ta.FocusedStyle.Base, ta.BlurredStyle.Base = bg, bg
	ta.FocusedStyle.CursorLine, ta.BlurredStyle.CursorLine = bg, bg
	ta.FocusedStyle.EndOfBuffer, ta.BlurredStyle.EndOfBuffer = bg, bg
	ta.FocusedStyle.Text, ta.BlurredStyle.Text = bg, bg
	ta.FocusedStyle.Placeholder, ta.BlurredStyle.Placeholder = styleDim.Background(colInputBg), styleDim.Background(colInputBg)
	// The prompt chevron carries the focus colour (there is no box border).
	ta.FocusedStyle.Prompt, ta.BlurredStyle.Prompt = styleBorderUser.Background(colInputBg), styleBorderMuted.Background(colInputBg)
	return ta
}

func newModel(ctx context.Context, c *client.Client, sessionID string) Model {
	ti := newInputArea()
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
	di := textinput.New()
	di.Prompt = "› "
	di.Placeholder = "path (absolute, ~, or relative to the session directory)"

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
		dirInput:    di,
		sp:          sp,
		hideKeys:    true, // the key bar is off until /help
		follow:      true,
		loading:     true,
	}
}

// Init starts the cursor blink, the spinner, the placeholder cycle and the
// reconcile snapshot.
func (m Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, textarea.Blink, m.sp.Tick, placeholderTickCmd(), reconcileCmd(m.ctx, m.c, m.sessionID))
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
		cmds = append(cmds, m.mouse(msg))

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
		cmds = append(cmds, subscribeCmd(m.ctx, m.c, m.sessionID, 0), sessionsCmd(m.ctx, m.c, m.session.Dir, true), presetsCmd(m.ctx, m.c, m.sessionID))

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
		if msg.err == nil {
			m.presets = msg.roles
		}
		if !msg.quiet {
			cmds = append(cmds, m.onRoles(msg))
		}
	case variantsMsg:
		cmds = append(cmds, m.onVariants(msg))
	case sessionsMsg:
		if msg.quiet {
			if msg.err == nil {
				m.seedHistory(msg.sessions)
			}
		} else {
			cmds = append(cmds, m.onSessions(msg))
		}
	case switchedMsg:
		if msg.err != nil {
			cmds = append(cmds, m.setStatus("resume: "+msg.err.Error(), true))
		} else {
			cmds = append(cmds, m.bindSession(msg.info))
		}
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
// (once there is one), the tab strip (as one stop), the
// input, and the sidebar (while visible).
func (m *Model) focusOrder() []focus {
	order := make([]focus, 0, 4)
	if !m.isHome() {
		order = append(order, focusChat)
	}
	order = append(order, focusInput) // top to bottom: under the rule come the input, the strip, the meta row
	if m.stripShown() {
		order = append(order, focusTabs)
	}
	order = append(order, focusMeta)
	if m.sidebarVisible() {
		order = append(order, focusSidebar)
	}
	return order
}

// stripShown reports whether the tab strip is drawn: in a session always,
// on the home (logo) screen never. A prompt that arrives on the home screen
// (the project trust prompt) still opens its dialog on its own; the strip
// appears with the first exchange.
func (m *Model) stripShown() bool {
	return !m.isHome()
}

// selectedDirs returns the selected agent's working directories.
func (m *Model) selectedDirs() []protocol.DirInfo {
	if a := m.selectedAgent(); a != nil {
		return a.Dirs
	}
	return nil
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
		if it.Status == "in_progress" {
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
		if want[a.ID] && a.State != "killed" {
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
// focusOrder, wrapping around. An open tab dialog counts as the strip.
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
	return m.setFocus(order[((i+delta)%n+n)%n])
}

// textEntry reports whether a text field outside the input has the keys:
// the question answer, the dirs path field, or a boundary prompt's edited
// directory. Enter submits there and space types a space.
func (m *Model) textEntry() bool {
	if m.permEdit != "" || (m.focus == focusDirs && m.dirEdit != "") {
		return true
	}
	if m.focus == focusQuestions && m.currentQuestion() != nil {
		return true // enter confirms an answer there
	}
	return false
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

// toggleMode is /auto or /yolo: no argument toggles between that mode and
// ask, "on"/"off" set it.
func (m *Model) toggleMode(mode, arg string) tea.Cmd {
	on := m.session.Mode != mode
	switch strings.ToLower(arg) {
	case "on", "true", "1":
		on = true
	case "off", "false", "0":
		on = false
	case "":
	default:
		return m.setStatus("usage: /"+mode+" [on|off]", true)
	}
	if !on {
		mode = protocol.ModeAsk
	}
	return setModeCmd(m.ctx, m.c, m.sessionID, mode)
}

// openMode is /mode: the three permission modes, the current one marked.
func (m *Model) openMode() tea.Cmd {
	o := newOverlay(ovMode, overlayList, "Permission mode")
	cur := m.session.Mode
	if cur == "" {
		cur = protocol.ModeAsk
	}
	var items []overlayItem
	for _, mode := range []string{protocol.ModeAsk, protocol.ModeAuto, protocol.ModeYolo} {
		hint := modeDesc(mode)
		if mode == cur {
			hint += "  · current"
		}
		items = append(items, overlayItem{id: mode, label: mode, hint: hint})
	}
	o.setItems(items)
	return m.openOverlay(o)
}

// closeDialog leaves an open tab dialog for whatever had focus when it was
// opened (the strip, the input, the chat…), or the input when that is no
// longer a stop. Back on the strip, the closed tab stays highlighted.
func (m *Model) closeDialog() tea.Cmd {
	closed := m.focus
	from := m.dialogFrom
	if isTab(from) || !m.focusAvailable(from) {
		from = focusInput
	}
	cmd := m.setFocus(from)
	if from == focusTabs {
		for i, t := range tabFocuses {
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

// tabsKey handles keys while the strip has focus: ←/→ move the highlight
// (no wrap), enter opens the highlighted tab's dialog, esc returns to the
// input.
func (m *Model) tabsKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.setFocus(focusInput)
	case key.Matches(msg, keys.TabLeft):
		if m.tabSel > 0 {
			m.tabSel--
		}
	case key.Matches(msg, keys.TabRight):
		if m.tabSel < len(tabFocuses)-1 {
			m.tabSel++
		}
	case key.Matches(msg, keys.Select):
		return m.setFocus(tabFocuses[m.tabSel])
	}
	return nil
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
	if prev == focusDirs && f != focusDirs {
		m.dirEdit = "" // leaving the dirs dialog drops a half-typed edit
		m.dirInput.Blur()
	}
	if prev == focusPermission && f != focusPermission {
		m.permEdit = ""
		m.dirInput.Blur()
	}
	if prev == focusQuestions && f != focusQuestions {
		m.q.typing = false
		m.promptInput.Blur()
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
		return m.input.Focus()
	case focusChat:
		m.follow = false
		m.chatCursor = m.chatItems() - 1
		m.refreshViewport()
		m.scrollToCursor()
	case focusQuestions:
		m.q.bind(m.currentQuestion())
	case focusSidebar:
		m.sbCursor = m.selected
	case focusAsync, focusTodo, focusMCP, focusDirs:
		m.agCursor = 0
	case focusMeta:
		m.metaSel = m.metaParts()[0] // always the leftmost part: YOLO while on, else the role
	case focusTabs:
		m.tabSel = 0 // always the leftmost tab: permission
	}
	return nil
}

// asyncKey handles keys while the async dialog is open: ↑/↓ (or j/k) move
// over what the selected agent is waiting on — the awaited agents first,
// then its running jobs; space on an agent row selects that agent and
// closes the dialog (a job row is informational), esc closes.
func (m *Model) asyncKey(msg tea.KeyMsg) tea.Cmd {
	agents := m.awaitedAgents()
	n := len(agents) + len(m.runningJobs())
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case key.Matches(msg, keys.SelUp), msg.String() == "k":
		if n > 0 {
			m.agCursor = ((m.agCursor-1)%n + n) % n
		}
	case key.Matches(msg, keys.SelDown), msg.String() == "j":
		if n > 0 {
			m.agCursor = (m.agCursor + 1) % n
		}
	case key.Matches(msg, keys.Select):
		if n == 0 || m.agCursor%n >= len(agents) {
			return nil // a job row: nothing to select
		}
		if i := m.findAgent(agents[m.agCursor%n].ID); i >= 0 && i != m.selected {
			m.selected = i
			m.follow = true
			m.refreshViewport()
		}
		return m.closeDialog()
	}
	return nil
}

// todoKey handles keys while the todo dialog is open: ↑/↓ (or j/k) move
// over the items (informational only), esc closes.
func (m *Model) todoKey(msg tea.KeyMsg) tea.Cmd {
	n := len(m.selectedTodos())
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
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

// dirsKey handles keys in the dirs dialog: ↑/↓ move, a adds a directory,
// enter edits the highlighted one (replacing it), ctrl+d removes it; while
// the path field is open, enter submits and esc cancels the edit. The
// session directory cannot be changed.
func (m *Model) dirsKey(msg tea.KeyMsg) tea.Cmd {
	dirs := m.selectedDirs()
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
			edit, agent := m.dirEdit, m.selectedID()
			m.dirEdit = ""
			m.dirInput.Blur()
			if edit == "add" {
				return addDirCmd(m.ctx, m.c, agent, path)
			}
			return replaceDirCmd(m.ctx, m.c, agent, edit, path)
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
	case key.Matches(msg, keys.SelUp), msg.String() == "k":
		if n > 0 {
			m.agCursor = ((m.agCursor-1)%n + n) % n
		}
	case key.Matches(msg, keys.SelDown), msg.String() == "j":
		if n > 0 {
			m.agCursor = (m.agCursor + 1) % n
		}
	case msg.String() == "a":
		m.dirEdit = "add"
		m.dirInput.SetValue("")
		m.dirInput.Placeholder = "path (absolute, ~, or relative to the session directory)"
		return m.dirInput.Focus()
	case key.Matches(msg, keys.Select):
		d := cur()
		if d == nil {
			return nil
		}
		if d.Source == "session" {
			return m.setStatus("the session directory cannot be changed", true)
		}
		m.dirEdit = d.Path
		m.dirInput.SetValue(d.Path)
		m.dirInput.CursorEnd()
		return m.dirInput.Focus()
	case key.Matches(msg, keys.OvRemove):
		d := cur()
		if d == nil {
			return nil
		}
		if d.Source == "session" {
			return m.setStatus("the session directory cannot be removed", true)
		}
		return removeDirCmd(m.ctx, m.c, m.selectedID(), d.Path)
	}
	return nil
}

// questionState is where the human is inside an ask_user batch: which
// question, which row the cursor is on, the picks so far (options and a
// typed answer), and whether the text field has the keys.
type questionState struct {
	id      string       // the prompt the state belongs to
	idx     int          // current question
	sel     int          // row under the cursor: an option, or the last row ("something else")
	marks   map[int]bool // toggled options of the current question
	custom  string       // the typed "something else" answer of the current question
	answers []string     // one per question, "" until answered
	typing  bool         // the free-text field has the keys
}

// bind resets the state when the batch under the dialog changes.
func (q *questionState) bind(p *protocol.PromptInfo) {
	if p == nil {
		*q = questionState{}
		return
	}
	if q.id == p.ID {
		return
	}
	*q = questionState{id: p.ID, marks: map[int]bool{}, answers: make([]string, len(p.Questions))}
}

// questionsKey handles keys in the questions dialog. Every question is a
// checklist: ↑/↓ move over the options and the last row, "something else";
// space toggles an option, or opens the text field on the last row; typing
// anywhere opens it too. Enter confirms the current question — the toggled
// options plus any typed text, joined — and moves on; the last confirmation
// submits the batch. ←/→ move between questions to review. Esc leaves the
// text field, or closes the dialog (the batch keeps waiting).
func (m *Model) questionsKey(msg tea.KeyMsg) tea.Cmd {
	p := m.currentQuestion()
	if p == nil {
		if key.Matches(msg, keys.OvClose) {
			return m.closeDialog()
		}
		return nil
	}
	m.q.bind(p)
	if m.q.idx >= len(p.Questions) {
		m.q.idx = len(p.Questions) - 1
	}
	cur := p.Questions[m.q.idx]
	nopt := len(cur.Options) // the row after the options is "something else"
	rows := nopt + 1
	picked := func() string {
		var out []string
		for i, o := range cur.Options {
			if m.q.marks[i] {
				out = append(out, o.Label)
			}
		}
		if c := strings.TrimSpace(m.q.custom); c != "" {
			out = append(out, c)
		}
		return strings.Join(out, ", ")
	}
	confirm := func() tea.Cmd {
		answer := picked()
		if answer == "" {
			return nil // nothing chosen yet
		}
		m.q.answers[m.q.idx] = answer
		m.q.typing = false
		m.promptInput.Reset()
		m.promptInput.Blur()
		if m.q.idx+1 < len(p.Questions) {
			m.q.idx++
			m.q.sel, m.q.marks, m.q.custom = 0, map[int]bool{}, ""
			return nil
		}
		return m.answerQuestions(p, m.q.answers)
	}
	if m.q.typing {
		switch {
		case key.Matches(msg, keys.OvClose):
			m.q.custom = strings.TrimSpace(m.promptInput.Value())
			m.q.typing = false
			m.promptInput.Blur()
			return nil
		case key.Matches(msg, keys.Submit):
			m.q.custom = strings.TrimSpace(m.promptInput.Value())
			return confirm()
		}
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		return cmd
	}
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case key.Matches(msg, keys.TabLeft):
		if m.q.idx > 0 {
			m.q.idx--
			m.q.sel, m.q.marks, m.q.custom = 0, map[int]bool{}, ""
		}
		return nil
	case key.Matches(msg, keys.TabRight):
		if m.q.idx+1 < len(p.Questions) {
			m.q.idx++
			m.q.sel, m.q.marks, m.q.custom = 0, map[int]bool{}, ""
		}
		return nil
	case key.Matches(msg, keys.SelUp):
		m.q.sel = ((m.q.sel-1)%rows + rows) % rows
		return nil
	case key.Matches(msg, keys.SelDown):
		m.q.sel = (m.q.sel + 1) % rows
		return nil
	case key.Matches(msg, keys.Select):
		if m.q.sel == nopt { // "something else": type it
			m.q.typing = true
			m.promptInput.SetValue(m.q.custom)
			m.promptInput.CursorEnd()
			return m.promptInput.Focus()
		}
		m.q.marks[m.q.sel] = !m.q.marks[m.q.sel]
		return nil
	case key.Matches(msg, keys.Submit):
		return confirm()
	case msg.Type == tea.KeyRunes || msg.Type == tea.KeyBackspace:
		// typing starts the "something else" answer
		m.q.typing = true
		m.q.sel = nopt
		m.promptInput.SetValue(m.q.custom)
		m.promptInput.CursorEnd()
		cmd := m.promptInput.Focus()
		var cmd2 tea.Cmd
		m.promptInput, cmd2 = m.promptInput.Update(msg)
		return tea.Batch(cmd, cmd2)
	}
	return nil
}

// answerQuestions sends a batch's answers.
func (m *Model) answerQuestions(p *protocol.PromptInfo, answers []string) tea.Cmd {
	if m.promptBusy == p.ID {
		return m.setStatus("answer in flight…", false)
	}
	if p.ClaimedBy != "" && !m.claimedByUs[p.ID] {
		return m.setStatus("claimed by another client", true)
	}
	m.promptBusy = p.ID
	m.claimedByUs[p.ID] = true
	return answerQuestionsCmd(m.ctx, m.c, p.ID, append([]string(nil), answers...))
}

// listKey is the key handling of a read-only list dialog: ↑/↓ (or j/k)
// move over n rows, esc closes.
func (m *Model) listKey(msg tea.KeyMsg, n int) tea.Cmd {
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
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

// mcpKey handles keys while the mcp dialog is open: ↑/↓ (or j/k) move over
// the rows, enter shows or hides a server's tools, esc closes.
func (m *Model) mcpKey(msg tea.KeyMsg) tea.Cmd {
	_, owners := mcpRows(m.selectedMCP(), m.mcpOpen, time.Now(), 200)
	n := len(owners)
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case key.Matches(msg, keys.SelUp), msg.String() == "k":
		if n > 0 {
			m.agCursor = ((m.agCursor-1)%n + n) % n
		}
	case key.Matches(msg, keys.SelDown), msg.String() == "j":
		if n > 0 {
			m.agCursor = (m.agCursor + 1) % n
		}
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

// selection is a mouse text selection: pressed while the button is down,
// active once the pointer has moved (a drag), kept highlighted after the
// release until the next press.
type selection struct {
	pressed, active bool
	ax, ay, bx, by  int // anchor (press) and pointer (latest drag) positions
}

// mouse routes mouse events: a left press anchors a possible selection, a
// drag extends and highlights it, and the release either copies the
// selection or, when nothing was dragged, counts as a click. Plain motion
// is hover.
func (m *Model) mouse(msg tea.MouseMsg) tea.Cmd {
	// Hover and clicks work in chat-column coordinates on the chat rows; the
	// sidebar, when shown, occupies the left edge beside the chat only. The
	// rows from the rule down span the window.
	cx, inMain := m.mainX(msg.X, msg.Y)
	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		m.sel = selection{pressed: true, ax: msg.X, ay: msg.Y, bx: msg.X, by: msg.Y}
		return nil
	case msg.Action == tea.MouseActionMotion && m.sel.pressed:
		if msg.X != m.sel.ax || msg.Y != m.sel.ay || m.sel.active {
			m.sel.active = true
			m.sel.bx, m.sel.by = msg.X, msg.Y
		}
		return nil
	case msg.Action == tea.MouseActionRelease && m.sel.pressed:
		m.sel.pressed = false
		if !m.sel.active {
			// A dialog (an overlay or the tab dialog) is hit-tested in
			// screen coordinates first; the tab dialog lets a miss fall
			// through to whatever is under the pointer, an overlay does not.
			if cmd, hit := m.dialogClick(msg.X, msg.Y); hit || m.ov != nil {
				return cmd
			}
			if !inMain {
				return m.setFocus(focusSidebar)
			}
			return m.mouseClick(cx, msg.Y)
		}
		text := m.selectedText()
		if text == "" {
			m.sel.active = false
			return nil
		}
		return tea.Batch(copyCmd(text), m.setStatus(fmt.Sprintf("copied %d characters", len([]rune(text))), false))
	case msg.Action == tea.MouseActionMotion:
		if m.ov != nil || isTab(m.focus) {
			m.dialogHover(msg.X, msg.Y) // a dialog owns hover; the chat behind it is left alone
			return nil
		}
		if !inMain {
			return m.mouseHover(-1, msg.Y) // outside the chat: hover releases, nothing else
		}
		return m.mouseHover(cx, msg.Y)
	}
	return nil
}

// dialogClick is a click while a dialog is open, in screen coordinates: on
// an overlay row it selects and submits; on a tab dialog row it moves the
// cursor (and, for agents, selects the agent like enter). hit reports
// whether the click landed on something.
func (m *Model) dialogClick(x, y int) (tea.Cmd, bool) {
	if m.ov != nil {
		if idx, ok := m.ov.itemAt(x, y, m.width, m.bodyHeight(), m.sp.View()); ok {
			m.ov.cursor = idx
			return m.overlaySubmit(false), true
		}
		return nil, false
	}
	if !isTab(m.focus) {
		return nil, false
	}
	if h := m.tabDialogHit(x, y); h.rowOK {
		m.agCursor = h.row
		if m.focus == focusAsync {
			return m.asyncKey(tea.KeyMsg{Type: tea.KeySpace}), true // a click selects like space
		}
		return nil, true
	}
	return nil, false
}

// dialogHover moves a dialog's cursor to the row under the pointer.
func (m *Model) dialogHover(x, y int) {
	if m.ov != nil {
		if idx, ok := m.ov.itemAt(x, y, m.width, m.bodyHeight(), m.sp.View()); ok {
			m.ov.cursor = idx
		}
		return
	}
	if h := m.tabDialogHit(x, y); h.rowOK {
		m.agCursor = h.row
	}
}

// tabHit is the body row a screen position lands on inside a tab dialog.
type tabHit struct {
	row   int
	rowOK bool
}

// tabDialogHit maps a screen position to the open tab dialog, using the
// same geometry as View and composite: the box is centred in the body;
// inside the border come the title, the hint line, the rule, then the rows.
func (m *Model) tabDialogHit(x, y int) tabHit {
	var h tabHit
	box := m.tabDialog(m.width)
	boxLines := strings.Split(box, "\n")
	bw := lipgloss.Width(box)
	x0 := (m.width - bw) / 2
	if x0 < 0 {
		x0 = 0
	}
	y0 := (m.bodyHeight() - len(boxLines)) / 2
	if y0 < 0 {
		y0 = 0
	}
	if x < x0 || x >= x0+bw || y <= y0 || y >= y0+len(boxLines)-1 {
		return h // outside, or on the border
	}
	if i := y - y0 - 1 - tabDialogHeader; i >= 0 && i < m.tabRowCount() {
		h.row, h.rowOK = i, true
	}
	return h
}

// tabRowCount is how many selectable rows the focused tab shows.
func (m *Model) tabRowCount() int {
	switch m.focus {
	case focusAsync:
		return len(m.awaitedAgents()) + len(m.runningJobs())
	case focusTodo:
		return len(m.selectedTodos())
	case focusMCP:
		_, owners := mcpRows(m.selectedMCP(), m.mcpOpen, time.Now(), 200)
		return len(owners)
	case focusDirs:
		return len(m.selectedDirs())
	case focusQuestions:
		if p := m.currentQuestion(); p != nil && m.q.idx < len(p.Questions) {
			return len(p.Questions[m.q.idx].Options) + 1 // plus "something else"
		}
	}
	return 0
}

// mainX maps a screen column to the chat column for the rows the sidebar
// shares (the chat and the status line): with the sidebar shown the chat
// starts after it and its separator. inMain is false over the sidebar.
// Lower rows span the window and are returned as they are.
func (m *Model) mainX(x, y int) (int, bool) {
	if !m.sidebarVisible() || y > m.vp.Height {
		return x, true
	}
	off := sidebarWidth + 2 // separator + gap
	if x < off {
		return x, false
	}
	return x - off, true
}

// selRange is the selection in reading order: (y0,x0) before (y1,x1),
// columns inclusive.
func (s selection) selRange() (x0, y0, x1, y1 int) {
	x0, y0, x1, y1 = s.ax, s.ay, s.bx, s.by
	if y1 < y0 || (y1 == y0 && x1 < x0) {
		x0, y0, x1, y1 = x1, y1, x0, y0
	}
	return
}

// selectedText is the plain text under the selection, in terminal order:
// the first row from the anchor column, whole rows in between, the last
// row up to the pointer column. Trailing spaces are trimmed per row.
func (m Model) selectedText() string {
	if !m.sel.active {
		return ""
	}
	saved := m.sel
	m.sel = selection{} // render the frame without the highlight
	lines := strings.Split(m.View(), "\n")
	m.sel = saved
	x0, y0, x1, y1 := m.sel.selRange()
	var out []string
	for y := y0; y <= y1 && y < len(lines); y++ {
		if y < 0 {
			continue
		}
		plain := ansi.Strip(lines[y])
		from, to := 0, ansi.StringWidth(plain)
		if y == y0 {
			from = x0
		}
		if y == y1 {
			to = x1 + 1
		}
		if from < 0 {
			from = 0
		}
		if to > ansi.StringWidth(plain) {
			to = ansi.StringWidth(plain)
		}
		if to <= from {
			out = append(out, "")
			continue
		}
		out = append(out, strings.TrimRight(ansi.Cut(plain, from, to), " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// highlightSelection paints the selection onto a rendered frame: the
// selected span of each row is shown in reverse video.
func (m Model) highlightSelection(frame string) string {
	if !m.sel.active {
		return frame
	}
	x0, y0, x1, y1 := m.sel.selRange()
	lines := strings.Split(frame, "\n")
	for y := y0; y <= y1 && y < len(lines); y++ {
		if y < 0 {
			continue
		}
		line := lines[y]
		w := ansi.StringWidth(line)
		from, to := 0, w
		if y == y0 {
			from = x0
		}
		if y == y1 {
			to = x1 + 1
		}
		if from >= w || to <= from {
			continue
		}
		if to > w {
			to = w
		}
		left := ansi.Cut(line, 0, from)
		mid := ansi.Strip(ansi.Cut(line, from, to))
		right := ""
		if to < w {
			right = ansi.Cut(line, to, w)
		}
		lines[y] = left + styleSelection.Render(mid) + right
	}
	return strings.Join(lines, "\n")
}

// textareaWrap mirrors the soft-wrap the bubbles textarea uses to draw a
// logical line (word wrap that keeps trailing spaces on the row and spills a
// row that exactly fills the width), so inputRows counts the rows the
// textarea will actually draw. A generic word wrap counts fewer rows and
// leaves the textarea scrolling inside a too-short box.
func textareaWrap(runes []rune, width int) [][]rune {
	var (
		lines  = [][]rune{{}}
		word   = []rune{}
		row    int
		spaces int
	)
	rw := func(rs []rune) int { return ansi.StringWidth(string(rs)) }
	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}
		if spaces > 0 {
			if rw(lines[row])+rw(word)+spaces > width {
				row++
				lines = append(lines, []rune{})
			}
			lines[row] = append(lines[row], word...)
			lines[row] = append(lines[row], []rune(strings.Repeat(" ", spaces))...)
			spaces = 0
			word = nil
		} else {
			last := ansi.StringWidth(string(word[len(word)-1]))
			if rw(word)+last > width {
				if len(lines[row]) > 0 {
					row++
					lines = append(lines, []rune{})
				}
				lines[row] = append(lines[row], word...)
				word = nil
			}
		}
	}
	if rw(lines[row])+rw(word)+spaces >= width {
		lines = append(lines, []rune{})
		lines[row+1] = append(lines[row+1], word...)
		spaces++
		lines[row+1] = append(lines[row+1], []rune(strings.Repeat(" ", spaces))...)
	} else {
		lines[row] = append(lines[row], word...)
		spaces++
		lines[row] = append(lines[row], []rune(strings.Repeat(" ", spaces))...)
	}
	return lines
}

// normalizePaste turns the carriage returns a terminal sends for pasted
// line endings (CR or CRLF) into the line feeds the textarea splits lines
// on; left alone they render into the line and overwrite it.
func normalizePaste(msg tea.KeyMsg) tea.KeyMsg {
	if msg.Type != tea.KeyRunes || !strings.ContainsRune(string(msg.Runes), '\r') {
		return msg
	}
	text := strings.ReplaceAll(string(msg.Runes), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	msg.Runes = []rune(text)
	return msg
}

// inputRows is how many rows the message needs: the textarea's own wrap
// of every logical line, at least one, at most inputMaxLines (past that the
// textarea scrolls to keep the cursor in view). The textarea is always
// inputMaxLines tall; inputView shows this many of its rows.
func (m Model) inputRows() int {
	w := m.input.Width()
	if w < 1 {
		w = 1
	}
	rows := 0
	for _, line := range strings.Split(m.input.Value(), "\n") {
		rows += len(textareaWrap([]rune(line), w))
	}
	if rows < 1 {
		rows = 1
	}
	if c := m.inputCap(); rows > c {
		rows = c
	}
	return rows
}

// inputView is the textarea trimmed to the rows the message needs.
func (m Model) inputView() string {
	lines := strings.Split(m.input.View(), "\n")
	if n := m.inputRows(); len(lines) > n {
		lines = lines[:n]
	}
	// The background spans the whole input width, not just the text: pad
	// every row out to the box in the same colour.
	bg := lipgloss.NewStyle().Background(colInputBg)
	width := m.boxWidth()
	for i, l := range lines {
		if pad := width - ansi.StringWidth(l); pad > 0 {
			lines[i] = l + bg.Render(strings.Repeat(" ", pad))
		}
	}
	return strings.Join(lines, "\n")
}

// mouseHover is mouse movement: over a chat item it does what ↑/↓ do (the
// chat takes focus, the cursor moves to that item, it previews); leaving
// the chat area gives focus back to the input, without scrolling. Focus the
// keyboard took is left alone.
func (m *Model) mouseHover(x, y int) tea.Cmd {
	if m.isHome() {
		return nil
	}
	inChat := x >= 0 && x < m.contentWidth() && y >= 0 && y < m.vp.Height
	if !inChat {
		if m.hoverFocus && m.focus == focusChat {
			// Give focus back to wherever hover took it from, without
			// scrolling the chat.
			m.hoverFocus = false
			m.focus = m.hoverFrom
			m.collapseAll()
			m.refreshViewport()
			m.follow = m.vp.AtBottom()
			if m.focus == focusInput {
				return m.input.Focus()
			}
			return m.syncPromptInput()
		}
		return nil
	}
	item, ok := m.itemAtRow(m.vp.YOffset + y)
	if !ok {
		return nil
	}
	if m.focus == focusChat && m.chatCursor == item {
		return nil
	}
	if m.focus != focusChat {
		m.hoverFocus, m.hoverFrom = true, m.focus
		m.focus = focusChat
		m.input.Blur()
		m.promptInput.Blur()
		m.follow = false
	}
	if m.chatCursor != item {
		m.collapseAll() // per-visit expansion, as with the arrow keys
	}
	m.chatCursor = item
	m.refreshViewport()
	return nil
}

// mouseClick is a left click: on a chat item it does what enter does on
// the hovered item (the click first moves the cursor there, like hover).
func (m *Model) mouseClick(x, y int) tea.Cmd {
	if m.isHome() {
		return m.setFocus(focusInput) // the input is the only thing to click on the logo screen
	}
	if x < 0 || y < 0 {
		return nil
	}
	if y < m.vp.Height && x >= m.contentWidth() {
		return nil // right of the chat column
	}
	lay := m.rows()
	switch {
	case y < m.vp.Height: // the chat: select, and toggle like enter
		item, ok := m.itemAtRow(m.vp.YOffset + y)
		if !ok {
			return nil
		}
		cmd := m.mouseHover(x, y)
		if m.focus == focusChat && m.chatCursor == item {
			m.toggleItem()
		}
		return cmd
	case y == lay.strip: // a tab label opens that tab's dialog
		if f, ok := m.tabAt(x); ok {
			return m.setFocus(f)
		}
	case y >= lay.input && y < lay.input+m.inputRows(): // the input lines
		return m.setFocus(focusInput)
	case y == lay.meta: // the meta row: its parts are buttons
		if part := m.metaHit(x); part != metaNone {
			return m.metaAction(part)
		}
		return m.setFocus(focusInput)
	}
	return nil
}

// metaParts lists the meta row's parts in order, the mode tag only while
// the mode is not ask.
func (m *Model) metaParts() []metaPart {
	parts := []metaPart{}
	if m.modeTag() != "" {
		parts = append(parts, metaYolo)
	}
	return append(parts, metaRole, metaModel, metaVariant)
}

// modeTag is the meta row's tag for the session's permission mode: "AUTO"
// or "YOLO", "" in ask mode.
func (m *Model) modeTag() string {
	switch m.session.Mode {
	case protocol.ModeAuto:
		return "AUTO"
	case protocol.ModeYolo:
		return "YOLO"
	}
	return ""
}

// metaAction is what a part of the meta row does when picked, by click or
// enter: the mode tag goes back to ask; the role, model and variant open
// their dialogs.
func (m *Model) metaAction(part metaPart) tea.Cmd {
	switch part {
	case metaYolo:
		return setModeCmd(m.ctx, m.c, m.sessionID, protocol.ModeAsk)
	case metaRole:
		return rolesCmd(m.ctx, m.c, m.sessionID)
	case metaModel:
		return modelsCmd(m.ctx, m.c)
	case metaVariant:
		return m.openVariants("")
	}
	return nil
}

// metaKey handles keys while the meta row has focus: ←/→ move between its
// parts (like the tabs on the strip), enter picks the highlighted one, esc
// returns to the input.
func (m *Model) metaKey(msg tea.KeyMsg) tea.Cmd {
	parts := m.metaParts()
	i := 0
	for k, p := range parts {
		if p == m.metaSel {
			i = k
		}
	}
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.setFocus(focusInput)
	case key.Matches(msg, keys.TabLeft):
		if i > 0 {
			m.metaSel = parts[i-1]
		}
	case key.Matches(msg, keys.TabRight):
		if i < len(parts)-1 {
			m.metaSel = parts[i+1]
		}
	case key.Matches(msg, keys.Select):
		return m.metaAction(m.metaSel)
	}
	return nil
}

// seedHistory gives a fresh session's ↑/↓ history the first prompts of
// this directory's earlier sessions (newest first under ↑), so the start
// screen recalls what was asked last time. Only when nothing has been typed
// here yet; the current and untitled sessions are skipped.
func (m *Model) seedHistory(sessions []protocol.SessionInfo) {
	if len(m.history) > 0 || !m.isHome() {
		return
	}
	seen := map[string]bool{}
	var titles []string
	for _, s := range sessions { // newest first
		if s.Title == "" || s.ID == m.sessionID || seen[s.Title] {
			continue
		}
		seen[s.Title] = true
		titles = append(titles, s.Title)
	}
	for i := len(titles) - 1; i >= 0; i-- { // history is oldest → newest
		m.history = append(m.history, titles[i])
	}
	m.histIdx = len(m.history)
}

// bodyHeight is the height of the main area a dialog is centred in (the
// window minus the key bar), as View computes it.
func (m *Model) bodyHeight() int {
	_, kb := m.keyBarView()
	h := m.height - kb
	if h < 1 {
		h = 1
	}
	return h
}

// metaPart names what sits under an x position on the meta row.
type metaPart int

const (
	metaNone    metaPart = iota
	metaYolo             // the AUTO/YOLO mode tag: click goes back to ask
	metaRole             // "label (role)": click opens /roles
	metaModel            // the model: click opens /models
	metaVariant          // the variant: click opens /variants
)

// metaHit maps an x position on the meta row to its part, following the
// layout metaLine draws: [YOLO · ]label (role) · model · variant.
func (m *Model) metaHit(x int) metaPart {
	label, role, model, variant := "agent", "", m.session.Model, ""
	if a := m.selectedAgent(); a != nil {
		label, role, variant = a.Label, a.Archetype, a.Variant
		if a.Model != "" {
			model = a.Model
		}
	}
	x0 := 0
	if m.modeTag() != "" {
		if x < 4 {
			return metaYolo
		}
		x0 = 4 + 3
	}
	name := label
	if role != "" {
		name = fmt.Sprintf("%s (%s)", label, role)
	}
	if x < x0 {
		return metaNone
	}
	if x < x0+ansi.StringWidth(name) {
		return metaRole
	}
	x0 += ansi.StringWidth(name) + 3
	if model == "" {
		return metaModel // "no model — /models" fills the rest
	}
	if x < x0 {
		return metaNone
	}
	short, _ := splitModel(model) // drawn without its provider
	if x < x0+ansi.StringWidth(short) {
		return metaModel
	}
	x0 += ansi.StringWidth(short) + 3
	if variant == "" {
		variant = "default"
	}
	if x >= x0 && x < x0+ansi.StringWidth(variant) {
		return metaVariant
	}
	return metaNone
}

// rowLayout is where the session view's pieces sit, in screen rows: under
// the rule come the palette (while open), the input, a blank line, the
// strip and the meta row.
type rowLayout struct {
	input int // first row of the input (it may span several)
	strip int // the tab strip line
	meta  int // the meta row
}

// rows derives the row layout the same way sessionView stacks its parts.
func (m *Model) rows() rowLayout {
	y := m.vp.Height + 2 // the status line, then the rule
	if pv := m.paletteViewFor(m.width); pv != "" {
		y += strings.Count(pv, "\n") + 1
	}
	lay := rowLayout{input: y}
	y += m.inputRows() + 1 // the input, then the blank line under it
	lay.strip = y
	lay.meta = y + 1
	return lay
}

// tabAt maps an x position on the strip to the tab label under it. The
// labels are laid out as sectionTabs draws them: permission, agents, async,
// separated by " · ".
func (m *Model) tabAt(x int) (focus, bool) {
	x0 := 0
	for i, text := range m.tabTexts() {
		w := ansi.StringWidth(text)
		if x >= x0 && x < x0+w {
			return tabFocuses[i], true
		}
		x0 += w + 3 // " · "
	}
	return 0, false
}

// itemAtRow maps a viewport content row to the chat item drawn there.
func (m *Model) itemAtRow(row int) (int, bool) {
	for item, r := range m.itemRows {
		if row >= r.first && row <= r.last {
			return item, true
		}
	}
	return 0, false
}

// cancelWindow is how long a first esc stays armed for the second.
const cancelWindow = 3 * time.Second

// ctrlC is the two-step quit: the first press clears the input, closes any
// dialog, focuses the input and warns; a second within cancelWindow quits.
func (m *Model) ctrlC() tea.Cmd {
	if !m.quitArmed.IsZero() && time.Since(m.quitArmed) <= cancelWindow {
		return tea.Quit
	}
	m.quitArmed = time.Now()
	m.cancelArmed = time.Time{}
	m.ov = nil
	m.input.Reset()
	return tea.Batch(m.setFocus(focusInput), m.input.Focus(), m.setStatusFor("press ctrl+c again to quit", true, cancelWindow))
}

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
	want := m.focus == focusQuestions && m.currentQuestion() != nil && m.q.typing
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
		return m.ctrlC()
	}
	m.quitArmed = time.Time{} // any other key disarms the two-step quit
	if m.ov != nil {
		return m.overlayKey(msg)
	}
	if !key.Matches(msg, keys.Clear) {
		m.cancelArmed = time.Time{} // any other key disarms the two-step cancel
	}
	// Enter anywhere but the input (and outside a text field) closes what is
	// open and goes back to typing; space is what selects, opens and toggles.
	if key.Matches(msg, keys.Submit) && m.focus != focusInput && !m.textEntry() {
		return m.setFocus(focusInput)
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
	case focusAsync:
		return m.asyncKey(msg)
	case focusTodo:
		return m.todoKey(msg)
	case focusMCP:
		return m.mcpKey(msg)
	case focusDirs:
		return m.dirsKey(msg)
	case focusSidebar:
		return m.sidebarKey(msg)
	case focusMeta:
		return m.metaKey(msg)
	case focusTabs:
		return m.tabsKey(msg)
	case focusChat:
		return m.chatKey(msg)
	case focusPermission:
		return m.permissionKey(msg)
	case focusQuestions:
		return m.questionsKey(msg)
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
		// ↑ moves the cursor up a row (a logical line, or a wrapped row of
		// one) while there is a row above; on the top row it walks history.
		if m.input.Line() > 0 || m.input.LineInfo().RowOffset > 0 {
			break
		}
		m.historyMove(-1)
		return nil
	case key.Matches(msg, keys.SelDown):
		if pm := paletteMatches(m.input.Value()); len(pm) > 0 {
			m.palIdx = (m.palIdx + 1) % len(pm)
			return nil
		}
		// ↓ moves down a row while there is one below; on the bottom row it
		// walks history forward.
		if li := m.input.LineInfo(); m.input.Line() < m.input.LineCount()-1 || li.RowOffset < li.Height-1 {
			break
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
	m.input, cmd = m.input.Update(normalizePaste(msg))
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

// permOption is one row of the permission dialog's single-select list.
type permOption struct {
	id    string // allow | always | prefix | add | add_other | deny | trust | skip
	label string
	desc  string
}

// permOptions are the hard-coded answers a prompt offers, top to bottom.
// A plain permission: once, this exact call for the session, the command's
// prefix for the session (shell, when one can be derived), deny. A boundary
// prompt: once, add the offered directory, add another one, deny. Trust:
// trust the project config, or not now.
func permOptions(p *protocol.PromptInfo) []permOption {
	switch {
	case p.Kind == "trust":
		return []permOption{
			{"trust", "Trust this project's config", "until these files change"},
			{"skip", "Not now", "run on the global config only"},
		}
	case p.Dir != "":
		return []permOption{
			{"allow", "Allow once", ""},
			{"add", "Allow and add " + shortHome(p.Dir), "the agent keeps the directory for the session"},
			{"add_other", "Allow and add another directory…", "type the path"},
			{"deny", "Deny", "with an optional reason"},
		}
	}
	what := "this exact call"
	if p.Tool == "shell" {
		what = "this exact command"
	}
	opts := []permOption{
		{"allow", "Allow once", ""},
		{"always", "Allow for this session", what},
	}
	if p.Tool == "shell" {
		if pre := protocol.CommandPrefix(fullToolArg(p.Tool, p.Input)); pre != "" {
			opts = append(opts, permOption{"prefix", "Allow " + pre + " for this session", "every command starting with it"})
		}
	}
	return append(opts, permOption{"deny", "Deny", "with an optional reason"})
}

// permSelection is the highlighted row for p: the stored one when it
// belongs to this prompt, else the top.
func (m Model) permSelection(p *protocol.PromptInfo) int {
	if m.permFor != p.ID {
		return 0
	}
	if n := len(permOptions(p)); m.permSel >= n {
		return n - 1
	}
	return m.permSel
}

// permissionKey handles keys while the permission dialog has focus: ↑/↓
// move through the options, space chooses one (Deny opens a row for an
// optional reason, "add another directory" a row for the path, both
// submitted with enter and cancelled with esc); esc closes the dialog with
// the prompt still waiting.
func (m *Model) permissionKey(msg tea.KeyMsg) tea.Cmd {
	p := m.currentPrompt()
	if p == nil { // empty dialog: nothing to answer
		if key.Matches(msg, keys.Clear) {
			return m.closeDialog()
		}
		return nil
	}
	if m.permFor != p.ID {
		m.permFor, m.permSel, m.permEdit = p.ID, 0, ""
	}
	if m.permEdit != "" {
		switch {
		case key.Matches(msg, keys.OvClose):
			m.permEdit = ""
			m.dirInput.Blur()
			return nil
		case key.Matches(msg, keys.Submit):
			text := strings.TrimSpace(m.dirInput.Value())
			edit := m.permEdit
			if edit == "dir" && text == "" {
				return nil
			}
			m.permEdit = ""
			m.dirInput.Blur()
			if edit == "dir" {
				return m.answerPromptDir(p, text)
			}
			return m.denyPrompt(p, text)
		}
		var cmd tea.Cmd
		m.dirInput, cmd = m.dirInput.Update(msg)
		return cmd
	}
	opts := permOptions(p)
	n := len(opts)
	switch {
	case key.Matches(msg, keys.Clear):
		return m.closeDialog()
	case key.Matches(msg, keys.SelUp):
		m.permSel = ((m.permSel-1)%n + n) % n
		return nil
	case key.Matches(msg, keys.SelDown):
		m.permSel = (m.permSel + 1) % n
		return nil
	case key.Matches(msg, keys.Select):
		if m.permSel >= n {
			m.permSel = n - 1
		}
		switch opts[m.permSel].id {
		case "allow", "trust":
			return m.answerPrompt(p, "allow")
		case "always", "add":
			return m.answerPrompt(p, "allow_always")
		case "prefix":
			return m.answerPromptPrefix(p, protocol.CommandPrefix(fullToolArg(p.Tool, p.Input)))
		case "skip":
			return m.answerPrompt(p, "deny")
		case "add_other":
			m.permEdit = "dir"
			m.dirInput.Placeholder = "path (absolute, ~, or relative to the session directory)"
			m.dirInput.SetValue(p.Dir)
			m.dirInput.CursorEnd()
			return m.dirInput.Focus()
		case "deny":
			m.permEdit = "deny"
			m.dirInput.Placeholder = "why not? (optional) · enter denies"
			m.dirInput.SetValue("")
			return m.dirInput.Focus()
		}
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
	case key.Matches(msg, keys.Select):
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
	case "/help", "/h", "/?":
		m.hideKeys = !m.hideKeys
		m.layout()
		if m.hideKeys {
			return m.setStatus("key bar hidden (/help shows it)", false)
		}
		return m.setStatus("key bar shown (/help hides it)", false)
	case "/tree":
		return m.toggleTree()
	case "/roles", "/role", "/presets":
		// The one role dialog: enter switches the selected agent's preset.
		// A name argument sets it directly.
		if c := needAgent(); c != nil {
			return c
		}
		if rest == "" {
			return rolesCmd(m.ctx, m.c, m.sessionID)
		}
		return pickRoleCmd(m.ctx, m.c, agent, strings.ToLower(rest))
	case "/sessions", "/resume", "/session":
		return sessionsCmd(m.ctx, m.c, m.session.Dir, false)
	case "/compact":
		if c := needAgent(); c != nil {
			return c
		}
		return tea.Batch(m.setStatus("compacting…", false), compactCmd(m.ctx, m.c, agent))
	case "/mode":
		return m.openMode()
	case "/yolo", "/auto":
		return m.toggleMode(name[1:], rest)
	case "/variants", "/variant":
		if c := needAgent(); c != nil {
			return c
		}
		return m.openVariants(rest)
	case "/queue":
		if c := needAgent(); c != nil {
			return c
		}
		if rest == "" {
			return m.setStatus("usage: /queue <text>", true)
		}
		return sendCmd(m.ctx, m.c, agent, protocol.KindPrompt, rest, "queued for after the current turn")
	case "/models", "/model":
		// The one model dialog: enter sets the selected agent's model, ctrl+s the session default.
		return modelsCmd(m.ctx, m.c)
	case "/providers", "/provider", "/connect", "/login":
		// The one provider dialog: sign in, re-sign in, sign out. A name
		// argument jumps straight to that provider's sign-in.
		return providersCmd(m.ctx, m.c, false, strings.ToLower(rest))
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
	case event.AgentKilled:
		if parent := m.parentOf[ev.Agent]; parent != "" {
			m.transcript(parent).ChildState(ev.Agent, "killed")
		}
		for _, t := range m.transcripts { // questions to it will never be answered
			t.AskerGone(ev.Agent)
		}
		if !m.loading {
			m.refreshViewport()
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
	case event.SessionYoloChanged: // legacy logs
		var p event.YoloPayload
		if ev.Decode(&p) == nil {
			m.session.Mode = protocol.ModeAsk
			if p.On {
				m.session.Mode = protocol.ModeYolo
			}
		}
		for _, a := range m.agents {
			m.transcript(a.ID).Apply(ev)
		}
		if !m.loading {
			m.refreshViewport()
		}
	case event.SessionModeChanged:
		var p event.ModePayload
		if ev.Decode(&p) == nil {
			m.session.Mode = p.Mode
		}
		// A session-wide switch with no agent of its own: note it in every
		// agent's chat, like model and role changes.
		for _, a := range m.agents {
			m.transcript(a.ID).Apply(ev)
		}
		if !m.loading {
			m.refreshViewport()
		}
	case event.TurnEnded:
		var p event.TurnEndedPayload
		if !m.loading && ev.Decode(&p) == nil && p.Reason == "error" &&
			(strings.Contains(p.Error, "not connected") || strings.Contains(p.Error, "/provider")) {
			cmds = append(cmds, m.setStatus("provider not connected — run /providers", true))
		}
	}

	// The parent's agent_create line follows the child's turns: yellow while
	// a turn runs, grey between turns.
	if parent := m.parentOf[ev.Agent]; parent != "" {
		switch ev.Type {
		case event.TurnStarted:
			m.transcript(parent).ChildState(ev.Agent, "running")
		case event.TurnEnded, event.TurnAborted:
			m.transcript(parent).ChildState(ev.Agent, "idle")
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
		event.MonitorStarted, event.MonitorFired, event.MonitorStopped,
		event.AgentDirAdded, event.AgentDirRemoved, event.TodoChanged,
		event.MCPStarted, event.MCPFailed, event.MCPStopped, event.ResponseReceived: // the tabs read these off the tree
		if !m.loading {
			cmds = append(cmds, m.markTreeDirty())
		}
	case event.ToolCallFinished: // a message or task just put another agent on the awaiting list
		var p event.ToolFinishedPayload
		if ev.Decode(&p) == nil && (p.Name == "agent_message" || p.Name == "agent_create") && !m.loading {
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
		if n.Prompt.Kind == "question" {
			return m.setFocus(focusQuestions)
		}
		return m.setFocus(focusPermission)
	}
	if m.focus == focusQuestions {
		m.q.bind(m.currentQuestion()) // a batch that changed under the dialog resets it
	}
	return nil
}

// currentPrompt is the head of the permission queue: the oldest waiting
// permission or trust prompt (questions have their own tab and queue).
func (m *Model) currentPrompt() *protocol.PromptInfo {
	for i := range m.prompts {
		if m.prompts[i].Kind != "question" {
			return &m.prompts[i]
		}
	}
	return nil
}

// currentQuestion is the oldest waiting ask_user batch.
func (m *Model) currentQuestion() *protocol.PromptInfo {
	for i := range m.prompts {
		if m.prompts[i].Kind == "question" {
			return &m.prompts[i]
		}
	}
	return nil
}

// promptCounts is how many permission-ish prompts and question batches wait.
func (m *Model) promptCounts() (perms, questions int) {
	for _, p := range m.prompts {
		if p.Kind == "question" {
			questions++
		} else {
			perms++
		}
	}
	return
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
	perms, questions := m.promptCounts()
	if perms == 0 && m.focus == focusPermission {
		m.closeDialog() // the last permission was answered: the dialog closes
	}
	if questions == 0 && m.focus == focusQuestions {
		m.closeDialog()
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

// denyPrompt denies a permission, passing the human's reason (may be empty).
func (m *Model) denyPrompt(p *protocol.PromptInfo, reason string) tea.Cmd {
	if p.Kind == "trust" {
		return m.answerPrompt(p, "deny") // trust has its own reply; no reason field
	}
	if m.promptBusy == p.ID {
		return m.setStatus("answer in flight…", false)
	}
	if p.ClaimedBy != "" && !m.claimedByUs[p.ID] {
		return m.setStatus("claimed by another client", true)
	}
	m.promptBusy = p.ID
	m.claimedByUs[p.ID] = true
	return denyPromptCmd(m.ctx, m.c, p.ID, reason)
}

// answerPromptPrefix allows the call and every command of the tool that
// starts with prefix for the rest of the session.
func (m *Model) answerPromptPrefix(p *protocol.PromptInfo, prefix string) tea.Cmd {
	if prefix == "" {
		return m.answerPrompt(p, "allow_always")
	}
	if m.promptBusy == p.ID {
		return m.setStatus("answer in flight…", false)
	}
	if p.ClaimedBy != "" && !m.claimedByUs[p.ID] {
		return m.setStatus("claimed by another client", true)
	}
	m.promptBusy = p.ID
	m.claimedByUs[p.ID] = true
	return allowPromptPrefixCmd(m.ctx, m.c, p.ID, prefix)
}

// answerPromptDir is allow_always on a boundary prompt with an edited
// directory: the call runs and that directory joins the agent's set.
func (m *Model) answerPromptDir(p *protocol.PromptInfo, dir string) tea.Cmd {
	if m.promptBusy == p.ID {
		return m.setStatus("answer in flight…", false)
	}
	if p.ClaimedBy != "" && !m.claimedByUs[p.ID] {
		return m.setStatus("claimed by another client", true)
	}
	m.promptBusy = p.ID
	m.claimedByUs[p.ID] = true
	return answerPromptDirCmd(m.ctx, m.c, p.ID, dir)
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
	case key.Matches(msg, keys.Select):
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
	m.input.SetWidth(boxW)
	m.input.SetHeight(m.inputCap())                                                            // the textarea is always cap tall; the view trims to the rows used
	m.promptInput.Width = dialogWidth(m.width) - 4 - 2 - len([]rune(m.promptInput.Prompt)) - 1 // inside the tab dialog, under promptBox's indent
	m.dirInput.Width = dialogWidth(m.width) - 4 - 2 - len([]rune(m.dirInput.Prompt)) - 1

	_, kb := m.keyBarView()
	bodyH := m.height - kb - 2 - (m.inputRows() + 1) // key bar, status line + rule, input rows + meta row
	if sv := m.sectionsView(m.width); sv != "" {
		bodyH -= strings.Count(sv, "\n") + 1 + 1 // plus the blank line between the input and the meta row
	}
	if pv := m.paletteViewFor(m.width); pv != "" {
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
	working, waiting, verb, stats, active := false, false, "", "", ""
	if t := m.transcripts[m.selectedID()]; t != nil && t.InTurn() {
		working, verb = true, t.TurnVerb()
		stats = turnStats(t.TurnStats(time.Now()))
		active = m.activeTodo()
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
		Active:   active,
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
	if m.ov == nil {
		m.dialogFrom = m.focus // one overlay replacing another keeps the original origin
	}
	m.ov = o
	m.input.Blur()
	return o.input.Focus()
}

// closeOverlay drops the overlay and gives focus back to what had it when
// the overlay opened (the input, the meta row…). The section's focus was
// never changed by the overlay; only the blurred input needs refocusing.
func (m *Model) closeOverlay() tea.Cmd {
	m.ov = nil
	from := m.dialogFrom
	if isTab(from) || !m.focusAvailable(from) {
		from = focusInput
	}
	if from != m.focus {
		return m.setFocus(from)
	}
	if from == focusInput {
		return m.input.Focus()
	}
	return nil
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
	case key.Matches(msg, keys.Select):
		return m.overlaySubmit(false)
	case key.Matches(msg, keys.OvSelect):
		return m.closeOverlayToInput() // enter: back to typing, nothing picked
	case key.Matches(msg, keys.OvAlt):
		return m.overlaySubmit(true)
	case key.Matches(msg, keys.OvRemove):
		return m.overlayRemove()
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
	case ovMode:
		it := o.selected()
		if it == nil {
			return nil
		}
		return tea.Batch(m.closeOverlay(), setModeCmd(m.ctx, m.c, m.sessionID, it.id))
	case ovSessions:
		it := o.selected()
		if it == nil {
			return nil
		}
		if it.id == m.sessionID {
			return tea.Batch(m.closeOverlay(), m.setStatus("already in this session", false))
		}
		return tea.Batch(m.closeOverlay(), switchSessionCmd(m.ctx, m.c, m.sessionID, it.id))
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
	ov := newOverlay(ovMethods, overlayList, "Login method")
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
		cmd = m.openOverlay(newOverlay(ovProviders, overlayLogin, ""))
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
	o := newOverlay(ovProviders, overlayList, "Providers")
	o.setItems(providerItems(msg.res.Providers))
	cmd := m.openOverlay(o)
	switch {
	case msg.jump != "":
		return tea.Batch(cmd, m.setStatus("unknown provider "+msg.jump, true))
	case msg.status != "":
		return tea.Batch(cmd, m.setStatus(msg.status, false))
	}
	return cmd
}

// overlayRemove is ctrl+d in a list overlay: in the providers dialog it
// signs out of the selected provider.
func (m *Model) overlayRemove() tea.Cmd {
	o := m.ov
	if o == nil || o.kind != ovProviders {
		return nil
	}
	it := o.selected()
	if it == nil {
		return nil
	}
	for _, p := range m.providers {
		if p.ID == it.id {
			if !p.Connected {
				return m.setStatus(it.label+" is not signed in", true)
			}
			return disconnectProviderCmd(m.ctx, m.c, p.ID)
		}
	}
	return nil
}

func (m *Model) onRoles(msg rolesMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("roles: "+msg.err.Error(), true)
	}
	label := m.agentLabel(m.selectedID())
	o := newOverlay(ovRoles, overlayList, "Change role of "+label)
	// The main agent may take primary or all roles, a subagent subagent or
	// all ones: a role's mode decides where it shows.
	primary := true
	if a := m.selectedAgent(); a != nil && a.Parent != "" {
		primary = false
	}
	items := make([]overlayItem, 0, len(msg.roles))
	for _, r := range msg.roles {
		if (primary && r.Mode == "subagent") || (!primary && r.Mode == "primary") {
			continue
		}
		items = append(items, overlayItem{id: r.Name, label: r.Name, hint: roleHint(r)})
	}
	if len(items) == 0 {
		o.setEmpty("no role may run here", false)
	}
	o.setItems(items)
	return m.openOverlay(o)
}

// roleHint is a role's one-line summary in the /roles picker: its
// description, then its mode when restricted, its default model when it
// has a whitelist, and what it spawns.
func roleHint(r protocol.PresetInfo) string {
	hint := r.Description
	if r.Mode != "" && r.Mode != "all" {
		hint += "  · " + r.Mode
	}
	if len(r.Models) > 0 {
		short, _ := splitModel(r.Models[0].ID)
		hint += "  · " + short
		if len(r.Models) > 1 {
			hint += fmt.Sprintf(" +%d", len(r.Models)-1)
		}
	}
	if len(r.Spawn) > 0 {
		hint += "  · spawns " + strings.Join(r.Spawn, ", ")
	}
	return hint
}

// roleInfo finds a cached role by name.
func (m *Model) roleInfo(name string) *protocol.PresetInfo {
	for i := range m.presets {
		if m.presets[i].Name == name {
			return &m.presets[i]
		}
	}
	return nil
}

// selectedRole is the selected agent's role, nil when unknown.
func (m *Model) selectedRole() *protocol.PresetInfo {
	if a := m.selectedAgent(); a != nil {
		return m.roleInfo(a.Archetype)
	}
	return nil
}

// roleTints maps every cached role to its colour name ("" for none).
func (m *Model) roleTints() map[string]string {
	out := map[string]string{}
	for _, r := range m.presets {
		if r.Color != "" {
			out[r.Name] = r.Color
		}
	}
	return out
}

// roleModelSpec is the whitelist entry of role r that admits model id
// (glob-aware), nil when r has no whitelist or none matches.
func roleModelSpec(r *protocol.PresetInfo, id string) *protocol.ModelSpec {
	if r == nil {
		return nil
	}
	for i := range r.Models {
		if ok, _ := path.Match(r.Models[i].ID, id); ok || r.Models[i].ID == id {
			return &r.Models[i]
		}
	}
	return nil
}

// roleAllowsModel: any model without a whitelist, else a matching entry.
func roleAllowsModel(r *protocol.PresetInfo, id string) bool {
	return r == nil || len(r.Models) == 0 || roleModelSpec(r, id) != nil
}

// onSessions opens the /sessions picker: this directory's sessions, newest
// first, each titled by its first prompt. Sessions nobody has prompted are
// left out (except the current one): there is nothing to resume there.
func (m *Model) onSessions(msg sessionsMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("sessions: "+msg.err.Error(), true)
	}
	o := newOverlay(ovSessions, overlayList, "Sessions in "+shortHome(m.session.Dir))
	items := make([]overlayItem, 0, len(msg.sessions))
	for _, s := range msg.sessions {
		if s.Title == "" && s.ID != m.sessionID {
			continue // never prompted: nothing to resume
		}
		items = append(items, sessionItem(s, s.ID == m.sessionID))
	}
	if len(items) == 0 {
		o.setEmpty("no sessions here yet", false)
	}
	o.setItems(items)
	return m.openOverlay(o)
}

// sessionItem is one row of the /sessions picker: the first prompt (or
// "(empty session)") with when it started, its model, cost and live agents.
func sessionItem(s protocol.SessionInfo, current bool) overlayItem {
	label := s.Title
	if label == "" {
		label = "(empty session)"
	}
	var meta []string
	if t, err := time.Parse(time.RFC3339, s.Created); err == nil {
		meta = append(meta, fmtElapsed(time.Since(t))+" ago")
	}
	if s.Model != "" {
		meta = append(meta, s.Model)
	}
	if s.CostUSD > 0 {
		meta = append(meta, "$"+fmtCost(s.CostUSD))
	}
	if s.Live > 0 {
		meta = append(meta, fmt.Sprintf("%d live", s.Live))
	}
	if current {
		meta = append(meta, "current")
	}
	return overlayItem{id: s.ID, label: truncRunes(label, 60), hint: strings.Join(meta, " · "), good: current}
}

// bindSession rebinds the TUI to another session: every per-session
// piece of state starts over and a fresh reconcile replays its history.
func (m *Model) bindSession(info protocol.SessionInfo) tea.Cmd {
	m.sessionID = info.ID
	m.session = info
	m.agents = nil
	m.selected = 0
	m.transcripts = map[string]*Transcript{}
	m.seq = 0
	m.prompts = nil
	m.claimedByUs = map[string]bool{}
	m.promptBusy = ""
	m.spawned = map[string]time.Time{}
	m.parentOf = map[string]string{}
	m.expanded = nil
	m.itemRows = nil
	m.chatCursor = 0
	m.agCursor = 0
	m.cancelArmed = time.Time{}
	m.quitArmed = time.Time{}
	m.reconciled = false
	m.loading = false
	m.replayTo = 0
	m.follow = true
	m.input.Reset()
	m.refreshViewport()
	m.layout()
	return tea.Batch(m.setFocus(focusInput), reconcileCmd(m.ctx, m.c, m.sessionID), m.setStatus("resumed "+shortID(info.ID), false))
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// openVariants is /variants: with no argument it opens the picker for the
// selected agent's model; with a name it sets that variant ("default"
// clears it).
func (m *Model) openVariants(arg string) tea.Cmd {
	a := m.selectedAgent()
	if a == nil {
		return m.setStatus("no agent selected", true)
	}
	modelID, current := m.session.Model, a.Variant
	if a.Model != "" {
		modelID = a.Model
	}
	if modelID == "" {
		return m.setStatus("no model selected — /models first", true)
	}
	if arg == "" {
		return variantsCmd(m.ctx, m.c, modelID, current)
	}
	v := strings.ToLower(arg)
	if v == "default" || v == "none" || v == "off" {
		v = ""
	}
	return pickVariantCmd(m.ctx, m.c, a.ID, v)
}

// onVariants opens the /variants picker: the provider default plus every
// variant the model offers, the one in force marked.
func (m *Model) onVariants(msg variantsMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("variants: "+msg.err.Error(), true)
	}
	label := m.agentLabel(m.selectedID())
	o := newOverlay(ovVariants, overlayList, "Variant for "+label+" · "+msg.model)
	if len(msg.variants) == 0 {
		o.setEmpty("this model has no variants", false)
	}
	mark := func(id string) string {
		if id == msg.current {
			return "  · current"
		}
		return ""
	}
	// A role that lists variants for this model narrows the picker to them
	// (and drops the provider default, which is then not allowed).
	var allowed []string
	if spec := roleModelSpec(m.selectedRole(), msg.model); spec != nil && len(spec.Variants) > 0 {
		allowed = spec.Variants
	}
	var items []overlayItem
	if allowed == nil {
		items = append(items, overlayItem{id: "", label: "default", hint: "provider default" + mark("")})
	}
	for _, v := range msg.variants {
		if allowed != nil && !containsStr(allowed, v) {
			continue
		}
		items = append(items, overlayItem{id: v, label: v, hint: "reasoning effort" + mark(v)})
	}
	o.setItems(items)
	return m.openOverlay(o)
}

func (m *Model) onModels(msg modelsMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("models: "+msg.err.Error(), true)
	}
	o := newOverlay(ovModels, overlayList, "Select a model")
	if len(msg.models) == 0 {
		o.setEmpty("no providers connected — run /provider", true)
	}
	// The selected agent's role may whitelist models: only those are offered.
	models := msg.models
	if r := m.selectedRole(); r != nil && len(r.Models) > 0 {
		o.title = "Select a model · allowed by " + r.Name
		models = nil
		for _, mi := range msg.models {
			if roleAllowsModel(r, mi.ID) {
				models = append(models, mi)
			}
		}
		if len(models) == 0 && len(msg.models) > 0 {
			o.setEmpty("role "+r.Name+" allows none of the connected models", true)
		}
	}
	o.setItems(modelItems(models))
	return m.openOverlay(o)
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// shortHome abbreviates the home directory prefix.
func shortHome(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}
