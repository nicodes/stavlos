package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/nicodes/stavlos/internal/model"
)

func text(role model.Role, s string) model.Message {
	return model.Message{Role: role, Blocks: []model.Block{{Type: model.BlockText, Text: s}}}
}

func TestToMessagesMergesAndAlternates(t *testing.T) {
	msgs := []model.Message{
		text(model.RoleUser, "a"),
		text(model.RoleUser, "b"),
		{Role: model.RoleAssistant, Blocks: []model.Block{
			{Type: model.BlockThinking, Text: "unsigned"},
			{Type: model.BlockThinking, Text: "signed", Signature: "sig"},
			{Type: model.BlockText, Text: ""},
			{Type: model.BlockToolUse, ID: "t1", Name: "ls", Input: json.RawMessage(`{"p":"."}`)},
		}},
		{Role: model.RoleUser, Blocks: []model.Block{
			{Type: model.BlockToolResult, ToolUseID: "t1", Content: "", IsError: true},
		}},
		text(model.RoleUser, "c"),
		text(model.RoleAssistant, "d"),
		text(model.RoleAssistant, "e"),
	}
	out := toMessages(msgs)

	wantRoles := []anthropic.MessageParamRole{
		anthropic.MessageParamRoleUser,
		anthropic.MessageParamRoleAssistant,
		anthropic.MessageParamRoleUser,
		anthropic.MessageParamRoleAssistant,
	}
	if len(out) != len(wantRoles) {
		t.Fatalf("got %d messages, want %d", len(out), len(wantRoles))
	}
	for i, r := range wantRoles {
		if out[i].Role != r {
			t.Errorf("message %d role = %s, want %s", i, out[i].Role, r)
		}
	}

	// user a+b merged into two text blocks
	if n := len(out[0].Content); n != 2 {
		t.Errorf("first user message has %d blocks, want 2", n)
	}
	// assistant: unsigned thinking and empty text dropped; signed thinking + tool_use kept
	if n := len(out[1].Content); n != 2 {
		t.Fatalf("assistant message has %d blocks, want 2", n)
	}
	if th := out[1].Content[0].OfThinking; th == nil || th.Signature != "sig" || th.Thinking != "signed" {
		t.Errorf("thinking block = %+v", out[1].Content[0])
	}
	if tu := out[1].Content[1].OfToolUse; tu == nil || tu.ID != "t1" || tu.Name != "ls" {
		t.Errorf("tool_use block = %+v", out[1].Content[1])
	}
	// tool_result + "c" merged into one user message; empty content substituted
	if n := len(out[2].Content); n != 2 {
		t.Fatalf("second user message has %d blocks, want 2", n)
	}
	tr := out[2].Content[0].OfToolResult
	if tr == nil || tr.ToolUseID != "t1" || !tr.IsError.Value {
		t.Errorf("tool_result block = %+v", out[2].Content[0])
	}
	if len(tr.Content) != 1 || tr.Content[0].OfText == nil || tr.Content[0].OfText.Text != emptyToolResult {
		t.Errorf("empty tool_result content = %+v", tr.Content)
	}
	// d+e merged
	if n := len(out[3].Content); n != 2 {
		t.Errorf("last assistant message has %d blocks, want 2", n)
	}
}

func TestToMessagesPrependsUserWhenAssistantFirst(t *testing.T) {
	out := toMessages([]model.Message{text(model.RoleAssistant, "hi")})
	if len(out) != 2 || out[0].Role != anthropic.MessageParamRoleUser {
		t.Fatalf("got %+v", out)
	}
	if tb := out[0].Content[0].OfText; tb == nil || tb.Text != "(continue)" {
		t.Errorf("prepended block = %+v", out[0].Content[0])
	}
	if out := toMessages(nil); len(out) != 1 || out[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("empty conversation → %+v", out)
	}
}

func TestBuildParams(t *testing.T) {
	req := model.Request{
		Model:    "claude-sonnet-5",
		System:   "be brief",
		Messages: []model.Message{text(model.RoleUser, "hi")},
		Tools: []model.ToolDef{
			{Name: "a", Description: "first", Schema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`)},
			{Name: "b", Schema: nil},
		},
	}
	p, err := buildParams(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxTokens != defaultMaxTokens {
		t.Errorf("MaxTokens = %d", p.MaxTokens)
	}
	if len(p.System) != 1 || p.System[0].Text != "be brief" || p.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("System = %+v", p.System)
	}
	if p.Thinking.OfAdaptive == nil {
		t.Error("sonnet-5 should get adaptive thinking")
	}
	if len(p.Tools) != 2 {
		t.Fatalf("Tools = %d", len(p.Tools))
	}
	a := p.Tools[0].OfTool
	if a.Description.Value != "first" || len(a.InputSchema.Required) != 1 || a.InputSchema.Required[0] != "x" {
		t.Errorf("tool a = %+v", a)
	}
	if a.InputSchema.ExtraFields["additionalProperties"] != false {
		t.Errorf("extra fields = %+v", a.InputSchema.ExtraFields)
	}
	if a.CacheControl.Type != "" {
		t.Error("cache_control should only be on the last tool")
	}
	if p.Tools[1].OfTool.CacheControl.Type != "ephemeral" {
		t.Error("last tool lacks cache_control")
	}
	body, err := json.Marshal(p.Tools[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	schema := got["input_schema"].(map[string]any)
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Errorf("marshalled schema = %v", schema)
	}

	req.Model = "claude-haiku-4-5"
	req.MaxTokens = 100
	p, _ = buildParams(req)
	if p.Thinking.OfAdaptive != nil || p.MaxTokens != 100 {
		t.Errorf("haiku params = thinking %+v max %d", p.Thinking, p.MaxTokens)
	}
}

func TestSupportsAdaptiveThinking(t *testing.T) {
	yes := []string{"claude-opus-4-6", "claude-sonnet-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-opus-5", "claude-sonnet-5", "claude-fable-5-1"}
	no := []string{"claude-haiku-4-5", "claude-sonnet-4-5", "claude-opus-4-5-20251101"}
	for _, id := range yes {
		if !supportsAdaptiveThinking(id) {
			t.Errorf("%s should support adaptive thinking", id)
		}
	}
	for _, id := range no {
		if supportsAdaptiveThinking(id) {
			t.Errorf("%s should not get adaptive thinking", id)
		}
	}
}

func TestFromStopReason(t *testing.T) {
	cases := map[anthropic.StopReason]model.StopReason{
		anthropic.StopReasonEndTurn:   model.StopEndTurn,
		anthropic.StopReasonToolUse:   model.StopToolUse,
		anthropic.StopReasonMaxTokens: model.StopMaxTokens,
		anthropic.StopReasonRefusal:   model.StopRefusal,
		anthropic.StopReasonPauseTurn: model.StopOther,
	}
	for in, want := range cases {
		if got := fromStopReason(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}
