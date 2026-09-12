// Package openai implements model.Provider for the OpenAI Chat Completions
// API and its many compatible servers (PRD §8.2) using net/http only.
package openai

import (
	"net/http"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
)

// defaultMaxTokens is used when Request.MaxTokens is zero.
const defaultMaxTokens = 16000

type provider struct {
	name    string
	baseURL string
	apiKey  string
	token   model.TokenSource // when set, wins over apiKey and is called per request
	http    *http.Client
}

// New returns a provider named name that speaks Chat Completions at
// baseURL (e.g. "https://api.openai.com/v1"). apiKey may be empty for
// local servers such as Ollama or LM Studio.
func New(name, baseURL, apiKey string) model.Provider {
	return &provider{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{}, // no timeout: streams are long; ctx bounds them
	}
}

// NewWithToken is New with a bearer token fetched per request from src,
// for subscription logins whose access token rotates (PRD §8.4).
func NewWithToken(name, baseURL string, src model.TokenSource) model.Provider {
	return &provider{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   src,
		http:    &http.Client{},
	}
}

func (p *provider) Name() string { return p.name }

func (p *provider) Open(modelID string) (model.Model, error) {
	return &client{p: p, id: modelID}, nil
}

// client is one opened model.
type client struct {
	p  *provider
	id string
}

// usesMaxCompletionTokens reports whether the endpoint wants the newer
// "max_completion_tokens" field instead of "max_tokens".
func (p *provider) usesMaxCompletionTokens() bool {
	return strings.Contains(p.baseURL, "api.openai.com")
}
