package registry

import (
	"fmt"

	"github.com/nicodes/stavlos/internal/model"
)

// Check reports whether full can be resolved right now.
func (r *Registry) Check(full string) error {
	_, _, err := r.lookup(full)
	return err
}

// Variants lists the variant names a model offers (nil when it has none or
// the provider is unknown).
func (r *Registry) Variants(full string) []string {
	p, id, err := r.lookup(full)
	if err != nil {
		return nil
	}
	if v, ok := p.(model.Variants); ok {
		return v.Variants(id)
	}
	return nil
}

// Resolve opens (or returns the cached) model for full and its metadata.
// Subscription models carry no per-token price, so cost stays zero.
func (r *Registry) Resolve(full string) (model.Model, model.Info, error) {
	p, id, err := r.lookup(full)
	if err != nil {
		return nil, model.Info{}, err
	}
	var info model.Info
	if cat := r.cat.Load(); cat != nil {
		key := p.Name()
		if sub, ok := subscriptionByID(key); ok {
			key = sub.catalogID()
		}
		info, _ = cat.Model(key, id)
		if name, _, _ := model.Split(full); isSubscription(name) {
			info = subscriptionInfo(info)
		}
	}
	if c, ok := p.(model.Capable); ok {
		info.Capabilities = c.Capabilities(id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.opened[full]; ok {
		return m, info, nil
	}
	m, err := p.Open(id)
	if err != nil {
		return nil, model.Info{}, fmt.Errorf("model %q: %w", full, err)
	}
	if name, _, _ := model.Split(full); isSubscription(name) {
		m = polledModel{Model: m, after: func() { go r.pollAfterCall(name) }}
	}
	r.opened[full] = m
	return m, info, nil
}

func (r *Registry) lookup(full string) (model.Provider, string, error) {
	name, id, err := model.Split(full)
	if err != nil {
		return nil, "", err
	}
	r.mu.Lock()
	p, ok := r.providers[name]
	r.mu.Unlock()
	if ok {
		return p, id, nil
	}
	s, known := subscriptionByID(name)
	if !known {
		return nil, "", fmt.Errorf("unknown provider %q: Stavlos supports %s; run /provider", name, supported())
	}
	if _, ok := r.credential(name); !ok {
		return nil, "", fmt.Errorf("%s is not connected: run /provider to sign in with your %s subscription", s.name, s.name)
	}
	return r.adapter(s), id, nil
}

// adapter is a subscription's provider, built on first use and reused: its
// token source reads the store on every call, so a login or refresh needs
// no rebuild.
func (r *Registry) adapter(s subscription) model.Provider {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.subs[s.id]
	if !ok {
		p = s.open(r, r.tokenSource(s.id))
		r.subs[s.id] = p
	}
	return p
}

func isSubscription(id string) bool {
	_, ok := subscriptionByID(id)
	return ok
}

// subscriptionInfo is a model's metadata as a subscriber sees it: the
// subscription pays, so every price is zero.
func subscriptionInfo(info model.Info) model.Info {
	info.InputPrice, info.OutputPrice, info.CacheReadPrice, info.CacheWritePrice = 0, 0, 0, 0
	return info
}
