package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Public client id of the official Codex CLI (same one opencode uses).
const (
	chatGPTClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatGPTIssuer   = "https://auth.openai.com"
)

// ChatGPT is the "ChatGPT Plus/Pro (headless)" device login.
type ChatGPT struct {
	Issuer string
}

func (c *ChatGPT) Provider() string { return "openai" }
func (c *ChatGPT) Label() string    { return "ChatGPT Plus/Pro subscription" }

func (c *ChatGPT) Start(ctx context.Context) (*Pending, error) {
	resp, err := postJSON(ctx, c.Issuer+"/api/accounts/deviceauth/usercode", map[string]string{"client_id": chatGPTClientID})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("chatgpt: device code request failed (%d): %s", resp.StatusCode, readErr(resp))
	}
	var d struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		Interval     json.RawMessage `json:"interval"` // string or number
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	if d.DeviceAuthID == "" || d.UserCode == "" {
		return nil, fmt.Errorf("chatgpt: device code response missing fields")
	}
	interval := 5
	if n, err := strconv.Atoi(trimQuotes(string(d.Interval))); err == nil && n > 0 {
		interval = n
	}
	return &Pending{
		Provider: "openai", URL: c.Issuer + "/codex/device", Code: fmtCode(d.UserCode),
		Instructions: "Sign in with the ChatGPT account that has your Plus or Pro subscription.",
		ExpiresIn:    15 * time.Minute, interval: time.Duration(interval) * time.Second, deviceAuthID: d.DeviceAuthID,
	}, nil
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func (c *ChatGPT) Wait(ctx context.Context, p *Pending) (Tokens, error) {
	deadline := time.Now().Add(p.ExpiresIn)
	for time.Now().Before(deadline) {
		resp, err := postJSON(ctx, c.Issuer+"/api/accounts/deviceauth/token", map[string]string{"device_auth_id": p.deviceAuthID, "user_code": p.Code})
		if err != nil {
			return Tokens{}, err
		}
		switch {
		case resp.StatusCode/100 == 2:
			var d struct {
				AuthorizationCode string `json:"authorization_code"`
				CodeVerifier      string `json:"code_verifier"`
			}
			err := json.NewDecoder(resp.Body).Decode(&d)
			resp.Body.Close()
			if err != nil {
				return Tokens{}, err
			}
			return c.exchange(ctx, d.AuthorizationCode, d.CodeVerifier)
		case resp.StatusCode == 403, resp.StatusCode == 404:
			resp.Body.Close() // still pending
		default:
			msg := readErr(resp)
			resp.Body.Close()
			return Tokens{}, fmt.Errorf("chatgpt: device authorization failed (%d): %s", resp.StatusCode, msg)
		}
		if err := sleepCtx(ctx, p.interval+pollMargin); err != nil {
			return Tokens{}, err
		}
	}
	return Tokens{}, ErrExpired
}

func (c *ChatGPT) exchange(ctx context.Context, code, verifier string) (Tokens, error) {
	resp, err := postForm(ctx, c.Issuer+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.Issuer + "/deviceauth/callback"},
		"client_id": {chatGPTClientID}, "code_verifier": {verifier},
	})
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Tokens{}, fmt.Errorf("chatgpt: token exchange failed (%d): %s", resp.StatusCode, readErr(resp))
	}
	var t tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return Tokens{}, err
	}
	return t.tokens(""), nil
}

func (c *ChatGPT) Refresh(ctx context.Context, refresh string) (Tokens, error) {
	resp, err := postForm(ctx, c.Issuer+"/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {chatGPTClientID},
	})
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Tokens{}, fmt.Errorf("chatgpt: token refresh failed (%d): %s", resp.StatusCode, readErr(resp))
	}
	var t tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return Tokens{}, err
	}
	return t.tokens(refresh), nil
}
