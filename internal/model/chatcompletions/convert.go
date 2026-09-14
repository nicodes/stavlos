package chatcompletions

import (
	"encoding/json"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
)

func strp(s string) *string { return &s }

// toMessages flattens the conversation into Chat Completions messages.
// Consecutive same-role model messages are merged first; tool results
// become role "tool" messages, which the API expects directly after the
// assistant message carrying the matching tool_calls.
func toMessages(system string, msgs []model.Message) []chatMessage {
	var out []chatMessage
	if system != "" {
		out = append(out, chatMessage{Role: "system", Content: strp(system)})
	}
	for _, m := range mergeSameRole(msgs) {
		if m.Role == model.RoleAssistant {
			out = append(out, assistantMessage(m.Blocks))
			continue
		}
		out = append(out, userMessages(m.Blocks)...)
	}
	return out
}

// mergeSameRole coalesces adjacent messages with the same role.
func mergeSameRole(msgs []model.Message) []model.Message {
	var out []model.Message
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Blocks = append(out[n-1].Blocks, m.Blocks...)
			continue
		}
		cp := model.Message{Role: m.Role, Blocks: append([]model.Block(nil), m.Blocks...)}
		out = append(out, cp)
	}
	return out
}

// assistantMessage joins text and carries tool calls; thinking is not replayed.
func assistantMessage(blocks []model.Block) chatMessage {
	var texts []string
	var calls []toolCall
	for _, b := range blocks {
		switch b.Type {
		case model.BlockText:
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case model.BlockToolUse:
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			calls = append(calls, toolCall{
				ID:       b.ID,
				Type:     "function",
				Function: toolFunction{Name: b.Name, Arguments: args},
			})
		}
	}
	msg := chatMessage{Role: "assistant", ToolCalls: calls}
	if len(texts) > 0 {
		msg.Content = strp(strings.Join(texts, "\n"))
	}
	return msg
}

// userMessages emits tool results first (one "tool" message each), then a
// single "user" message with the remaining text.
func userMessages(blocks []model.Block) []chatMessage {
	var out []chatMessage
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case model.BlockToolResult:
			content := b.Content
			if b.IsError && !strings.HasPrefix(content, "Error") {
				content = "Error: " + content
			}
			out = append(out, chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: strp(content)})
		case model.BlockText:
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
	}
	if len(texts) > 0 {
		out = append(out, chatMessage{Role: "user", Content: strp(strings.Join(texts, "\n"))})
	}
	return out
}

func toTools(defs []model.ToolDef) []chatTool {
	if len(defs) == 0 {
		return nil
	}
	out := make([]chatTool, 0, len(defs))
	for _, d := range defs {
		params := d.Schema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, chatTool{
			Type:     "function",
			Function: toolDefinion{Name: d.Name, Description: d.Description, Parameters: params},
		})
	}
	return out
}

func fromFinishReason(reason string, hasToolCalls bool) model.StopReason {
	switch reason {
	case "tool_calls", "function_call":
		return model.StopToolUse
	case "stop":
		if hasToolCalls {
			return model.StopToolUse
		}
		return model.StopEndTurn
	case "length":
		return model.StopMaxTokens
	case "content_filter":
		return model.StopRefusal
	case "":
		if hasToolCalls {
			return model.StopToolUse
		}
		return model.StopEndTurn
	default:
		return model.StopOther
	}
}
