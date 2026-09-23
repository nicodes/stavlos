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
	user := func(text string) model.Message {
		return model.Message{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: text}}}
	}
	use := func(id, name string) model.Message {
		return model.Message{Role: model.RoleAssistant, Blocks: []model.Block{{Type: model.BlockToolUse, ID: id, Name: name}}}
	}
	result := func(id string) model.Message {
		return model.Message{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockToolResult, ToolUseID: id, Content: big}}}
	}
	msgs := []model.Message{
		user("first task"),
		use("r1", "read"), result("r1"),
		use("m1", "message"), result("m1"),
		use("r2", "read"), result("r2"),
		use("r3", "read"), result("r3"),
		user("second task"),
		use("r4", "read"), result("r4"),
		user("third task"),
		use("r5", "read"), result("r5"),
	}
	keep := map[string]bool{"message": true}
	// r5 and r4 are in the two most recent turns: untouchable. Of the rest,
	// the newest 1,500 tokens' worth (r3, and r2 up to the budget) stays; r1
	// goes; m1 is the conversation and stays.
	if n := ClearOld(msgs, 1500, 500, keep); n != 1000 {
		t.Fatalf("cleared %d tokens, want the one oldest read (1000)", n)
	}
	for i, want := range map[int]bool{2: true, 4: false, 6: false, 8: false, 11: false, 14: false} {
		if got := msgs[i].Blocks[0].Content == ClearedNote; got != want {
			t.Errorf("message %d cleared=%v, want %v", i, got, want)
		}
	}
	if msgs[1].Blocks[0].Type != model.BlockToolUse {
		t.Fatal("the tool_use record was touched")
	}
	if n := ClearOld(msgs, 1500, 500, keep); n != 0 {
		t.Fatalf("a second pass cleared %d more", n)
	}
	if n := ClearOld(msgs, 0, 0, keep); n != 0 {
		t.Fatal("keep 0 means never")
	}
	// too little to be worth a cache miss is left alone
	fresh := []model.Message{user("a"), use("x1", "read"), result("x1"), user("b"), user("c")}
	if n := ClearOld(fresh, 100, 5000, keep); n != 0 || fresh[2].Blocks[0].Content != big {
		t.Fatalf("cleared %d below the minimum", n)
	}
}
