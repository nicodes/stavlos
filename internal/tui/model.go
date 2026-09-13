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
	spawned   map[string]time.Time // agent id → spawn time, for the monitors block

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
	showTree      bool // right sidebar toggle (/tree, ctrl+b)
	showTips      bool // home-state tips block (/tips)
	details       bool // expanded tool output (/details)
	follow        bool // auto-scroll to bottom

	status      string
	statusErr   bool
	statusToken int

	confirmKill bool

	// Focus: the sidebar takes ↑/↓/enter when focused; otherwise ↑/↓ in
	// the input walk the prompt history.
	sidebarFocus bool
	sbCursor     int
	history      []string // prompts sent from this client (and replayed human prompts)
	histIdx      int      // == len(history) when editing a new line
	histDraft    string   // unsent text saved while browsing history
	loading      bool     // replaying events up to replayTo
	replayTo     int64    // seq from reconcile
	treeTimer    bool     // a debounced tree refresh is scheduled
	reconciled   bool     // the first reconcile landed

	ov        *overlay                // open modal, or nil
	providers []protocol.ProviderInfo // last provider.list result
	login     loginFlow               // device-code sign-in in progress

	fatal error
}

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

	return Model{
		ctx:         ctx,
		c:           c,
		sessionID:   sessionID,
		transcripts: map[string]*Transcript{},
		claimedByUs: map[string]bool{},
		vp:          vp,
		input:       ti,
		sp:          sp,
		showTips:    true,
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
		m.follow = m.vp.AtBottom()
		cmds = append(cmds, cmd)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		cmds = append(cmds, cmd)
		// Running tool lines carry the spinner glyph, so they need redraws.
		if t := m.transcripts[m.selectedID()]; t != nil && t.Running() {
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
		m.applyPromptNotification(msg.n)

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

	case modelsMsg:
		cmds = append(cmds, m.onModels(msg))

	default:
		// Cursor blink and other component-internal messages.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
		if m.ov != nil {
			cmds = append(cmds, m.ov.update(msg))
		}
	}

	m.layout()
	return m, tea.Batch(cmds...)
}

