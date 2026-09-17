package chatcompletions

import (
	"strings"

	"github.com/nicodes/stavlos/internal/model"
)

func strp(s string) *string { return &s }

// toMessages flattens the conversation into Chat Completions messages.
// Consecutive same-role model messages are merged first; tool results
// become role "tool" messages, which the API expects directly after the
// assistant message carrying the matching tool_calls.
// With replayReasoning, an assistant message carries its reasoning back.
func toMessages(system string, msgs []model.Message, replayReasoning bool) []chatMessage {
	var out []chatMessage
	if system != "" {
		out = append(out, chatMessage{Role: "system", Content: strp(system)})
	}
	for _, m := range mergeSameRole(msgs) {
		if m.Role == model.RoleAssistant {
			out = append(out, assistantMessage(m.Blocks, replayReasoning))
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

// assistantMessage joins text and carries tool calls; with replayReasoning
// its thinking goes back as reasoning_content (a Chat Completions reasoning
// text: blocks with an opaque payload are another API's).
func assistantMessage(blocks []model.Block, replayReasoning bool) chatMessage {
	var texts, reasoning []string
	var calls []toolCall
	for _, b := range blocks {
		switch b.Type {
		case model.BlockThinking:
			if replayReasoning && b.Opaque == "" && b.Text != "" {
				reasoning = append(reasoning, b.Text)
			}
		case model.BlockText:
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case model.BlockToolUse:
			calls = append(calls, toolCall{ID: b.ID, Type: "function", Function: toolFunction{Name: b.Name, Arguments: model.ToolArguments(b)}})
		}
	}
	msg := chatMessage{Role: "assistant", ToolCalls: calls, ReasoningContent: strings.Join(reasoning, "\n")}
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
			out = append(out, chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: strp(model.ToolResultText(b))})
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
		out = append(out, chatTool{Type: "function", Function: toolDefinition{Name: d.Name, Description: d.Description, Parameters: model.ToolSchema(d)}})
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
