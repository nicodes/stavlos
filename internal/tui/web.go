package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/pkg/client"
)

// The web UI (docs/web-ui.md) is a loopback listener in the daemon. Its row in
// the nav's Clients section shows whether it is on; a click turns it on and opens it in a
// browser tab, and a click while it is on opens its controls.

type webMsg struct {
	status protocol.WebStatus
	err    error
	action string
}
type webTickMsg struct{ epoch uint64 }

// servicePoll is how often a status the daemon pushes changes of is asked
// for anyway, in case a notice was shed from a full queue.
const servicePoll = 30 * time.Second

// changedMsg is the daemon saying a status is stale (protocol.NChanged).
type changedMsg struct{ what string }

// onService handles what the Clients rows of the nav are told: a status, the
// poll that asks for one again, and the daemon saying one is stale. A poll
// from before the last change of epoch is dropped.
func (m *Model) onService(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case webMsg:
		return m.onWeb(msg)
	case discordMsg:
		return m.onDiscord(msg)
	case changedMsg:
		return m.onChanged(msg)
	case webTickMsg:
		if msg.epoch == m.webEpoch {
			return webCmd(m.ctx, m.c, "status")
		}
	case discordTickMsg:
		if msg.epoch == m.discordEpoch {
			return discordCmd(m.ctx, m.c, "status", msg.epoch)
		}
	}
	return nil
}

// onChanged asks once for what changed. Each status reply arms the next
// poll, so the epoch moves first: the poll already armed finds itself stale
// and one chain of polls stays one.
func (m *Model) onChanged(msg changedMsg) tea.Cmd {
	switch msg.what {
	case protocol.ChangedWeb:
		m.webEpoch++
		return webCmd(m.ctx, m.c, "status")
	case protocol.ChangedPlan:
		return planUsageCmd(m.ctx, m.c) // asked once and answered once: nothing to chain
	case protocol.ChangedDiscord:
		m.discordEpoch++
		return discordCmd(m.ctx, m.c, "status", m.discordEpoch)
	}
	return nil
}

func webCmd(ctx context.Context, c *client.Client, action string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		m := protocol.WebStatusMethod
		switch action {
		case "on":
			m = protocol.WebEnable
		case "off":
			m = protocol.WebDisable
		case "open":
			m = protocol.WebOpen
		}
		s, err := client.Do(ctx, c, m, protocol.None{})
		return webMsg{status: s, err: err, action: action}
	})
}

// serviceCommand runs the commands of the daemon's background services.
func (m *Model) serviceCommand(name, rest string) (tea.Cmd, bool) {
	switch name {
	case "/discord":
		return m.openDiscord(rest), true
	case "/web":
		return m.openWeb(rest), true
	}
	return nil, false
}

// openWeb runs /web [on|off|open]; bare /web opens the controls.
func (m *Model) openWeb(action string) tea.Cmd {
	switch action = strings.ToLower(strings.TrimSpace(action)); action {
	case "":
		o := newOverlay(ovWeb, overlayList, "Web UI")
		m.setWebItems(o)
		return tea.Batch(m.openOverlay(o), webCmd(m.ctx, m.c, "status"))
	case "on", "off", "open":
		return webCmd(m.ctx, m.c, action)
	}
	return m.setStatus("usage: /web [on|off|open]", true)
}

// webClick is a click on the nav's Web UI row: off turns it on (which opens
// the browser tab), on opens the controls.
func (m *Model) webClick() tea.Cmd {
	if m.webKnown && m.webStatus.Enabled {
		return m.openWeb("")
	}
	return m.openWeb("on")
}

func (m *Model) submitWeb() tea.Cmd {
	if it := m.ov.selected(); it != nil {
		switch it.id {
		case "on", "off", "open":
			return m.openWeb(it.id)
		}
	}
	return nil
}

func (m *Model) setWebItems(o *overlay) {
	s := m.webStatus
	var items []overlayItem
	if s.Enabled {
		items = append(items,
			overlayItem{id: "open", label: "Open in a browser tab", hint: format.Trunc(s.URL, 40)},
			overlayItem{id: "off", label: "Disable", hint: "Stop the listener and sign every browser out"})
	} else {
		items = append(items, overlayItem{id: "on", label: "Enable", hint: "Serve on " + s.URL + " and open it"})
	}
	if s.Error != "" {
		items = append(items, overlayItem{id: "error", label: "Error", hint: s.Error})
	}
	state := "off"
	if s.Enabled {
		state = "on"
	}
	o.title = "Web UI · " + state
	o.setItems(items)
}

func (m *Model) onWeb(msg webMsg) tea.Cmd {
	s := msg.status
	s.URL, s.Error = textsafe.Clean(s.URL), textsafe.Clean(s.Error)
	if msg.err != nil {
		s.Error = textsafe.Clean(msg.err.Error())
		if msg.action != "status" {
			s.Enabled, s.URL = m.webStatus.Enabled && msg.action != "on", m.webStatus.URL
		}
	}
	m.webStatus, m.webKnown = s, true
	if m.ov != nil && m.ov.kind == ovWeb {
		m.setWebItems(m.ov)
	}
	var cmds []tea.Cmd
	if msg.action == "status" {
		cmds = append(cmds, tick(servicePoll, webTickMsg{m.webEpoch})) // the daemon says when it changes; this is the net under that
	}
	switch {
	case msg.err != nil && msg.action != "status":
		cmds = append(cmds, m.setStatus("web UI: "+s.Error, true))
	case msg.status.OpenURL != "":
		// the URL carries a one-time code: it goes to the browser, never to the screen
		cmds = append(cmds, openBrowserCmd(msg.status.OpenURL), m.setStatus("web UI opened in your browser · "+s.URL, false))
	case msg.action == "off":
		cmds = append(cmds, m.setStatus("web UI disabled", false))
	}
	return tea.Batch(cmds...)
}

// webIndicator is the nav's Web UI row: a full dot and the port while the
// listener is up, an empty one while it is off.
func (m Model) webIndicator(width int) string {
	text, style := "○ Web UI off", theme.StyleDim
	switch s := m.webStatus; {
	case !m.webKnown:
		text = "○ Web UI"
	case s.Error != "" && !s.Enabled:
		text, style = "! Web UI error", theme.StyleError
	case s.Enabled:
		text, style = "● Web UI "+strings.TrimSuffix(strings.TrimPrefix(s.URL, "http://"), "/"), theme.StyleStatusOK
	}
	return style.Render(format.Trunc(text, width))
}
