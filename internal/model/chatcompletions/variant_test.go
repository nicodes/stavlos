package chatcompletions

import (
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/model"
)

// TestTraitsShapeTheRequest: the adapter knows no provider by name. What a
// provider's prompt cache and its models need is said by its traits, and
// only that is sent: a variant only to a model that takes it (#36: a model
// with no reasoning effort rejects the field, and an agent already in the log
// may carry one from a model it has left), a cache key only where the
// provider reads one, earlier reasoning only to a provider that replays it.
// Which provider has which traits is the registry's table, tested there.
func TestTraitsShapeTheRequest(t *testing.T) {
	req := model.Request{Variant: "high", CacheKey: "a1b2c3", Messages: []model.Message{
		{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "x"}}},
		{Role: model.RoleAssistant, Blocks: []model.Block{{Type: model.BlockThinking, Text: "let me think"}, {Type: model.BlockText, Text: "y"}}},
		{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "z"}}},
	}}
	efforts := func(id string) []string {
		if strings.Contains(id, "mini") {
			return []string{"low", "high"}
		}
		return nil
	}
	for _, tc := range []struct {
		name, id                string
		traits                  Traits
		variant, key, reasoning bool
	}{
		{"nothing declared", "m", Traits{}, false, false, false},
		{"a model that takes the variant", "m-mini", Traits{Variants: efforts}, true, false, false},
		{"a model of the same provider that does not", "m", Traits{Variants: efforts}, false, false, false},
		{"a cache key in the body, reasoning replayed", "m", Traits{CacheKeyField: true, ReplaysReasoning: true}, false, true, true},
		{"routed by header: nothing about it in the body", "m", Traits{CacheHeader: "x-conv", ReplaysReasoning: true}, false, false, true},
	} {
		body, err := NewWithToken("p", "http://unused", nil, tc.traits).(*provider).buildBody(tc.id, req)
		if err != nil {
			t.Fatal(err)
		}
		for field, want := range map[string]bool{`"reasoning_effort":"high"`: tc.variant, `"prompt_cache_key":"a1b2c3"`: tc.key, `"reasoning_content":"let me think"`: tc.reasoning} {
			if got := strings.Contains(string(body), field); got != want {
				t.Errorf("%s: %s sent = %v, want %v", tc.name, field, got, want)
			}
		}
	}
	req.Variant = "medium" // another provider's word for it
	if body, _ := NewWithToken("p", "http://unused", nil, Traits{Variants: efforts}).(*provider).buildBody("m-mini", req); strings.Contains(string(body), "reasoning_effort") {
		t.Errorf("a variant the model does not take was sent: %s", body)
	}
}
