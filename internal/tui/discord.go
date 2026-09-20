package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/pkg/client"
)

type discordMsg struct {
	status protocol.DiscordStatus
	err    error
	epoch  uint64
}
type discordTickMsg struct{ epoch uint64 }

func discordCmd(ctx context.Context, c *client.Client, action string, epoch uint64) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		m := protocol.DiscordStatusMethod
		switch action {
		case "connect":
			m = protocol.DiscordConnect
		case "disconnect":
			m = protocol.DiscordDisconnect
		}
		s, err := client.Do(ctx, c, m, protocol.None{})
		if err != nil && action != "status" {
			// Keep status/error details without retrying the mutating command.
			if current, e := client.Do(ctx, c, protocol.DiscordStatusMethod, protocol.None{}); e == nil {
				s = current
			}
		}
		return discordMsg{status: s, err: err, epoch: epoch}
	})
}

func (m *Model) openDiscord(action string) tea.Cmd {
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = "status"
	}
	if action != "status" && action != "connect" && action != "disconnect" {
		return m.setStatus("usage: /discord [status|connect|disconnect]", true)
	}
	m.discordEpoch++
	o := newOverlay(ovDiscord, overlayList, "Discord")
	o.setEmpty("checking connection…", false)
	return tea.Batch(m.openOverlay(o), discordCmd(m.ctx, m.c, action, m.discordEpoch))
}

func (m *Model) submitDiscord() tea.Cmd {
	it := m.ov.selected()
	if it == nil {
		return nil
	}
	switch it.id {
	case "connect", "disconnect", "status":
		return m.openDiscord(it.id)
	}
	return nil
}

func (m *Model) onDiscord(msg discordMsg) tea.Cmd {
	if msg.epoch != m.discordEpoch {
		return nil
	}
	s := msg.status
	if msg.err != nil {
		s.Error = msg.err.Error()
		if s.State == "" {
			s.State = "unavailable"
		}
	}
	s.State, s.Bot, s.GuildName = textsafe.Clean(s.State), textsafe.Clean(s.Bot), textsafe.Clean(s.GuildName)
	s.Guild, s.Error, s.ConfigPath = textsafe.Clean(s.Guild), textsafe.Clean(s.Error), textsafe.Clean(s.ConfigPath)
	if s.State == "" {
		s.State = "disconnected"
	}
	m.discordStatus, m.discordKnown = s, true
	refresh := tick(servicePoll, discordTickMsg{epoch: m.discordEpoch}) // the daemon says when it changes
	if m.ov == nil || m.ov.kind != ovDiscord {
		return refresh
	}
	o := m.ov
	selected := ""
	if it := o.selected(); it != nil {
		selected = it.id
	}
	o.title = "Discord · " + s.State
	var items []overlayItem
	if s.ConfigPath != "" {
		hint := "Global settings"
		if !s.Configured {
			hint = "Add a discord block"
		}
		items = append(items, overlayItem{id: "config", label: "Config: " + format.ShortHome(s.ConfigPath), hint: hint})
	}
	if s.Bot != "" {
		items = append(items, overlayItem{id: "bot", label: "Bot: " + s.Bot})
	}
	guild := s.GuildName
	if guild == "" {
		guild = s.Guild
	}
	if guild != "" {
		items = append(items, overlayItem{id: "guild", label: "Server: " + guild})
	}
	items = append(items, overlayItem{id: "channels", label: fmt.Sprintf("Bridged channels: %d", s.Channels)})
	auto := "off"
	if s.Enabled {
		auto = "on"
	}
	items = append(items, overlayItem{id: "enabled", label: "Automatic connection: " + auto})
	if s.Error != "" {
		items = append(items, overlayItem{id: "error", label: "Connection error", hint: s.Error})
	}
	items = append(items,
		overlayItem{id: "connect", label: "Connect", hint: "Use saved config and enable automatic connection"},
		overlayItem{id: "disconnect", label: "Disconnect", hint: "Stop Discord and disable automatic connection"},
		overlayItem{id: "status", label: "Refresh status"})
	if selected == "" {
		selected = "connect"
		if s.Enabled && s.State != "error" {
			selected = "disconnect"
		}
	}
	o.setItems(items)
	for i, it := range o.shown {
		if it.id == selected {
			o.cursor = i
			o.clampOffset()
			break
		}
	}
	return refresh
}

// discordIndicator shows the observed connection, not just the autoconnect
// preference. It is refreshed even when the status panel is closed.
func (m Model) discordIndicator(width int) string {
	state, mark, style := "checking…", "○", theme.StyleDim
	if m.discordKnown {
		s := m.discordStatus
		switch s.State {
		case "connected":
			state, mark, style = "connected", "●", theme.StyleStatusOK
			if s.Error != "" {
				state, style = "connected !", theme.StyleWarn
			}
		case "connecting", "reconnecting", "stopping":
			state, mark, style = s.State, "◐", theme.StyleWarn
		case "error", "unavailable":
			state, mark, style = s.State, "!", theme.StyleError
		case "disconnected":
			state = "disconnected"
			if !s.Configured {
				state = "not configured"
			}
			if s.Error != "" {
				state, mark, style = "error", "!", theme.StyleError
			}
		default:
			state, mark, style = "unknown", "?", theme.StyleWarn
		}
	}
	return style.Render(format.Trunc(mark+" Discord "+state, width))
}
