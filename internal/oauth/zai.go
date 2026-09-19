package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Z.ai's GLM Coding Plan has no OAuth: the subscription is bound to an API
// key the user creates in the console, and every request carries it as a
// bearer token. It is presented as a login all the same, so it reaches the
// provider list, the sign-in dialog and the credential store by the same
// path as the others — the one difference being that its method is pasting
// a key rather than opening a URL, and that there is nothing to refresh.
//
// The plan is served over the OpenAI Chat Completions protocol at
// api.z.ai/api/coding/paas/v4 (docs.z.ai/devpack). That endpoint is the
// coding plan's; the pay-as-you-go API lives at api.z.ai/api/paas/v4 and
// bills credits instead of the subscription, so the two must not be
// confused.

// zaiKeysURL is where a key is created.
const zaiKeysURL = "https://z.ai/manage-apikey/apikey-list"

// zaiKeyTTL bounds how long a pasted-key login stays open.
const zaiKeyTTL = 30 * time.Minute

// ZAI is the GLM Coding Plan login.
type ZAI struct{}

func (z *ZAI) Provider() string { return "zai" }
func (z *ZAI) Label() string    { return "Z.ai GLM Coding Plan" }

func (z *ZAI) Methods() []Method {
	return []Method{{ID: MethodAPIKey, Label: "Z.ai GLM Coding Plan (API key)"}}
}

func (z *ZAI) Start(_ context.Context, method string) (*Pending, error) {
	if method != "" && method != MethodAPIKey {
		return nil, fmt.Errorf("z.ai has no %q sign-in: it is an API key from the console", method)
	}
	return &Pending{
		Provider:     z.Provider(),
		Method:       MethodAPIKey,
		URL:          zaiKeysURL,
		Instructions: "Create a key for your GLM Coding Plan and paste it here. It is stored in the auth file, like any other login.",
		ExpiresIn:    zaiKeyTTL,
		key:          make(chan string, 1),
	}, nil
}

// Wait blocks until a client delivers the key, the login expires, or ctx
// ends. The key becomes the access token; it does not expire, so nothing
// refreshes it.
func (z *ZAI) Wait(ctx context.Context, p *Pending) (Tokens, error) {
	if p == nil || p.key == nil {
		return Tokens{}, errors.New("no z.ai login in progress")
	}
	select {
	case key := <-p.key:
		return Tokens{Access: key}, nil
	case <-time.After(p.ExpiresIn):
		return Tokens{}, ErrExpired
	case <-ctx.Done():
		return Tokens{}, ctx.Err()
	}
}

// Refresh never runs: an API key is not a session. The registry knows not
// to call it, and this says so if anything ever does.
func (z *ZAI) Refresh(context.Context, string) (Tokens, error) {
	return Tokens{}, errors.New("a z.ai API key does not expire and cannot be refreshed; replace it in the console and sign in again")
}
