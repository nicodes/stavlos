// Package registry serves the providers Stavlos supports, each through the
// user's own subscription rather than a platform API key (PRD §8.4):
//
//   - openai: ChatGPT Plus/Pro, via the Codex sign-in and backend
//   - xai:    SuperGrok, via the Grok CLI sign-in and api.x.ai
//   - zai:    the GLM Coding Plan, via a key from the Z.ai console
//   - kimi:   Kimi For Coding, via a key from the Kimi Code console
//
// Credentials live in the auth store; access tokens are refreshed on
// demand. The coding plans issue no OAuth credential, so each is bound to
// a key the user pastes once: still a subscription, still one credential
// in the same store, with nothing to refresh.
package registry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nicodes/stavlos/internal/auth"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/chatcompletions"
	"github.com/nicodes/stavlos/internal/model/codex"
	"github.com/nicodes/stavlos/internal/model/stream"
	"github.com/nicodes/stavlos/internal/modelsdev"
	"github.com/nicodes/stavlos/internal/oauth"
)

// Kind classifies how a provider is served.
type Kind string

const (
	KindSubscription Kind = "subscription"
	KindPlugin       Kind = "plugin" // explicitly registered model.Provider (tests, future plugins)
)

// Status is the daemon-facing view of one provider.
type Status struct {
	ID        string
	Name      string
	Kind      Kind
	Label     string // login method label
	Connected bool
	Account   string // email or account id
	Priority  int
	Models    int
	Methods   []oauth.Method
}

// refreshSkew: refresh an access token this long before it expires.
const refreshSkew = 2 * time.Minute

// xaiBaseURL is the Grok API.
const xaiBaseURL = "https://api.x.ai/v1"

// zaiBaseURL is the GLM Coding Plan's OpenAI Chat Completions endpoint
// (docs.z.ai/devpack). The plan's endpoint, not api.z.ai/api/paas/v4,
// which bills pay-as-you-go credits instead of the subscription.
const zaiBaseURL = "https://api.z.ai/api/coding/paas/v4"

// kimiBaseURL is Kimi For Coding's OpenAI Chat Completions endpoint. The
// plan's endpoint, not api.moonshot.ai/v1, which is the pay-as-you-go
// platform. This is the global one; the China site serves the same plan at
// api.kimi.com/coding/v1.
const kimiBaseURL = "https://api.kimi.ai/coding/v1"

// subscription is everything provider-specific about one supported
// subscription; the rest of the registry is generic over this table.
type subscription struct {
	id, name string
	priority int
	// catalog is the models.dev provider whose models this subscription
	// serves, when it is not the provider id: a plan is often its own entry
	// there, listing what the plan includes rather than everything the
	// vendor sells.
	catalog string
	// key marks a subscription bound to a pasted key rather than an OAuth
	// login: there is no refresh, and the credential holds a key.
	key bool
	// allow keeps the catalog models the subscription actually serves. A
	// plan whose catalog entry is already its own list needs none.
	allow func(model string) bool
	// open builds the adapter; src reads (and refreshes) the stored login.
	open func(r *Registry, src model.TokenSource) model.Provider
}

// supported names the subscriptions for an error message, from the table:
// "openai (ChatGPT), xai (Grok), …". It used to be written out by hand and
// went on saying two after there were four.
func supported() string {
	names := make([]string, len(subscriptions))
	for i, s := range subscriptions {
		names[i] = s.id + " (" + s.name + ")"
	}
	return strings.Join(names, ", ")
}

// catalogID is the models.dev provider to read this subscription's models
// from.
func (s subscription) catalogID() string { return cmp.Or(s.catalog, s.id) }

