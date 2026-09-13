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
	showTree      bool      // right sidebar toggle (/tree, ctrl+b)
	hideKeys      bool      // the key bar (divider + legend) at the bottom is hidden; /help shows it
	cancelArmed   time.Time // when esc was last pressed on an empty input while the agent was busy; a second esc within cancelWindow cancels
	hoverFocus    bool      // the chat has focus because the mouse is over it (released when the mouse leaves)
	hoverFrom     focus     // where focus was before hover took it, restored when the mouse leaves the chat
	sel           selection // mouse text selection (drag to select, release to copy)
	metaSel       metaPart  // the highlighted part of the meta row while it has focus
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
	focusMeta                    // the meta row under the input: ←/→ pick yolo/role/model/variant, enter opens it
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

// inputMaxLines caps how tall the input grows before it scrolls inside.
const inputMaxLines = 8

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
	ta.MaxHeight = inputMaxLines
	ta.SetHeight(inputMaxLines)
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
		cmds = append(cmds, subscribeCmd(m.ctx, m.c, m.sessionID, 0), sessionsCmd(m.ctx, m.c, m.session.Dir, true))

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
		cmds = append(cmds, m.onRoles(msg))
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
// (once there is one), the tab strip (as one stop, see defaultTab), the
// input, and the sidebar (while visible).
func (m *Model) focusOrder() []focus {
	order := make([]focus, 0, 4)
	if !m.isHome() {
		order = append(order, focusChat)
	}
	order = append(order, focusMeta) // top to bottom: the meta row sits under the rule
	if m.stripShown() {
		order = append(order, focusTabs)
	}
	order = append(order, focusInput)
	if m.sidebarVisible() {
		order = append(order, focusSidebar)
	}
	return order
}

// stripShown reports whether the permission/agents/async strip is drawn:
// always in a session, and on the home (logo) screen only once something
// is in it, so a trust prompt or an early child is still reachable.
func (m *Model) stripShown() bool {
	if !m.isHome() {
		return true
	}
	return m.currentPrompt() != nil || len(m.liveChildren())+len(m.runningJobs()) > 0
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
	case focusPermission:
		if p := m.currentPrompt(); p != nil && p.Kind == "question" {
			return m.promptInput.Focus()
		}
	case focusSidebar:
		m.sbCursor = m.selected
	case focusAgents, focusAsync:
		m.agCursor = 0
	case focusMeta:
		if !m.metaHas(m.metaSel) {
			m.metaSel = metaRole
		}
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
			return m.mouseClick(msg.X, msg.Y)
		}
		text := m.selectedText()
		if text == "" {
			m.sel.active = false
			return nil
		}
		return tea.Batch(copyCmd(text), m.setStatus(fmt.Sprintf("copied %d characters", len([]rune(text))), false))
	case msg.Action == tea.MouseActionMotion:
		return m.mouseHover(msg.X, msg.Y)
	}
	return nil
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
	if rows > inputMaxLines {
		rows = inputMaxLines
	}
	return rows
}

