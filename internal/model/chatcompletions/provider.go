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

type provider struct {
	name    string
	baseURL string
	token   model.TokenSource // called per request, so a rotated token is picked up at once
	http    *http.Client
}

// NewWithToken returns a provider named name that speaks Chat Completions
// at baseURL with a bearer token fetched per request from src (a
// subscription login whose access token rotates, PRD §8.4).
func NewWithToken(name, baseURL string, src model.TokenSource) model.Provider {
	return &provider{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   src,
		http:    stream.NewHTTPClient(),
	}
}

func (p *provider) Name() string { return p.name }

// xai reports whether this is xAI's Grok, whose prompt cache wants the
// conversation id header and the earlier reasoning replayed.
func (p *provider) xai() bool { return p.name == "xai" }

// replaysReasoning reports whether an assistant turn's thinking goes back
// to the provider as reasoning_content. Grok, GLM and Kimi all read it
// (models.dev marks them interleaved on that field); sending it to a
// provider that does not would be a stray field in every request.
func (p *provider) replaysReasoning() bool {
	switch p.name {
	case "xai", "zai", "kimi":
		return true
	}
	return false
}

// Variants implements model.Variants. Grok's reasoning models (the "mini"
// ones) take reasoning_effort low|high; the others reject the field.
func (p *provider) Variants(id string) []string {
	if p.name == "xai" && strings.Contains(id, "mini") {
		return []string{"low", "high"}
	}
	return nil
}

func (p *provider) Open(modelID string) (model.Model, error) {
	return &client{p: p, id: modelID}, nil
}

// client is one opened model.
type client struct {
	p  *provider
	id string
}
