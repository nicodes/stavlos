// Package codex implements model.Provider for the ChatGPT Codex backend:
// the OpenAI Responses API served at chatgpt.com and authenticated with a
// ChatGPT OAuth token, so Plus/Pro subscribers can use their subscription.
// It mirrors what the official Codex CLI (and opencode's codex plugin) send.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

// DefaultEndpoint is the Codex Responses endpoint.
const DefaultEndpoint = "https://chatgpt.com/backend-api/codex/responses"

// maxErrorBody bounds how much of an error response we quote.
const maxErrorBody = 2048

// userAgent identifies this client to the backend.
var userAgent = fmt.Sprintf("stavlos/0.1 (%s %s)", runtime.GOOS, runtime.GOARCH)

type provider struct {
	src      model.TokenSource
	endpoint string
	http     *http.Client
}

// New returns the "openai" provider backed by the Codex endpoint. src is
// called on every Complete so a refreshed token is picked up immediately.
func New(src model.TokenSource) model.Provider {
	return NewWithEndpoint(src, DefaultEndpoint)
}

// NewWithEndpoint is New with a custom URL (tests, proxies).
func NewWithEndpoint(src model.TokenSource, endpoint string) model.Provider {
	return &provider{
		src:      src,
		endpoint: endpoint,
		// No overall timeout: streams are long and ctx bounds them. Do
		// bound how long the backend may take to start answering.
		http: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 30 * time.Second,
		}},
	}
}

func (p *provider) Name() string { return "openai" }

func (p *provider) Open(modelID string) (model.Model, error) {
	return &client{p: p, id: modelID}, nil
}

// client is one opened model.
type client struct {
	p  *provider
	id string
}

// post sends the request body and returns the streaming response, or a
// descriptive error for non-2xx statuses.
func (p *provider) post(ctx context.Context, body []byte) (*http.Response, error) {
	if p.src == nil {
		return nil, fmt.Errorf("codex: no token source configured")
	}
	tok, err := p.src(ctx)
	if err != nil {
		return nil, fmt.Errorf("codex: token: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+tok.Access)
	if tok.AccountID != "" {
		httpReq.Header.Set("ChatGPT-Account-Id", tok.AccountID)
	}
	httpReq.Header.Set("originator", "stavlos")
	httpReq.Header.Set("User-Agent", userAgent)

	resp, err := p.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("codex: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("codex: unauthorized (token expired or revoked): run /provider to log in again")
		}
		return nil, fmt.Errorf("codex: status %d: %s", resp.StatusCode, errorText(raw))
	}
	return resp, nil
}

// errorText extracts the API's message from an error body, or quotes it.
func errorText(raw []byte) string {
	var env struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Error != nil && env.Error.Message != "" {
		if env.Error.Type != "" {
			return env.Error.Type + ": " + env.Error.Message
		}
		return env.Error.Message
	}
	return strings.TrimSpace(string(raw))
}
