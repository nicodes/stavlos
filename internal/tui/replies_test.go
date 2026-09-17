package tui

import (
	"github.com/nicodes/stavlos/internal/event"
	"strings"
	"testing"
)

func TestReplyQueueShowsEachRequestFromTheSameSender(t *testing.T) {
	m := channelModel()
	m.agents[0].PendingReplies = []event.ReplyRequest{
		{ID: "r1", From: "b", FromName: "scout", Text: "first question"},
		{ID: "r2", From: "b", FromName: "scout", Text: "second question"},
	}
	if m.asyncCount() != 2 {
		t.Fatal("reply queue collapsed by sender")
	}
	m.focus = focusAsync
	lines, rows := m.tabBodyRows(100)
	text := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(text, "r1 @scout: first question") || !strings.Contains(text, "r2 @scout: second question") {
		t.Fatal(text)
	}
	selectable := 0
	for _, row := range rows {
		if row >= 0 {
			selectable++
		}
	}
	if selectable != 2 {
		t.Fatalf("expected two independent reply rows: %v", rows)
	}
}
