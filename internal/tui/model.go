// Package tui is the Bubble Tea terminal frontend (PRD §7.2). It is the
// reference client for the protocol: everything it shows comes from the event
// stream, and every action it takes is a protocol call issued as a tea.Cmd.
package tui

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
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
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/toolname"
	"github.com/nicodes/stavlos/internal/tui/dialog"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/render"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/internal/tui/transcript"
	"github.com/nicodes/stavlos/pkg/client"
)

const (
	statusDuration = 5 * time.Second
	selectDuration = 2 * time.Second // "→ label" flash when the sidebar is hidden
)

// Run drives the TUI for one channel until the user quits. c is already
// attached (tier interactive). Returns nil on a clean quit.
func Run(ctx context.Context, c *client.Client, channelID string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	m := newModel(ctx, c, channelID)
	m.superChat = true // the channel chat is the default view
	m.input.Placeholder = m.placeholder()
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseAllMotion())
	go forwardNotifications(ctx, c, p)

	final, err := p.Run()

	// Best-effort unsubscribe; the daemon drops it on disconnect anyway.
	select {
	case <-c.Closed():
	default:
		uctx, ucancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.Unsubscribe(uctx, channelID)
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

// Model is the Bubble Tea model for one channel. Update only performs state
// transitions; every daemon interaction is a tea.Cmd from commands.go.
type Model struct {
	ctx context.Context
	c   *client.Client

	channelState // the bound channel; replaced whole when switching
	uiPrefs      // display choices; they survive a switch
	dialogs      // the open overlay and sign-in
	promptState  // what waits on the human, in every channel; survives a switch

	navChannels []protocol.ChannelInfo // the sidebar's channels section: other channels of this directory, newest first
	dirsNext    bool                   // another channel's gear was chosen: its dirs dialog opens once the switch lands

	presets []protocol.PresetInfo

	vp    viewport.Model
	input textarea.Model // grows with the text, up to inputMaxLines
	sp    spinner.Model

	width, height int
	hoverFocus    bool      // the chat has focus because the mouse is over it (released when the mouse leaves)
	hoverFrom     focus     // where focus was before hover took it, restored when the mouse leaves the chat
	sel           selection // mouse text selection (drag to select, release to copy)
	metaSel       metaPart  // the highlighted part of the meta row while it has focus
	tabSel        int       // the highlighted tab (index into tabFocuses) while the strip has focus
	follow        bool      // auto-scroll to bottom

	status      string
	statusErr   bool
	statusToken int
	compactTick bool // the compaction animation tick is scheduled (a transcript has a running compaction)
	treeTimer   bool // a debounced tree refresh is scheduled

	// Keyboard focus (tab / shift+tab cycle the sections).
	focus       focus
	promptInput textinput.Model // answer field of a question prompt
	dirInput    textinput.Model // path field of the dirs dialog while adding or editing
	sbCursor    int
	palIdx      int      // highlighted row in the "/" command palette
	history     []string // prompts sent from this client (and replayed human prompts)
	histIdx     int      // == len(history) when editing a new line
	histDraft   string   // unsent text saved while browsing history

	fatal error
}

// channelState is everything that belongs to the bound channel. Switching
// channels replaces it whole (bindChannel), so nothing of the previous
// channel (a half-answered question, an armed esc, a cursor) leaks into
// the next.
type channelState struct {
	channelID   string
	channel     protocol.ChannelInfo
	agents      []protocol.AgentInfo // pre-order, root first
	selected    int
	spawned     map[string]time.Time // agent id → spawn time, for the agents block
	parentOf    map[string]string    // child agent id → parent id, for the parent's agent_create line
	transcripts map[string]*transcript.Transcript
	renders     map[string]*render.Cache // per agent: rendered rows of its transcript's items
	superChat   bool                     // the channel chat is shown instead of the selected agent's own (docs/super-chat.md)
	opened      bool                     // reached from inside the TUI (+ channel, the sidebar, the picker): its chat shows even while empty, never the splash
	seq         int64
	loading     bool  // replaying events up to replayTo
	replayTo    int64 // seq from reconcile
	reconciled  bool  // the first reconcile landed

	dirEdit string          // "" | "add" | the path being replaced
	mcpOpen map[string]bool // MCP servers whose tool list is expanded in the mcp dialog

	// The chat cursor walks transcript items; expanded holds per-item tool
	// output overrides keyed by agent id; itemRows maps items to rendered
	// viewport rows.
	chatCursor int
	expanded   map[string]map[int]bool
	itemRows   map[int]render.RowRange
	agCursor   int // highlighted row in the open list dialog

	cancelArmed time.Time // when esc was last pressed on an empty input while the agent was busy; a second esc within cancelWindow cancels
	quitArmed   time.Time // when ctrl+c was last pressed; a second within cancelWindow quits
}

// newChannelState is the state of channel id before anything is known
// about it but info.
func newChannelState(id string, info protocol.ChannelInfo) channelState {
	return channelState{
		channelID: id, channel: info,
		spawned: map[string]time.Time{}, parentOf: map[string]string{},
		transcripts: map[string]*transcript.Transcript{}, renders: map[string]*render.Cache{},
	}
}

// promptState is what waits on the human across every channel (the daemon
// sends every channel's prompts) and where the human is in answering it.
type promptState struct {
	prompts     []protocol.PromptInfo // pending, oldest first, every channel's
	claimedByUs map[string]bool
	promptBusy  string        // prompt id with a claim/reply in flight
	permSel     int           // highlighted option of the permission dialog
	permFor     string        // the prompt id permSel belongs to (a new prompt starts at the top)
	permEdit    string        // "" | "deny" (reason row open) | "dir" (path row open) in the permission dialog
	q           questionState // the questions dialog: where the human is in the current batch
	scope       promptScope   // what the open permission or questions dialog is limited to; zero = every channel
}

// promptScope limits the permission and questions dialogs to one channel's
// prompts, or one agent's: set when opening a channel or agent that waits on
// the human opens its dialog, zero when a tab opens it.
type promptScope struct{ channel, agent string }

// holds reports whether p is within the scope.
func (s promptScope) holds(p protocol.PromptInfo) bool {
	return (s.channel == "" || p.Channel == s.channel) && (s.agent == "" || p.Agent == s.agent)
}

// uiPrefs are the user's display choices.
type uiPrefs struct {
	showTree bool // the sidebar (/tree, ctrl+b)
	hideKeys bool // the key bar (divider + legend) at the bottom is hidden; /help shows it
	details  bool // expanded tool output (/details)
}

// dialogs is the modal state not tied to a channel.
type dialogs struct {
	ov         *overlay                // open modal, or nil
	dialogFrom focus                   // what had focus when the open dialog (a tab's or an overlay) was opened; closing returns there
	providers  []protocol.ProviderInfo // last provider.list result
	login      loginFlow               // device-code sign-in in progress
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
	focusDue                     // the due tab: who is waiting on the selected agent's reply
	focusTodo                    // the todo tab: the selected agent's todo list
	focusMCP                     // the mcp tab: the selected agent's MCP servers
	focusDirs                    // the dirs tab: the channel's working directories (every agent's)
	focusSidebar                 // the agent tree (↑/↓ enter)
	focusTabs                    // the tab strip: ←/→ highlight a tab, enter opens its dialog
	focusMeta                    // the meta row under the input: ←/→ pick yolo/role/model/variant, enter opens it
)

// tabFocuses are the tabs of the strip under the chat, left to right. They
// are one stop in the tab cycle; ←/→ move between them.
// tabRows is the strip's two rows: on top what belongs to the whole channel
// (the prompt queue every agent adds to, the working directories every agent
// shares), below what belongs to the selected
// agent.
var tabRows = [][]focus{
	{focusPermission, focusQuestions, focusDirs},
	{focusAsync, focusDue, focusTodo, focusMCP},
}

// tabFocuses is every tab in strip order: the top row, then the bottom.
var tabFocuses = slices.Concat(tabRows...)

// tabLayout is the tab rows as they are drawn: tabRows while the sidebar is
// hidden; with it showing, ! and ? on top (in the sidebar, above the
// channels) and dirs out of the tabs, behind each channel's gear instead,
// since the directories are that channel's.
func (m Model) tabLayout() [][]focus {
	top := tabRows[0]
	if m.sidebarVisible() {
		top = []focus{focusPermission, focusQuestions}
	}
	rows := [][]focus{top}
	if !m.superChat { // async · due · todo · mcp are an agent's: its own chat has them, the channel chat does not
		rows = append(rows, tabRows[1])
	}
	return rows
}

// tabOrder is tabLayout's tabs in order: what the strip's highlight walks.
func (m Model) tabOrder() []focus { return slices.Concat(m.tabLayout()...) }

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
	bg := lipgloss.NewStyle().Background(theme.ColInputBg)
	ta.FocusedStyle.Base, ta.BlurredStyle.Base = bg, bg
	ta.FocusedStyle.CursorLine, ta.BlurredStyle.CursorLine = bg, bg
	ta.FocusedStyle.EndOfBuffer, ta.BlurredStyle.EndOfBuffer = bg, bg
	ta.FocusedStyle.Text, ta.BlurredStyle.Text = bg, bg
	ta.FocusedStyle.Placeholder, ta.BlurredStyle.Placeholder = theme.StyleDim.Background(theme.ColInputBg), theme.StyleDim.Background(theme.ColInputBg)
	// The prompt chevron carries the focus colour (there is no box border).
	ta.FocusedStyle.Prompt, ta.BlurredStyle.Prompt = theme.StyleBorderUser.Background(theme.ColInputBg), theme.StyleBorderMuted.Background(theme.ColInputBg)
	return ta
}

