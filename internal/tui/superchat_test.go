package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/event"
)

func chatEvent(seq int64, agent string, typ event.Type, p any) event.Event {
	return event.Event{Seq: seq, Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
}

// TestSuperChatView: the session chat shows posts and messages to the
// human, typing there posts to the session, space on a message opens its
// agent's own chat, and the sidebar's chat row goes back.
func TestSuperChatView(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.superChat = true
	m.applyEvent(chatEvent(1, "b", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b", Parent: "a", Label: "world-politics"}))
	m.applyEvent(chatEvent(2, "", event.ChatPosted, event.ChatPayload{Text: "@world-politics summarise", To: []string{"world-politics"}}))
	m.applyEvent(chatEvent(3, "b", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "shell"}))
	m.applyEvent(chatEvent(4, "b", event.MessageToUser, event.ChatPayload{From: "world-politics", Text: "three headlines"}))
	view := stripANSI(m.vp.View())
	if !strings.Contains(view, "@world-politics summarise") || !strings.Contains(view, "three headlines") || strings.Contains(view, "Shell") {
		t.Fatalf("chat view:\n%s", view)
	}
	if t2 := m.transcripts["b"]; t2 == nil || t2.Items() == 0 {
		t.Fatal("the agent's own transcript still gets its events")
	}

	m.setFocus(focusInput)
	m.input.SetValue("hello everyone")
	if cmd := m.submit(); cmd == nil || m.statusErr {
		t.Fatalf("typing in the chat posts: cmd=%v status=%q", cmd != nil, m.status)
	}

	m.setFocus(focusChat) // the cursor parks on the last item: the message
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if m.superChat || m.selectedID() != "b" {
		t.Fatalf("space on a message opens its agent: super=%v selected=%s", m.superChat, m.selectedID())
	}
	if m.viewID() != "b" || strings.Contains(m.input.Placeholder, "@name") {
		t.Fatalf("an agent's own chat: view %s placeholder %q", m.viewID(), m.input.Placeholder)
	}

	m.setFocus(focusSidebar)
	if m.sbCursor != 2 {
		t.Fatalf("the sidebar cursor starts on the shown agent's row: %d", m.sbCursor)
	}
	m.sbCursor = 0
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if !m.superChat || m.focus != focusInput || !strings.Contains(m.input.Placeholder, "@name") {
		t.Fatalf("the chat row goes back: super=%v focus=%v placeholder=%q", m.superChat, m.focus, m.input.Placeholder)
	}
}

// TestMentionAutocomplete: typing @ in the session chat offers the live
// agents by name; tab completes, and an @ inside a word is not a mention.
func TestMentionAutocomplete(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.superChat = true
	m.setFocus(focusInput)
	for _, r := range "ask @b" {
		press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if mm := m.mentionMatches(); len(mm) != 1 || mm[0].Label != "business" {
		t.Fatalf("matches %+v", mm)
	}
	if pv := stripANSI(m.paletteViewFor(80)); !strings.Contains(pv, "▸ @business") {
		t.Fatalf("dropdown:\n%s", pv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyTab})
	if m.input.Value() != "ask @business " || m.focus != focusInput || len(m.mentionMatches()) != 0 {
		t.Fatalf("tab completes: %q focus %v", m.input.Value(), m.focus)
	}
	for in, want := range map[string]bool{"@": true, "hi @wor": true, "me@exa": false, "@a b": false} {
		if _, ok := mentionPrefix(in); ok != want {
			t.Errorf("mentionPrefix(%q) = %v", in, ok)
		}
	}
	m.superChat = false
	m.input.SetValue("@b")
	if len(m.mentionMatches()) != 0 {
		t.Fatal("an agent's own chat has no mentions")
	}
}