// --- keys ---

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	if key.Matches(msg, keys.Quit) {
		return tea.Quit
	}
	if m.ov != nil {
		return m.overlayKey(msg)
	}

	if m.confirmKill {
		m.confirmKill = false
		if key.Matches(msg, keys.Yes) {
			return m.killSelected()
		}
		return m.setStatus("kill cancelled", false)
	}

	if m.sidebarFocus && m.sidebarVisible() {
		return m.sidebarKey(msg)
	}
	m.sidebarFocus = false

	switch {
	case key.Matches(msg, keys.NextAgent):
		return m.moveSelection(1)
	case key.Matches(msg, keys.PrevAgent):
		return m.moveSelection(-1)
	case key.Matches(msg, keys.ToggleTree):
		return m.toggleTree()
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
		m.historyMove(-1)
		return nil
	case key.Matches(msg, keys.SelDown):
		m.historyMove(1)
		return nil
	case key.Matches(msg, keys.Clear):
		m.input.Reset()
		return nil
	case key.Matches(msg, keys.Submit):
		return m.submit()
	}

	// Prompt hotkeys apply only with an empty input so they cannot swallow
	// the first letter of a message.
	if p := m.currentPrompt(); p != nil && m.input.Value() == "" && p.Kind != "question" {
		switch {
		case key.Matches(msg, keys.Yes):
			return m.answerPrompt(p, "allow")
		case key.Matches(msg, keys.No):
			return m.answerPrompt(p, "deny")
		case key.Matches(msg, keys.Always) && p.Kind != "trust":
			return m.answerPrompt(p, "allow_always")
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// submit handles Enter: a question answer, a /command, or a prompt envelope.
func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	m.input.Reset()
	if text == "" {
		return nil
	}
	m.pushHistory(text)
	if p := m.currentPrompt(); p != nil && p.Kind == "question" && !strings.HasPrefix(text, "/") {
		if n, err := strconv.Atoi(text); err == nil && n >= 1 && n <= len(p.Options) {
			text = p.Options[n-1]
		}
		return m.answerPrompt(p, text)
	}
	if strings.HasPrefix(text, "/") {
		return m.command(text)
	}
	agent := m.selectedID()
	if agent == "" {
		return m.setStatus("no agent selected", true)
	}
	return sendCmd(m.ctx, m.c, agent, protocol.KindPrompt, text, "")
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
		m.notice(helpLines...)
		return nil
	case "/tree":
		return m.toggleTree()
	case "/details":
		m.details = !m.details
		m.refreshViewport()
		if m.details {
			return m.setStatus("tool output expanded", false)
		}
		return m.setStatus("tool output collapsed", false)
	case "/tips":
		m.showTips = !m.showTips
		return nil
	case "/presets":
		return presetsCmd(m.ctx, m.c, m.sessionID)
	case "/steer":
		if c := needAgent(); c != nil {
			return c
		}
		if rest == "" {
			return m.setStatus("usage: /steer <text>", true)
		}
		return sendCmd(m.ctx, m.c, agent, protocol.KindSteer, rest, "steer sent")
	case "/cancel":
		if c := needAgent(); c != nil {
			return c
		}
		return sendCmd(m.ctx, m.c, agent, protocol.KindCancel, "", "cancel sent")
	case "/kill":
		if c := needAgent(); c != nil {
			return c
		}
		m.confirmKill = true
		return nil
	case "/spawn":
		if c := needAgent(); c != nil {
			return c
		}
		parts := strings.Fields(rest)
		if len(parts) < 3 {
			return m.setStatus("usage: /spawn <archetype> <label> <task…>", true)
		}
		return spawnCmd(m.ctx, m.c, protocol.AgentSpawnParams{
			Parent:    agent,
			Archetype: parts[0],
			Label:     parts[1],
			Task:      strings.Join(parts[2:], " "),
		})
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

func (m *Model) killSelected() tea.Cmd {
	agent := m.selectedID()
	if agent == "" {
		return m.setStatus("no agent selected", true)
	}
	return sendCmd(m.ctx, m.c, agent, protocol.KindKill, "", "kill sent")
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
		event.AgentModelChanged, event.SessionModelChanged:
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

func (m *Model) applyPromptNotification(n protocol.PromptNotification) {
	if n.Prompt.Session != "" && n.Prompt.Session != m.sessionID {
		return
	}
	switch n.Action {
	case "requested", "escalated", "claimed":
		m.upsertPrompt(n.Prompt)
	case "answered", "withdrawn", "defaulted":
		m.removePrompt(n.Prompt.ID)
	}
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
		m.follow = true
		m.refreshViewport()
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
	m.follow = true
	m.refreshViewport()
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
		m.focusSidebar()
	} else {
		m.focusInput()
	}
	return nil
}

// focusSidebar moves keyboard focus to the agent list.
func (m *Model) focusSidebar() {
	m.sidebarFocus = true
	m.sbCursor = m.selected
	m.input.Blur()
}

// focusInput returns keyboard focus to the text input.
func (m *Model) focusInput() {
	m.sidebarFocus = false
	m.input.Focus()
}

// sidebarKey handles keys while the sidebar has focus: ↑/↓ (or j/k) move
// the cursor, enter selects that agent and returns to the input, esc
// returns without changing the selection, ctrl+b closes the sidebar.
func (m *Model) sidebarKey(msg tea.KeyMsg) tea.Cmd {
	n := len(m.agents)
	switch {
	case key.Matches(msg, keys.ToggleTree):
		return m.toggleTree()
	case key.Matches(msg, keys.OvClose), key.Matches(msg, keys.NextAgent):
		m.focusInput()
		return nil
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
		m.focusInput()
		return nil
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
	m.input.Width = boxW - 3 - len([]rune(m.input.Prompt)) - 1 // border + padding + cursor

	_, kb := m.keyBarView()
	bodyH := m.height - 1 - kb - 1 - inputBoxLines // footer, key bar, spacer, input box
	if mv := m.monitorsView(m.contentWidth()); mv != "" {
		bodyH -= strings.Count(mv, "\n") + 1
	}
	if pb := m.promptView(m.contentWidth()); pb != "" {
		bodyH -= strings.Count(pb, "\n") + 1
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

// refreshViewport re-renders the selected transcript into the viewport.
func (m *Model) refreshViewport() {
	var lines []Line
	if t := m.transcripts[m.selectedID()]; t != nil {
		lines = t.All()
	}
	m.vp.SetContent(Render(lines, RenderOpts{Width: m.vp.Width, Details: m.details, Spinner: m.sp.View()}))
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