func newModel(ctx context.Context, c *client.Client, channelID string) Model {
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

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(theme.StyleRunning))

	pi := textinput.New()
	pi.Prompt = "› "
	pi.Placeholder = "answer"
	di := textinput.New()
	di.Prompt = "› "
	di.Placeholder = "path (absolute, ~, or relative to the channel directory)"

	m := Model{
		ctx:          ctx,
		c:            c,
		channelState: newChannelState(channelID, protocol.ChannelInfo{}),
		promptState:  promptState{claimedByUs: map[string]bool{}},
		uiPrefs:      uiPrefs{hideKeys: true}, // the key bar is off until /help
		vp:           vp,
		input:        ti,
		promptInput:  pi,
		dirInput:     di,
		sp:           sp,
		follow:       true,
	}
	m.loading = true // until the reconcile lands
	return m
}

// Init starts the cursor blink, the spinner, the placeholder cycle and the
// reconcile snapshot.
func (m Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, textarea.Blink, m.sp.Tick, placeholderTickCmd(), reconcileCmd(m.ctx, m.c, m.channelID))
}

// Update is the single-threaded state machine.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmds, quit := m.update(msg)
	if quit {
		return m, tea.Quit
	}
	cmds = append(cmds, m.ensureFocus())
	m.layout()
	return m, tea.Batch(cmds...)
}

// update applies one message; quit reports a fatal condition (m.fatal says
// which).
func (m *Model) update(msg tea.Msg) (cmds []tea.Cmd, quit bool) {
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
		cmds = append(cmds, cmd, m.mouse(msg))
	case spinner.TickMsg, placeholderTickMsg, compactTickMsg, treeTickMsg, clearStatusMsg:
		cmds = append(cmds, m.onTick(msg))
	case reconcileMsg, subscribedMsg, eventMsg, streamMsg, promptMsg, disconnectedMsg, treeMsg, resultMsg, promptReplyMsg:
		return m.onDaemon(msg)
	case providersMsg, loginStartMsg, loginDoneMsg, rolesMsg, variantsMsg, channelsMsg, switchedMsg, modelsMsg:
		cmds = append(cmds, m.onListed(msg))
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
	return cmds, false
}

// onTick handles the timers: the spinner, the placeholder cycle, the
// compaction bar, the debounced tree refresh and the status line expiry.
func (m *Model) onTick(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		// The "working…" indicator and the chat's loaders carry the
		// spinner, so redraw while either shows.
		if t := m.transcripts[m.viewID()]; t != nil && (t.InTurn() || t.Running() || len(t.Waiting()) > 0) {
			m.refreshViewport()
		}
		return cmd
	case placeholderTickMsg:
		m.input.Placeholder = m.placeholder()
		return placeholderTickCmd()
	case compactTickMsg:
		if !m.anyCompacting() {
			m.compactTick = false
			return nil
		}
		if t := m.transcripts[m.selectedID()]; t != nil && t.Compacting() {
			m.refreshViewport()
		}
		return compactTickCmd()
	case treeTickMsg:
		m.treeTimer = false
		return treeCmd(m.ctx, m.c, m.channelID)
	case clearStatusMsg:
		if msg.token == m.statusToken {
			m.status = ""
		}
	}
	return nil
}

// onDaemon handles what the daemon sends: the reconcile snapshot, events,
// streams, prompts, tree refreshes and call results. quit reports a lost
// connection or a failed attach.
func (m *Model) onDaemon(msg tea.Msg) (cmds []tea.Cmd, quit bool) {
	switch msg := msg.(type) {
	case reconcileMsg:
		if msg.err != nil {
			m.fatal = fmt.Errorf("reconcile: %w", msg.err)
			return nil, true
		}
		m.channel = cleanChannel(msg.res.Channel)
		m.reconciled = true
		m.setAgents(msg.res.Agents)
		for _, p := range msg.res.Prompts {
			m.upsertPrompt(p)
		}
		m.replayTo = msg.res.Seq
		m.loading = msg.res.Seq > 0
		return []tea.Cmd{subscribeCmd(m.ctx, m.c, m.channelID, 0), channelsCmd(m.ctx, m.c, m.channel.Dir, channelsHistory), rolesCmd(m.ctx, m.c, m.channelID, true)}, false
	case subscribedMsg:
		if msg.err != nil {
			m.fatal = fmt.Errorf("subscribe: %w", msg.err)
			return nil, true
		}
	case eventMsg:
		return []tea.Cmd{m.applyEvent(msg.ev)}, false
	case streamMsg:
		if msg.n.Channel == "" || msg.n.Channel == m.channelID {
			m.transcript(msg.n.Agent).ApplyStream(msg.n)
			if msg.n.Agent == m.viewID() {
				m.refreshViewport()
			}
		}
	case promptMsg:
		return []tea.Cmd{m.applyPromptNotification(msg.n)}, false
	case disconnectedMsg:
		m.fatal = msg.err
		m.status, m.statusErr = "daemon disconnected", true
		return nil, true
	case treeMsg:
		if msg.err != nil {
			return []tea.Cmd{m.setStatus("tree: "+msg.err.Error(), true)}, false
		}
		m.setAgents(msg.agents)
		if m.sidebarVisible() {
			return []tea.Cmd{channelsCmd(m.ctx, m.c, m.channel.Dir, channelsNav)}, false
		}
	case resultMsg:
		if msg.err != nil {
			return []tea.Cmd{m.setStatus(msg.err.Error(), true)}, false
		}
		if msg.ok != "" {
			return []tea.Cmd{m.setStatus(msg.ok, false)}, false
		}
	case promptReplyMsg:
		return []tea.Cmd{m.onPromptReply(msg)}, false
	}
	return nil, false
}

