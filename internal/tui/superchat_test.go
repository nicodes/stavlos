package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/event"
)

// drawnChat is the chat as drawn after what the test applied: events mark it
// dirty, and outside Update nothing else redraws it.
func drawnChat(m *Model) string {
	if m.viewDirty {
		m.refreshViewport()
	}
	return m.vp.View()
}

func chatEvent(seq int64, agent string, typ event.Type, p any) event.Event {
	return event.Event{Seq: seq, Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
}

// TestSuperChatView: the channel chat shows posts and messages to the
// human, typing there posts to the channel, space on a message opens its
// agent's own chat, and the sidebar's chat row goes back.
func TestSuperChatView(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.superChat = true
	m.applyEvent(chatEvent(1, "b", event.AgentSpawned, event.AgentSpawnedPayload{ID: "b", Parent: "a", Name: "world-politics"}))
	m.applyEvent(chatEvent(2, "", event.ChatPosted, event.ChatPayload{Text: "summarise", To: []string{"world-politics"}}))
	m.applyEvent(chatEvent(3, "b", event.ToolStarted, event.ToolStartedPayload{CallID: "c1", Name: "shell"}))
	m.applyEvent(chatEvent(4, "b", event.ChatMessage, event.ChatPayload{From: "world-politics", Text: "three headlines"}))
	view := stripANSI(drawnChat(&m))
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
	if m.sbCursor != 3 { // + channel, this channel, then the agents
		t.Fatalf("the sidebar cursor starts on the shown agent's row: %d", m.sbCursor)
	}
	m.sbCursor = 1 // this channel's row
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if !m.superChat || m.focus != focusInput || !strings.Contains(m.input.Placeholder, "@name") {
		t.Fatalf("the chat row goes back: super=%v focus=%v placeholder=%q", m.superChat, m.focus, m.input.Placeholder)
	}
}

// TestMentionAutocomplete: typing @ in the channel chat offers the live
// agents by name; tab completes, and an @ inside a word is not a mention.
func TestMentionAutocomplete(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.superChat = true
	m.setFocus(focusInput)
	for _, r := range "@main @b" {
		press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if mm := m.mentionMatches(); len(mm) != 1 || mm[0].Name != "business" {
		t.Fatalf("matches %+v", mm)
	}
	if pv := stripANSI(m.paletteViewFor(80)); !strings.Contains(pv, "▸ @business") {
		t.Fatalf("dropdown:\n%s", pv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyTab})
	if m.input.Value() != "@main @business " || m.focus != focusInput || len(m.mentionMatches()) != 0 {
		t.Fatalf("tab completes: %q focus %v", m.input.Value(), m.focus)
	}
	for in, want := range map[string]bool{"@": true, "@main @wor": true, "hi @wor": false, "me@exa": false, "@a b": false} { // only among the leading names
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

// TestSuperChatSpaceExpandsALongReply: space on a thread with a long reply
// expands it in place instead of leaving the chat.
func TestSuperChatSpaceExpandsALongReply(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.superChat = true
	m.applyEvent(chatEvent(1, "a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a", Name: "main"}))
	m.applyEvent(chatEvent(2, "", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "summarise", To: []string{"main"}}))
	m.applyEvent(chatEvent(3, "a", event.ChatMessage, event.ChatPayload{From: "main", Text: "alpha\nbravo\ncharlie\ndelta\nfoxtrot", Post: "p1"}))
	if view := stripANSI(drawnChat(&m)); !strings.Contains(view, "@main: alpha") || strings.Contains(view, "foxtrot") {
		t.Fatalf("collapsed view:\n%s", view)
	}
	m.setFocus(focusChat)
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if !m.superChat || !strings.Contains(stripANSI(drawnChat(&m)), "foxtrot") {
		t.Fatalf("space should expand the reply in the chat: super=%v\n%s", m.superChat, stripANSI(drawnChat(&m)))
	}
}

// TestSuperChatLoaderUntilReply: a post shows a loader in the chat until its
// agent replies.
func TestSuperChatLoaderUntilReply(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.superChat = true
	m.applyEvent(chatEvent(1, "a", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a", Name: "main"}))
	m.applyEvent(chatEvent(2, "", event.ChatPosted, event.ChatPayload{ID: "p1", Text: "hello", To: []string{"main"}}))
	if view := stripANSI(drawnChat(&m)); !strings.Contains(view, "… · @main") {
		t.Fatalf("a loader should show under the unanswered post:\n%s", view)
	}
	m.applyEvent(chatEvent(3, "a", event.ChatMessage, event.ChatPayload{From: "main", Text: "hi", Post: "p1"}))
	if view := stripANSI(drawnChat(&m)); strings.Contains(view, "… · @main") || !strings.Contains(view, "@main: hi") {
		t.Fatalf("the reply replaces the loader:\n%s", view)
	}
}

// TestAsyncTabHoldsWhatIsDue: the async tab counts and lists both
// directions: the agents the selected agent waits on (and its jobs), then who
// waits on its reply, you first; space opens that chat.
func TestAsyncTabHoldsWhatIsDue(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.agents[0].Due = []string{"b", "user"}
	if labels := strings.Join(tabTexts(m), " · "); !strings.Contains(labels, "async 4 · todo") || strings.Contains(labels, "due") {
		t.Fatalf("strip: %s", labels)
	}
	m.setFocus(focusAsync)
	body := stripANSI(strings.Join(m.tabBodyLines(80), "\n"))
	if !inOrder(body, "waiting on", "world-politics (general)", "business (general)", "owes a reply to", "you  the channel chat", "world-politics (general)") {
		t.Fatalf("async body:\n%s", body)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeySpace})
	if !m.superChat || isTab(m.focus) {
		t.Fatalf("space on you opens the channel chat: super %v focus %v", m.superChat, m.focus)
	}
	m.openAgent(0)
	m.setFocus(focusAsync)
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeySpace})
	if m.selectedID() != "b" || m.superChat || isTab(m.focus) {
		t.Fatalf("space on an agent it owes opens that chat: selected %s super %v focus %v", m.selectedID(), m.superChat, m.focus)
	}
	m.setFocus(focusAsync)
	if body := stripANSI(strings.Join(m.tabBodyLines(80), "\n")); !strings.Contains(body, "not waiting on anything, and no replies due") {
		t.Fatalf("b waits on nothing and owes nothing:\n%s", body)
	}
}

// tabTexts is the strip's tabs spelled out, "async 4", in tab order.
func tabTexts(m Model) []string {
	var out []string
	for _, t := range m.tabs() {
		out = append(out, t.text())
	}
	return out
}
