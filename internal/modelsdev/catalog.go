// Package modelsdev consumes the models.dev community database for model
// metadata: pricing, limits, and provider auth env vars (PRD §8.1, §8.4).
// Wire protocols are implemented elsewhere; this package is metadata only.
package modelsdev

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/nicodes/stavlos/internal/model"
)

// ProviderInfo is the provider-level metadata from models.dev.
type ProviderInfo struct {
	Name    string   // provider key, e.g. "anthropic"
	Display string   // human-facing name, e.g. "Anthropic"
	EnvVars []string // env vars that may hold the API key (the "env" field)
	API     string   // base URL for the wire protocol (the "api" field)
	NPM     string   // Vercel AI SDK package name (the "npm" field); hints the protocol
}

// Catalog is a parsed models.dev database.
type Catalog struct {
	providers map[string]rawProvider
}

// rawProvider mirrors one top-level entry of api.json.
type rawProvider struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Env    []string            `json:"env"`
	API    string              `json:"api"`
	NPM    string              `json:"npm"`
	Models map[string]rawModel `json:"models"`
}

// rawModel mirrors one model entry. Only the fields we consume are declared.
type rawModel struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Limit rawLimit `json:"limit"`
	Cost  *rawCost `json:"cost"`
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

// Parse decodes a models.dev api.json document.
func Parse(data []byte) (*Catalog, error) {
	var providers map[string]rawProvider
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("modelsdev: parse: %w", err)
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("modelsdev: parse: empty catalog")
	}
	return &Catalog{providers: providers}, nil
}

// Provider returns provider metadata by key.
func (c *Catalog) Provider(name string) (ProviderInfo, bool) {
	p, ok := c.providers[name]
	if !ok {
		return ProviderInfo{}, false
	}
	disp := p.Name
	if disp == "" {
		disp = name
	}
	return ProviderInfo{Name: name, Display: disp, EnvVars: append([]string(nil), p.Env...), API: p.API, NPM: p.NPM}, true
}

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

// Providers lists every provider key, sorted.
func (c *Catalog) Providers() []string {
	out := make([]string, 0, len(c.providers))
	for k := range c.providers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

// ModelName returns the display name of a model, or its id.
func (c *Catalog) ModelName(provider, id string) string {
	if p, ok := c.providers[provider]; ok {
		if m, ok := p.Models[id]; ok && m.Name != "" {
			return m.Name
		}
	}
	return id
}
