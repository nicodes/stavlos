// Package registry serves the two providers Stavlos supports, both through
// the user's own subscription rather than platform API keys (PRD §8.4):
//
//   - openai: ChatGPT Plus/Pro, via the Codex sign-in and backend
//   - xai:    SuperGrok, via the Grok CLI sign-in and api.x.ai
//
// Credentials live in the auth store; access tokens are refreshed on demand.
package registry

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicodes/stavlos/internal/auth"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/codex"
	"github.com/nicodes/stavlos/internal/model/openai"
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

// Registry is the provider map. It is safe for concurrent use.
type Registry struct {
	cat   *modelsdev.Catalog
	store *auth.Store
	flows map[string]oauth.Flow

	// endpoints override the real services (tests).
	codexEndpoint string
	xaiBaseURL    string

	mu         sync.Mutex
	providers  map[string]model.Provider // explicit registrations
	opened     map[string]model.Model    // keyed by full "provider/id"
	refreshing map[string]*sync.Mutex    // single-flight refresh per provider
}

// New returns a registry backed by cat for model metadata (may be nil).
func New(cat *modelsdev.Catalog) *Registry {
	return &Registry{cat: cat, flows: oauth.Flows(), providers: map[string]model.Provider{}, opened: map[string]model.Model{}, refreshing: map[string]*sync.Mutex{}}
}

// WithStore attaches the credential store.
func (r *Registry) WithStore(s *auth.Store) *Registry { r.store = s; return r }

// WithFlows replaces the login flows (tests).
func (r *Registry) WithFlows(f map[string]oauth.Flow) *Registry { r.flows = f; return r }

// WithEndpoints overrides service URLs (tests).
func (r *Registry) WithEndpoints(codexEndpoint, xaiBaseURL string) *Registry {
	r.codexEndpoint, r.xaiBaseURL = codexEndpoint, xaiBaseURL
	return r
}

// Catalog returns the metadata catalog (may be nil).
func (r *Registry) Catalog() *modelsdev.Catalog { return r.cat }

// Store returns the credential store (may be nil).
func (r *Registry) Store() *auth.Store { return r.store }

// Default loads the models.dev catalog and returns the registry.
func Default(ctx context.Context, store *auth.Store) (*Registry, error) {
	cat, err := modelsdev.Load(ctx)
	if err != nil {
		return nil, err
	}
	return New(cat).WithStore(store), nil
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

var subscriptions = []struct {
	id, name string
	priority int
}{
	{"openai", "ChatGPT", 0},
	{"xai", "Grok", 1},
}

// List returns every provider's status, ChatGPT first.
func (r *Registry) List() []Status {
	var out []Status
	for _, s := range subscriptions {
		if st, ok := r.Status(s.id); ok {
			out = append(out, st)
		}
	}
	r.mu.Lock()
	for name := range r.providers {
		out = append(out, Status{ID: name, Name: name, Kind: KindPlugin, Connected: true, Priority: 50})
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
		return Status{ID: name, Name: name, Kind: KindPlugin, Connected: true, Priority: 50}, true
	}
	for _, s := range subscriptions {
		if s.id != name {
			continue
		}
		st := Status{ID: s.id, Name: s.name, Kind: KindSubscription, Priority: s.priority, Models: len(r.Models(s.id, true))}
		if f, ok := r.flows[s.id]; ok {
			st.Label = f.Label()
			st.Methods = f.Methods()
		}
		if r.store != nil {
			if c, ok := r.store.Get(s.id); ok && c.Type == "oauth" && c.Refresh != "" {
				st.Connected = true
				st.Account = c.Email
				if st.Account == "" {
					st.Account = c.AccountID
				}
			}
		}
		return st, true
	}
	return Status{}, false
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
	if err := r.store.Set(provider, auth.Credential{Type: "oauth", Access: t.Access, Refresh: t.Refresh, Expires: t.ExpiresAt.UnixMilli(), AccountID: t.AccountID, Email: t.Email}); err != nil {
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

// tokenSource returns a TokenSource that refreshes the stored access token
// when it is about to expire and persists the rotated pair.
func (r *Registry) tokenSource(provider string) model.TokenSource {
	return func(ctx context.Context) (model.Token, error) {
		c, ok := r.store.Get(provider)
		if !ok || c.Type != "oauth" || c.Refresh == "" {
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
		if t.AccountID == "" {
			t.AccountID = c.AccountID
		}
		if t.Email == "" {
			t.Email = c.Email
		}
		if err := r.store.Set(provider, auth.Credential{Type: "oauth", Access: t.Access, Refresh: t.Refresh, Expires: t.ExpiresAt.UnixMilli(), AccountID: t.AccountID, Email: t.Email}); err != nil {
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

// Resolve opens (or returns the cached) model for full and its metadata.
// Subscription models carry no per-token price, so cost stays zero.
func (r *Registry) Resolve(full string) (model.Model, model.Info, error) {
	p, id, err := r.lookup(full)
	if err != nil {
		return nil, model.Info{}, err
	}
	var info model.Info
	if r.cat != nil {
		info, _ = r.cat.Model(p.Name(), id)
		if _, sub := r.flows[p.Name()]; sub {
			info.InputPrice, info.OutputPrice, info.CacheReadPrice, info.CacheWritePrice = 0, 0, 0, 0
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
	st, known := r.Status(name)
	if !known {
		return nil, "", fmt.Errorf("unknown provider %q: Stavlos supports openai (ChatGPT) and xai (Grok); run /provider", name)
	}
	if !st.Connected {
		return nil, "", fmt.Errorf("%s is not connected: run /provider to sign in with your %s subscription", st.Name, st.Name)
	}
	switch name {
	case "openai":
		if r.codexEndpoint != "" {
			return codex.NewWithEndpoint(r.tokenSource(name), r.codexEndpoint), id, nil
		}
		return codex.New(r.tokenSource(name)), id, nil
	case "xai":
		base := r.xaiBaseURL
		if base == "" {
			base = "https://api.x.ai/v1"
		}
		return openai.NewWithToken("xai", base, r.tokenSource(name)), id, nil
	}
	return nil, "", fmt.Errorf("provider %q has no adapter", name)
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

// Models lists models for a provider (all if empty), connected providers
// only unless all is true. Prices are zero: these are subscriptions.
func (r *Registry) Models(provider string, all bool) []ModelEntry {
	if r.cat == nil {
		return nil
	}
	var out []ModelEntry
	for _, s := range subscriptions {
		if provider != "" && s.id != provider {
			continue
		}
		if !all {
			if st, _ := r.Status(s.id); !st.Connected {
				continue
			}
		}
		for _, id := range r.cat.Models(s.id) {
			if s.id == "openai" && !chatGPTAllowed(id) {
				continue
			}
			if s.id == "xai" && !strings.HasPrefix(id, "grok") {
				continue
			}
			info, _ := r.cat.Model(s.id, id)
			info.InputPrice, info.OutputPrice, info.CacheReadPrice, info.CacheWritePrice = 0, 0, 0, 0
			out = append(out, ModelEntry{ID: s.id + "/" + id, Provider: s.id, Name: r.cat.ModelName(s.id, id), Info: info})
		}
	}
	return out
}
