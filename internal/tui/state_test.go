package tui

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/protocol"
)

// TestBindChannelStartsOver: switching channels leaves nothing of the old
// one behind — not the agents and transcripts, an open dirs edit, an armed
// esc or a cursor — while display choices and the prompts (every channel's)
// stay.
func TestBindChannelStartsOver(t *testing.T) {
	m := channelModel()
	m.prompts = []protocol.PromptInfo{{ID: "p"}}
	m.dirEdit = "add"
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
	if len(m.prompts) != 1 {
		t.Fatal("prompts are every channel's: they survive a switch")
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

// TestSwitchBackReplaysOnlyWhatItMissed: a channel switched away from keeps
// what its replay built, so switching back subscribes after its last seq
// and, with nothing new in the log, is caught up at once; the per-channel
// cursors and armed keys still start over, and a stream cut off on leaving
// is dropped.
func TestSwitchBackReplaysOnlyWhatItMissed(t *testing.T) {
	m := channelModel()
	id := m.channelID
	m.reconciled, m.loading, m.seq = true, false, 42
	tr := m.transcript(chatView)
	tr.ApplyStream(protocol.StreamNotification{Turn: 3, Text: "half a"})
	m.chatCursor = 3

	m.bindChannel(protocol.ChannelInfo{ID: "other", Dir: "/x"})
	if m.seq != 0 || len(m.transcripts) != 0 {
		t.Fatalf("a channel never visited starts empty: seq=%d transcripts=%d", m.seq, len(m.transcripts))
	}
	m.reconciled = true
	m.bindChannel(protocol.ChannelInfo{ID: id, Dir: "/x"})
	if m.seq != 42 || m.transcripts[chatView] != tr || m.chatCursor != 0 {
		t.Fatalf("back on the channel: seq=%d same transcript=%v cursor=%d", m.seq, m.transcripts[chatView] == tr, m.chatCursor)
	}
	if len(tr.Tail()) != 0 {
		t.Fatal("the stream cut off on leaving is dropped")
	}
	if _, kept := m.visited["other"]; !kept || len(m.visited) != 1 {
		t.Fatalf("the channel left is kept, the one bound is not: %v", m.visited)
	}
	nm, _ := m.Update(reconcileMsg{res: protocol.ReconcileResult{Channel: protocol.ChannelInfo{ID: id, Dir: "/x"}, Seq: 42}})
	if m = nm.(Model); m.loading {
		t.Fatal("nothing past the kept seq: no replay to wait for")
	}
	nm, _ = m.Update(reconcileMsg{res: protocol.ReconcileResult{Channel: protocol.ChannelInfo{ID: id, Dir: "/x"}, Seq: 50}})
	if m = nm.(Model); !m.loading || m.replayTo != 50 {
		t.Fatalf("events past the kept seq replay: loading=%v to=%d", m.loading, m.replayTo)
	}
}

// TestVisitedIsBounded: only the channels left most recently keep their
// replay.
func TestVisitedIsBounded(t *testing.T) {
	m := channelModel()
	for i := range maxVisited + 3 {
		m.reconciled = true
		m.bindChannel(protocol.ChannelInfo{ID: fmt.Sprintf("c%d", i), Dir: "/x"})
	}
	if len(m.visited) != maxVisited {
		t.Fatalf("visited holds %d channels, want %d", len(m.visited), maxVisited)
	}
	if _, ok := m.visited["c1"]; ok {
		t.Fatal("the channel left longest ago is dropped first")
	}
}
