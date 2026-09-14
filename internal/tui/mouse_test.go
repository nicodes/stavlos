package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/protocol"
)

// TestPromptOptionsTakeTheMouse: the rows of the permission and questions
// dialogs are hit where they are drawn, below a subject of any height:
// hover moves the selection, a click acts like space, and the subject,
// the question and an open text row do not respond.
func TestPromptOptionsTakeTheMouse(t *testing.T) {
	m := sessionModel()
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

	move(at("Allow for this session"))
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
