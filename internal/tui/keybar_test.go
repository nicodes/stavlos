package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestKeyBarLinesWrap(t *testing.T) {
	hints := []keyHint{{"enter", "send"}, {"tab", "agents"}, {"ctrl+b", "sidebar"}, {"ctrl+c", "quit"}}
	rows := keyBarLines(hints, 30, 2)
	if len(rows) != 2 {
		t.Fatalf("rows %d: %q", len(rows), rows)
	}
	for _, r := range rows {
		if w := lipgloss.Width(r); w > 30 {
			t.Fatalf("row too wide (%d): %q", w, r)
		}
	}
	if rows := keyBarLines(hints, 200, 2); len(rows) != 1 || !strings.Contains(rows[0], "ctrl+c") {
		t.Fatalf("%q", rows)
	}
	if rows := keyBarLines(hints, 12, 1); len(rows) != 1 {
		t.Fatalf("maxRows: %q", rows)
	}
}

func TestKeyHintsByContext(t *testing.T) {
	m := Model{width: 100, height: 30, sessionState: newSessionState("", protocol.SessionInfo{})}
	has := func(hs []keyHint, key string) bool {
		for _, h := range hs {
			if h.key == key {
				return true
			}
		}
		return false
	}
	if hs := m.keyHints(); !has(hs, "/") || !has(hs, "ctrl+c") || has(hs, "/providers") || has(hs, "/models") {
		t.Fatalf("home: %+v", hs)
	}
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission"}}
	if hs := m.keyHints(); has(hs, "a") || !has(hs, "tab") {
		t.Fatalf("input focus with a prompt: %+v", hs)
	}
	m.focus = focusPermission
	if hs := m.keyHints(); !has(hs, "space") || !has(hs, "↑/↓") || has(hs, "a") || has(hs, "n") {
		t.Fatalf("permission: %+v", hs)
	}
	m.focus = focusChat
	if hs := m.keyHints(); !has(hs, "↑/↓") || !has(hs, "esc") || has(hs, "/steer") {
		t.Fatalf("chat: %+v", hs)
	}
	m.focus = focusInput
	m.prompts = nil
	m.ov = newOverlay(ovModels, overlayList, "")
	if hs := m.keyHints(); !has(hs, "ctrl+s") {
		t.Fatalf("models: %+v", hs)
	}
	m.ov.switchLogin("ChatGPT")
	if hs := m.keyHints(); !has(hs, "o") || has(hs, "enter") {
		t.Fatalf("login: %+v", hs)
	}
	m.ov.setLoginError("boom")
	if hs := m.keyHints(); hs[0].key != "enter" {
		t.Fatalf("login error: %+v", hs)
	}
	s, n := m.keyBarView()
	if n != strings.Count(s, "\n")+1 || !strings.Contains(s, "─") {
		t.Fatalf("keybar %d %q", n, s)
	}
}

func TestStepCursor(t *testing.T) {
	up, down := tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeyDown}
	k := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")}
	j := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}
	cur := 0
	if !stepCursor(up, &cur, 3, false) || cur != 2 {
		t.Fatalf("up from the top wraps to the bottom: %d", cur)
	}
	if !stepCursor(down, &cur, 3, false) || cur != 0 {
		t.Fatalf("down from the bottom wraps to the top: %d", cur)
	}
	if stepCursor(j, &cur, 3, false) || cur != 0 {
		t.Fatalf("j is not a move without letters: %d", cur)
	}
	if !stepCursor(j, &cur, 3, true) || cur != 1 || !stepCursor(k, &cur, 3, true) || cur != 0 {
		t.Fatalf("j/k move with letters: %d", cur)
	}
	if !stepCursor(down, &cur, 0, true) || cur != 0 {
		t.Fatalf("a move over no rows is consumed and changes nothing: %d", cur)
	}
	if stepCursor(tea.KeyMsg{Type: tea.KeyEnter}, &cur, 3, true) {
		t.Fatal("enter is not a move")
	}
}