// onPromptReply settles an answer the daemon accepted or refused.
func (m *Model) onPromptReply(msg promptReplyMsg) tea.Cmd {
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
		return m.setStatus("claimed by another client", true)
	default:
		delete(m.claimedByUs, msg.id)
		return m.setStatus("prompt: "+msg.err.Error(), true)
	}
	return nil
}

// onListed handles list and sign-in results: providers, logins, roles,
// variants, channels, a resumed channel and models.
func (m *Model) onListed(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case providersMsg:
		return m.onProviders(msg)
	case loginStartMsg:
		return m.onLoginStart(msg)
	case loginDoneMsg:
		return m.onLoginDone(msg)
	case rolesMsg:
		if msg.err == nil {
			m.presets = msg.roles
		}
		if !msg.quiet {
			return m.onRoles(msg)
		}
	case variantsMsg:
		return m.onVariants(msg)
	case channelsMsg:
		return m.onChannelsListed(msg)
	case switchedMsg:
		if msg.err != nil {
			m.dirsNext = false
			return m.setStatus("channel: "+msg.err.Error(), true)
		}
		cmd := m.bindChannel(cleanChannel(msg.info))
		m.opened = true // switched to from inside the TUI: the channel's chat, not the splash
		if m.dirsNext { // reached through its gear: its dirs dialog
			m.dirsNext = false
			return tea.Batch(cmd, m.openTab(focusDirs))
		}
		if open, ok := m.openWaiting(m.channelID, ""); ok {
			return tea.Batch(cmd, open)
		}
		return cmd
	case modelsMsg:
		return m.onModels(msg)
	}
	return nil
}

