// Package chatcompletions implements model.Provider for the Chat Completions wire
// protocol with a rotating bearer token. The registry uses it for the Grok
// (xAI) subscription; the ChatGPT subscription speaks the Responses
// protocol (internal/model/codex).
package chatcompletions

import (
	"net/http"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/stream"
)

// defaultMaxTokens is used when Request.MaxTokens is zero.
const defaultMaxTokens = 16000

// Traits is what differs between the providers that speak this protocol.
// The adapter used to ask "is this xai?" and "is this kimi?" in five places;
// a provider now says what it is when it is built (the registry's table), and
// the adapter knows no provider by name.
type Traits struct {
	// CacheHeader names the request header that carries the conversation's
	// id, for a provider that routes a call to its cached prefix by header
	// (xAI: x-grok-conv-id). CacheKeyField sends it as prompt_cache_key in
	// the body (Kimi, as kimi-cli does). A provider that caches a matching
	// prefix unprompted sets neither (GLM).
	CacheHeader   string
	CacheKeyField bool
	// ReplaysReasoning sends an assistant turn's thinking back as
	// reasoning_content: leaving it out is the top cause of cache misses for
	// the providers that read it, and a stray field for one that does not.
	ReplaysReasoning bool
	// Variants lists the reasoning efforts a model takes; nil for none. A
	// model that takes none rejects the field, so nothing else is ever sent.
	Variants func(modelID string) []string
	// IsLimit reports whether a refusal means the plan's allowance is used
	// up, so the agent is moved to another model rather than retried; nil
	// uses the transport's general reading (stream.PlanLimit).
	IsLimit func(status int, body []byte) bool
}

type provider struct {
	name    string
	baseURL string
	token   model.TokenSource // called per request, so a rotated token is picked up at once
	http    *http.Client
	traits  Traits
}

// NewWithToken returns a provider named name that speaks Chat Completions
// at baseURL with a bearer token fetched per request from src (a
// subscription login whose access token rotates, PRD §8.4).
func NewWithToken(name, baseURL string, src model.TokenSource, traits Traits) model.Provider {
	return &provider{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   src,
		http:    stream.NewHTTPClient(),
		traits:  traits,
	}
}

func (p *provider) Name() string { return p.name }

// Variants implements model.Variants.
func (p *provider) Variants(id string) []string {
	if p.traits.Variants == nil {
		return nil
	}
	return p.traits.Variants(id)
}

func (p *provider) Open(modelID string) (model.Model, error) {
	return &client{p: p, id: modelID}, nil
}

// client is one opened model.
type client struct {
	p  *provider
	id string
}
