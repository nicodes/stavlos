package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// Public client id of the official Grok CLI (same one opencode uses).
const (
	grokClientID  = "b1a00492-073a-47ea-816f-4c329264a828"
	grokDeviceURL = "https://auth.x.ai/oauth2/device/code"
	grokTokenURL  = "https://auth.x.ai/oauth2/token"
	grokScope     = "openid profile email offline_access grok-cli:access api:access"
	grokGrant     = "urn:ietf:params:oauth:grant-type:device_code"
)

// Grok is the "SuperGrok subscription" RFC 8628 device login.
type Grok struct {
	DeviceURL string
	TokenURL  string
}

func (g *Grok) Provider() string { return "xai" }
func (g *Grok) Label() string    { return "SuperGrok subscription" }

func (g *Grok) Start(ctx context.Context) (*Pending, error) {
	resp, err := postForm(ctx, g.DeviceURL, url.Values{"client_id": {grokClientID}, "scope": {grokScope}, "referrer": {"stavlos"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("grok: device code request failed (%d): %s", resp.StatusCode, readErr(resp))
	}
	var d struct {
		DeviceCode      string  `json:"device_code"`
		UserCode        string  `json:"user_code"`
		VerificationURI string  `json:"verification_uri"`
		Complete        string  `json:"verification_uri_complete"`
		ExpiresIn       float64 `json:"expires_in"`
		Interval        float64 `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	if d.DeviceCode == "" || d.UserCode == "" || d.VerificationURI == "" {
		return nil, fmt.Errorf("grok: device code response missing fields")
	}
	u := d.VerificationURI
	if d.Complete != "" {
		u = d.Complete
	}
	exp := 5 * time.Minute
	if d.ExpiresIn > 0 {
		exp = time.Duration(d.ExpiresIn) * time.Second
	}
	iv := 5 * time.Second
	if d.Interval >= 1 {
		iv = time.Duration(d.Interval) * time.Second
	}
	return &Pending{
		Provider: "xai", URL: u, Code: fmtCode(d.UserCode),
		Instructions: "Sign in with the X account that has your SuperGrok subscription.",
		ExpiresIn:    exp, interval: iv, deviceCode: d.DeviceCode,
	}, nil
}

func (g *Grok) Wait(ctx context.Context, p *Pending) (Tokens, error) {
	deadline := time.Now().Add(p.ExpiresIn)
	iv := p.interval
	for time.Now().Before(deadline) {
		resp, err := postForm(ctx, g.TokenURL, url.Values{"grant_type": {grokGrant}, "client_id": {grokClientID}, "device_code": {p.deviceCode}})
		if err != nil {
			return Tokens{}, err
		}
		var t tokenResponse
		_ = json.NewDecoder(resp.Body).Decode(&t)
		resp.Body.Close()
		if resp.StatusCode/100 == 2 && t.AccessToken != "" {
			return t.tokens(""), nil
		}
		switch t.Error {
		case "authorization_pending":
		case "slow_down":
			iv += 5 * time.Second
		case "access_denied", "authorization_denied":
			return Tokens{}, ErrDenied
		case "expired_token":
			return Tokens{}, ErrExpired
		default:
			msg := t.ErrorDesc
			if msg == "" {
				msg = t.Error
			}
			return Tokens{}, fmt.Errorf("grok: device token exchange failed (%d): %s", resp.StatusCode, msg)
		}
		if err := sleepCtx(ctx, iv+pollMargin); err != nil {
			return Tokens{}, err
		}
	}
	return Tokens{}, ErrExpired
}

func (g *Grok) Refresh(ctx context.Context, refresh string) (Tokens, error) {
	resp, err := postForm(ctx, g.TokenURL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {grokClientID}})
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Tokens{}, fmt.Errorf("grok: token refresh failed (%d): %s", resp.StatusCode, readErr(resp))
	}
	var t tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return Tokens{}, err
	}
	return t.tokens(refresh), nil
}
