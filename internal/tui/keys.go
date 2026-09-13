package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap holds every binding the TUI understands. Keys handled by the
// textinput (editing) are not listed.
type keyMap struct {
	Quit        key.Binding
	NextSection key.Binding // tab: cycle focus chat → permission → input → sidebar
	PrevSection key.Binding
	NextAgent   key.Binding
	PrevAgent   key.Binding
	SelUp       key.Binding
	SelDown     key.Binding
	PageUp      key.Binding
	PageDown    key.Binding
	Top         key.Binding
	Bottom      key.Binding
	ChatTop     key.Binding // chat focus only: plain home/end also jump
	ChatBottom  key.Binding
	Submit      key.Binding
	Clear       key.Binding
	Yes         key.Binding
	No          key.Binding
	Always      key.Binding
	ToggleTree  key.Binding

	// Overlay (modal list / field) keys.
	OvUp     key.Binding
	OvDown   key.Binding
	OvSelect key.Binding
	OvClose  key.Binding
	OvAlt    key.Binding // secondary action (models: session default)
	OvOpen   key.Binding // login: open the URL in the browser again
}

var keys = keyMap{
	Quit:        key.NewBinding(key.WithKeys("ctrl+c")),
	NextSection: key.NewBinding(key.WithKeys("tab")),
	PrevSection: key.NewBinding(key.WithKeys("shift+tab")),
	NextAgent:   key.NewBinding(key.WithKeys("ctrl+n")),
	PrevAgent:   key.NewBinding(key.WithKeys("ctrl+p")),
	SelUp:       key.NewBinding(key.WithKeys("up")),
	SelDown:     key.NewBinding(key.WithKeys("down")),
	PageUp:      key.NewBinding(key.WithKeys("pgup")),
	PageDown:    key.NewBinding(key.WithKeys("pgdown")),
	Top:         key.NewBinding(key.WithKeys("ctrl+home")),
	Bottom:      key.NewBinding(key.WithKeys("ctrl+end")),
	ChatTop:     key.NewBinding(key.WithKeys("home", "ctrl+home")),
	ChatBottom:  key.NewBinding(key.WithKeys("end", "ctrl+end")),
	Submit:      key.NewBinding(key.WithKeys("enter")),
	Clear:       key.NewBinding(key.WithKeys("esc")),
	Yes:         key.NewBinding(key.WithKeys("y", "Y")),
	No:          key.NewBinding(key.WithKeys("n", "N")),
	Always:      key.NewBinding(key.WithKeys("a", "A")),
	ToggleTree:  key.NewBinding(key.WithKeys("ctrl+b")),

	OvUp:     key.NewBinding(key.WithKeys("up", "ctrl+p")),
	OvDown:   key.NewBinding(key.WithKeys("down", "ctrl+n")),
	OvSelect: key.NewBinding(key.WithKeys("enter")),
	OvClose:  key.NewBinding(key.WithKeys("esc")),
	OvAlt:    key.NewBinding(key.WithKeys("ctrl+s")),
	OvOpen:   key.NewBinding(key.WithKeys("o", "O")),
}

// helpKeyLines is the key reference appended to /help.
var helpKeyLines = []string{
	"focus: tab/shift+tab cycle the sections chat → permission → input → sidebar · esc returns to the input",
	"input: enter send · ↑/↓ prompt history · esc clear · ctrl+n/ctrl+p cycle agents · pgup/pgdn scroll",
	"chat: ↑/↓ or j/k move by item · enter expand/collapse a tool's output · pgup/pgdn page · home/end first/last",
	"permission: y allow once · a allow for session · n deny · questions: type in the box, enter answers",
	"sidebar: ctrl+b open/close · ↑/↓ move · enter pick an agent",
	"background: the section above the input lists the selected agent's live children (⑂) and running async jobs (◷); tab to it, ↑/↓ move, enter selects an agent",
	"overlays: ↑/↓ or ctrl+p/ctrl+n move · enter select · esc close · type to filter · pgup/pgdn page",
	"sign-in: open the URL on any device and enter the code · o open in browser · esc cancel",
}
