package oauth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Some coding plans issue no OAuth credential: the subscription is bound to
// a key the user creates in the vendor's console, and every request carries
// it as a bearer token. They are presented as logins all the same, so they
// reach the provider list, the sign-in dialog and the credential store by
// the same path as the others — the one difference being that the method is
// pasting a key rather than opening a URL, and that there is nothing to
// refresh.
//
// Each plan's console is not its vendor's general API console: a key made
// for the pay-as-you-go platform bills credits, and usually cannot reach
// the plan's endpoint at all. The instructions say so, because the two
// pages look alike.

// keyTTL bounds how long a pasted-key login stays open.
const keyTTL = 30 * time.Minute

// KeyFlow is a login whose one method is pasting a key.
type KeyFlow struct {
	provider, label string
	console         string // where the key is created
	note            string // what to create it for, and where not to
}

// ZAI is the Z.ai GLM Coding Plan login.
func ZAI() *KeyFlow {
	return &KeyFlow{
		provider: "zai", label: "Z.ai GLM Coding Plan",
		console: "https://z.ai/manage-apikey/apikey-list",
		note:    "Create a key for your GLM Coding Plan and paste it here. It is stored in the auth file, like any other login.",
	}
}

// Kimi is the Kimi For Coding login.
func Kimi() *KeyFlow {
	return &KeyFlow{
		provider: "kimi", label: "Kimi For Coding",
		console: "https://www.kimi.ai/code",
		note:    "Create a key in the Kimi Code console — a key from the Kimi API platform is pay-as-you-go and will not reach the plan. It is stored in the auth file, like any other login.",
	}
}

func (k *KeyFlow) Provider() string { return k.provider }
func (k *KeyFlow) Label() string    { return k.label }

func (k *KeyFlow) Methods() []Method {
	return []Method{{ID: MethodAPIKey, Label: k.label + " (API key)"}}
}

func (k *KeyFlow) Start(_ context.Context, method string) (*Pending, error) {
	if method != "" && method != MethodAPIKey {
		return nil, fmt.Errorf("%s has no %q sign-in: it is a key from the console", k.provider, method)
	}
	return &Pending{
		Provider:     k.provider,
		Method:       MethodAPIKey,
		URL:          k.console,
		Instructions: k.note,
		ExpiresIn:    keyTTL,
		key:          make(chan string, 1),
	}, nil
}

// Wait blocks until a client delivers the key, the login expires, or ctx
// ends. The key becomes the access token; it does not expire, so nothing
// refreshes it.
func (k *KeyFlow) Wait(ctx context.Context, p *Pending) (Tokens, error) {
	if p == nil || p.key == nil {
		return Tokens{}, fmt.Errorf("no %s login in progress", k.provider)
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
func (k *KeyFlow) Refresh(context.Context, string) (Tokens, error) {
	return Tokens{}, errors.New("an API key does not expire and cannot be refreshed; replace it in the console and sign in again")
}
