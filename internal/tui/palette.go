package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/theme"
)

// Command is one slash command the input understands. The registry drives
// the "/" palette and the /help text so they cannot drift.
type Command struct {
	Name    string   // "/queue"
	Args    string   // "<text>", "[openai|xai]", or ""
	Desc    string   // one line
	Aliases []string // "/connect", "/login"
	Direct  bool     // runs on enter in the palette (no arguments needed)
}

// commands is every slash command, in palette order.
var commands = []Command{
	{Name: "/queue", Args: "<text>", Desc: "send after the current turn ends (plain enter reaches a busy agent at its next step)"},
	{Name: "/providers", Args: "[openai|xai]", Desc: "manage providers: sign in with ChatGPT or Grok (enter) or sign out (ctrl+d); a name jumps to its sign-in", Aliases: []string{"/provider", "/connect", "/login"}, Direct: true},
	{Name: "/models", Desc: "pick a model: enter sets the selected agent's, ctrl+s the channel default", Aliases: []string{"/model"}, Direct: true},
	{Name: "/variants", Args: "[name|default]", Desc: "pick a model variant (reasoning effort) for the selected agent; no arg opens a picker", Aliases: []string{"/variant"}, Direct: true},
	{Name: "/roles", Args: "[name]", Desc: "pick the selected agent's role (preset); a name sets it directly", Aliases: []string{"/role", "/presets"}, Direct: true},
	{Name: "/mode", Desc: "permission mode for the channel: ask, auto (free inside the channel's directories) or yolo", Direct: true},
	{Name: "/auto", Args: "[on|off]", Desc: "auto mode: approve permissions inside the channel's directories, deny outside (no arg toggles)", Direct: true},
	{Name: "/yolo", Args: "[on|off]", Desc: "yolo mode: approve every permission, directories included (no arg toggles)", Direct: true},
	{Name: "/channels", Desc: "pick any channel across your working directories", Aliases: []string{"/resume", "/channel"}, Direct: true},
	{Name: "/dir", Desc: "set this channel's default directory (or open its directories)", Direct: true},
	{Name: "/discord", Desc: "Discord status and connect/disconnect controls", Direct: true},
	{Name: "/rename", Args: "<name>", Desc: "rename this channel (#name, unique across the daemon)", Direct: true},
	{Name: "/tree", Desc: "toggle the agent sidebar (also ctrl+b)", Direct: true},
	{Name: "/chat", Desc: "the channel chat: talk to every agent, @name addresses one (the sidebar opens an agent's own chat)", Direct: true},
	{Name: "/tokens", Args: "[system]", Desc: "chart tokens over time: the selected chat's (the channel's, or the agent's), or the system's", Direct: true},
	{Name: "/cost", Args: "[system]", Desc: "chart cost over time: the selected chat's (the channel's, or the agent's), or the system's", Direct: true},
	{Name: "/compact", Desc: "summarise the selected agent's completed turns now to free context (automatic at 80% of the window)", Direct: true},
	{Name: "/help", Desc: "show or hide the key bar at the bottom (off by default)", Aliases: []string{"/h", "/?"}, Direct: true},
	{Name: "/settings", Args: "[project|system]", Desc: "edit project or system config files", Aliases: []string{"/config"}, Direct: true},
}

// paletteMax is how many rows the palette shows at once.
const paletteMax = 8

// paletteActive reports whether the input is typing a command name: it
// starts with "/" and has no space yet.
func paletteActive(input string) bool {
	return strings.HasPrefix(input, "/") && !strings.Contains(input, " ")
}

// paletteMatches filters the registry by the typed prefix (after "/"),
// matching names first, then aliases, preserving registry order.
func paletteMatches(input string) []Command {
	return matchCommands(input, commands)
}

