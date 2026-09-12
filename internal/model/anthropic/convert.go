package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/nicodes/stavlos/internal/model"
)

// redactedPrefix marks a Block.Signature that carries an opaque
// redacted_thinking payload rather than a thinking signature.
const redactedPrefix = "redacted:"

// emptyToolResult stands in for an empty tool output; the API rejects empty text.
const emptyToolResult = "(no output)"

// buildParams maps a model.Request onto SDK params.
func buildParams(req model.Request) (anthropic.MessageNewParams, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(maxTokens),
		Messages:  toMessages(req.Messages),
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{
			Text:         req.System,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}
	tools, err := toTools(req.Tools)
	if err != nil {
		return params, err
	}
	params.Tools = tools
	if supportsAdaptiveThinking(req.Model) {
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{
				Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized,
			},
		}
	}
	return params, nil
}

// toMessages converts the conversation, merging consecutive same-role
// messages (the API requires strict alternation) and ensuring the first
// message is from the user.
func toMessages(msgs []model.Message) []anthropic.MessageParam {
	var out []anthropic.MessageParam
	for _, m := range msgs {
		role := anthropic.MessageParamRoleUser
		if m.Role == model.RoleAssistant {
			role = anthropic.MessageParamRoleAssistant
		}
		blocks := toBlocks(m.Blocks)
		if len(blocks) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}
		out = append(out, anthropic.MessageParam{Role: role, Content: blocks})
	}
	if len(out) == 0 || out[0].Role != anthropic.MessageParamRoleUser {
		out = append([]anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("(continue)"))}, out...)
	}
	return out
}

// toBlocks converts content blocks, dropping anything the API would reject
// (empty text, unsigned thinking).
func toBlocks(blocks []model.Block) []anthropic.ContentBlockParamUnion {
	out := make([]anthropic.ContentBlockParamUnion, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case model.BlockText:
			if b.Text != "" {
				out = append(out, anthropic.NewTextBlock(b.Text))
			}
		case model.BlockThinking:
			switch {
			case b.Signature == "":
				// Unsigned thinking cannot be replayed; drop it.
			case len(b.Signature) > len(redactedPrefix) && b.Signature[:len(redactedPrefix)] == redactedPrefix:
				out = append(out, anthropic.NewRedactedThinkingBlock(b.Signature[len(redactedPrefix):]))
			default:
				out = append(out, anthropic.NewThinkingBlock(b.Signature, b.Text))
			}
		case model.BlockToolUse:
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out = append(out, anthropic.NewToolUseBlock(b.ID, input, b.Name))
		case model.BlockToolResult:
			content := b.Content
			if content == "" {
				content = emptyToolResult
			}
			out = append(out, anthropic.NewToolResultBlock(b.ToolUseID, content, b.IsError))
		}
	}
	return out
}

// toTools converts tool definitions and places a cache breakpoint on the last one.
func toTools(defs []model.ToolDef) ([]anthropic.ToolUnionParam, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	out := make([]anthropic.ToolUnionParam, 0, len(defs))
	for _, d := range defs {
		schema, err := toInputSchema(d.Schema)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", d.Name, err)
		}
		t := &anthropic.ToolParam{Name: d.Name, InputSchema: schema}
		if d.Description != "" {
			t.Description = anthropic.String(d.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: t})
	}
	out[len(out)-1].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
	return out, nil
}

// toInputSchema lifts a raw JSON Schema object into the SDK's typed param.
// "properties" and "required" map to fields; other keys ride along as extras.
func toInputSchema(raw json.RawMessage) (anthropic.ToolInputSchemaParam, error) {
	schema := anthropic.ToolInputSchemaParam{}
	if len(raw) == 0 {
		schema.Properties = map[string]any{}
		return schema, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return schema, fmt.Errorf("schema: %w", err)
	}
	if props, ok := m["properties"]; ok {
		schema.Properties = props
	} else {
		schema.Properties = map[string]any{}
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				schema.Required = append(schema.Required, s)
			}
		}
	}
	for k, v := range m {
		switch k {
		case "properties", "required", "type":
		default:
			if schema.ExtraFields == nil {
				schema.ExtraFields = map[string]any{}
			}
			schema.ExtraFields[k] = v
		}
	}
	return schema, nil
}

// fromContent converts an accumulated response into model blocks.
func fromContent(content []anthropic.ContentBlockUnion) []model.Block {
	out := make([]model.Block, 0, len(content))
	for _, c := range content {
		switch v := c.AsAny().(type) {
		case anthropic.TextBlock:
			out = append(out, model.Block{Type: model.BlockText, Text: v.Text})
		case anthropic.ThinkingBlock:
			out = append(out, model.Block{Type: model.BlockThinking, Text: v.Thinking, Signature: v.Signature})
		case anthropic.RedactedThinkingBlock:
			out = append(out, model.Block{Type: model.BlockThinking, Signature: redactedPrefix + v.Data})
		case anthropic.ToolUseBlock:
			input := json.RawMessage(v.Input)
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out = append(out, model.Block{Type: model.BlockToolUse, ID: v.ID, Name: v.Name, Input: input})
		}
	}
	return out
}

func fromStopReason(r anthropic.StopReason) model.StopReason {
	switch r {
	case anthropic.StopReasonEndTurn:
		return model.StopEndTurn
	case anthropic.StopReasonToolUse:
		return model.StopToolUse
	case anthropic.StopReasonMaxTokens:
		return model.StopMaxTokens
	case anthropic.StopReasonRefusal:
		return model.StopRefusal
	default:
		return model.StopOther
	}
}

func fromUsage(u anthropic.Usage) model.Usage {
	return model.Usage{
		InputTokens:      int(u.InputTokens),
		OutputTokens:     int(u.OutputTokens),
		CacheReadTokens:  int(u.CacheReadInputTokens),
		CacheWriteTokens: int(u.CacheCreationInputTokens),
	}
}
