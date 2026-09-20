package registry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/auth"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/oauth"
)

// Flow returns the login flow for a provider.
func (r *Registry) Flow(provider string) (oauth.Flow, error) {
	f, ok := r.flows[provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q: Stavlos supports %s", provider, supported())
	}
	return f, nil
}

// SaveLogin stores tokens from a completed login.
func (r *Registry) SaveLogin(provider string, t oauth.Tokens) error {
	if r.store == nil {
		return errors.New("no credential store")
	}
	if err := r.store.Set(provider, credentialFor(provider, t)); err != nil {
		return err
	}
	r.Invalidate(provider)
	return nil
}

// Disconnect removes a stored login.
func (r *Registry) Disconnect(provider string) error {
	if r.store == nil {
		return errors.New("no credential store")
	}
	if err := r.store.Remove(provider); err != nil {
		return err
	}
	r.Invalidate(provider)
	return nil
}

// Invalidate drops cached models for a provider.
func (r *Registry) Invalidate(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.opened {
		if strings.HasPrefix(k, provider+"/") {
			delete(r.opened, k)
		}
	}
}

// credentialFor is how a completed login is stored: an OAuth pair, or the
// key a pasted-key subscription is bound to.
func credentialFor(provider string, t oauth.Tokens) auth.Credential {
	if s, ok := subscriptionByID(provider); ok && s.key {
		return auth.Credential{Type: auth.TypeAPIKey, Key: t.Access}
	}
	return auth.Credential{Type: auth.TypeOAuth, Access: t.Access, Refresh: t.Refresh, Expires: t.ExpiresAt.UnixMilli(), AccountID: t.AccountID, Email: t.Email}
}

// credential is a provider's usable login: an OAuth credential with a
// refresh token, or a stored key.
func (r *Registry) credential(provider string) (auth.Credential, bool) {
	if r.store == nil {
		return auth.Credential{}, false
	}
	c, ok := r.store.Get(provider)
	if !ok {
		return auth.Credential{}, false
	}
	if c.Type == auth.TypeAPIKey {
		return c, c.Key != ""
	}
	return c, c.Type == auth.TypeOAuth && c.Refresh != ""
}

// tokenSource returns a TokenSource that refreshes the stored access token
// when it is about to expire and persists the rotated pair.
func (r *Registry) tokenSource(provider string) model.TokenSource {
	return func(ctx context.Context) (model.Token, error) {
		c, ok := r.credential(provider)
		if !ok {
			return model.Token{}, fmt.Errorf("provider %q is not connected: run /provider to sign in", provider)
		}
		if c.Type == auth.TypeAPIKey {
			return model.Token{Access: c.Key}, nil // a key is not a session: nothing expires, nothing rotates
		}
		expSoon := c.Expires == 0 || time.UnixMilli(c.Expires).Before(time.Now().Add(refreshSkew)) || oauth.Expiring(c.Access, refreshSkew)
		if !expSoon {
			return model.Token{Access: c.Access, AccountID: c.AccountID}, nil
		}
		r.mu.Lock()
		mu := r.refreshing[provider]
		if mu == nil {
			mu = &sync.Mutex{}
			r.refreshing[provider] = mu
		}
		r.mu.Unlock()
		mu.Lock()
		defer mu.Unlock()
		// another caller may have refreshed while we waited
		if c2, ok := r.store.Get(provider); ok && c2.Access != c.Access && time.UnixMilli(c2.Expires).After(time.Now().Add(refreshSkew)) {
			return model.Token{Access: c2.Access, AccountID: c2.AccountID}, nil
		}
		f, err := r.Flow(provider)
		if err != nil {
			return model.Token{}, err
		}
		t, err := f.Refresh(ctx, c.Refresh)
		if err != nil {
			return model.Token{}, fmt.Errorf("%s session expired; run /provider to sign in again (%v)", provider, err)
		}
		t.AccountID = cmp.Or(t.AccountID, c.AccountID)
		t.Email = cmp.Or(t.Email, c.Email)
		if err := r.store.Set(provider, credentialFor(provider, t)); err != nil {
			return model.Token{}, err
		}
		return model.Token{Access: t.Access, AccountID: t.AccountID}, nil
	}
}
