// Package codex implements model.Provider for the ChatGPT Codex backend:
// the OpenAI Responses API served at chatgpt.com and authenticated with a
// ChatGPT OAuth token, so Plus/Pro subscribers can use their subscription.
// It mirrors what the official Codex CLI (and opencode's codex plugin) send.
package codex

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/stream"
)

// DefaultEndpoint is the Codex Responses endpoint.
const DefaultEndpoint = "https://chatgpt.com/backend-api/codex/responses"

// userAgent identifies this client to the backend.
var userAgent = fmt.Sprintf("stavlos/0.1 (%s %s)", runtime.GOOS, runtime.GOARCH)

type provider struct {
	src      model.TokenSource
	endpoint string
	http     *http.Client
	onUsage  func(model.PlanUsage) // nil for none
}

// NewWithEndpoint is New with a custom URL (tests, proxies).
func NewWithEndpoint(src model.TokenSource, endpoint string) model.Provider {
	return NewWithUsage(src, endpoint, nil)
}

// NewWithUsage is NewWithEndpoint whose calls report the plan's usage
// windows to onUsage as responses carry them (passively: no request of its
// own; docs/plan-usage.md).
func NewWithUsage(src model.TokenSource, endpoint string, onUsage func(model.PlanUsage)) model.Provider {
	return &provider{src: src, endpoint: endpoint, http: stream.NewHTTPClient(), onUsage: onUsage}
}

// observe reports the usage a response's headers carry.
func (p *provider) observe(h http.Header) {
	if p.onUsage == nil {
		return
	}
	if u, ok := usageFromHeaders(h, time.Now()); ok {
		p.onUsage(u)
	}
}

func (p *provider) Name() string { return "openai" }

// codexVariants are the reasoning efforts the Codex backend accepts; the
// default (no variant) is medium.
var codexVariants = []string{"low", "medium", "high", "xhigh"}

// Variants implements model.Variants: every ChatGPT model reasons, so all
// get the same list.
func (p *provider) Variants(string) []string { return append([]string(nil), codexVariants...) }

// Capabilities implements model.Capable: the backend rejects
// max_output_tokens.
func (p *provider) Capabilities(string) model.Capabilities {
	return model.Capabilities{IgnoresMaxTokens: true}
}

func (p *provider) Open(modelID string) (model.Model, error) {
	return &client{p: p, id: modelID}, nil
}

// client is one opened model.
type client struct {
	p  *provider
	id string
}

// header sets the ChatGPT credential and the headers the backend expects
// from a Codex client.
func (p *provider) header(ctx context.Context, h http.Header) error {
	if p.src == nil {
		return fmt.Errorf("codex: no token source configured")
	}
	tok, err := p.src(ctx)
	if err != nil {
		return fmt.Errorf("codex: token: %w", err)
	}
	h.Set("Authorization", "Bearer "+tok.Access)
	if tok.AccountID != "" {
		h.Set("ChatGPT-Account-Id", tok.AccountID)
	}
	h.Set("originator", "stavlos")
	h.Set("User-Agent", userAgent)
	return nil
}

// onStatus turns a rejected token into instructions.
func onStatus(code int, _ []byte) error {
	if code == http.StatusUnauthorized {
		return fmt.Errorf("codex: unauthorized (token expired or revoked): run /providers to log in again")
	}
	return nil
}
