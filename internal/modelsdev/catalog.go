// Package modelsdev consumes the models.dev community database for model
// metadata: context windows, output limits, names (PRD §8.1). Wire
// protocols are implemented elsewhere; this package is metadata only.
package modelsdev

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/nicodes/stavlos/internal/model"
)

// Catalog is a parsed models.dev database.
type Catalog struct {
	providers map[string]rawProvider
}

// rawProvider mirrors one top-level entry of api.json (only its models are
// consumed).
type rawProvider struct {
	Models map[string]rawModel `json:"models"`
}

// rawModel mirrors one model entry. Only the fields we consume are declared.
type rawModel struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Limit rawLimit `json:"limit"`
	Cost  *rawCost `json:"cost,omitempty"`
}

type rawLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// rawCost is USD per million tokens. Cache fields are absent for many models.
type rawCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Parse decodes a models.dev api.json document. With keep, only those
// providers are decoded and kept: the database lists every provider there
// is (megabytes of it), and Stavlos serves a few.
func Parse(data []byte, keep ...string) (*Catalog, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("modelsdev: parse: %w", err)
	}
	providers := make(map[string]rawProvider, len(raw))
	for id, b := range raw {
		if len(keep) > 0 && !slices.Contains(keep, id) {
			continue
		}
		var p rawProvider
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("modelsdev: parse %s: %w", id, err)
		}
		providers[id] = p
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("modelsdev: parse: empty catalog")
	}
	return &Catalog{providers: providers}, nil
}

// encode is the catalog as a models.dev document holding only what it kept:
// its providers, and the fields Stavlos reads.
func (c *Catalog) encode() ([]byte, error) { return json.Marshal(c.providers) }

// Model returns metadata for a bare model id under a provider.
func (c *Catalog) Model(provider, id string) (model.Info, bool) {
	p, ok := c.providers[provider]
	if !ok {
		return model.Info{}, false
	}
	m, ok := p.Models[id]
	if !ok {
		return model.Info{}, false
	}
	info := model.Info{ContextWindow: m.Limit.Context, MaxOutput: m.Limit.Output}
	if m.Cost != nil {
		info.InputPrice = m.Cost.Input
		info.OutputPrice = m.Cost.Output
		info.CacheReadPrice = m.Cost.CacheRead
		info.CacheWritePrice = m.Cost.CacheWrite
	}
	return info, true
}

// Models lists the bare model ids under a provider, sorted.
func (c *Catalog) Models(provider string) []string {
	p, ok := c.providers[provider]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(p.Models))
	for k := range p.Models {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// covers reports whether the catalog holds every provider asked for. A
// cache trimmed by an earlier build knows nothing of a provider added
// since, so it cannot serve this one however recently it was written.
func (c *Catalog) covers(keep ...string) bool {
	for _, k := range keep {
		if _, ok := c.providers[k]; !ok {
			return false
		}
	}
	return true
}

// ModelName returns the display name of a model, or its id.
func (c *Catalog) ModelName(provider, id string) string {
	if p, ok := c.providers[provider]; ok {
		if m, ok := p.Models[id]; ok && m.Name != "" {
			return m.Name
		}
	}
	return id
}