var subscriptions = []subscription{
	{
		id: "openai", name: "ChatGPT", priority: 0,
		allow: chatGPTAllowed,
		open: func(r *Registry, src model.TokenSource) model.Provider {
			return codex.NewWithUsage(src, cmp.Or(r.codexEndpoint, codex.DefaultEndpoint), func(u model.PlanUsage) { r.observeUsage("openai", u) })
		},
	},
	{
		id: "xai", name: "Grok", priority: 1,
		allow: grokAllowed,
		open: func(r *Registry, src model.TokenSource) model.Provider {
			return chatcompletions.NewWithToken("xai", cmp.Or(r.xaiBaseURL, xaiBaseURL), src, chatcompletions.Traits{
				CacheHeader:      "x-grok-conv-id", // docs.x.ai prompt caching
				ReplaysReasoning: true,
				// Grok's reasoning models (the "mini" ones) take low|high; the others reject the field
				Variants: func(id string) []string {
					if strings.Contains(id, "mini") {
						return []string{"low", "high"}
					}
					return nil
				},
				// a SuperGrok account out of credits is refused with a 403
				IsLimit: func(status int, body []byte) bool {
					return status == http.StatusForbidden && stream.HasAny(body, "spending-limit", "out of credits") || stream.PlanLimit(status, body)
				},
			})
		},
	},
	{
		id: "zai", name: "Z.ai Coding Plan", priority: 2, catalog: "zai-coding-plan", key: true,
		allow: zaiAllowed,
		open: func(r *Registry, src model.TokenSource) model.Provider {
			// GLM caches a matching prefix with no hint, and documents none
			return chatcompletions.NewWithToken("zai", cmp.Or(r.zaiBaseURL, zaiBaseURL), src, chatcompletions.Traits{ReplaysReasoning: true})
		},
	},
	{
		id: "kimi", name: "Kimi For Coding", priority: 3, catalog: "kimi-code-plan-global", key: true,
		open: func(r *Registry, src model.TokenSource) model.Provider {
			// prompt_cache_key as kimi-cli sends its session id
			return chatcompletions.NewWithToken("kimi", cmp.Or(r.kimiBaseURL, kimiBaseURL), src, chatcompletions.Traits{CacheKeyField: true, ReplaysReasoning: true})
		},
	},
}

// catalogIDs are the models.dev providers the catalog is kept for.
func catalogIDs() []string {
	ids := make([]string, len(subscriptions))
	for i, s := range subscriptions {
		ids[i] = s.catalogID()
	}
	return ids
}

func subscriptionByID(id string) (subscription, bool) {
	for _, s := range subscriptions {
		if s.id == id {
			return s, true
		}
	}
	return subscription{}, false
}

// Registry is the provider map. It is safe for concurrent use.
type Registry struct {
	cat    atomic.Pointer[modelsdev.Catalog] // swapped whole by a background refresh
	counts atomic.Pointer[modelCounts]       // per-subscription model counts for cat
	store  *auth.Store
	flows  map[string]oauth.Flow

	// endpoints override the real services (tests); read under mu.
	codexEndpoint string
	xaiBaseURL    string
	zaiBaseURL    string
	kimiBaseURL   string

	mu         sync.Mutex
	providers  map[string]model.Provider // explicit registrations
	subs       map[string]model.Provider // subscription adapters, built once (their token source reads the store per call)
	opened     map[string]model.Model    // keyed by full "provider/id"
	refreshing map[string]*sync.Mutex    // single-flight refresh per provider

	usageTracker // what the subscriptions have used, under its own lock (usage.go)
}

// New returns a registry backed by cat for model metadata (may be nil).
func New(cat *modelsdev.Catalog) *Registry {
	r := &Registry{flows: oauth.Flows(), providers: map[string]model.Provider{}, subs: map[string]model.Provider{}, opened: map[string]model.Model{}, refreshing: map[string]*sync.Mutex{}}
	r.cat.Store(cat)
	return r
}

// WithStore attaches the credential store.
func (r *Registry) WithStore(s *auth.Store) *Registry { r.store = s; return r }

// WithFlows replaces the login flows (tests).
func (r *Registry) WithFlows(f map[string]oauth.Flow) *Registry { r.flows = f; return r }

// WithEndpoints overrides service URLs (tests). Adapters built for the old
// endpoints are dropped.
func (r *Registry) WithEndpoints(codexEndpoint, xaiBaseURL, zaiBaseURL, kimiBaseURL string) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codexEndpoint, r.xaiBaseURL, r.zaiBaseURL, r.kimiBaseURL = codexEndpoint, xaiBaseURL, zaiBaseURL, kimiBaseURL
	r.subs = map[string]model.Provider{}
	r.opened = map[string]model.Model{}
	return r
}

