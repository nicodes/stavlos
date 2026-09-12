package project

import (
	"encoding/json"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

func ev(seq int64, t event.Type, p any) event.Event {
	return event.Event{Seq: seq, Type: t, Payload: event.MustPayload(p)}
}

func TestCancelledTurnRepair(t *testing.T) {
	evs := []event.Event{
		ev(1, event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi"}),
		ev(2, event.AssistantMessage, event.AssistantMessagePayload{Blocks: []model.Block{
			{Type: model.BlockText, Text: "running"},
			{Type: model.BlockToolUse, ID: "c1", Name: "bash", Input: json.RawMessage(`{"cmd":"sleep"}`)},
			{Type: model.BlockToolUse, ID: "c2", Name: "read", Input: json.RawMessage(`{}`)},
		}}),
		ev(3, event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c2", Output: "file"}),
		ev(4, event.TurnEnded, event.TurnEndedPayload{Reason: "cancelled"}),
		ev(5, event.UserMessage, event.UserMessagePayload{Kind: "steer", Text: "stop that"}),
	}
	h := Project(evs)
	if len(h) != 3 {
		t.Fatalf("want 3 messages got %d: %+v", len(h), h)
	}
	u := h[2]
	if u.Role != model.RoleUser || len(u.Blocks) != 3 {
		t.Fatalf("user msg %+v", u)
	}
	if u.Blocks[0].ToolUseID != "c2" || u.Blocks[1].ToolUseID != "c1" || !u.Blocks[1].Cancelled || u.Blocks[2].Text != "stop that" {
		t.Fatalf("blocks %+v", u.Blocks)
	}
}

func TestCompaction(t *testing.T) {
	evs := []event.Event{
		ev(1, event.UserMessage, event.UserMessagePayload{Text: "a"}),
		ev(2, event.AssistantMessage, event.AssistantMessagePayload{Blocks: []model.Block{{Type: model.BlockText, Text: "b"}}}),
		ev(3, event.UserMessage, event.UserMessagePayload{Text: "c"}),
		ev(4, event.AssistantMessage, event.AssistantMessagePayload{Blocks: []model.Block{{Type: model.BlockText, Text: "d"}}}),
		ev(5, event.Compacted, event.CompactedPayload{FromSeq: 1, ToSeq: 2, Summary: "S"}),
	}
	h := Project(evs)
	if len(h) != 4 || h[0].Blocks[0].Text[:5] != "[The " || h[2].Blocks[0].Text != "c" {
		t.Fatalf("%+v", h)
	}
}
