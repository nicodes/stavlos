package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap holds every binding the TUI understands. Keys handled by the
// textinput (editing) are not listed.
type keyMap struct {
	Quit        key.Binding
	NextSection key.Binding // tab: cycle focus chat → tab strip → input → meta row → sidebar
	PrevSection key.Binding
	TabLeft     key.Binding // ←/→: move between the strip's tabs
	TabRight    key.Binding
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
	Select      key.Binding
	Clear       key.Binding
	ToggleTree  key.Binding
	FocusInput  key.Binding // ctrl+space: back to typing from anywhere, a dialog's text field included

	// Overlay (modal list / field) keys.
	OvUp     key.Binding
	OvDown   key.Binding
	OvSelect key.Binding
	OvClose  key.Binding
	OvAlt    key.Binding // secondary action (models: channel default)
	OvOpen   key.Binding // login: open the URL in the browser again
	OvRemove key.Binding // providers: sign out of the selected provider

	// Letters, which act only where no text is being typed.
	VimUp       key.Binding
	VimDown     key.Binding
	NextWaiting key.Binding // the sidebar: the next agent that needs you
	AddDir      key.Binding // the dirs tab
	ShowTokens  key.Binding // a usage chart
	ShowCost    key.Binding

	// The configuration editor (/settings).
	EdClose   key.Binding
	EdSave    key.Binding
	EdConfirm key.Binding
	EdRaw     key.Binding // switch between the fields and the raw text of a file
	EdPane    key.Binding // files ↔ editor
	EdUp      key.Binding
	EdDown    key.Binding
	EdPrev    key.Binding // the section to the left
	EdNext    key.Binding
	EdOpen    key.Binding
	EdNew     key.Binding
	EdRename  key.Binding
	EdDelete  key.Binding
	EdReload  key.Binding
}

var keys = keyMap{
	Quit:        key.NewBinding(key.WithKeys("ctrl+c")),
	NextSection: key.NewBinding(key.WithKeys("tab")),
	PrevSection: key.NewBinding(key.WithKeys("shift+tab")),
	TabLeft:     key.NewBinding(key.WithKeys("left")),
	TabRight:    key.NewBinding(key.WithKeys("right")),
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
	Select:      key.NewBinding(key.WithKeys(" ", "enter")), // space and enter: select, open, toggle — outside text fields
	Clear:       key.NewBinding(key.WithKeys("esc")),
	ToggleTree:  key.NewBinding(key.WithKeys("ctrl+b")),
	FocusInput:  key.NewBinding(key.WithKeys("ctrl+@", "ctrl+space")), // ctrl+space reaches a program as ctrl+@

	OvUp:     key.NewBinding(key.WithKeys("up", "ctrl+p")),
	OvDown:   key.NewBinding(key.WithKeys("down", "ctrl+n")),
	OvSelect: key.NewBinding(key.WithKeys("enter")),
	OvClose:  key.NewBinding(key.WithKeys("esc")),
	OvAlt:    key.NewBinding(key.WithKeys("ctrl+s")),
	OvOpen:   key.NewBinding(key.WithKeys("o", "O")),
	OvRemove: key.NewBinding(key.WithKeys("ctrl+d")),

	VimUp:       key.NewBinding(key.WithKeys("k")),
	VimDown:     key.NewBinding(key.WithKeys("j")),
	NextWaiting: key.NewBinding(key.WithKeys("n")),
	AddDir:      key.NewBinding(key.WithKeys("a")),
	ShowTokens:  key.NewBinding(key.WithKeys("t")),
	ShowCost:    key.NewBinding(key.WithKeys("c")),

	EdClose:   key.NewBinding(key.WithKeys("esc", "ctrl+c")),
	EdSave:    key.NewBinding(key.WithKeys("ctrl+s")),
	EdConfirm: key.NewBinding(key.WithKeys("enter")),
	EdRaw:     key.NewBinding(key.WithKeys("f4")),
	EdPane:    key.NewBinding(key.WithKeys("tab", "shift+tab")),
	EdUp:      key.NewBinding(key.WithKeys("up", "k")),
	EdDown:    key.NewBinding(key.WithKeys("down", "j")),
	EdPrev:    key.NewBinding(key.WithKeys("left")),
	EdNext:    key.NewBinding(key.WithKeys("right")),
	EdOpen:    key.NewBinding(key.WithKeys("enter", " ")),
	EdNew:     key.NewBinding(key.WithKeys("ctrl+n")),
	EdRename:  key.NewBinding(key.WithKeys("f2")),
	EdDelete:  key.NewBinding(key.WithKeys("ctrl+d")),
	EdReload:  key.NewBinding(key.WithKeys("ctrl+l")),
}
