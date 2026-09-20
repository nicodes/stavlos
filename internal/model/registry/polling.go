package registry

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"time"

	"github.com/nicodes/stavlos/internal/httpx"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/codex"
	"github.com/nicodes/stavlos/internal/model/quota"
)

// Asking the subscriptions that report their plan usage nowhere but at a
// usage endpoint (docs/plan-usage.md). What is learnt goes to the tracker
// (usage.go); what it costs here is a credential and a request.

// EnablePlanPolling turns the questions on. They are off until the daemon
// says so, so a test that points a subscription at its own server is never
// asked for a usage page it does not serve.
func (r *Registry) EnablePlanPolling() {
	r.usageTracker.mu.Lock()
	r.polls = true
	r.usageTracker.mu.Unlock()
}

// polledModel asks for its provider's plan usage after each call.
type polledModel struct {
	model.Model
	after func()
}

func (m polledModel) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (model.Response, error) {
	resp, err := m.Model.Complete(ctx, req, onDelta)
	m.after()
	return resp, err
}

func (r *Registry) pollAfterCall(provider string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.PollPlanUsage(ctx, provider, planPollEvery); err != nil {
		log.Printf("%v", err)
	}
}

// PollPlanUsage asks provider for its plan usage unless it was asked less
// than minAge ago, and keeps the answer like a reading a model call carried.
// It is a no-op for a provider that is not signed in or has nowhere to ask.
func (r *Registry) PollPlanUsage(ctx context.Context, provider string, minAge time.Duration) error {
	r.usageTracker.mu.Lock()
	on := r.polls
	r.usageTracker.mu.Unlock()
	if !on {
		return nil
	}
	r.mu.Lock()
	base := map[string]string{
		"openai": cmp.Or(r.codexEndpoint, codex.DefaultEndpoint), "xai": cmp.Or(r.xaiBaseURL, xaiBaseURL),
		"zai": cmp.Or(r.zaiBaseURL, zaiBaseURL), "kimi": cmp.Or(r.kimiBaseURL, kimiBaseURL),
	}[provider]
	r.mu.Unlock()
	endpoint := quota.URL(provider, base)
	if st, ok := r.Status(provider); endpoint == "" || !ok || !st.Connected {
		return nil
	}
	r.usageTracker.mu.Lock()
	if time.Since(r.polled[provider]) < minAge {
		r.usageTracker.mu.Unlock()
		return nil
	}
	if r.polled == nil {
		r.polled = map[string]time.Time{}
	}
	r.polled[provider] = time.Now() // a failure waits its turn too: no retry storm against a vendor
	r.usageTracker.mu.Unlock()
	tok, err := r.tokenSource(provider)(ctx)
	if err != nil {
		return fmt.Errorf("%s plan usage: %w", provider, err)
	}
	u, err := quota.Fetch(ctx, quotaHTTP, provider, endpoint, tok)
	if err != nil {
		return err
	}
	r.observeUsage(provider, u)
	return nil
}

// PollAllPlanUsage asks every signed-in subscription (PollPlanUsage).
func (r *Registry) PollAllPlanUsage(ctx context.Context, minAge time.Duration) {
	for _, s := range subscriptions {
		if err := r.PollPlanUsage(ctx, s.id, minAge); err != nil {
			log.Printf("%v", err)
		}
	}
}

var quotaHTTP = httpx.New(httpx.Options{Timeout: 15 * time.Second})