// inputView is the textarea trimmed to the rows the message needs.
func (m Model) inputView() string {
	lines := strings.Split(m.input.View(), "\n")
	if n := m.inputRows(); len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// mouseHover is mouse movement: over a chat item it does what ↑/↓ do (the
// chat takes focus, the cursor moves to that item, it previews); leaving
// the chat area gives focus back to the input, without scrolling. Focus the
// keyboard took is left alone.
func (m *Model) mouseHover(x, y int) tea.Cmd {
	if m.ov != nil {
		if idx, ok := m.ov.itemAt(x, y, m.width, m.bodyHeight(), m.sp.View()); ok {
			m.ov.cursor = idx
		}
		return nil
	}
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
	if m.ov != nil {
		if idx, ok := m.ov.itemAt(x, y, m.width, m.bodyHeight(), m.sp.View()); ok {
			m.ov.cursor = idx
			return m.overlaySubmit(false)
		}
		return nil
	}
	if m.isHome() {
		return m.setFocus(focusInput) // the input is the only thing to click on the logo screen
	}
	if x < 0 || y < 0 {
		return nil
	}
	if m.sidebarVisible() && x >= m.contentWidth() {
		return m.setFocus(focusSidebar)
	}
	if x >= m.contentWidth() {
		return nil
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
	case y == lay.strip: // a tab label opens that tab
		if f, ok := m.tabAt(x); ok {
			return m.setFocus(f)
		}
	case y > lay.strip && y <= lay.stripEnd: // a row inside the open tab
		if isTab(m.focus) && m.focus != focusPermission {
			m.agCursor = y - lay.strip - 1
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

// metaParts lists the meta row's parts in order, YOLO only while it is on.
func (m *Model) metaParts() []metaPart {
	parts := []metaPart{}
	if m.session.Yolo {
		parts = append(parts, metaYolo)
	}
	return append(parts, metaRole, metaModel, metaVariant)
}

func (m *Model) metaHas(p metaPart) bool {
	for _, q := range m.metaParts() {
		if q == p {
			return true
		}
	}
	return false
}

// metaAction is what a part of the meta row does when picked, by click or
// enter: YOLO turns yolo off; the role, model and variant open their
// dialogs.
func (m *Model) metaAction(part metaPart) tea.Cmd {
	switch part {
	case metaYolo:
		return setYoloCmd(m.ctx, m.c, m.sessionID, false)
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
	case key.Matches(msg, keys.Submit):
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
	metaYolo             // the YOLO tag: click turns yolo off
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
	if m.session.Yolo {
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

// rowLayout is where the session view's pieces sit, in screen rows.
type rowLayout struct {
	strip    int // the tab strip line
	stripEnd int // last row of the strip block (its body when a tab is open)
	input    int // first row of the input (it may span several)
	meta     int // the meta row (right under the rule)
}

// rows derives the row layout the same way sessionView stacks its parts.
func (m *Model) rows() rowLayout {
	meta := m.vp.Height + 2 // blank line, then the rule, then the meta row
	y := meta + 1           // the strip
	lay := rowLayout{meta: meta, strip: y, stripEnd: y}
	if sv := m.sectionsView(m.contentWidth()); sv != "" {
		lay.stripEnd = y + strings.Count(sv, "\n")
		y = lay.stripEnd + 2 // blank line after the strip block
	}
	if pv := m.paletteViewFor(m.contentWidth()); pv != "" {
		y += strings.Count(pv, "\n") + 1
	}
	lay.input = y
	return lay
}

// tabAt maps an x position on the strip to the tab label under it. The
// labels are laid out as sectionTabs draws them: permission, agents, async,
// separated by " · ".
func (m *Model) tabAt(x int) (focus, bool) {
	perm := fmt.Sprintf("permission (%d)", len(m.prompts))
	if p := m.currentPrompt(); p != nil && p.Kind != "permission" {
		perm = fmt.Sprintf("%s (%d)", p.Kind, len(m.prompts))
	}
	labels := []struct {
		text string
		f    focus
	}{
		{perm, focusPermission},
		{fmt.Sprintf("agents (%d)", len(m.liveChildren())), focusAgents},
		{fmt.Sprintf("async (%d)", len(m.runningJobs())), focusAsync},
	}
	x0 := 0
	for _, l := range labels {
		w := ansi.StringWidth(l.text)
		if x >= x0 && x < x0+w {
			return l.f, true
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
	case focusMeta:
		return m.metaKey(msg)
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
		if m.input.Line() > 0 { // inside a multi-line draft ↑ moves up a line
			break
		}
		m.historyMove(-1)
		return nil
	case key.Matches(msg, keys.SelDown):
		if pm := paletteMatches(m.input.Value()); len(pm) > 0 {
			m.palIdx = (m.palIdx + 1) % len(pm)
			return nil
		}
		if m.input.Line() < m.input.LineCount()-1 { // ↓ moves down a line until the last
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
	m.input.SetWidth(boxW)
	m.promptInput.Width = boxW - 4 - len([]rune(m.promptInput.Prompt)) - 1

	_, kb := m.keyBarView()
	bodyH := m.height - kb - 2 - (m.inputRows() + 1) // key bar, blank + chat rule, input rows + meta row
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
	o := newOverlay(ovProviders, overlayList, "Providers", "enter: sign in · ctrl+d: sign out · esc: close")
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

// onSessions opens the /sessions picker: this directory's sessions, newest
// first, each titled by its first prompt. Sessions nobody has prompted are
// left out (except the current one): there is nothing to resume there.
func (m *Model) onSessions(msg sessionsMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("sessions: "+msg.err.Error(), true)
	}
	o := newOverlay(ovSessions, overlayList, "Sessions in "+shortHome(m.session.Dir), "enter: resume where it left off · esc: close")
	items := make([]overlayItem, 0, len(msg.sessions))
	for _, s := range msg.sessions {
		if s.Title == "" && s.ID != m.sessionID {
			continue // never prompted: nothing to resume
		}
		items = append(items, sessionItem(s, s.ID == m.sessionID))
	}
	if len(items) == 0 {
		o.setInfo("no sessions here yet", false)
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
