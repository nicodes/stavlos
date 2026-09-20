package registry

import (
	"strconv"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/modelsdev"
)

// modelCounts memoises how many models each subscription lists for one
// catalog, so a provider listing does not walk the catalog every time.
type modelCounts struct {
	cat *modelsdev.Catalog
	n   map[string]int
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

// ModelEntry is a catalog model for pickers.
type ModelEntry struct {
	ID, Provider, Name string
	Info               model.Info
}

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

// zaiAllowed keeps the GLM models of the coding plan. The plan's own
// models.dev entry already lists only what the plan serves, so everything
// in it counts.
func zaiAllowed(id string) bool { return strings.HasPrefix(id, "glm") }

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
		// the catalog entry may be the plan's rather than the vendor's, so
		// the models listed are the ones the subscription actually serves
		key := s.catalogID()
		for _, id := range cat.Models(key) {
			if s.allow != nil && !s.allow(id) {
				continue
			}
			info, _ := cat.Model(key, id)
			out = append(out, ModelEntry{ID: s.id + "/" + id, Provider: s.id, Name: cat.ModelName(key, id), Info: subscriptionInfo(info)})
		}
	}
	return out
}
