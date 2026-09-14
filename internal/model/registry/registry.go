// Package registry serves the two providers Stavlos supports, both through
// the user's own subscription rather than platform API keys (PRD §8.4):
//
//   - openai: ChatGPT Plus/Pro, via the Codex sign-in and backend
//   - xai:    SuperGrok, via the Grok CLI sign-in and api.x.ai
//
// Credentials live in the auth store; access tokens are refreshed on demand.
package registry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nicodes/stavlos/internal/auth"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/chatcompletions"
	"github.com/nicodes/stavlos/internal/model/codex"
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

// subscription is everything provider-specific about one supported
// subscription; the rest of the registry is generic over this table.
type subscription struct {
	id, name string
	priority int
	// allow keeps the catalog models the subscription actually serves.
	allow func(model string) bool
	// open builds the adapter; src reads (and refreshes) the stored login.
	open func(r *Registry, src model.TokenSource) model.Provider
}

var subscriptions = []subscription{
	{
		id: "openai", name: "ChatGPT", priority: 0,
		allow: chatGPTAllowed,
		open: func(r *Registry, src model.TokenSource) model.Provider {
			return codex.NewWithEndpoint(src, cmp.Or(r.codexEndpoint, codex.DefaultEndpoint))
		},
	},
	{
		id: "xai", name: "Grok", priority: 1,
		allow: grokAllowed,
		open: func(r *Registry, src model.TokenSource) model.Provider {
			return chatcompletions.NewWithToken("xai", cmp.Or(r.xaiBaseURL, xaiBaseURL), src)
		},
	},
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

	mu         sync.Mutex
	providers  map[string]model.Provider // explicit registrations
	subs       map[string]model.Provider // subscription adapters, built once (their token source reads the store per call)
	opened     map[string]model.Model    // keyed by full "provider/id"
	refreshing map[string]*sync.Mutex    // single-flight refresh per provider
}

// modelCounts memoises how many models each subscription lists for one
// catalog, so a provider listing does not walk the catalog every time.
type modelCounts struct {
	cat *modelsdev.Catalog
	n   map[string]int
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
func (r *Registry) WithEndpoints(codexEndpoint, xaiBaseURL string) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.codexEndpoint, r.xaiBaseURL = codexEndpoint, xaiBaseURL
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
	cat, stale, err := modelsdev.Load(ctx)
	if err != nil {
		return nil, err
	}
	r := New(cat).WithStore(store)
	if stale {
		go func() {
			if c, err := modelsdev.Refresh(context.WithoutCancel(ctx)); err == nil {
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

// modelCount is len(Models(id, true)), computed once per catalog.
func (r *Registry) modelCount(id string) int {
	cat := r.cat.Load()
	if mc := r.counts.Load(); mc != nil && mc.cat == cat {
		return mc.n[id]
	}
	mc := &modelCounts{cat: cat, n: map[string]int{}}
	for _, m := range r.Models("", true) {
		mc.n[m.Provider]++
	}
	r.counts.Store(mc)
	return mc.n[id]
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

// Flow returns the login flow for a provider.
func (r *Registry) Flow(provider string) (oauth.Flow, error) {
	f, ok := r.flows[provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q: Stavlos supports openai (ChatGPT) and xai (Grok)", provider)
	}
	return f, nil
}

// SaveLogin stores tokens from a completed login.
func (r *Registry) SaveLogin(provider string, t oauth.Tokens) error {
	if r.store == nil {
		return errors.New("no credential store")
	}
	if err := r.store.Set(provider, credentialOf(t)); err != nil {
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

func credentialOf(t oauth.Tokens) auth.Credential {
	return auth.Credential{Type: "oauth", Access: t.Access, Refresh: t.Refresh, Expires: t.ExpiresAt.UnixMilli(), AccountID: t.AccountID, Email: t.Email}
}

// credential is a provider's usable login: an OAuth credential with a
// refresh token.
func (r *Registry) credential(provider string) (auth.Credential, bool) {
	if r.store == nil {
		return auth.Credential{}, false
	}
	c, ok := r.store.Get(provider)
	return c, ok && c.Type == "oauth" && c.Refresh != ""
}

// tokenSource returns a TokenSource that refreshes the stored access token
// when it is about to expire and persists the rotated pair.
func (r *Registry) tokenSource(provider string) model.TokenSource {
	return func(ctx context.Context) (model.Token, error) {
		c, ok := r.credential(provider)
		if !ok {
			return model.Token{}, fmt.Errorf("provider %q is not connected: run /provider to sign in", provider)
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
		if err := r.store.Set(provider, credentialOf(t)); err != nil {
			return model.Token{}, err
		}
		return model.Token{Access: t.Access, AccountID: t.AccountID}, nil
	}
}

// --- resolution ---

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
		info, _ = cat.Model(p.Name(), id)
		if name, _, _ := model.Split(full); isSubscription(name) {
			info = subscriptionInfo(info)
		}
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
		return nil, "", fmt.Errorf("unknown provider %q: Stavlos supports openai (ChatGPT) and xai (Grok); run /provider", name)
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

// --- models ---

// ModelEntry is a catalog model for pickers.
type ModelEntry struct {
	ID, Provider, Name string
	Info               model.Info
}

var gptVersion = regexp.MustCompile(`^gpt-(\d+)(?:\.(\d+))?`)

// chatGPTAllowed mirrors the models the ChatGPT subscription backend
// serves (the same list opencode uses).
func chatGPTAllowed(id string) bool {
	switch id {
	case "gpt-5.5", "gpt-5.3-codex-spark", "gpt-5.4", "gpt-5.4-mini":
		return true
	case "gpt-5.5-pro", "gpt-5.6":
		return false
	}
	if strings.Contains(id, "-pro") {
		return false
	}
	m := gptVersion.FindStringSubmatch(id)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor := 0
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	return major > 5 || (major == 5 && minor > 4)
}

// grokAllowed keeps Grok's chat models; the image and video generators
// (grok-imagine-*) cannot drive an agent.
func grokAllowed(id string) bool {
	return strings.HasPrefix(id, "grok") && !strings.Contains(id, "imagine")
}

// Models lists models for a provider (all if empty), connected providers
// only unless all is true. Prices are zero: these are subscriptions.
func (r *Registry) Models(provider string, all bool) []ModelEntry {
	cat := r.cat.Load()
	if cat == nil {
		return nil
	}
	var out []ModelEntry
	for _, s := range subscriptions {
		if provider != "" && s.id != provider {
			continue
		}
		if _, ok := r.credential(s.id); !ok && !all {
			continue
		}
		for _, id := range cat.Models(s.id) {
			if !s.allow(id) {
				continue
			}
			info, _ := cat.Model(s.id, id)
			out = append(out, ModelEntry{ID: s.id + "/" + id, Provider: s.id, Name: cat.ModelName(s.id, id), Info: subscriptionInfo(info)})
		}
	}
	return out
}
