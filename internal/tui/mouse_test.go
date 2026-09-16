package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/render"
)

// TestPromptOptionsTakeTheMouse: the rows of the permission and questions
// dialogs are hit where they are drawn, below a subject of any height:
// hover moves the selection, a click acts like space, and the subject,
// the question and an open text row do not respond.
func TestPromptOptionsTakeTheMouse(t *testing.T) {
	m := channelModel()
	m.showTree = false
	m.width, m.height = 100, 40
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Tool: "shell", Agent: "a",
		Input: []byte(`{"command":"go test ./... -run ` + strings.Repeat("TestSomethingLong|", 8) + `"}`), Prefix: "go test"}}
	m.setFocus(focusPermission)
	m.layout()
	at := func(text string) (int, int) {
		t.Helper()
		for y, l := range strings.Split(stripANSI(m.View()), "\n") {
			if i := strings.Index(l, text); i >= 0 {
				return len([]rune(l[:i])), y
			}
		}
		t.Fatalf("%q not on screen:\n%s", text, stripANSI(m.View()))
		return 0, 0
	}
	move := func(x, y int) {
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
		m = nm.(Model)
	}
	click := func(x, y int) {
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		nm, _ = nm.(Model).Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
		m = nm.(Model)
	}

	move(at("Allow for this channel"))
	if m.focus != focusPermission || m.permSel != 1 {
		t.Fatalf("hover: focus=%v sel=%d", m.focus, m.permSel)
	}
	click(at("$ go test"))
	if m.permSel != 1 || m.promptBusy != "" || m.focus != focusPermission {
		t.Fatalf("the subject is not an option: sel=%d busy=%q", m.permSel, m.promptBusy)
	}
	click(at("Deny  with"))
	if m.permSel != 3 || m.permEdit != "deny" {
		t.Fatalf("clicking Deny should open the reason row: sel=%d edit=%q", m.permSel, m.permEdit)
	}
	move(at("Allow once"))
	if m.permSel != 3 {
		t.Fatalf("with the reason row open the list ignores the mouse: sel=%d", m.permSel)
	}
	m.permEdit = ""
	click(at("Allow once"))
	if m.permSel != 0 || m.promptBusy != "p" {
		t.Fatalf("clicking Allow once should answer: sel=%d busy=%q", m.permSel, m.promptBusy)
	}

	m.prompts, m.promptBusy = nil, ""
	m.setFocus(focusInput) // a new question opens its dialog when the input is idle
	m.applyPromptNotification(protocol.PromptNotification{Action: "requested", Prompt: protocol.PromptInfo{ID: "q1", Kind: "question", Agent: "a", Tool: "ask_user",
		Questions: []protocol.Question{{Question: "Which backend?", Options: []protocol.QuestionOption{{Label: "Postgres"}, {Label: "SQLite"}}}}}})
	if m.focus != focusQuestions {
		t.Fatalf("the question should open its dialog: %v", m.focus)
	}
	move(at("SQLite"))
	if m.q.sel != 1 {
		t.Fatalf("hover: sel=%d", m.q.sel)
	}
	click(at("SQLite"))
	if !m.q.marks[1] || m.q.marks[0] {
		t.Fatalf("click should toggle SQLite: %v", m.q.marks)
	}
	click(at("Which backend?"))
	if m.q.sel != 1 || !m.q.marks[1] {
		t.Fatalf("the question is not an option: sel=%d marks=%v", m.q.sel, m.q.marks)
	}
	click(at("something else"))
	if !m.q.typing || m.q.sel != 2 {
		t.Fatalf("clicking something else should open the field: typing=%v sel=%d", m.q.typing, m.q.sel)
	}
}

// TestSidebarHoverMarksTheRow: the pointer over a sidebar row gives it the
// cursor and its background, as it does a chat item, without selecting it.
func TestSidebarHoverMarksTheRow(t *testing.T) {
	prev := render.SwapHighlight(func(s string, _ int) string { return render.GutterMark + s })
	t.Cleanup(func() { render.SwapHighlight(prev) })
	m := sidebarNavModel()
	m.prompts = nil
	m.setFocus(focusInput)
	move := func(x, y int) {
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
		m = nm.(Model)
	}
	header := len(m.sidebarHeader(sidebarWidth - 1))
	move(3, header+4)
	if m.focus != focusSidebar || !m.hoverFocus || m.sbCursor != 4 {
		t.Fatalf("hover on a tree row: focus=%v hover=%v cursor=%d", m.focus, m.hoverFocus, m.sbCursor)
	}
	if id := m.selectedID(); id == "c" {
		t.Fatal("hover must not select the agent under the pointer")
	}
	body, items := m.sidebarBody(sidebarWidth - 1)
	for i, it := range items {
		if it == m.sbCursor && !strings.Contains(body[i], render.GutterMark) {
			t.Fatalf("the hovered row should carry the highlight: %q", stripANSI(body[i]))
		}
	}
	move(3, 1) // the header: hover releases
	if m.focus != focusInput || m.hoverFocus {
		t.Fatalf("hover on the header should release: focus=%v hover=%v", m.focus, m.hoverFocus)
	}
}

// TestSidebarScrolls: the nav scrolls on its own — the wheel over it and
// pgup/pgdn move its window while the chat stays put, and the cursor
// moving brings its row into view.
func TestSidebarScrolls(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	many := make([]protocol.AgentInfo, 40)
	for i := range many {
		id := string(rune('a'+i%26)) + strings.Repeat("-", i/26+1)
		many[i] = protocol.AgentInfo{ID: id, Name: "agent" + id, Role: "general", State: "idle"}
	}
	m.agents, m.selected = many, 0
	m.height = 18 // a short window: the tree is taller than the room for it
	m.layout()
	first := func() string {
		rows, items := m.sidebarLines(m.vp.Height)
		for i, it := range items {
			if it >= 0 {
				return stripANSI(rows[i])
			}
		}
		return ""
	}
	top, chatAt := first(), m.vp.YOffset
	nm, _ := m.Update(tea.MouseMsg{X: 2, Y: 8, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	m = nm.(Model)
	if m.sbTop == 0 || first() == top {
		t.Fatalf("the wheel over the nav should scroll it: top=%d first=%q", m.sbTop, first())
	}
	if m.vp.YOffset != chatAt {
		t.Fatalf("the wheel over the nav must leave the chat alone: %d → %d", chatAt, m.vp.YOffset)
	}
	m.setFocus(focusSidebar)
	m.sbTop = 0
	press(&m, tea.KeyMsg{Type: tea.KeyPgDown})
	if m.sbTop == 0 {
		t.Fatal("pgdn should scroll the nav")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.sbTop != 0 {
		t.Fatalf("pgup should scroll it back: %d", m.sbTop)
	}
	// walking the cursor down keeps it in view
	for range many {
		press(&m, tea.KeyMsg{Type: tea.KeyDown})
	}
	_, items := m.sidebarLines(m.vp.Height)
	seen := false
	for _, it := range items {
		seen = seen || it == m.sbCursor
	}
	if !seen {
		t.Fatalf("the cursor should stay in view: cursor=%d top=%d items=%v", m.sbCursor, m.sbTop, items)
	}
}
