package tui

import (
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
)

// cancelWindow is how long a first esc stays armed for the second.
const cancelWindow = 3 * time.Second

// ctrlC is the two-step quit: the first press clears the input, closes any
// dialog, focuses the input and warns; a second within cancelWindow quits.
func (m *Model) ctrlC() tea.Cmd {
	if !m.quitArmed.IsZero() && time.Since(m.quitArmed) <= cancelWindow {
		return tea.Quit
	}
	m.quitArmed = clock()
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
	m.cancelArmed = clock()
	return m.setStatusFor("press esc again to cancel "+a.Name+"'s turn", true, cancelWindow)
}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	if m.cfgEditor != nil {
		return m.configEditorKey(msg)
	}
	if key.Matches(msg, keys.Quit) {
		return m.ctrlC()
	}
	m.quitArmed = time.Time{} // any other key disarms the two-step quit
	// ctrl+space goes back to typing from anywhere, an overlay or a dialog's
	// text field included; space and enter both select, open and toggle
	// outside one.
	if key.Matches(msg, keys.FocusInput) {
		if m.ov != nil {
			return m.closeOverlayToInput() // the overlay closes with nothing picked
		}
		return m.setFocus(focusInput)
	}
	if m.ov != nil {
		return m.overlayKey(msg)
	}
	if !key.Matches(msg, keys.Clear) {
		m.cancelArmed = time.Time{} // any other key disarms the two-step cancel
	}

	// While the "/" palette is open in the input, tab completes the command
	// (handled below) instead of cycling focus.
	paletteOpen := m.focus == focusInput && (len(m.paletteMatches(m.input.Value())) > 0 || len(m.mentionMatches()) > 0)

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
	case focusUsage:
		return m.usageKey(msg)
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
	case focusPermission, focusInlinePermission:
		return m.permissionKey(msg)
	case focusQuestions:
		return m.questionsKey(msg)
	}

	return m.inputKey(msg)
}

func (m *Model) command(text string) tea.Cmd {
	fields := strings.Fields(text)
	name := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	if c, ok := commandNamed(name); ok {
		run := commandRuns[c.Name]
		if run.needsAgent && m.selectedID() == "" {
			return m.setStatus("no agent selected", true)
		}
		return run.run(m, name, rest)
	}
	if rest != "" {
		return m.setStatus("custom commands do not take arguments", true)
	}
	agent := m.selectedID()
	if m.superChat {
		agent = ""
	}
	return customCommandRunCmd(m.ctx, m.c, m.requestScope(), strings.TrimPrefix(name, "/"), agent)
}

// commandNamed finds a built-in command by its name or an alias.
func commandNamed(name string) (Command, bool) {
	for _, c := range commands {
		if c.Name == name || slices.Contains(c.Aliases, name) {
			return c, true
		}
	}
	return Command{}, false
}

// commandRun is what a built-in command does. name is what was typed (an
// alias, or one of two commands that share a run); rest is its argument.
type commandRun struct {
	needsAgent bool
	run        func(m *Model, name, rest string) tea.Cmd
}

// commandRuns is what each command in the palette does, keyed by its name
// there: the palette lists a command, this table runs it, and a test holds
// them to each other. (It is filled in init because the runs reach code that
// reads the palette, which a package-level initialiser may not.)
var commandRuns map[string]commandRun

func init() {
	usage := func(m *Model, name, rest string) tea.Cmd { cmd, _ := m.usageCommand(name, rest); return cmd }
	service := func(m *Model, name, rest string) tea.Cmd { cmd, _ := m.serviceCommand(name, rest); return cmd }
	view := func(m *Model, name, _ string) tea.Cmd { return m.viewCommand(name) }
	mode := func(m *Model, name, rest string) tea.Cmd { return m.toggleMode(name[1:], rest) }
	commandRuns = map[string]commandRun{
		"/tokens": {run: usage}, "/cost": {run: usage}, "/plan": {run: usage}, "/recap": {run: usage},
		"/discord": {run: service}, "/web": {run: service},
		"/tree": {run: view}, "/history": {run: view},
		"/yolo": {run: mode}, "/auto": {run: mode},
		"/settings": {run: func(m *Model, _, rest string) tea.Cmd { return m.settingsCommand(rest) }},
		"/help": {run: func(m *Model, _, _ string) tea.Cmd {
			m.hideKeys = !m.hideKeys
			m.layout()
			if m.hideKeys {
				return m.setStatus("key bar hidden (/help shows it)", false)
			}
			return m.setStatus("key bar shown (/help hides it)", false)
		}},
		"/chat": {run: func(m *Model, _, _ string) tea.Cmd { return m.openChat() }},
		// The one role dialog: enter switches the selected agent's preset. A
		// name argument sets it directly.
		"/roles": {needsAgent: true, run: func(m *Model, _, rest string) tea.Cmd {
			if rest == "" {
				return rolesCmd(m.ctx, m.c, m.requestScope(), false)
			}
			return pickRoleCmd(m.ctx, m.c, m.selectedID(), strings.ToLower(rest))
		}},
		"/channels": {run: func(m *Model, _, _ string) tea.Cmd {
			return channelsCmd(m.ctx, m.c, m.requestScope(), channelsPicker)
		}},
		"/dir": {run: func(m *Model, _, rest string) tea.Cmd {
			if rest == "" {
				return m.openTab(focusDirs)
			}
			return setDirCmd(m.ctx, m.c, m.requestScope(), rest)
		}},
		"/rename": {run: func(m *Model, _, rest string) tea.Cmd {
			if rest == "" {
				return m.setStatus("usage: /rename <name>", true)
			}
			return renameChannelCmd(m.ctx, m.c, m.channelID, strings.TrimPrefix(rest, "#"))
		}},
		// (the compaction events drive the bar and the result line)
		"/compact":  {needsAgent: true, run: func(m *Model, _, _ string) tea.Cmd { return compactCmd(m.ctx, m.c, m.selectedID()) }},
		"/mode":     {run: func(m *Model, _, _ string) tea.Cmd { return m.openMode() }},
		"/variants": {needsAgent: true, run: func(m *Model, _, rest string) tea.Cmd { return m.openVariants(rest) }},
		"/queue": {needsAgent: true, run: func(m *Model, _, rest string) tea.Cmd {
			if rest == "" {
				return m.setStatus("usage: /queue <text>", true)
			}
			return sendCmd(m.ctx, m.c, m.selectedID(), protocol.KindPrompt, rest, "queued for after the current turn")
		}},
		// The one model dialog: enter sets the selected agent's model, ctrl+s
		// the channel default.
		"/models": {run: func(m *Model, _, _ string) tea.Cmd { return modelsCmd(m.ctx, m.c, m.requestScope()) }},
		// The one provider dialog: sign in, re-sign in, sign out. A name
		// argument jumps straight to that provider's sign-in.
		"/providers": {run: func(m *Model, _, rest string) tea.Cmd {
			return providersCmd(m.ctx, m.c, providersMsg{jump: strings.ToLower(rest)})
		}},
	}
}
