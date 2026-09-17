package tui

import (
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

func TestCustomCommandsJoinPaletteWithoutReplacingBuiltins(t *testing.T) {
	m := channelModel()
	scope := m.requestScope()
	m.onCustomCommands(customCommandsMsg{scope: scope, commands: []protocol.CommandInfo{
		{Name: "cmd", Description: "Run tests with coverage"},
		{Name: "help", Description: "must not replace help"},
		{Name: "login", Description: "must not replace an alias"},
	}})
	if got := m.paletteMatches("/cm"); len(got) != 1 || got[0].Name != "/cmd" || got[0].Desc != "Run tests with coverage" || !got[0].Direct {
		t.Fatalf("custom palette: %+v", got)
	}
	if got := m.paletteMatches("/help"); len(got) != 1 || got[0].Desc == "must not replace help" {
		t.Fatal("custom command shadowed a builtin")
	}
	if got := m.paletteMatches("/login"); len(got) != 1 || got[0].Name != "/providers" {
		t.Fatal("custom command shadowed an alias")
	}
	m.bindChannel(protocol.ChannelInfo{ID: "other"})
	m.onCustomCommands(customCommandsMsg{scope: scope, commands: []protocol.CommandInfo{{Name: "old"}}})
	if len(m.paletteMatches("/cmd")) != 0 || len(m.paletteMatches("/old")) != 0 {
		t.Fatal("commands leaked across channel scopes")
	}
}
