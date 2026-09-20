package tui

import (
	"testing"

	"github.com/nicodes/stavlos/internal/event"
)

// A chat opens with a tail; a channel the TUI already holds part of, and one
// /history asked for, take everything after what is held.
func TestWhichHistoryIsAskedFor(t *testing.T) {
	m := channelModel()
	m.seq = 0
	if m.tail() != historyTail {
		t.Fatalf("a first replay asks for a tail of %d, want %d", m.tail(), historyTail)
	}
	m.seq = 40
	if m.tail() != 0 {
		t.Fatal("a replay that continues was cut")
	}
}

// /history drops what the tail built and ignores the old subscription's
// events until the new replay's first arrives, so nothing is drawn twice.
func TestHistoryReloadsFromTheFirstEvent(t *testing.T) {
	m := channelModel()
	if m.loadHistory(); m.awaitFirst {
		t.Fatal("/history reloaded a channel whose history is all here")
	}
	m.historyFrom, m.seq = 5000, 8000
	m.loadHistory()
	if !m.wholeLog || !m.awaitFirst || m.seq != 0 || m.tail() != 0 || len(m.transcripts) != 0 {
		t.Fatalf("after /history: whole %v await %v seq %d tail %d transcripts %d", m.wholeLog, m.awaitFirst, m.seq, m.tail(), len(m.transcripts))
	}
	m.applyEvent(event.Event{Channel: m.channelID, Seq: 8001, Type: event.TurnStarted, Agent: "a"})
	if m.seq != 0 {
		t.Fatalf("an event of the replaced subscription was taken: seq %d", m.seq)
	}
	m.applyEvent(event.Event{Channel: m.channelID, Seq: 1, Type: event.ChannelCreated})
	m.applyEvent(event.Event{Channel: m.channelID, Seq: 2, Type: event.TurnStarted, Agent: "a"})
	if m.awaitFirst || m.seq != 2 {
		t.Fatalf("the new replay was not taken: await %v seq %d", m.awaitFirst, m.seq)
	}
}
