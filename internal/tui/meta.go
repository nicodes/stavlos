package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
)

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
	case metaMode:
		if m.modeTag() == "ASK" {
			return m.openMode()
		}
		return setModeCmd(m.ctx, m.c, m.channelID, protocol.ModeAsk)
	case metaRole:
		return rolesCmd(m.ctx, m.c, m.requestScope(), false)
	case metaModel:
		return modelsCmd(m.ctx, m.c, m.requestScope())
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

// metaPart names what sits under an x position on the meta row.
type metaPart int

const (
	metaNone    metaPart = iota
	metaMode             // the ASK/AUTO/YOLO mode tag: AUTO and YOLO go back to ask, ASK opens /mode
	metaRole             // "label (role)": click opens /roles
	metaModel            // the model: click opens /models
	metaVariant          // the variant: click opens /variants
)
