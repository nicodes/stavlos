// Package anthropic implements model.Provider for the Anthropic Messages API
// using the official Go SDK (PRD §8.2): streaming, thinking blocks, prompt
// caching, and tool-use blocks.
package anthropic

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/nicodes/stavlos/internal/model"
)

// Name is the provider prefix served by this package.
const Name = "anthropic"

// defaultMaxTokens is used when Request.MaxTokens is zero.
const defaultMaxTokens = 16000

type provider struct {
	client anthropic.Client
}

// New returns the anthropic provider. apiKey is sent as x-api-key; the SDK
// honours ANTHROPIC_BASE_URL for the endpoint.
func New(apiKey string) model.Provider {
	return &provider{client: anthropic.NewClient(option.WithAPIKey(apiKey))}
}

func (p *provider) Name() string { return Name }

func (p *provider) Open(modelID string) (model.Model, error) {
	return &client{c: p.client, id: modelID}, nil
}

// client is one opened model.
type client struct {
	c  anthropic.Client
	id string
}

// adaptiveThinkingFamilies are model-id substrings that accept the adaptive
// thinking config. Older models use budget_tokens; we send nothing for them.
var adaptiveThinkingFamilies = []string{
	"opus-4-6", "sonnet-4-6", "opus-4-7", "opus-4-8", "opus-5", "sonnet-5", "fable",
}

// supportsAdaptiveThinking reports whether id should get Thinking: adaptive.
func supportsAdaptiveThinking(id string) bool {
	id = strings.ToLower(id)
	for _, f := range adaptiveThinkingFamilies {
		if strings.Contains(id, f) {
			return true
		}
	}
	return false
}
