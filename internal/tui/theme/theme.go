// Package theme is the TUI's palette: every colour and the styles built
// from them. Colours adapt to light and dark terminals.
package theme

import "github.com/charmbracelet/lipgloss"

// Colours, styles, and the dialog styles.
var (
	ColAccent  = lipgloss.AdaptiveColor{Light: "#3B6FD9", Dark: "#5B8DEF"}
	ColMuted   = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#8A8F98"}
	ColSuccess = lipgloss.AdaptiveColor{Light: "#1F8F4E", Dark: "#3DD68C"}
	ColWarning = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#F5A524"}
	ColError   = lipgloss.AdaptiveColor{Light: "#C0392B", Dark: "#F26D6D"}
	ColBlocked = lipgloss.AdaptiveColor{Light: "#9333EA", Dark: "#C084FC"}
	ColBorder  = ColMuted
	ColSelBg   = lipgloss.AdaptiveColor{Light: "#E5E7EB", Dark: "#2A2F3A"} // chat cursor row background
	ColYellow  = lipgloss.AdaptiveColor{Light: "#A16207", Dark: "#FACC15"}
	ColPink    = lipgloss.AdaptiveColor{Light: "#BE185D", Dark: "#F472B6"}
	ColCyan    = lipgloss.AdaptiveColor{Light: "#0E7490", Dark: "#22D3EE"}
	ColInputBg = lipgloss.AdaptiveColor{Light: "#F3F4F6", Dark: "#1C2129"} // the message input's background

	StyleDim      = lipgloss.NewStyle().Foreground(ColMuted)
	StyleLit      = lipgloss.NewStyle() // the chat item being read: the text colour, as the human's posts have, instead of grey
	StyleKey      = lipgloss.NewStyle().Foreground(ColAccent).Bold(true)
	StyleWorking  = lipgloss.NewStyle().Foreground(ColWarning)
	StyleBold     = lipgloss.NewStyle().Bold(true)
	StyleAccent   = lipgloss.NewStyle().Foreground(ColAccent)
	StyleNotice   = lipgloss.NewStyle().Foreground(ColMuted).Italic(true)
	StyleTool     = lipgloss.NewStyle().Foreground(ColMuted)
	StyleToolName = lipgloss.NewStyle().Foreground(ColMuted).Bold(true)
	StyleToolOut  = lipgloss.NewStyle().Foreground(ColMuted)
	StyleFinished = lipgloss.NewStyle().Foreground(ColSuccess).Bold(true)
	StyleRule     = lipgloss.NewStyle().Foreground(ColBorder)
	StyleError    = lipgloss.NewStyle().Foreground(ColError)
	StyleWarn     = lipgloss.NewStyle().Foreground(ColWarning)
	StyleRunning  = lipgloss.NewStyle().Foreground(ColWarning) // spinner colour outside the chat

	StyleLogoMuted  = lipgloss.NewStyle().Foreground(ColMuted)
	StyleLogoBright = lipgloss.NewStyle().Bold(true)

	StyleStatusOK      = lipgloss.NewStyle().Foreground(ColSuccess)
	StyleStatusErr     = lipgloss.NewStyle().Foreground(ColError).Bold(true)
	StyleSelected      = lipgloss.NewStyle().Bold(true)
	StyleSep           = lipgloss.NewStyle().Foreground(ColBorder)
	StyleBoxTitleFocus = lipgloss.NewStyle().Foreground(ColAccent).Bold(true)
	StyleCursorRow     = lipgloss.NewStyle().Background(ColSelBg) // chat cursor: the item's rows get this background
	StyleSelection     = lipgloss.NewStyle().Reverse(true)        // mouse text selection

	StyleBorderMuted = lipgloss.NewStyle().Foreground(ColMuted)
	StyleBorderUser  = lipgloss.NewStyle().Foreground(ColAccent) // the input prompt while it has focus
)

var (
	StyleOvBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(ColAccent).Padding(0, 1)
	StyleOvTitle  = lipgloss.NewStyle().Bold(true)
	StyleOvGood   = lipgloss.NewStyle().Foreground(ColSuccess)
	StyleOvCur    = lipgloss.NewStyle().Bold(true)
	StyleOvMarker = lipgloss.NewStyle().Foreground(ColAccent)
	StyleOvURL    = lipgloss.NewStyle().Foreground(ColAccent).Bold(true)
	StyleOvCode   = lipgloss.NewStyle().Bold(true)
)
