package tui

import (
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
	agent := m.selectedID()

	needAgent := func() tea.Cmd {
		if agent == "" {
			return m.setStatus("no agent selected", true)
		}
		return nil
	}
	if cmd, ok := m.usageCommand(name, rest); ok { // /tokens, /cost, /plan and /recap
		return cmd
	}

	switch name {
	case "/settings", "/config":
		return m.settingsCommand(rest)
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
	case "/discord":
		return m.openDiscord(rest)
	case "/roles", "/role", "/presets":
		// The one role dialog: enter switches the selected agent's preset.
		// A name argument sets it directly.
		if c := needAgent(); c != nil {
			return c
		}
		if rest == "" {
			return rolesCmd(m.ctx, m.c, m.requestScope(), false)
		}
		return pickRoleCmd(m.ctx, m.c, agent, strings.ToLower(rest))
	case "/channels", "/resume", "/channel":
		return channelsCmd(m.ctx, m.c, m.requestScope(), channelsPicker)
	case "/dir":
		if rest == "" {
			return m.openTab(focusDirs)
		}
		return setDirCmd(m.ctx, m.c, m.requestScope(), rest)
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
		return modelsCmd(m.ctx, m.c, m.requestScope())
	case "/providers", "/provider", "/connect", "/login":
		// The one provider dialog: sign in, re-sign in, sign out. A name
		// argument jumps straight to that provider's sign-in.
		return providersCmd(m.ctx, m.c, providersMsg{jump: strings.ToLower(rest)})
	}
	if rest != "" {
		return m.setStatus("custom commands do not take arguments", true)
	}
	if m.superChat {
		agent = ""
	}
	return customCommandRunCmd(m.ctx, m.c, m.requestScope(), strings.TrimPrefix(name, "/"), agent)
}
