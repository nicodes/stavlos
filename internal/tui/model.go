// Package tui is the Bubble Tea terminal frontend (PRD §7.2). It is the
// reference client for the protocol: everything it shows comes from the event
// stream, and every action it takes is a protocol call issued as a tea.Cmd.
package tui

import (
	"context"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/tui/dialog"
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
		_ = call(uctx, c, protocol.Unsubscribe, protocol.SubscribeParams{Channel: channelID})
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
	visited     map[string]replayed    // channels switched away from: what their replay built, so a return replays only what it missed
	dirsNext    bool                   // another channel's gear was chosen: its dirs dialog opens once the switch lands

	presets []protocol.PresetInfo

	vp       viewport.Model
	input    textarea.Model // grows with the text, up to inputMaxLines
	sp       spinner.Model
	spinning bool // a spinner tick is scheduled: it runs only while something animates

	width, height int
	hoverFocus    bool      // the chat has focus because the mouse is over it (released when the mouse leaves)
	hoverFrom     focus     // where focus was before hover took it, restored when the mouse leaves the chat
	sel           selection // mouse text selection (drag to select, release to copy)
	metaSel       metaPart  // the highlighted part of the meta row while it has focus
	tabSel        int       // the highlighted tab (index into tabFocuses) while the strip has focus
	follow        bool      // auto-scroll to bottom
	viewDirty     bool      // the chat changed (an event, a stream delta, a tick): Update redraws it once, at the end

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

// replayed is what replaying a channel's log built: kept when the TUI
// switches away (Model.visited), so switching back subscribes from the
// next seq instead of replaying the whole log again.
type replayed struct {
	spawned     map[string]time.Time
	parentOf    map[string]string
	transcripts map[string]*transcript.Transcript
	seq         int64
	left        time.Time
}

// maxVisited is how many channels keep their replay once left.
const maxVisited = 8

// stash keeps the bound channel's replay in visited, making room by
// dropping the channel left longest ago. A channel whose snapshot never
// landed has nothing worth keeping.
func (m *Model) stash() {
	if !m.reconciled || m.channelID == "" {
		return
	}
	if m.visited == nil {
		m.visited = map[string]replayed{}
	}
	for len(m.visited) >= maxVisited {
		oldest := ""
		for id, r := range m.visited {
			if oldest == "" || r.left.Before(m.visited[oldest].left) {
				oldest = id
			}
		}
		delete(m.visited, oldest)
	}
	m.visited[m.channelID] = replayed{m.spawned, m.parentOf, m.transcripts, m.seq, time.Now()}
}

// restore puts back what replaying id built before, if it was visited:
// the reconcile then subscribes from the seq after it. A stream cut off
// when the TUI left is dropped; the events replayed since settle the rest.
func (m *Model) restore(id string) {
	r, ok := m.visited[id]
	if !ok {
		return
	}
	delete(m.visited, id)
	m.spawned, m.parentOf, m.transcripts, m.seq = r.spawned, r.parentOf, r.transcripts, r.seq
	for _, t := range m.transcripts {
		t.ApplyStream(protocol.StreamNotification{Reset: true})
	}
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

// chatPage is how many items pgup/pgdn move the chat cursor.
const chatPage = 5

// inputMaxLines is the least the input may grow to before it scrolls
// inside; inputHardMax the most. Between them the cap follows the window
// (inputCap), so a big paste on a tall terminal is shown whole.
const (
	inputMaxLines = 8
	inputHardMax  = 40
)

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
		spinning:     true, // Init schedules the first tick
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
	cmds = append(cmds, m.ensureFocus(), m.ensureSpin())
	if m.viewDirty {
		m.refreshViewport()
	}
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

// --- focus ---

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

// --- keys ---

// --- events ---

// --- prompts ---

// --- agents / selection ---

// --- prompt history ---

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
	m.status, m.statusErr = textsafe.Clean(text), isErr // errors carry the daemon's and providers' text
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

	vw := m.contentWidth()
	widthChanged := vw != m.vp.Width
	m.vp.Width, m.vp.Height = vw, m.computeFrame().bodyH
	if widthChanged {
		m.refreshViewport()
	} else if m.follow {
		m.vp.GotoBottom()
	}
}

// --- overlay (provider / model pickers) ---