// Catalog returns the metadata catalog (may be nil).
func (r *Registry) Catalog() *modelsdev.Catalog { return r.cat.Load() }

// SetCatalog replaces the metadata catalog (a background refresh).
func (r *Registry) SetCatalog(c *modelsdev.Catalog) {
	if c != nil {
		r.cat.Store(c)
	}
}

// Store returns the credential store (may be nil).
func (r *Registry) Store() *auth.Store { return r.store }

// Default loads the models.dev catalog and returns the registry.
// A stale catalog (an old cache, or the embedded copy) is used at once and
// refreshed in the background, so startup never waits on the network.
func Default(ctx context.Context, store *auth.Store) (*Registry, error) {
	cat, stale, err := modelsdev.Load(ctx, catalogIDs()...)
	if err != nil {
		return nil, err
	}
	r := New(cat).WithStore(store)
	if stale {
		go func() {
			if c, err := modelsdev.Refresh(context.WithoutCancel(ctx), catalogIDs()...); err == nil {
				r.SetCatalog(c)
			}
		}()
	}
	return r, nil
}

// Register adds an explicit provider. A second provider claiming the same
// name is an error, not last-wins (PRD §11.4).
func (r *Registry) Register(p model.Provider) error {
	name := p.Name()
	if name == "" {
		return errors.New("registry: provider has empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.providers[name]; dup {
		return fmt.Errorf("registry: provider %q registered twice", name)
	}
	r.providers[name] = p
	return nil
}

// --- listing ---

// List returns every provider's status, ChatGPT first.
func (r *Registry) List() []Status {
	var out []Status
	for _, s := range subscriptions {
		out = append(out, r.subscriptionStatus(s))
	}
	r.mu.Lock()
	for name := range r.providers {
		out = append(out, pluginStatus(name))
	}
	r.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Status returns one provider's status.
func (r *Registry) Status(name string) (Status, bool) {
	r.mu.Lock()
	_, explicit := r.providers[name]
	r.mu.Unlock()
	if explicit {
		return pluginStatus(name), true
	}
	s, ok := subscriptionByID(name)
	if !ok {
		return Status{}, false
	}
	return r.subscriptionStatus(s), true
}

func pluginStatus(name string) Status {
	return Status{ID: name, Name: name, Kind: KindPlugin, Connected: true, Priority: 50}
}

func (r *Registry) subscriptionStatus(s subscription) Status {
	st := Status{ID: s.id, Name: s.name, Kind: KindSubscription, Priority: s.priority, Models: r.modelCount(s.id)}
	if f, ok := r.flows[s.id]; ok {
		st.Label = f.Label()
		st.Methods = f.Methods()
	}
	if c, ok := r.credential(s.id); ok {
		st.Connected = true
		st.Account = cmp.Or(c.Email, c.AccountID)
	}
	return st
}

// Providers lists connected provider ids.
func (r *Registry) Providers() []string {
	var out []string
	for _, st := range r.List() {
		if st.Connected {
			out = append(out, st.ID)
		}
	}
	return out
}

// --- login ---

// --- plan usage ---

// SubscriptionName is a subscription's display name ("ChatGPT").
func SubscriptionName(id string) string {
	if s, ok := subscriptionByID(id); ok {
		return s.name
	}
	return id
}

// --- resolution ---

// --- models ---

var gptVersion = regexp.MustCompile(`^gpt-(\d+)(?:\.(\d+))?`)

// --- asking for plan usage ---

// Only ChatGPT reports plan usage on its model calls. The other
// subscriptions have a usage endpoint instead (package quota), and ChatGPT's
// own goes stale while another provider does the work, so the registry asks:
// after a model call to that provider, when a client opens its chart, and
// once when the daemon starts, never more often than planPollEvery. A
// daemon nobody uses asks nothing.

// planPollEvery is the least time between two questions to one provider.
const planPollEvery = 5 * time.Minute
