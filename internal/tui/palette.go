package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
	{Name: "/models", Desc: "pick a model: enter sets the selected agent's, ctrl+s the session default", Aliases: []string{"/model"}, Direct: true},
	{Name: "/variants", Args: "[name|default]", Desc: "pick a model variant (reasoning effort) for the selected agent; no arg opens a picker", Aliases: []string{"/variant"}, Direct: true},
	{Name: "/roles", Args: "[name]", Desc: "pick the selected agent's role (preset); a name sets it directly", Aliases: []string{"/role", "/presets"}, Direct: true},
	{Name: "/mode", Desc: "permission mode for the session: ask, auto (free inside the agent's directories) or yolo", Direct: true},
	{Name: "/auto", Args: "[on|off]", Desc: "auto mode: approve permissions inside the agent's directories, ask outside (no arg toggles)", Direct: true},
	{Name: "/yolo", Args: "[on|off]", Desc: "yolo mode: approve every permission, directories included (no arg toggles)", Direct: true},
	{Name: "/sessions", Desc: "pick a session of this directory to resume where it left off", Aliases: []string{"/resume", "/session"}, Direct: true},
	{Name: "/tree", Desc: "toggle the agent sidebar (also ctrl+b)", Direct: true},
	{Name: "/compact", Desc: "summarise the selected agent's completed turns now to free context (automatic at 80% of the window)", Direct: true},
	{Name: "/help", Desc: "show or hide the key bar at the bottom (off by default)", Aliases: []string{"/h", "/?"}, Direct: true},
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
	if !paletteActive(input) {
		return nil
	}
	q := strings.ToLower(strings.TrimPrefix(input, "/"))
	var out []Command
	for _, c := range commands {
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
	// window of paletteMax rows around the selection
	start := 0
	if idx >= paletteMax {
		start = idx - paletteMax + 1
	}
	end := start + paletteMax
	if end > len(matches) {
		end = len(matches)
	}
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
			left += " " + styleDim.Render(c.Args)
		}
		pad := nameW - len([]rune(c.Name+" "+c.Args))
		if c.Args == "" {
			pad = nameW - len([]rune(c.Name))
		}
		if pad < 0 {
			pad = 0
		}
		marker := "  "
		nameStyle := styleAccent
		if i == idx {
			marker = styleAccent.Render("▸") + " "
			nameStyle = styleAccent.Bold(true)
		}
		row := marker + nameStyle.Render(strings.Split(left, " ")[0]) + strings.TrimPrefix(left, strings.Split(left, " ")[0]) + strings.Repeat(" ", pad+2) + styleDim.Render(c.Desc)
		rows = append(rows, ansi.Truncate(row, width, "…"))
	}
	if len(matches) > paletteMax {
		rows = append(rows, styleDim.Render(fmt.Sprintf("  … %d of %d · ↑/↓ move · tab complete · enter run", idx+1, len(matches))))
	} else {
		rows = append(rows, styleDim.Render("  ↑/↓ move · tab complete · enter run · esc clear"))
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(rows, "\n"))
}

// helpLines is generated from the registry plus the key reference.
func helpLines() []string {
	out := []string{"commands:", "  <text>                           send to the selected agent (a busy agent takes it at its next step)"}
	for _, c := range commands {
		left := c.Name
		if c.Args != "" {
			left += " " + c.Args
		}
		desc := c.Desc
		if len(c.Aliases) > 0 {
			desc += " (alias " + strings.Join(c.Aliases, ", ") + ")"
		}
		out = append(out, fmt.Sprintf("  %-33s%s", left, desc))
	}
	return append(out, helpKeyLines...)
}
