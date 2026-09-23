package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/pkg/client"
)

// The command sandbox, as a person meets it: on or off is theirs to choose
// (/sandbox, or a click on the nav's row), and "limited" is the machine's
// doing, which the dialog explains and says how to mend. A full sandbox says
// nothing anywhere.

type sandboxMsg struct {
	status protocol.SandboxStatus
	err    error
	set    bool // the reply to a change, not to a question
}

func sandboxCmd(ctx context.Context, c *client.Client, action string) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		if action == "status" {
			s, err := client.Do(ctx, c, protocol.SandboxStatusMethod, protocol.None{})
			return sandboxMsg{status: s, err: err}
		}
		s, err := client.Do(ctx, c, protocol.SandboxSet, protocol.SandboxSetParams{Enabled: action == "on"})
		return sandboxMsg{status: s, err: err, set: true}
	})
}

// openSandbox runs /sandbox [on|off]; bare /sandbox opens the dialog.
func (m *Model) openSandbox(action string) tea.Cmd {
	switch action = strings.ToLower(strings.TrimSpace(action)); action {
	case "":
		o := newOverlay(ovSandbox, overlayList, "Sandbox")
		m.setSandboxItems(o)
		return tea.Batch(m.openOverlay(o), sandboxCmd(m.ctx, m.c, "status"))
	case "on", "off":
		return sandboxCmd(m.ctx, m.c, action)
	}
	return m.setStatus("usage: /sandbox [on|off]", true)
}

func (m *Model) setSandboxItems(o *overlay) {
	s := m.sandboxStatus
	var items []overlayItem
	state := "on"
	switch {
	case !m.sandboxKnown:
		state = "…"
	case !s.Enabled:
		state = "off"
		items = append(items, overlayItem{id: "on", label: "Turn on", hint: "Commands write only inside the channel's directories and cannot see your credentials"})
	default:
		if s.Level != "full" {
			state = "on · " + s.Level
		}
		for _, missing := range s.Missing {
			items = append(items, overlayItem{id: "info", label: "Missing", hint: missing})
		}
		if s.Level != "full" && s.Why != "" {
			items = append(items, overlayItem{id: "info", label: "Because", hint: s.Why})
		}
		if s.Fix != "" {
			items = append(items, overlayItem{id: "fix", label: "Fix", hint: s.Fix + "  (enter copies it; then restart the daemon)"})
		}
		items = append(items, overlayItem{id: "off", label: "Turn off", hint: "Commands run with your full access; nothing is hidden from them"})
	}
	o.title = "Sandbox · " + state
	o.setItems(items)
}

func (m *Model) onSandbox(msg sandboxMsg) tea.Cmd {
	if msg.err != nil {
		return m.setStatus("sandbox: "+textsafe.Clean(msg.err.Error()), true)
	}
	s := msg.status
	s.Level, s.Why, s.Fix = textsafe.Clean(s.Level), textsafe.Clean(s.Why), textsafe.Clean(s.Fix)
	for i := range s.Missing {
		s.Missing[i] = textsafe.Clean(s.Missing[i])
	}
	m.sandboxStatus, m.sandboxKnown = s, true
	if m.ov != nil && m.ov.kind == ovSandbox {
		m.setSandboxItems(m.ov)
	}
	if !msg.set {
		return nil
	}
	said := "sandbox on"
	if !s.Enabled {
		said = "sandbox off: commands run with your full access"
	}
	// the channel's own row comes from its configuration, loaded again by now
	return tea.Batch(m.setStatus(said, !s.Enabled), reconcileCmd(m.ctx, m.c, m.requestScope()))
}

func (m *Model) submitSandbox() tea.Cmd {
	it := m.ov.selected()
	if it == nil {
		return nil
	}
	switch it.id {
	case "on", "off":
		return m.openSandbox(it.id)
	case "fix":
		return tea.Batch(copyCmd(m.sandboxStatus.Fix), m.setStatus("copied: "+m.sandboxStatus.Fix, false))
	}
	return nil
}
