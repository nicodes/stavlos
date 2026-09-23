package project

import (
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/model"
)

// Old tool results are cleared to a note, newest kept, the conversation's
// own tools never touched, and the tool_use blocks left where they were.
func TestClearOldKeepsTheRecentAndTheConversation(t *testing.T) {
	big := strings.Repeat("x", 4000) // about 1,000 tokens
	msgs := []model.Message{
		{Role: model.RoleAssistant, Blocks: []model.Block{{Type: model.BlockToolUse, ID: "r1", Name: "read"}, {Type: model.BlockToolUse, ID: "m1", Name: "message"}}},
		{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockToolResult, ToolUseID: "r1", Content: big}, {Type: model.BlockToolResult, ToolUseID: "m1", Content: big}}},
		{Role: model.RoleAssistant, Blocks: []model.Block{{Type: model.BlockToolUse, ID: "r2", Name: "read"}}},
		{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockToolResult, ToolUseID: "r2", Content: big}}},
		{Role: model.RoleAssistant, Blocks: []model.Block{{Type: model.BlockToolUse, ID: "r3", Name: "shell"}}},
		{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockToolResult, ToolUseID: "r3", Content: big}}},
	}
	keep := map[string]bool{"message": true}
	if n := ClearOld(msgs, 1500, keep); n != 1000 {
		t.Fatalf("cleared %d tokens, want the one oldest read (1000)", n)
	}
	if msgs[1].Blocks[0].Content != ClearedNote || msgs[1].Blocks[1].Content != big || msgs[3].Blocks[0].Content != big || msgs[5].Blocks[0].Content != big {
		t.Fatalf("wrong results cleared: %q %q %q %q", short(msgs[1].Blocks[0].Content), short(msgs[1].Blocks[1].Content), short(msgs[3].Blocks[0].Content), short(msgs[5].Blocks[0].Content))
	}
	if len(msgs[0].Blocks) != 2 || msgs[0].Blocks[0].Type != model.BlockToolUse {
		t.Fatal("the tool_use record was touched")
	}
	if n := ClearOld(msgs, 1500, keep); n != 0 {
		t.Fatalf("a second pass cleared %d more", n)
	}
	if n := ClearOld(msgs, 0, keep); n != 0 {
		t.Fatal("keep 0 means never")
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}
