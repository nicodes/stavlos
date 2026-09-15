package tui

import (
	"reflect"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/protocol"
)

// TestBindChannelStartsOver: switching channels leaves nothing of the old
// one behind — not the agents and transcripts, and not a half-answered
// question, an open reason row, an armed esc or a cursor — while display
// choices stay.
func TestBindChannelStartsOver(t *testing.T) {
	m := channelModel()
	m.prompts = []protocol.PromptInfo{{ID: "p"}}
	m.promptBusy, m.permSel, m.permFor, m.permEdit, m.dirEdit = "p", 2, "p", "deny", "add"
	m.q = questionState{id: "q", idx: 1, typing: true}
	m.claimedByUs["p"] = true
	m.mcpOpen = map[string]bool{"github": true}
	m.chatCursor, m.agCursor, m.selected = 3, 2, 1
	m.cancelArmed, m.quitArmed = time.Now(), time.Now()
	m.expanded = map[string]map[int]bool{"a": {1: true}}
	m.showTree, m.details = true, true

	info := protocol.ChannelInfo{ID: "next", Dir: "/x"}
	m.bindChannel(info)
	want := newChannelState("next", info)
	want.superChat = true                               // a bound channel opens on its chat
	want.itemRows, want.renders = m.itemRows, m.renders // drawn by the refresh inside bindChannel
	if !reflect.DeepEqual(m.channelState, want) {
		t.Fatalf("channel state after bind:\n%+v\nwant\n%+v", m.channelState, want)
	}
	if !m.showTree || !m.details || !m.follow {
		t.Fatalf("display choices should survive: tree=%v details=%v follow=%v", m.showTree, m.details, m.follow)
	}
}

// TestClaimThenGuards: one answer at a time per prompt, none to a prompt
// another client holds, whatever the answer is.
func TestClaimThenGuards(t *testing.T) {
	m := channelModel()
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
