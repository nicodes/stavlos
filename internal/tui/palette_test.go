package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPaletteMatches(t *testing.T) {
	if paletteMatches("hello") != nil || paletteMatches("/queue x") != nil {
		t.Fatal("palette should only be active while typing a bare /command")
	}
	all := paletteMatches("/")
	if len(all) != len(commands) {
		t.Fatalf("bare slash lists everything: %d", len(all))
	}
	pm := paletteMatches("/mo")
	if len(pm) != 2 || pm[0].Name != "/models" || pm[1].Name != "/mode" {
		t.Fatalf("prefix filter: %+v", pm)
	}
	if pm := paletteMatches("/conn"); len(pm) != 1 || pm[0].Name != "/providers" {
		t.Fatalf("alias filter: %+v", pm)
	}
	if pm := paletteMatches("/zzz"); len(pm) != 0 {
		t.Fatalf("no match: %+v", pm)
	}
	view := stripANSI(paletteView(paletteMatches("/"), 1, 100))
	if !strings.Contains(view, "▸ /providers") || !strings.Contains(view, "/queue <text>") || !strings.Contains(view, "tab complete") {
		t.Fatalf("view:\n%s", view)
	}
	help := helpLines()
	if !strings.Contains(strings.Join(help, "\n"), "/models") {
		t.Fatalf("help generated from the registry:\n%s", strings.Join(help, "\n"))
	}
}

func TestPaletteKeys(t *testing.T) {
	m := newModel(context.Background(), nil, "s")
	m.width, m.height = 100, 40
	m.reconciled = true
	type_ := func(s string) {
		for _, r := range s {
			press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
	type_("/")
	if pv := m.paletteViewFor(80); !strings.Contains(stripANSI(pv), "/models") {
		t.Fatalf("palette not shown:\n%s", pv)
	}
	// ↓ moves the highlight instead of walking history; tab completes the
	// highlighted command (the second in the list takes an argument, so a
	// space follows and the palette closes)
	press(&m, tea.KeyMsg{Type: tea.KeyDown})
	if m.palIdx != 1 {
		t.Fatalf("palIdx %d", m.palIdx)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyTab})
	if m.input.Value() != commands[1].Name+" " || m.focus != focusInput {
		t.Fatalf("tab completion: %q focus %v", m.input.Value(), m.focus)
	}
	if pv := m.paletteViewFor(80); pv != "" {
		t.Fatalf("palette should close once a space is typed:\n%s", pv)
	}
	// enter on a partial name of a command that takes arguments completes
	// it rather than sending text; enter on a no-argument command runs it
	m.input.Reset()
	type_("/que")
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "/queue " {
		t.Fatalf("enter should complete a partial command: %q", m.input.Value())
	}
	m.input.Reset()
	type_("/tre")
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "" || !m.showTree {
		t.Fatalf("enter on /tre should run /tree: input %q showTree %v", m.input.Value(), m.showTree)
	}
	m.setFocus(focusInput)
	// esc clears and closes
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.input.Value() != "" || m.paletteViewFor(80) != "" {
		t.Fatal("esc should clear the input and close the palette")
	}
}