// onChannelsListed routes a channel list to what asked for it.
func (m *Model) onChannelsListed(msg channelsMsg) tea.Cmd {
	msg.channels = cleanChannels(msg.channels)
	switch msg.purpose {
	case channelsNav:
		if msg.err == nil {
			m.navChannels = resumable(msg.channels, m.channelID)
		}
	case channelsHistory:
		if msg.err == nil {
			m.seedHistory(msg.channels)
		}
	case channelsPicker:
		return m.onChannels(msg)
	}
	return nil
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
	if len(m.metaParts()) > 0 { // the channel chat's meta row has nothing to pick
		order = append(order, focusMeta)
	}
	if m.sidebarVisible() {
		order = append(order, focusSidebar)
	}
	return order
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

// onDirChanged keeps the channel's working directories in step with the log.
// The attach snapshot may already hold a replayed add, so adds are
// idempotent.
func (m *Model) onDirChanged(ev event.Event) {
	switch ev.Type {
	case event.ChannelDirAdded:
		var p event.DirAddedPayload
		if ev.Decode(&p) != nil || p.Dir == m.channel.Dir || slices.ContainsFunc(m.channel.Dirs, func(d protocol.DirInfo) bool { return d.Path == p.Dir }) {
			return
		}
		m.channel.Dirs = append(m.channel.Dirs, protocol.DirInfo{Path: textsafe.Clean(p.Dir), Source: p.Source})
	default:
		var p event.DirRefPayload
		if ev.Decode(&p) == nil {
			m.channel.Dirs = slices.DeleteFunc(m.channel.Dirs, func(d protocol.DirInfo) bool { return d.Path == p.Dir })
		}
	}
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

// dueCount is how many replies the selected agent owes.
func (m *Model) dueCount() int {
	human, agents := m.dueOf()
	if human {
		return len(agents) + 1
	}
	return len(agents)
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
	on := m.channel.Mode != mode
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
	return setModeCmd(m.ctx, m.c, m.channelID, mode)
}

// openMode is /mode: the three permission modes, the current one marked.
func (m *Model) openMode() tea.Cmd {
	o := newOverlay(ovMode, overlayList, "Permission mode")
	cur := m.channel.Mode
	if cur == "" {
		cur = protocol.ModeAsk
	}
	var items []overlayItem
	for _, mode := range []string{protocol.ModeAsk, protocol.ModeAuto, protocol.ModeYolo} {
		hint := protocol.ModeSummary(mode)
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
		return m.input.Focus()
	case focusChat:
		m.follow = false
		m.chatCursor = m.chatItems() - 1
		m.refreshViewport()
		m.scrollToCursor()
	case focusQuestions:
		m.q.bind(m.currentQuestion())
	case focusSidebar:
		m.sbCursor = m.channelRow()
		if !m.superChat {
			m.sbCursor = m.channelRow() + 1 + m.selected
		}
	case focusAsync, focusDue, focusTodo, focusMCP, focusDirs:
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
	case stepCursor(msg, &m.agCursor, n, true):
	case key.Matches(msg, keys.Select):
		if n == 0 || m.agCursor%n >= len(agents) {
			return nil // a job row: nothing to select
		}
		m.openAgent(m.findAgent(agents[m.agCursor%n].ID))
		return m.closeDialog()
	}
	return nil
}

// dueKey handles keys while the due dialog is open: ↑/↓ (or j/k) move over
// who is waiting on the selected agent's reply; space on "you" opens the
// channel chat and on an agent opens that agent's chat, closing the dialog.
func (m *Model) dueKey(msg tea.KeyMsg) tea.Cmd {
	human, agents := m.dueOf()
	off := 0
	if human {
		off = 1
	}
	n := off + len(agents)
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case stepCursor(msg, &m.agCursor, n, true):
	case key.Matches(msg, keys.Select):
		if n == 0 {
			return nil
		}
		if i := m.agCursor % n; i < off {
			m.openChat()
		} else {
			m.openAgent(m.findAgent(agents[i-off].ID))
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
// channel directory cannot be changed.
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
		if d.Source == "channel" {
			return m.setStatus("the channel directory cannot be changed", true)
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
		if d.Source == "channel" {
			return m.setStatus("the channel directory cannot be removed", true)
		}
		return removeDirCmd(m.ctx, m.c, m.channelID, d.Path)
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
	case stepCursor(msg, &m.q.sel, rows, false): // no j/k: letters start the typed answer
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
	answers = append([]string(nil), answers...)
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error {
		return c.AnswerQuestions(ctx, id, answers)
	})
}

// wrapIndex brings i into [0, n) cyclically; n must be positive.
func wrapIndex(i, n int) int { return (i%n + n) % n }

// stepCursor applies ↑/↓ to a cursor over n rows, wrapping at both ends;
// with letters, k and j move it too (lists with no text field taking the
// keys). It reports whether the key was a move, even over no rows.
func stepCursor(msg tea.KeyMsg, cur *int, n int, letters bool) bool {
	up := key.Matches(msg, keys.SelUp) || letters && msg.String() == "k"
	down := key.Matches(msg, keys.SelDown) || letters && msg.String() == "j"
	if !up && !down {
		return false
	}
	if n > 0 {
		d := 1
		if up {
			d = -1
		}
		*cur = wrapIndex(*cur+d, n)
	}
	return true
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
	_, owners := mcpRows(m.selectedMCP(), m.mcpOpen, time.Now(), 200)
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
				return m.sidebarClick(msg.X, msg.Y)
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
		return m.pickTabRow(h.row, true), true
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
		m.pickTabRow(h.row, false)
	}
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
	case focusQuestions:
		p := m.currentQuestion()
		if p == nil {
			return nil
		}
		m.q.bind(p)
		if m.q.typing {
			return nil
		}
		m.q.sel = row
		if choose {
			return m.questionsKey(space)
		}
	default:
		m.agCursor = row
		if choose && m.focus == focusAsync {
			return m.asyncKey(space)
		}
		if choose && m.focus == focusDue {
			return m.dueKey(space)
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
		lines[y] = left + theme.StyleSelection.Render(mid) + right
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

// modeTagCols is the columns the mode tag takes before the input's ›: the
// widest tag ("AUTO", "YOLO") and a space.
const modeTagCols = 5

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
	// every row out to the box in the same colour. The mode tag leads the
	// first row, right-aligned before the ›; later rows keep its columns.
	bg := lipgloss.NewStyle().Background(theme.ColInputBg)
	width := m.boxWidth()
	tag := m.modeTag()
	for i, l := range lines {
		lead := bg.Render(strings.Repeat(" ", modeTagCols))
		if i == 0 {
			lead = bg.Render(strings.Repeat(" ", modeTagCols-1-len(tag))) + modeTagStyle(tag).Background(theme.ColInputBg).Render(tag) + bg.Render(" ")
		}
		l = lead + l
		if pad := width - ansi.StringWidth(l); pad > 0 {
			l += bg.Render(strings.Repeat(" ", pad))
		}
		lines[i] = l
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
	case y >= lay.strip && y < lay.strip+m.stripRows(): // a tab label opens that tab's dialog
		if f, ok := m.tabAt(x, y-lay.strip+len(m.tabLayout())-m.stripRows()); ok {
			return m.openTab(f)
		}
	case y >= lay.input && y < lay.input+m.inputRows(): // the input lines; the mode tag before the › is a button
		if y == lay.input && x < modeTagCols-1 {
			return m.metaAction(metaYolo)
		}
		return m.setFocus(focusInput)
	case y == lay.meta: // the meta row: its parts are buttons
		if part := m.metaHit(x); part != metaNone {
			return m.metaAction(part)
		}
		return m.setFocus(focusInput)
	}
	return nil
}

// metaParts lists the meta row's parts in order: the mode tag, the role,
// the model and the variant.
func (m *Model) metaParts() []metaPart {
	if m.superChat {
		return nil // the channel chat: role, model and variant are an agent's (the mode tag leads the input)
	}
	return []metaPart{metaRole, metaModel, metaVariant}
}

// modeTag leads the input, before its ›: the channel's permission mode, "ASK",
// "AUTO" or "YOLO". It is always there, so turning auto or yolo off leaves
// the tag in place rather than taking it away.
func (m *Model) modeTag() string {
	switch m.channel.Mode {
	case protocol.ModeAuto:
		return "AUTO"
	case protocol.ModeYolo:
		return "YOLO"
	}
	return "ASK"
}

// metaAction is what a part of the meta row does when picked, by click or
// enter: AUTO and YOLO go back to ask, ASK opens /mode; the role, model and
// variant open their dialogs.
func (m *Model) metaAction(part metaPart) tea.Cmd {
	switch part {
	case metaYolo:
		if m.modeTag() == "ASK" {
			return m.openMode()
		}
		return setModeCmd(m.ctx, m.c, m.channelID, protocol.ModeAsk)
	case metaRole:
		return rolesCmd(m.ctx, m.c, m.channelID, false)
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

// seedHistory gives a fresh channel's ↑/↓ history the first prompts of
// this directory's earlier channels (newest first under ↑), so the start
// screen recalls what was asked last time. Only when nothing has been typed
// here yet; the current and untitled channels are skipped.
func (m *Model) seedHistory(channels []protocol.ChannelInfo) {
	if len(m.history) > 0 || !m.isHome() {
		return
	}
	seen := map[string]bool{}
	var titles []string
	for _, s := range channels { // newest first
		if s.Title == "" || s.ID == m.channelID || seen[s.Title] {
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
	metaYolo             // the ASK/AUTO/YOLO mode tag: AUTO and YOLO go back to ask, ASK opens /mode
	metaRole             // "label (role)": click opens /roles
	metaModel            // the model: click opens /models
	metaVariant          // the variant: click opens /variants
)

// metaHit maps an x position on the meta row to the part drawn there.
func (m *Model) metaHit(x int) metaPart {
	_, spans := m.metaLeft()
	if part, ok := hitSpan(spans, x); ok {
		return part
	}
	return metaNone
}

// rowLayout is where the channel view's pieces sit, in screen rows: under
// the rule come the palette (while open), the input, a blank line, the
// strip and the meta row.
type rowLayout struct {
	input int // first row of the input (it may span several)
	strip int // the tab strip's first line (it has len(tabRows))
	meta  int // the meta row
}

// rows derives the row layout the same way channelView stacks its parts.
func (m *Model) rows() rowLayout {
	y := m.vp.Height + 2 // the status line, then the rule
	if pv := m.paletteViewFor(m.width); pv != "" {
		y += strings.Count(pv, "\n") + 1
	}
	lay := rowLayout{input: y}
	y += m.inputRows()
	if m.stripRows() > 0 || m.metaShown() {
		y++ // the blank line under the input
	}
	lay.strip = y
	lay.meta = y + m.stripRows()
	if !m.metaShown() {
		lay.meta = -1
	}
	return lay
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

// itemAtRow maps a viewport content row to the chat item drawn there.
func (m *Model) itemAtRow(row int) (int, bool) {
	for item, r := range m.itemRows {
		if row >= r.First && row <= r.Last {
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
	if a := m.selectedAgent(); a != nil && a.State.Busy() {
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
	paletteOpen := m.focus == focusInput && (len(paletteMatches(m.input.Value())) > 0 || len(m.mentionMatches()) > 0)

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
	case focusDue:
		return m.dueKey(msg)
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

	return m.inputKey(msg)
}

// inputKey handles keys while the input has focus: paging the chat, ↑/↓
// through the "/" palette or the prompt history, esc to clear (or cancel a
// busy turn), tab to complete a command, enter to run or send; anything
// else edits the text.
func (m *Model) inputKey(msg tea.KeyMsg) tea.Cmd {
	if m.mentionKey(msg) {
		return nil
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

// mentionKey handles the @name dropdown while it is open in the channel
// chat: ↑/↓ pick a name, tab or enter completes it. It reports whether it
// used the key.
func (m *Model) mentionKey(msg tea.KeyMsg) bool {
	mm := m.mentionMatches()
	if len(mm) == 0 {
		return false
	}
	switch {
	case key.Matches(msg, keys.SelUp):
		m.palIdx = (m.palIdx - 1 + len(mm)) % len(mm)
	case key.Matches(msg, keys.SelDown):
		m.palIdx = (m.palIdx + 1) % len(mm)
	case msg.Type == tea.KeyTab, key.Matches(msg, keys.Submit):
		m.completeMention(mm[m.clampPal(len(mm))])
	default:
		return false
	}
	return true
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
// A plain permission: once, this exact call for the channel, the command's
// prefix for the channel (shell, when one can be derived), deny. A boundary
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
			{"add", "Allow and add " + format.ShortHome(p.Dir), "every agent in the channel can use it"},
			{"add_other", "Allow and add another directory…", "type the path"},
			{"deny", "Deny", "with an optional reason"},
		}
	}
	what := "this exact call"
	switch p.Tool {
	case toolname.Shell:
		what = "this exact command"
	case toolname.WebFetch:
		what = "this exact URL"
	}
	opts := []permOption{
		{"allow", "Allow once", ""},
		{"always", "Allow for this channel", what},
	}
	if pre := p.Prefix; pre != "" {
		desc := "every command starting with it"
		if p.Tool == toolname.WebFetch {
			desc = "every page on this host"
		}
		opts = append(opts, permOption{"prefix", "Allow " + pre + " for this channel", desc})
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
	case stepCursor(msg, &m.permSel, n, false):
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
			return m.answerPromptPrefix(p)
		case "skip":
			return m.answerPrompt(p, "deny")
		case "add_other":
			m.permEdit = "dir"
			m.dirInput.Placeholder = "path (absolute, ~, or relative to the channel directory)"
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
	if m.superChat {
		return postCmd(m.ctx, m.c, m.channelID, text) // the daemon delivers it by @mention
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
	case "/chat":
		return m.openChat()
	case "/roles", "/role", "/presets":
		// The one role dialog: enter switches the selected agent's preset.
		// A name argument sets it directly.
		if c := needAgent(); c != nil {
			return c
		}
		if rest == "" {
			return rolesCmd(m.ctx, m.c, m.channelID, false)
		}
		return pickRoleCmd(m.ctx, m.c, agent, strings.ToLower(rest))
	case "/channels", "/resume", "/channel":
		return channelsCmd(m.ctx, m.c, m.channel.Dir, channelsPicker)
	case "/rename":
		if rest == "" {
			return m.setStatus("usage: /rename <name>", true)
		}
		return renameChannelCmd(m.ctx, m.c, m.channelID, strings.TrimPrefix(rest, "#"))
	case "/compact":
		if c := needAgent(); c != nil {
			return c
		}
		return compactCmd(m.ctx, m.c, agent) // the compaction events drive the bar and the result line
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
		// The one model dialog: enter sets the selected agent's model, ctrl+s the channel default.
		return modelsCmd(m.ctx, m.c)
	case "/providers", "/provider", "/connect", "/login":
		// The one provider dialog: sign in, re-sign in, sign out. A name
		// argument jumps straight to that provider's sign-in.
		return providersCmd(m.ctx, m.c, providersMsg{jump: strings.ToLower(rest)})
	}
	return m.setStatus("unknown command "+name+" (try /help)", true)
}

// --- events ---

// applyEvent routes one log event into the right transcript and schedules a
// tree refresh for events that change agent state or cost.
func (m *Model) applyEvent(ev event.Event) tea.Cmd {
	if ev.Channel != "" && ev.Channel != m.channelID {
		return nil
	}
	if ev.Seq > m.seq {
		m.seq = ev.Seq
	}
	target, cmds := m.eventSideEffects(ev)
	m.followChild(ev)
	if target != "" {
		m.transcript(target).Apply(ev)
		if !m.loading && target == m.viewID() {
			m.refreshViewport()
		}
	}
	if transcript.ChatEvent(ev.Type) {
		m.transcript(chatView).Apply(ev)
		if !m.loading && m.superChat {
			m.refreshViewport()
		}
	}
	if !m.loading && changesTree(ev) {
		cmds = append(cmds, m.markTreeDirty())
	}
	if m.loading && ev.Seq >= m.replayTo {
		m.loading = false
		m.refreshViewport()
		cmds = append(cmds, m.markTreeDirty())
		if m.anyCompacting() && !m.compactTick { // a compaction was running when we attached
			m.compactTick = true
			cmds = append(cmds, compactTickCmd())
		}
	}
	return tea.Batch(cmds...)
}

// eventSideEffects applies what an event changes outside its transcript and
// returns the agent whose transcript shows it.
func (m *Model) eventSideEffects(ev event.Event) (target string, cmds []tea.Cmd) {
	target = ev.Agent
	switch ev.Type {
	case event.ChannelDirAdded, event.ChannelDirRemoved:
		m.onDirChanged(ev) // the channel's set; the chat of the agent whose prompt added one notes it
	case event.AgentSpawned:
		if id := m.onAgentSpawned(ev); id != "" {
			target = id
		}
	case event.AgentKilled:
		m.onAgentKilled(ev.Agent)
	case event.PromptQueued:
		m.rememberPrompt(ev)
	case event.CompactionStarted: // the chat item's bar animates until the result lands
		if !m.loading && !m.compactTick {
			m.compactTick = true
			cmds = append(cmds, compactTickCmd())
		}
	case event.ChannelRenamed:
		var p event.NamePayload
		if ev.Decode(&p) == nil {
			m.channel.Name = textsafe.Clean(p.Name)
		}
	case event.ChannelModelChanged:
		var p event.ModelChangedPayload
		if ev.Decode(&p) == nil {
			m.channel.Model = p.Model
		}
	case event.ChannelModeChanged:
		m.onModeChanged(ev)
	case event.TurnEnded:
		var p event.TurnEndedPayload
		if !m.loading && ev.Decode(&p) == nil && p.Reason == "error" &&
			(strings.Contains(p.Error, "not connected") || strings.Contains(p.Error, "/provider")) {
			cmds = append(cmds, m.setStatus("provider not connected — run /providers", true))
		}
	}
	return target, cmds
}

// onAgentSpawned records a new agent (a placeholder row until the tree
// refresh lands) and ties it to its parent's agent_create line. It returns
// the new agent's id, "" when the payload is unusable.
func (m *Model) onAgentSpawned(ev event.Event) string {
	var p event.AgentSpawnedPayload
	if ev.Decode(&p) != nil || p.ID == "" {
		return ""
	}
	if m.spawned == nil {
		m.spawned = map[string]time.Time{}
	}
	m.spawned[p.ID] = ev.Time
	if !m.loading && m.findAgent(p.ID) < 0 {
		// Placeholder until the debounced tree refresh lands.
		m.agents = append(m.agents, protocol.AgentInfo{
			ID: p.ID, Channel: ev.Channel, Parent: p.Parent, Archetype: p.Archetype,
			Label: p.Label, Model: p.Model, Depth: p.Depth, State: protocol.AgentIdle,
		})
	}
	if p.Parent != "" {
		// The parent's agent_create line tracks this child's life.
		if m.parentOf == nil {
			m.parentOf = map[string]string{}
		}
		m.parentOf[p.ID] = p.Parent
		m.transcript(p.Parent).ChildSpawned(p.ID)
		if !m.loading && p.Parent == m.viewID() {
			m.refreshViewport()
		}
	}
	return p.ID
}

// onAgentKilled reddens the killed agent's agent_create line and every
// question asked of it: no answer is coming.
func (m *Model) onAgentKilled(id string) {
	if parent := m.parentOf[id]; parent != "" {
		m.transcript(parent).ChildState(id, protocol.AgentKilled)
	}
	name := m.agentLabel(id)
	for _, t := range m.transcripts {
		t.AskerGone(name)
	}
	if !m.loading {
		m.refreshViewport()
	}
}

// rememberPrompt replays a human prompt into the input history.
func (m *Model) rememberPrompt(ev event.Event) {
	var p event.TextPayload
	if !m.loading || ev.Decode(&p) != nil || !strings.HasPrefix(p.Source, "human:") || p.Text == "" {
		return
	}
	if n := len(m.history); n == 0 || m.history[n-1] != p.Text {
		m.history = append(m.history, p.Text)
	}
	m.histIdx = len(m.history)
}

// onModeChanged records the channel's permission mode and notes the switch
// in every agent's chat, like model and role changes: the event has no
// agent of its own.
func (m *Model) onModeChanged(ev event.Event) {
	var p event.ModePayload
	if ev.Decode(&p) == nil {
		m.channel.Mode = p.Mode
	}
	for _, a := range m.agents {
		m.transcript(a.ID).Apply(ev)
	}
	if !m.loading {
		m.refreshViewport()
	}
}

// followChild keeps the parent's agent_create line in step with the
// child's turns: yellow while a turn runs, grey between turns.
func (m *Model) followChild(ev event.Event) {
	parent := m.parentOf[ev.Agent]
	if parent == "" {
		return
	}
	switch ev.Type {
	case event.TurnStarted:
		m.transcript(parent).ChildState(ev.Agent, protocol.AgentRunning)
	case event.TurnEnded, event.TurnAborted:
		m.transcript(parent).ChildState(ev.Agent, protocol.AgentIdle)
	}
}

// changesTree reports whether ev changes what the tabs read off the agent
// tree, so the tree needs refreshing.
func changesTree(ev event.Event) bool {
	switch ev.Type {
	case event.AgentSpawned, event.AgentKilled,
		event.TurnStarted, event.TurnEnded, event.Usage,
		event.AgentModelChanged, event.AgentRoleChanged, event.AgentVariantChanged, event.ChannelModelChanged,
		event.MonitorStarted, event.MonitorFired, event.MonitorStopped,
		event.TodoChanged,
		event.MCPStarted, event.MCPFailed, event.MCPStopped, event.ResponseReceived,
		event.UserMessage, event.MessageToUser: // what an agent owes changes
		return true
	case event.ToolCallFinished: // a message or task just put another agent on the awaiting list
		var p event.ToolFinishedPayload
		if ev.Decode(&p) != nil {
			return false
		}
		name := p.Name
		return name == toolname.Message || name == toolname.AgentCreate
	}
	return false
}

// anyCompacting reports whether some agent's chat shows a running compaction.
func (m *Model) anyCompacting() bool {
	for _, t := range m.transcripts {
		if t.Compacting() {
			return true
		}
	}
	return false
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
	before := len(m.prompts) // every channel's prompts are kept: the tabs span channels
	switch n.Action {
	case protocol.ActionRequested, protocol.ActionEscalated, protocol.ActionClaimed:
		m.upsertPrompt(n.Prompt)
	case protocol.ActionAnswered, protocol.ActionWithdrawn, protocol.ActionDefaulted:
		m.removePrompt(n.Prompt.ID)
	}
	// The turn indicator switches between "working…" and "permission
	// requested" on prompt changes, which arrive outside the event stream.
	if n.Prompt.Agent == m.selectedID() {
		m.refreshViewport()
	}
	if before == 0 && len(m.prompts) > 0 && (n.Prompt.Channel == "" || n.Prompt.Channel == m.channelID) && m.focus == focusInput && m.ov == nil && strings.TrimSpace(m.input.Value()) == "" {
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
func (m *Model) currentPrompt() *protocol.PromptInfo { return m.firstPrompt(false) }

// currentQuestion is the waiting ask_user batch the questions dialog shows.
func (m *Model) currentQuestion() *protocol.PromptInfo { return m.firstPrompt(true) }

// firstPrompt picks the prompt a dialog shows: the selected agent's oldest
// one when it has any (the footer controls the selected agent; the nav
// badges point at the others), else the oldest overall so nothing waits
// unseen. question selects the question batches or the permission-ish
// prompts.
func (m *Model) firstPrompt(question bool) *protocol.PromptInfo {
	sel := m.selectedID()
	var first *protocol.PromptInfo
	for i := range m.prompts {
		p := &m.prompts[i]
		if (p.Kind == "question") != question || !m.scope.holds(*p) {
			continue
		}
		if p.Agent == sel {
			return p
		}
		if first == nil {
			first = p
		}
	}
	return first
}

// promptCounts is how many permission-ish prompts and question batches wait
// in every channel.
func (m *Model) promptCounts() (perms, questions int) { return m.promptCountsIn(promptScope{}) }

// promptCountsIn counts only what scope holds.
func (m *Model) promptCountsIn(scope promptScope) (perms, questions int) {
	for _, p := range m.prompts {
		if !scope.holds(p) {
			continue
		}
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
	cleanPrompt(&p)
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
	perms, questions := m.promptCountsIn(m.scope)
	if perms == 0 && m.focus == focusPermission {
		m.closeDialog() // the last permission it shows was answered: the dialog closes
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
	// A trust prompt is answered like any other, by id: the daemon knows
	// which directory and hash it asked about.
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error { return c.ReplyPrompt(ctx, id, answer) })
}

// claimThen sends one answer to p through reply, unless an answer to it is
// already in flight or another client holds it; the command claims the
// prompt first.
func (m *Model) claimThen(p *protocol.PromptInfo, reply func(ctx context.Context, c *client.Client, id string) error) tea.Cmd {
	if m.promptBusy == p.ID {
		return m.setStatus("answer in flight…", false)
	}
	if p.ClaimedBy != "" && !m.claimedByUs[p.ID] {
		return m.setStatus("claimed by another client", true)
	}
	id := p.ID
	m.promptBusy = id
	m.claimedByUs[id] = true
	return replyCmd(m.ctx, m.c, id, func(ctx context.Context, c *client.Client) error { return reply(ctx, c, id) })
}

// denyPrompt denies a permission, passing the human's reason (may be empty).
func (m *Model) denyPrompt(p *protocol.PromptInfo, reason string) tea.Cmd {
	if p.Kind == "trust" {
		return m.answerPrompt(p, "deny") // trust has its own reply; no reason field
	}
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error { return c.DenyPrompt(ctx, id, reason) })
}

// answerPromptPrefix allows the call and every command of the tool that
// starts with prefix for the channel.
func (m *Model) answerPromptPrefix(p *protocol.PromptInfo) tea.Cmd {
	if p.Prefix == "" {
		return m.answerPrompt(p, "allow_always")
	}
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error { return c.AllowPromptPrefix(ctx, id) })
}

// answerPromptDir is allow_always on a boundary prompt with an edited
// directory: the call runs and that directory joins the agent's set.
func (m *Model) answerPromptDir(p *protocol.PromptInfo, dir string) tea.Cmd {
	return m.claimThen(p, func(ctx context.Context, c *client.Client, id string) error {
		return c.ReplyPromptDir(ctx, id, "allow_always", dir)
	})
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
		return tea.Batch(m.setFocus(focusSidebar), channelsCmd(m.ctx, m.c, m.channel.Dir, channelsNav))
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
		return m.sidebarSelect(m.sbCursor)
	case key.Matches(msg, keys.TabRight): // → on the title: its +; on a channel row: its gear, the channel's dirs
		if m.sbCursor == 0 {
			return m.newChannel()
		}
		return m.channelSettings(m.sbCursor)
	case msg.String() == "n": // the next agent that needs you, selected at once
		if i := m.nextNeedy(max(-1, m.sbCursor-m.channelRow()-1)); i >= 0 {
			m.sbCursor = m.channelRow() + 1 + i
			m.openAgent(i)
		} else {
			return m.setStatus("no agent is waiting on you", false)
		}
		return nil
	}
	return nil
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
// the directory's channels and this channel's agents. The cursor counts them
// top to bottom: 0 is + channel, then the channels alphabetically with this
// channel's agents right under its row (channelRow).
func (m *Model) sidebarItems() int {
	return len(m.agents) + 2 + len(m.navChannels)
}

// channelRow is the sidebar cursor index of this channel's row: after
// + channel and the other channels named before it (navChannels is kept in
// alphabetical order). Its agent i is channelRow()+1+i.
func (m Model) channelRow() int {
	n := 1
	for _, s := range m.navChannels {
		if compareChannels(s, m.channel) < 0 {
			n++
		}
	}
	return n
}

// sidebarSelect acts on the item under the cursor: + channel creates a
// channel in this directory and opens it; this channel's row shows its chat
// and an agent row that agent's own chat (both focus the input); another
// channel's row opens that channel in place of this one.
func (m *Model) sidebarSelect(i int) tea.Cmd {
	na, here := len(m.agents), m.channelRow()
	switch {
	case i == 0:
		return m.newChannel()
	case i < here:
		return m.openOther(i - 1)
	case i == here:
		chat := m.openChat()
		if cmd, ok := m.openWaiting(m.channelID, ""); ok {
			return tea.Batch(chat, cmd)
		}
		return tea.Batch(chat, m.setFocus(focusInput))
	case i <= here+na:
		m.openAgent(i - here - 1)
		if cmd, ok := m.openWaiting(m.channelID, m.selectedID()); ok {
			return cmd
		}
		return m.setFocus(focusInput)
	case i-na-2 < len(m.navChannels):
		return m.openOther(i - na - 2)
	}
	return nil
}

// openTab opens tab f's dialog from the tabs (the strip or the sidebar): a
// permission or questions dialog opened this way shows every channel's.
func (m *Model) openTab(f focus) tea.Cmd {
	m.scope = promptScope{}
	cmd := m.setFocus(f)
	if f == focusQuestions {
		m.q.bind(m.currentQuestion())
	}
	return cmd
}

// openWaiting opens the permission dialog, else the questions dialog, on
// what waits in channel (only agent's when agent is set) when anything does,
// and reports whether it opened one: opening a channel or an agent that waits
// on the human goes straight to its prompts.
func (m *Model) openWaiting(channel, agent string) (tea.Cmd, bool) {
	scope := promptScope{channel, agent}
	perms, questions := m.promptCountsIn(scope)
	f := focusPermission
	switch {
	case perms > 0:
	case questions > 0:
		f = focusQuestions
	default:
		return nil, false
	}
	m.scope = scope
	cmd := m.setFocus(f)
	if f == focusQuestions {
		m.q.bind(m.currentQuestion())
	}
	return cmd, true
}

// channelAt is the channel a sidebar cursor index names: this channel (k
// -1) or the directory's other channel k; ok is false for any other row.
func (m Model) channelAt(i int) (k int, ok bool) {
	here, na := m.channelRow(), len(m.agents)
	switch {
	case i == here:
		return -1, true
	case i >= 1 && i < here:
		return i - 1, true
	case i > here+na && i-na-2 < len(m.navChannels):
		return i - na - 2, true
	}
	return 0, false
}

// channelSettings is the gear of the channel on sidebar row i (→ on the row,
// or a click on the gear): this channel's dirs dialog, or another channel
// opened on its dirs dialog.
func (m *Model) channelSettings(i int) tea.Cmd {
	k, ok := m.channelAt(i)
	switch {
	case !ok:
		return nil
	case k < 0:
		return m.openTab(focusDirs)
	}
	m.dirsNext = true
	return m.openOther(k)
}

// openOther opens the directory's other channel k (navChannels order) in
// place of this one.
func (m *Model) openOther(k int) tea.Cmd {
	s := m.navChannels[k]
	return tea.Batch(m.setStatus("opening #"+s.Name, false), switchChannelCmd(m.ctx, m.c, m.channelID, s.ID))
}

// newChannel is + channel: a popup names a new channel of this directory,
// which then opens in place of this one.
func (m *Model) newChannel() tea.Cmd {
	o := newOverlay(ovNewChannel, overlayInput, "New channel in "+format.ShortHome(m.channel.Dir))
	o.input.Placeholder = "name, shown as #name"
	return m.openOverlay(o)
}

// sidebarClick focuses the sidebar and acts on the row under the pointer
// like space: the chat row or an agent row opens that chat (the sidebar
// keeps focus), + channel or another channel's row acts like space.
func (m *Model) sidebarClick(x, y int) tea.Cmd {
	cmd := m.setFocus(focusSidebar)
	if y == sidebarTabsRow { // the ! ? dirs tabs: a click opens that tab
		if f, ok := m.tabAt(x, 0); ok && m.sidebarVisible() {
			return tea.Batch(cmd, m.openTab(f))
		}
		return cmd
	}
	_, items := m.sidebarBody(sidebarWidth - 1)
	row := y - len(m.sidebarHeader(sidebarWidth-1))
	if row < 0 || row >= len(items) || items[row] < 0 {
		return cmd
	}
	i := items[row]
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
	switch {
	case i == m.channelRow():
		chat := m.openChat()
		if open, ok := m.openWaiting(m.channelID, ""); ok {
			return tea.Batch(cmd, chat, open)
		}
		return tea.Batch(cmd, chat)
	case i > m.channelRow() && i <= m.channelRow()+len(m.agents):
		m.openAgent(i - m.channelRow() - 1)
		if open, ok := m.openWaiting(m.channelID, m.selectedID()); ok {
			return tea.Batch(cmd, open)
		}
		return cmd
	}
	return tea.Batch(cmd, m.sidebarSelect(i))
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

// placeholder is the input's hint: how the channel chat addresses agents,
// or a cycling suggestion in an agent's own chat.
func (m *Model) placeholder() string {
	if m.superChat {
		return "Message the channel · @name addresses an agent, no mention goes to the root"
	}
	return placeholders[placeholderIndex(time.Now())]
}

// totalTokens sums every agent's tokens for the channel rollup.
func (m *Model) totalTokens() int {
	n := 0
	for _, a := range m.agents {
		n += a.Tokens
	}
	return n
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
	m.input.SetWidth(boxW - modeTagCols)                                                        // the mode tag sits before the ›
	m.input.SetHeight(m.inputCap())                                                             // the textarea is always cap tall; the view trims to the rows used
	m.promptInput.Width = dialog.Width(m.width) - 4 - 2 - len([]rune(m.promptInput.Prompt)) - 1 // inside the tab dialog, under promptBox's indent
	m.dirInput.Width = dialog.Width(m.width) - 4 - 2 - len([]rune(m.dirInput.Prompt)) - 1

	_, kb := m.keyBarView()
	bodyH := m.height - kb - 2 - m.inputRows() // key bar, status line + rule, the input rows
	sv := m.sectionsView(m.width)
	if sv != "" {
		bodyH -= strings.Count(sv, "\n") + 1 // the strip, which the channel chat may not have at all
	}
	if m.metaShown() {
		bodyH-- // the meta row
	}
	if sv != "" || m.metaShown() {
		bodyH-- // the blank line under the input
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
		stats = render.TurnStats(t.TurnStats(time.Now()))
		active = m.activeTodo()
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
		Width:    m.vp.Width,
		Details:  m.details,
		Spinner:  m.sp.View(),
		Working:  working,
		Waiting:  waiting,
		Verb:     verb,
		Active:   active,
		Stats:    stats,
		Expanded: m.expanded[m.viewID()],
		TurnGaps: !m.superChat,
		WhoStyle: m.whoStyle,
		WhoKey:   m.whoKey(),
		Cursor:   m.chatCursor,
		Focused:  m.focus == focusChat,

		CompactFrame: render.CompactFrame(time.Now()),
	}
	var content string
	var rows map[int]render.RowRange
	if t != nil {
		content, rows = render.Transcript(t, m.chatCache(m.viewID()), opts) // unchanged items come from the cache
	} else {
		content, rows = render.Lines(nil, opts)
	}
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
	if o.mode == overlayInput { // a text field: space types, enter submits
		switch {
		case key.Matches(msg, keys.OvClose):
			return m.closeOverlay()
		case key.Matches(msg, keys.OvSelect):
			return m.overlaySubmit(false)
		}
		return o.update(msg)
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
		return tea.Batch(m.closeOverlay(), setModeCmd(m.ctx, m.c, m.channelID, it.id))
	case ovNewChannel:
		name := strings.TrimSpace(o.input.Value())
		if name == "" {
			return nil
		}
		return tea.Batch(m.closeOverlay(), m.setStatus("creating a channel", false), newChannelCmd(m.ctx, m.c, m.channelID, m.channel.Dir, name))
	case ovChannels:
		it := o.selected()
		if it == nil {
			return nil
		}
		if it.id == m.channelID {
			return tea.Batch(m.closeOverlay(), m.setStatus("already in this channel", false))
		}
		return tea.Batch(m.closeOverlay(), switchChannelCmd(m.ctx, m.c, m.channelID, it.id))
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
			return tea.Batch(m.closeOverlay(), pickChannelModelCmd(m.ctx, m.c, m.channelID, it.id))
		}
		agent := m.selectedID()
		if agent == "" {
			return m.setStatus("no agent selected (ctrl+s sets the channel default)", true)
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
	cmds = append(cmds, m.setStatus("connected "+name+" ✓", false), providersCmd(m.ctx, m.c, providersMsg{refresh: true}))
	if m.channel.Model == "" && (len(m.agents) == 0 || m.agents[0].Model == "") {
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
		short, _ := transcript.SplitModel(r.Models[0].ID)
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

// whoStyle is the colour a chat line takes for someone it names: an agent's
// role colour (green when its role sets none), blue for the human.
func (m *Model) whoStyle(name string) lipgloss.Style {
	if name == "user" {
		return lipgloss.NewStyle().Foreground(theme.ColAccent)
	}
	tint := "green"
	for _, a := range m.agents {
		if a.Label == name {
			if r := m.roleInfo(a.Archetype); r != nil && r.Color != "" {
				tint = r.Color
			}
			break
		}
	}
	return roleStyle(tint)
}

// whoKey changes whenever whoStyle's colours do, so cached chat rows redraw.
func (m *Model) whoKey() string {
	var b strings.Builder
	for _, a := range m.agents {
		b.WriteString(a.Label + "=")
		if r := m.roleInfo(a.Archetype); r != nil {
			b.WriteString(r.Color)
		}
		b.WriteByte(';')
	}
	return b.String()
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

// onChannels opens the /channels picker: this directory's channels, newest
// first, each titled by its first prompt. Channels nobody has prompted are
// left out (except the current one): there is nothing to resume there.
func (m *Model) onChannels(msg channelsMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("channels: "+msg.err.Error(), true)
	}
	o := newOverlay(ovChannels, overlayList, "Channels in "+format.ShortHome(m.channel.Dir))
	sortChannels(msg.channels)
	items := make([]overlayItem, 0, len(msg.channels))
	for _, s := range msg.channels {
		items = append(items, channelItem(s, s.ID == m.channelID))
	}
	if len(items) == 0 {
		o.setEmpty("no channels here yet", false)
	}
	o.setItems(items)
	return m.openOverlay(o)
}

// channelItem is one row of the /channels picker: its #name, then its first
// prompt, when it started, its model, cost and live agents.
func channelItem(s protocol.ChannelInfo, current bool) overlayItem {
	var meta []string
	if title := strings.Join(strings.Fields(s.Title), " "); title != "" {
		meta = append(meta, format.Trunc(title, 40))
	}
	if t, err := time.Parse(time.RFC3339, s.Created); err == nil {
		meta = append(meta, format.Elapsed(time.Since(t))+" ago")
	}
	if s.Model != "" {
		meta = append(meta, s.Model)
	}
	if s.CostUSD > 0 {
		meta = append(meta, "$"+format.Cost(s.CostUSD))
	}
	if s.Live > 0 {
		meta = append(meta, fmt.Sprintf("%d live", s.Live))
	}
	if current {
		meta = append(meta, "current")
	}
	return overlayItem{id: s.ID, label: channelLabel(s), hint: strings.Join(meta, " · "), good: current}
}

// channelRef names a channel in a status line: "#name", or its short id
// before it has one.
func channelRef(info protocol.ChannelInfo) string {
	if info.Name == "" {
		return format.ShortID(info.ID)
	}
	return "#" + info.Name
}

// bindChannel rebinds the TUI to another channel: every per-channel
// piece of state starts over and a fresh reconcile replays its history.
func (m *Model) bindChannel(info protocol.ChannelInfo) tea.Cmd {
	m.channelState = newChannelState(info.ID, info)
	m.superChat = true
	m.layout()
	m.input.Placeholder = m.placeholder()
	m.follow = true
	m.input.Reset()
	m.refreshViewport()
	m.layout()
	return tea.Batch(m.setFocus(focusInput), reconcileCmd(m.ctx, m.c, m.channelID), m.setStatus("opened "+channelRef(info), false))
}

// openVariants is /variants: with no argument it opens the picker for the
// selected agent's model; with a name it sets that variant ("default"
// clears it).
func (m *Model) openVariants(arg string) tea.Cmd {
	a := m.selectedAgent()
	if a == nil {
		return m.setStatus("no agent selected", true)
	}
	modelID, current := m.channel.Model, a.Variant
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

// chatCache is the render cache of agent id's transcript.
func (m *Model) chatCache(id string) *render.Cache {
	c := m.renders[id]
	if c == nil {
		c = &render.Cache{}
		m.renders[id] = c
	}
	return c
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
