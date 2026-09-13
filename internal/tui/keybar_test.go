package tui

import (
	"strings"
	"testing"

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
	m := Model{width: 100, height: 30, transcripts: map[string]*Transcript{}}
	has := func(hs []keyHint, key string) bool {
		for _, h := range hs {
			if h.key == key {
				return true
			}
		}
		return false
	}
	if hs := m.keyHints(); !has(hs, "/provider") || !has(hs, "ctrl+c") || has(hs, "/steer") {
		t.Fatalf("home: %+v", hs)
	}
	m.confirmKill = true
	if hs := m.keyHints(); len(hs) != 2 || hs[0].key != "y" {
		t.Fatalf("kill: %+v", hs)
	}
	m.confirmKill = false
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission"}}
	if hs := m.keyHints(); !has(hs, "a") || !has(hs, "n") {
		t.Fatalf("permission: %+v", hs)
	}
	m.prompts = nil
	m.ov = newOverlay(ovModels, overlayList, "", "")
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
