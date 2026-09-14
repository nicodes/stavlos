package tui

import (
	"reflect"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/protocol"
)

// TestBindSessionStartsOver: switching sessions leaves nothing of the old
// one behind — not the agents and transcripts, and not a half-answered
// question, an open reason row, an armed esc or a cursor — while display
// choices stay.
func TestBindSessionStartsOver(t *testing.T) {
	m := sessionModel()
	m.prompts = []protocol.PromptInfo{{ID: "p"}}
	m.promptBusy, m.permSel, m.permFor, m.permEdit, m.dirEdit = "p", 2, "p", "deny", "add"
	m.q = questionState{id: "q", idx: 1, typing: true}
	m.claimedByUs["p"] = true
	m.mcpOpen = map[string]bool{"github": true}
	m.chatCursor, m.agCursor, m.selected = 3, 2, 1
	m.cancelArmed, m.quitArmed = time.Now(), time.Now()
	m.expanded = map[string]map[int]bool{"a": {1: true}}
	m.showTree, m.details = true, true

	info := protocol.SessionInfo{ID: "next", Dir: "/x"}
	m.bindSession(info)
	want := newSessionState("next", info)
	want.itemRows = m.itemRows // drawn by the refresh inside bindSession
	if !reflect.DeepEqual(m.sessionState, want) {
		t.Fatalf("session state after bind:\n%+v\nwant\n%+v", m.sessionState, want)
	}
	if !m.showTree || !m.details || !m.follow {
		t.Fatalf("display choices should survive: tree=%v details=%v follow=%v", m.showTree, m.details, m.follow)
	}
}

// TestClaimThenGuards: one answer at a time per prompt, none to a prompt
// another client holds, whatever the answer is.
func TestClaimThenGuards(t *testing.T) {
	m := sessionModel()
	p := &protocol.PromptInfo{ID: "p", Kind: protocol.PromptPermission}
	if cmd := m.answerPrompt(p, "allow"); cmd == nil || m.promptBusy != "p" || !m.claimedByUs["p"] {
		t.Fatalf("first answer: busy=%q claimed=%v", m.promptBusy, m.claimedByUs["p"])
	}
	m.denyPrompt(p, "no")
	if m.status != "answer in flight…" || m.statusErr {
		t.Fatalf("second answer while one is in flight: %q", m.status)
	}
	other := &protocol.PromptInfo{ID: "o", ClaimedBy: "someone"}
	m.answerQuestions(other, []string{"x"})
	if m.status != "claimed by another client" || !m.statusErr || m.promptBusy != "p" || m.claimedByUs["o"] {
		t.Fatalf("a prompt held elsewhere: status=%q busy=%q", m.status, m.promptBusy)
	}
	m.claimedByUs["o"] = true // we hold it: our own claim does not block us
	m.promptBusy = ""
	if cmd := m.answerPromptDir(other, "/d"); cmd == nil || m.promptBusy != "o" {
		t.Fatalf("our own claim: busy=%q", m.promptBusy)
	}
}
