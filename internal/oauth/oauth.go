// Package oauth implements the subscription logins Stavlos supports:
// ChatGPT Plus/Pro (via the Codex sign-in), SuperGrok (via the Grok CLI
// sign-in), the Z.ai GLM Coding Plan and Kimi For Coding. The first two are device-code
// flows using the official CLIs' public client ids, the same mechanism
// opencode uses: the user opens a URL on any device, enters a short code,
// and the daemon polls for the tokens. Z.ai issues no OAuth credential at
// all, so its plan is presented as a login of the same shape whose one
// method is pasting a key (zai.go).
package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nicodes/stavlos/internal/httpx"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

// UserAgent is sent on every auth request.
var UserAgent = "stavlos/0.1 (" + runtime.GOOS + " " + runtime.GOARCH + ")"

// Tokens is the result of a login or refresh.
type Tokens struct {
	Access    string
	Refresh   string
	IDToken   string
	ExpiresAt time.Time
	AccountID string // ChatGPT account id (openai only)
	Email     string
}

// Method ids.
const (
	MethodBrowser = "browser" // authorize in a browser; callback to localhost
	MethodDevice  = "device"  // show a URL and a code; poll (headless)
	MethodAPIKey  = "apikey"  // the user pastes a key from the provider's console
)

// Method is one way to sign in to a provider.
type Method struct {
	ID    string
	Label string
}

// Pending is a login waiting for the user.
type Pending struct {
	Provider     string
	Method       string
	URL          string // where the user goes
	Code         string // what they type there (device method only)
	Instructions string
	ExpiresIn    time.Duration
	interval     time.Duration
	deviceCode   string // xai
	deviceAuthID string // openai device
	browser      *browserLogin
	key          chan string // apikey: the key the client delivers
}

// Deliver hands a pasted key to a waiting apikey login. It is how the
// apikey method completes: there is nothing to poll, so Wait blocks here
// until a client calls provider.login.key.
func (p *Pending) Deliver(key string) error {
	if p == nil || p.key == nil {
		return errors.New("this login does not take a key")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("the key is empty")
	}
	select {
	case p.key <- key:
		return nil
	default:
		return errors.New("this login already has a key")
	}
}

// Close abandons a pending login: the browser method's loopback server is
// shut down and its port released. Wait releases it too; Close is for a
// login nobody waited on.
func (p *Pending) Close() {
	if p != nil && p.browser != nil {
		p.browser.finish(browserResult{err: errors.New("login abandoned")})
		p.browser.shutdown()
	}
}

// Flow is one provider's login and refresh logic.
type Flow interface {
	// Provider id: "openai" or "xai".
	Provider() string
	// Label is the human-facing subscription name.
	Label() string
	// Methods lists sign-in methods, default first.
	Methods() []Method
	// Start begins a login with the given method ("" = default).
	Start(ctx context.Context, method string) (*Pending, error)
	// Wait blocks until the user completes the login, it expires, or ctx ends.
	Wait(ctx context.Context, p *Pending) (Tokens, error)
	// Refresh exchanges a refresh token for a new pair.
	Refresh(ctx context.Context, refresh string) (Tokens, error)
}

// Flows returns the supported flows keyed by provider id. The env vars
// STAVLOS_OAUTH_OPENAI_ISSUER and STAVLOS_OAUTH_XAI_BASE point the flows at
// a stand-in server for development; they are never set in normal use.
func Flows() map[string]Flow {
	c := &ChatGPT{Issuer: chatGPTIssuer}
	if v := os.Getenv("STAVLOS_OAUTH_OPENAI_ISSUER"); v != "" {
		c.Issuer = strings.TrimRight(v, "/")
		fmt.Fprintf(os.Stderr, "stavlos: STAVLOS_OAUTH_OPENAI_ISSUER is set: ChatGPT sign-ins and tokens go to %s\n", c.Issuer)
	}
	g := &Grok{DeviceURL: grokDeviceURL, TokenURL: grokTokenURL}
	if v := os.Getenv("STAVLOS_OAUTH_XAI_BASE"); v != "" {
		g.DeviceURL, g.TokenURL = strings.TrimRight(v, "/")+"/device/code", strings.TrimRight(v, "/")+"/token"
		fmt.Fprintf(os.Stderr, "stavlos: STAVLOS_OAUTH_XAI_BASE is set: Grok sign-ins and tokens go to %s\n", strings.TrimRight(v, "/"))
	}
	return map[string]Flow{"openai": c, "xai": g, "zai": ZAI(), "kimi": Kimi()}
}

// ErrDenied is returned when the user rejects the login.
var ErrDenied = errors.New("login was denied")

// ErrExpired is returned when the code expired before the user finished.
var ErrExpired = errors.New("login code expired; start again")

var client = httpx.New(httpx.Options{Timeout: 30 * time.Second})

// pollMargin is added to every poll interval (RFC 8628 §3.5 safety margin).
var pollMargin = 3 * time.Second

func postForm(ctx context.Context, u string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	return client.Do(req)
}

func postJSON(ctx context.Context, u string, body any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	return client.Do(req)
}

func readErr(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return strings.TrimSpace(string(b))
}

// tokenResponse is the OAuth token endpoint shape shared by both providers.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (t tokenResponse) tokens(fallbackRefresh string) Tokens {
	exp := t.ExpiresIn
	if exp <= 0 {
		exp = 3600
	}
	out := Tokens{Access: t.AccessToken, Refresh: t.RefreshToken, IDToken: t.IDToken, ExpiresAt: time.Now().Add(time.Duration(exp) * time.Second)}
	if out.Refresh == "" {
		out.Refresh = fallbackRefresh
	}
	for _, tok := range []string{t.IDToken, t.AccessToken} {
		c := Claims(tok)
		if out.AccountID == "" {
			out.AccountID = c.AccountID()
		}
		if out.Email == "" {
			out.Email = c.Email
		}
	}
	return out
}

// JWTClaims are the claims we read (unverified; used for display and the
// account id header, never for trust decisions).
type JWTClaims struct {
	Exp           int64  `json:"exp"`
	Email         string `json:"email"`
	ChatGPTAcct   string `json:"chatgpt_account_id"`
	Organizations []struct {
		ID string `json:"id"`
	} `json:"organizations"`
	OpenAIAuth struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
}

// AccountID picks the ChatGPT account id from the claims.
func (c JWTClaims) AccountID() string {
	switch {
	case c.ChatGPTAcct != "":
		return c.ChatGPTAcct
	case c.OpenAIAuth.ChatGPTAccountID != "":
		return c.OpenAIAuth.ChatGPTAccountID
	case len(c.Organizations) > 0:
		return c.Organizations[0].ID
	}
	return ""
}

// Claims decodes a JWT payload without verifying it. Opaque tokens yield
// zero claims.
func Claims(token string) JWTClaims {
	var c JWTClaims
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return c
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		b, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return c
		}
	}
	_ = json.Unmarshal(b, &c)
	return c
}

// Expiring reports whether a JWT's exp claim is within skew of now.
func Expiring(token string, skew time.Duration) bool {
	c := Claims(token)
	if c.Exp == 0 {
		return false
	}
	return time.Unix(c.Exp, 0).Before(time.Now().Add(skew))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func fmtCode(c string) string { return strings.ToUpper(strings.TrimSpace(c)) }
