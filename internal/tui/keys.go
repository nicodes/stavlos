package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap holds every binding the TUI understands. Keys handled by the
// textinput (editing) are not listed.
type keyMap struct {
	Quit       key.Binding
	NextAgent  key.Binding
	PrevAgent  key.Binding
	SelUp      key.Binding
	SelDown    key.Binding
	PageUp     key.Binding
	PageDown   key.Binding
	Top        key.Binding
	Bottom     key.Binding
	Submit     key.Binding
	Clear      key.Binding
	Yes        key.Binding
	No         key.Binding
	Always     key.Binding
	ToggleTree key.Binding

	// Overlay (modal list / field) keys.
	OvUp     key.Binding
	OvDown   key.Binding
	OvSelect key.Binding
	OvClose  key.Binding
	OvAlt    key.Binding // secondary action (models: session default)
	OvOpen   key.Binding // login: open the URL in the browser again
}

var keys = keyMap{
	Quit:       key.NewBinding(key.WithKeys("ctrl+c")),
	NextAgent:  key.NewBinding(key.WithKeys("tab", "ctrl+n")),
	PrevAgent:  key.NewBinding(key.WithKeys("shift+tab", "ctrl+p")),
	SelUp:      key.NewBinding(key.WithKeys("up")),
	SelDown:    key.NewBinding(key.WithKeys("down")),
	PageUp:     key.NewBinding(key.WithKeys("pgup")),
	PageDown:   key.NewBinding(key.WithKeys("pgdown")),
	Top:        key.NewBinding(key.WithKeys("ctrl+home")),
	Bottom:     key.NewBinding(key.WithKeys("ctrl+end")),
	Submit:     key.NewBinding(key.WithKeys("enter")),
	Clear:      key.NewBinding(key.WithKeys("esc")),
	Yes:        key.NewBinding(key.WithKeys("y", "Y")),
	No:         key.NewBinding(key.WithKeys("n", "N")),
	Always:     key.NewBinding(key.WithKeys("a", "A")),
	ToggleTree: key.NewBinding(key.WithKeys("ctrl+b")),

	OvUp:     key.NewBinding(key.WithKeys("up", "ctrl+p")),
	OvDown:   key.NewBinding(key.WithKeys("down", "ctrl+n")),
	OvSelect: key.NewBinding(key.WithKeys("enter")),
	OvClose:  key.NewBinding(key.WithKeys("esc")),
	OvAlt:    key.NewBinding(key.WithKeys("ctrl+s")),
	OvOpen:   key.NewBinding(key.WithKeys("o", "O")),
}

// helpLines is the /help notice.
var helpLines = []string{
	"commands:",
	"  <text>                           send a prompt to the selected agent",
	"  /steer <text>                    steer the selected agent (preempts at next model call)",
	"  /cancel                          cancel the selected agent's current turn",
	"  /kill                            kill the selected agent and its subtree (asks y/n)",
	"  /spawn <archetype> <label> <task> spawn a child of the selected agent",
	"  /provider [openai|xai]           sign in with your ChatGPT or Grok subscription (alias /connect, /login)",
	"  /providers                       show which providers are signed in",
	"  /disconnect <openai|xai>         sign out of a provider",
	"  /models                          pick a model (enter: selected agent · ctrl+s: session default)",
	"  /model <provider/id>             set the selected agent's model directly (no arg: same as /models)",
	"  /session-model <provider/id>     set the session default model",
	"  /presets                         list available archetypes",
	"  /tree                            toggle the agent sidebar (also ctrl+b)",
	"  /details                         expand or collapse tool output",
	"  /tips                            toggle the home-screen tips",
	"  /help                            this list",
	"  /quit                            exit",
	"keys: tab/shift+tab or ctrl+n/ctrl+p cycle agents · ↑/↓ (empty input) select · pgup/pgdn scroll · esc clear · ctrl+b sidebar",
	"overlays: ↑/↓ or ctrl+p/ctrl+n move · enter select · esc close · type to filter · pgup/pgdn page",
	"sign-in: open the URL on any device and enter the code · o open in browser · esc cancel",
	"prompts: y allow · n deny · a allow always · questions: type and press enter",
}
