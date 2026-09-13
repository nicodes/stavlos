package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Public client id of the official Codex CLI (same one opencode uses).
const (
	chatGPTClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatGPTIssuer   = "https://auth.openai.com"
	chatGPTPort     = 1455 // the redirect URI registered for the Codex CLI
)

// ChatGPT signs in with a ChatGPT Plus/Pro account. Two methods, as in
// opencode and the Codex CLI:
//
//   - browser (default): open auth.openai.com in a browser with PKCE; OpenAI
//     redirects to http://localhost:1455/auth/callback on this machine.
//   - device (headless): show a URL and a code; requires "Device code
//     authorization for Codex" to be enabled in ChatGPT's Security settings.
type ChatGPT struct {
	Issuer string
	Port   int // 0 → 1455
}

func (c *ChatGPT) Provider() string { return "openai" }
func (c *ChatGPT) Label() string    { return "ChatGPT Plus/Pro subscription" }

func (c *ChatGPT) Methods() []Method {
	return []Method{
		{MethodBrowser, "ChatGPT Plus/Pro (browser)"},
		{MethodDevice, "ChatGPT Plus/Pro (headless: URL + code)"},
	}
}

func (c *ChatGPT) port() int {
	if c.Port > 0 {
		return c.Port
	}
	return chatGPTPort
}

func (c *ChatGPT) Start(ctx context.Context, method string) (*Pending, error) {
	switch method {
	case "", MethodBrowser:
		return c.startBrowser(ctx)
	case MethodDevice:
		return c.startDevice(ctx)
	}
	return nil, fmt.Errorf("chatgpt: unsupported login method %q", method)
}

func (c *ChatGPT) Wait(ctx context.Context, p *Pending) (Tokens, error) {
	if p.browser != nil {
		return c.waitBrowser(ctx, p)
	}
	return c.waitDevice(ctx, p)
}

// --- browser (PKCE + loopback redirect) ---

type browserLogin struct {
	srv      *http.Server
	verifier string
	state    string
	redirect string
	done     chan browserResult
	once     sync.Once
}

type browserResult struct {
	code string
	err  error
}

func randB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (c *ChatGPT) startBrowser(ctx context.Context) (*Pending, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", c.port()))
	if err != nil {
		return nil, fmt.Errorf("chatgpt: cannot listen on localhost:%d for the sign-in callback (%v); is another login running? The headless method does not need the port", c.port(), err)
	}
	verifier := randB64(64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randB64(32)
	redirect := fmt.Sprintf("http://localhost:%d/auth/callback", c.port())

	bl := &browserLogin{verifier: verifier, state: state, redirect: redirect, done: make(chan browserResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if e := q.Get("error"); e != "" {
			msg := q.Get("error_description")
			if msg == "" {
				msg = e
			}
			fmt.Fprint(w, page("Sign-in failed", msg))
			bl.finish(browserResult{err: errors.New(msg)})
			return
		}
		code := q.Get("code")
		if code == "" {
			w.WriteHeader(400)
			fmt.Fprint(w, page("Sign-in failed", "missing authorization code"))
			bl.finish(browserResult{err: errors.New("missing authorization code")})
			return
		}
		if q.Get("state") != bl.state {
			w.WriteHeader(400)
			fmt.Fprint(w, page("Sign-in failed", "state mismatch"))
			bl.finish(browserResult{err: errors.New("state mismatch (possible CSRF); start the login again")})
			return
		}
		fmt.Fprint(w, page("Signed in", "You can close this tab and return to Stavlos."))
		bl.finish(browserResult{code: code})
	})
	mux.HandleFunc("/cancel", func(w http.ResponseWriter, r *http.Request) {
		bl.finish(browserResult{err: errors.New("login cancelled")})
		fmt.Fprint(w, "cancelled")
	})
	bl.srv = &http.Server{Handler: mux}
	go bl.srv.Serve(ln)

	params := url.Values{
		"response_type":              {"code"},
		"client_id":                  {chatGPTClientID},
		"redirect_uri":               {redirect},
		"scope":                      {"openid profile email offline_access"},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"state":                      {state},
		"originator":                 {"stavlos"},
	}
	return &Pending{
		Provider: "openai", Method: MethodBrowser, URL: c.Issuer + "/oauth/authorize?" + params.Encode(),
		Instructions: "Complete the sign-in in your browser; it will redirect back to this machine.",
		ExpiresIn:    10 * time.Minute, browser: bl,
	}, nil
}

func (b *browserLogin) finish(r browserResult) {
	b.once.Do(func() { b.done <- r })
}

func (c *ChatGPT) waitBrowser(ctx context.Context, p *Pending) (Tokens, error) {
	bl := p.browser
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = bl.srv.Shutdown(sctx)
	}()
	timer := time.NewTimer(p.ExpiresIn)
	defer timer.Stop()
	select {
	case r := <-bl.done:
		if r.err != nil {
			return Tokens{}, r.err
		}
		return c.exchange(ctx, r.code, bl.redirect, bl.verifier)
	case <-timer.C:
		return Tokens{}, ErrExpired
	case <-ctx.Done():
		return Tokens{}, ctx.Err()
	}
}

func page(title, body string) string {
	return "<!doctype html><meta charset=utf-8><title>Stavlos · " + title + "</title>" +
		"<body style=\"font-family:system-ui;background:#111;color:#eee;display:grid;place-items:center;height:100vh;margin:0\">" +
		"<div style=\"text-align:center\"><h1 style=\"font-weight:600\">" + title + "</h1><p style=\"color:#aaa\">" + body + "</p></div>"
}

// --- device (headless) ---

func (c *ChatGPT) startDevice(ctx context.Context) (*Pending, error) {
	resp, err := postJSON(ctx, c.Issuer+"/api/accounts/deviceauth/usercode", map[string]string{"client_id": chatGPTClientID})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg := readErr(resp)
		if resp.StatusCode == 403 || resp.StatusCode == 400 {
			return nil, fmt.Errorf("chatgpt: %s (ChatGPT → Settings → Security → enable \"Device code authorization for Codex\", or use the browser method)", msg)
		}
		return nil, fmt.Errorf("chatgpt: device code request failed (%d): %s", resp.StatusCode, msg)
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
		Provider: "openai", Method: MethodDevice, URL: c.Issuer + "/codex/device", Code: fmtCode(d.UserCode),
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

func (c *ChatGPT) waitDevice(ctx context.Context, p *Pending) (Tokens, error) {
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
			return c.exchange(ctx, d.AuthorizationCode, c.Issuer+"/deviceauth/callback", d.CodeVerifier)
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

// --- shared ---

func (c *ChatGPT) exchange(ctx context.Context, code, redirect, verifier string) (Tokens, error) {
	resp, err := postForm(ctx, c.Issuer+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
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
