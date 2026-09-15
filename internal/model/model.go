// Package model defines the provider-neutral model contract (PRD §8).
// Adapters (anthropic, openai-compatible, plugins) implement Model.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Role of a conversation message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType enumerates content block kinds.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockThinking   BlockType = "thinking"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// Block is one content block. Only the fields relevant to Type are set.
type Block struct {
	Type BlockType `json:"type"`

	// text / thinking
	Text string `json:"text,omitempty"`
	// thinking: the provider's own id for the reasoning item and its opaque
	// (encrypted) content, replayed verbatim on the next call.
	ProviderID string `json:"provider_id,omitempty"`
	Opaque     string `json:"opaque,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"` // synthesized by the projector (PRD §4.3)
}

// Message is one turn of conversation as the model sees it.
type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

// ToolDef is a tool the model may call.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"` // JSON Schema object for the input
}

// Request is one model call.
type Request struct {
	Model     string    // bare model id (provider prefix already stripped)
	System    string    // system prompt; may be empty
	Messages  []Message // must alternate roles starting with user
	Tools     []ToolDef
	MaxTokens int // 0 → adapter default
	// Variant selects a provider-defined flavour of the model (reasoning
	// effort, thinking budget…); "" is the provider default. Providers list
	// the valid names through the optional Variants interface.
	Variant string
}

// StopReason reports why generation stopped.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopOther     StopReason = "other"
)

// Usage is token accounting for one call (PRD §4.4).
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// Response is the completed assistant message plus accounting.
type Response struct {
	Blocks     []Block
	StopReason StopReason
	Usage      Usage
}

// Delta is a streaming fragment. Exactly one field is set.
type Delta struct {
	Text     string // visible text
	Thinking string // thinking summary text, if the provider exposes it
	ToolName string // a tool call has started (name only; input arrives with the Response)
}

// Model is the wire-protocol seam. Implementations must honour ctx cancellation
// promptly: a cancelled turn must not block on a half-finished stream.
type Model interface {
	// Complete runs one call. onDelta may be nil. It is called from the
	// adapter's goroutine and must not block for long.
	Complete(ctx context.Context, req Request, onDelta func(Delta)) (Response, error)
}

// Provider constructs Models for a provider prefix (PRD §8.3, §11.4).
type Provider interface {
	// Name is the prefix before the slash in "provider/model-id".
	Name() string
	// Open returns a Model for the bare id.
	Open(modelID string) (Model, error)
}

// Capabilities are the ways a provider's models depart from the common
// request shape. The zero value is a model that honours every field.
type Capabilities struct {
	// IgnoresMaxTokens: Request.MaxTokens is not sent (the backend rejects
	// it), so a caller wanting a short answer must ask for one in words.
	IgnoresMaxTokens bool
}

// Capable is implemented by providers whose models have Capabilities.
type Capable interface {
	Capabilities(modelID string) Capabilities
}

// Variants is implemented by providers whose models come in flavours
// (reasoning effort, thinking budget). The names are provider-defined and
// shown to the user as-is; an empty list means the model has none.
type Variants interface {
	Variants(modelID string) []string
}

// Split parses "provider/model-id". The model id may itself contain slashes.
func Split(full string) (provider, id string, err error) {
	i := strings.IndexByte(full, '/')
	if i <= 0 || i == len(full)-1 {
		return "", "", fmt.Errorf("model %q must be provider/model-id", full)
	}
	return full[:i], full[i+1:], nil
}

// Info is metadata about a model from models.dev (PRD §8.1).
type Info struct {
	Capabilities
	ContextWindow int
	MaxOutput     int
	// USD per million tokens.
	InputPrice, OutputPrice, CacheReadPrice, CacheWritePrice float64
}

// Cost computes USD for a usage record given pricing.
func (i Info) Cost(u Usage) float64 {
	return (float64(u.InputTokens)*i.InputPrice +
		float64(u.OutputTokens)*i.OutputPrice +
		float64(u.CacheReadTokens)*i.CacheReadPrice +
		float64(u.CacheWriteTokens)*i.CacheWritePrice) / 1e6
}

// Token is a bearer credential for one request.
type Token struct {
	Access    string
	AccountID string // ChatGPT account id; empty for other providers
}

// TokenSource yields a fresh bearer token, refreshing if needed. Adapters
// call it per request so a rotated token is picked up without a restart.
type TokenSource func(ctx context.Context) (Token, error)

// ToolSchema is a tool's input schema as providers want it: an empty
// object schema when the tool declares none.
func ToolSchema(d ToolDef) json.RawMessage {
	if len(d.Schema) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return d.Schema
}

// ToolArguments is a tool call's input as the wire carries it: "{}" when
// empty.
func ToolArguments(b Block) string {
	if len(b.Input) == 0 {
		return "{}"
	}
	return string(b.Input)
}

// ToolResultText is a tool result as every provider receives it: "(no
// output)" for an empty one, and an error marked as such.
func ToolResultText(b Block) string {
	out := b.Content
	if out == "" {
		out = "(no output)"
	}
	if b.IsError && !strings.HasPrefix(out, "Error") {
		out = "Error: " + out
	}
	return out
}

// UsageFrom is the usage of a call whose provider counts cached input
// inside the input total: the cached part is reported apart.
func UsageFrom(input, output, cached int) Usage {
	return Usage{InputTokens: max(input-cached, 0), OutputTokens: output, CacheReadTokens: cached}
}
