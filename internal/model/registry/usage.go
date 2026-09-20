package registry

import (
	"cmp"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

// usageTracker is what each subscription has used of its plan, as last
// observed, and until when one is known to be at its limit. It is the one
// answer to "is this provider limited, and until when"; the registry embeds
// it, and nothing in it touches the registry's own lock, so a reading that
// arrives with a model call never waits for a catalogue lookup or a token
// refresh.
type usageTracker struct {
	mu      sync.Mutex
	usage   map[string]model.PlanUsage               // provider → latest observed plan usage
	onUsage func(provider string, u model.PlanUsage) // nil for none
	polls   bool                                     // ask the providers that report usage nowhere else (EnablePlanPolling)
	polled  map[string]time.Time                     // provider → when it was last asked, answered or not
}

// observeUsage keeps provider's latest plan usage, as a call's response
// reported it, and hands it to the usage hook.
func (t *usageTracker) observeUsage(provider string, u model.PlanUsage) {
	t.mu.Lock()
	if t.usage == nil {
		t.usage = map[string]model.PlanUsage{}
	}
	if old, ok := t.usage[provider]; ok {
		if old.Observed.After(u.Observed) {
			t.mu.Unlock()
			return // an older response finished last
		}
		u.Plan = cmp.Or(u.Plan, old.Plan) // a call's headers carry no plan name; the usage endpoint does
		if u.LimitedUntil.IsZero() {
			u.LimitedUntil = old.LimitedUntil // a refusal outlasts the readings that follow it
		}
	}
	t.usage[provider] = u
	hook := t.onUsage
	t.mu.Unlock()
	if hook != nil {
		hook(provider, u)
	}
}

// MarkLimited records that provider refused a model call for a limit, so
// the agents choosing a model pass it over until then. The reading itself is
// left as it was.
func (t *usageTracker) MarkLimited(provider string, until time.Time) {
	t.mu.Lock()
	if t.usage == nil {
		t.usage = map[string]model.PlanUsage{}
	}
	u := t.usage[provider]
	if until.After(u.LimitedUntil) {
		u.LimitedUntil = until
		t.usage[provider] = u
	}
	t.mu.Unlock()
}

// SeedPlanUsage restores a provider's usage as last observed (by an earlier
// daemon), unless a newer reading is already kept.
func (t *usageTracker) SeedPlanUsage(provider string, u model.PlanUsage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.usage == nil {
		t.usage = map[string]model.PlanUsage{}
	}
	if old, ok := t.usage[provider]; !ok || u.Observed.After(old.Observed) {
		t.usage[provider] = u
	}
}

// OnPlanUsage sets the hook every newly observed plan usage is handed to
// (the daemon keeps it on disk); it runs on the calling agent's goroutine.
func (t *usageTracker) OnPlanUsage(hook func(provider string, u model.PlanUsage)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onUsage = hook
}

// PlanUsage is every provider's latest observed plan usage, keyed by
// provider id.
func (t *usageTracker) PlanUsage() map[string]model.PlanUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]model.PlanUsage, len(t.usage))
	for k, v := range t.usage {
		out[k] = v
	}
	return out
}
