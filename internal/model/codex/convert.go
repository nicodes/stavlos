package codex

import (
	"encoding/json"

	"github.com/nicodes/stavlos/internal/model"
)

// responsesRequest is the Responses API request body.
type responsesRequest struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions,omitempty"`
	Input             []inputItem     `json:"input"`
	Tools             []toolDef       `json:"tools,omitempty"`
	ToolChoice        string          `json:"tool_choice"`
	ParallelToolCalls bool            `json:"parallel_tool_calls"`
	Store             bool            `json:"store"`
	Stream            bool            `json:"stream"`
	Include           []string        `json:"include"`
	Reasoning         reasoningConfig `json:"reasoning"`
	Text              textConfig      `json:"text"`
}

type reasoningConfig struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type textConfig struct {
	Verbosity string `json:"verbosity"`
}

type toolDef struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

// inputItem is one Responses "input" item. Only the fields for Type are set.
type inputItem struct {
	Type string `json:"type"`

	// message
	Role    string        `json:"role,omitempty"`
	Content []contentPart `json:"content,omitempty"`

	// reasoning
	ID               string   `json:"id,omitempty"`
	Summary          []string `json:"summary,omitempty"`
	EncryptedContent string   `json:"encrypted_content,omitempty"`

	// function_call / function_call_output
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MarshalJSON emits "summary": [] (not omitted) for reasoning items, which
// the backend requires alongside encrypted_content.
func (it inputItem) MarshalJSON() ([]byte, error) {
	type plain inputItem
	if it.Type != "reasoning" {
		return json.Marshal(plain(it))
	}
	return json.Marshal(struct {
		plain
		Summary []string `json:"summary"`
	}{plain: plain(it), Summary: []string{}})
}

func buildBody(id string, req model.Request) ([]byte, error) {
	rr := responsesRequest{
		Model:             id,
		Instructions:      req.System,
		Input:             toInput(req.Messages),
		Tools:             toTools(req.Tools),
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		Store:             false,
		Stream:            true,
		Include:           []string{"reasoning.encrypted_content"},
		Reasoning:         reasoningConfig{Effort: "medium", Summary: "auto"},
		Text:              textConfig{Verbosity: "medium"},
	}
	if req.Variant != "" {
		rr.Reasoning.Effort = req.Variant
	}
	// Request.MaxTokens is not forwarded: the ChatGPT Codex backend rejects
	// max_output_tokens ("Unsupported parameter"), so the model's own limit
	// applies. Callers that set it (compaction) get a longer answer at
	// worst.
	return json.Marshal(rr)
}

func toTools(defs []model.ToolDef) []toolDef {
	if len(defs) == 0 {
		return nil
	}
	out := make([]toolDef, 0, len(defs))
	for _, d := range defs {
		params := d.Schema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, toolDef{
			Type:        "function",
			Name:        d.Name,
			Description: d.Description,
			Parameters:  params,
			Strict:      false,
		})
	}
	return out
}

// toInput flattens messages into Responses items, preserving block order.
// Consecutive text blocks in one message fold into a single message item.
func toInput(msgs []model.Message) []inputItem {
	items := make([]inputItem, 0, len(msgs))
	for _, m := range msgs {
		var msgItem *inputItem // open message item for consecutive text blocks
		flush := func() {
			if msgItem != nil {
				items = append(items, *msgItem)
				msgItem = nil
			}
		}
		for _, b := range m.Blocks {
			switch b.Type {
			case model.BlockText:
				partType, role := "input_text", "user"
				if m.Role == model.RoleAssistant {
					partType, role = "output_text", "assistant"
				}
				if msgItem == nil {
					msgItem = &inputItem{Type: "message", Role: role}
				}
				msgItem.Content = append(msgItem.Content, contentPart{Type: partType, Text: b.Text})
			case model.BlockThinking:
				flush()
				if b.Signature == "" {
					continue
				}
				items = append(items, inputItem{
					Type:             "reasoning",
					ID:               b.ID,
					EncryptedContent: b.Signature,
				})
			case model.BlockToolUse:
				flush()
				args := string(b.Input)
				if len(b.Input) == 0 {
					args = "{}"
				}
				items = append(items, inputItem{
					Type:      "function_call",
					CallID:    b.ID,
					Name:      b.Name,
					Arguments: args,
				})
			case model.BlockToolResult:
				flush()
				out := b.Content
				if out == "" {
					out = "(no output)"
				}
				if b.IsError {
					out = "ERROR: " + out
				}
				items = append(items, inputItem{
					Type:   "function_call_output",
					CallID: b.ToolUseID,
					Output: out,
				})
			}
		}
		flush()
	}
	return items
}