func matchCommands(input string, registry []Command) []Command {
	if !paletteActive(input) {
		return nil
	}
	q := strings.ToLower(strings.TrimPrefix(input, "/"))
	var out []Command
	for _, c := range registry {
		if strings.HasPrefix(strings.TrimPrefix(c.Name, "/"), q) {
			out = append(out, c)
			continue
		}
		for _, a := range c.Aliases {
			if q != "" && strings.HasPrefix(strings.TrimPrefix(a, "/"), q) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// paletteView renders the dropdown for the current matches with row idx
// highlighted; empty when there is nothing to show.
func paletteView(matches []Command, idx, width int) string {
	if len(matches) == 0 {
		return ""
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(matches) {
		idx = len(matches) - 1
	}
	start, end := listWindow(idx, 0, len(matches), paletteMax)
	nameW := 0
	for _, c := range matches {
		if n := len([]rune(c.Name + " " + c.Args)); n > nameW {
			nameW = n
		}
	}
	var rows []string
	for i := start; i < end; i++ {
		c := matches[i]
		left := c.Name
		if c.Args != "" {
			left += " " + theme.StyleDim.Render(c.Args)
		}
		pad := nameW - len([]rune(c.Name+" "+c.Args))
		if c.Args == "" {
			pad = nameW - len([]rune(c.Name))
		}
		if pad < 0 {
			pad = 0
		}
		marker, nameStyle := cursorMarker(i == idx), theme.StyleAccent
		if i == idx {
			nameStyle = theme.StyleAccent.Bold(true)
		}
		row := marker + nameStyle.Render(strings.Split(left, " ")[0]) + strings.TrimPrefix(left, strings.Split(left, " ")[0]) + strings.Repeat(" ", pad+2) + theme.StyleDim.Render(c.Desc)
		rows = append(rows, ansi.Truncate(row, width, "…"))
	}
	if len(matches) > paletteMax {
		rows = append(rows, theme.StyleDim.Render(fmt.Sprintf("  … %d of %d · ↑/↓ move · tab complete · enter run", idx+1, len(matches))))
	} else {
		rows = append(rows, theme.StyleDim.Render("  ↑/↓ move · tab complete · enter run · esc clear"))
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(rows, "\n"))
}

// paletteStep moves the palette's cursor on ↑/↓ while the palette is open;
// it reports whether it used the key.
func (m *Model) paletteStep(msg tea.KeyMsg) bool {
	pm := m.paletteMatches(m.input.Value())
	return len(pm) > 0 && stepCursor(msg, &m.palIdx, len(pm), false)
}

// mentionPrefix is the @name being typed among the names at the front of
// the input (the post's recipients): what follows the last "@", while every
// word before it is an @name too and it is still a name itself.
func mentionPrefix(input string) (string, bool) {
	i := strings.LastIndexByte(input, '@')
	if i < 0 || i > 0 && input[i-1] != ' ' {
		return "", false
	}
	for _, word := range strings.Fields(input[:i]) {
		if !strings.HasPrefix(word, "@") {
			return "", false
		}
	}
	for j := i + 1; j < len(input); j++ {
		if !nameByte(input[j]) {
			return "", false
		}
	}
	return strings.ToLower(input[i+1:]), true
}

func nameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_'
}

// mentionMatches are the live agents whose name starts with the @name being
// typed in the channel chat's input, in tree order.
func (m *Model) mentionMatches() []protocol.AgentInfo {
	if !m.superChat || m.focus != focusInput {
		return nil
	}
	prefix, ok := mentionPrefix(m.input.Value())
	if !ok {
		return nil
	}
	var out []protocol.AgentInfo
	for _, a := range m.agents {
		if a.State != protocol.AgentKilled && strings.HasPrefix(strings.ToLower(a.Name), prefix) {
			out = append(out, a)
		}
	}
	return out
}

// completeMention replaces the @name being typed with a's name and a space.
func (m *Model) completeMention(a protocol.AgentInfo) {
	v := m.input.Value()
	m.input.SetValue(v[:strings.LastIndexByte(v, '@')+1] + a.Name + " ")
	m.input.CursorEnd()
	m.palIdx = 0
}

// mentionView is the @name dropdown, laid out like the command palette.
func mentionView(matches []protocol.AgentInfo, idx, width int) string {
	if idx < 0 || idx >= len(matches) {
		idx = 0
	}
	start, end := listWindow(idx, 0, len(matches), paletteMax)
	var rows []string
	for i := start; i < end; i++ {
		a := matches[i]
		marker, style := cursorMarker(i == idx), theme.StyleAccent
		if i == idx {
			style = theme.StyleAccent.Bold(true)
		}
		rows = append(rows, ansi.Truncate(marker+style.Render("@"+a.Name)+"  "+theme.StyleDim.Render(a.Role+" · "+string(a.State)), width, "…"))
	}
	rows = append(rows, theme.StyleDim.Render("  ↑/↓ move · tab or enter complete · esc clear"))
	return lipgloss.NewStyle().Width(width).Render(strings.Join(rows, "\n"))
}
