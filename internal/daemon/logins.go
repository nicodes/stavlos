package daemon

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/oauth"
	"github.com/nicodes/stavlos/internal/protocol"
)

// Subscription sign-ins in progress (PRD §8.4).

type pendingLogin struct {
	provider string
	pending  *oauth.Pending
	started  time.Time
}

// LoginStart begins a device-code login and returns what to show the user.
func (d *Daemon) LoginStart(ctx context.Context, provider, method string) (protocol.LoginStartResult, error) {
	f, err := d.Registry.Flow(provider)
	if err != nil {
		return protocol.LoginStartResult{}, err
	}
	p, err := f.Start(context.Background(), method) // outlives the request; Wait owns cancellation
	if err != nil {
		return protocol.LoginStartResult{}, err
	}
	id := agent.NewID("l")
	d.loginMu.Lock()
	for k, v := range d.logins { // drop stale ones, releasing their loopback port
		if time.Since(v.started) > 30*time.Minute {
			v.pending.Close()
			delete(d.logins, k)
		}
	}
	d.logins[id] = &pendingLogin{provider: provider, pending: p, started: time.Now()}
	d.loginMu.Unlock()
	return protocol.LoginStartResult{ID: id, Provider: provider, Method: p.Method, URL: p.URL, Code: p.Code, Instructions: p.Instructions, ExpiresIn: int(p.ExpiresIn.Seconds())}, nil
}

// LoginKey hands a pasted key to a login waiting for one, which is what
// completes an "apikey" sign-in: the waiting LoginWait then stores it like
// any other credential.
func (d *Daemon) LoginKey(id, key string) error {
	d.loginMu.Lock()
	pl, ok := d.logins[id]
	d.loginMu.Unlock()
	if !ok {
		return fmt.Errorf("no login in progress with id %q", id)
	}
	return pl.pending.Deliver(key)
}

// LoginWait polls until the login completes, then stores the tokens.
func (d *Daemon) LoginWait(ctx context.Context, id string) (registry.Status, error) {
	d.loginMu.Lock()
	pl, ok := d.logins[id]
	d.loginMu.Unlock()
	if !ok {
		return registry.Status{}, fmt.Errorf("no login in progress with id %q", id)
	}
	f, err := d.Registry.Flow(pl.provider)
	if err != nil {
		return registry.Status{}, err
	}
	tok, err := f.Wait(ctx, pl.pending)
	if err != nil {
		if ctx.Err() == nil { // terminal failure: forget it
			pl.pending.Close()
			d.loginMu.Lock()
			delete(d.logins, id)
			d.loginMu.Unlock()
		}
		return registry.Status{}, err
	}
	d.loginMu.Lock()
	delete(d.logins, id)
	d.loginMu.Unlock()
	if err := d.Registry.SaveLogin(pl.provider, tok); err != nil {
		return registry.Status{}, err
	}
	log.Printf("%s login completed (%s)", pl.provider, tok.Email)
	st, _ := d.Registry.Status(pl.provider)
	return st, nil
}
