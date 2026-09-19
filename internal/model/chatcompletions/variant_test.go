package chatcompletions

import (
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/model"
)

// TestOnlyAVariantTheModelTakesIsSent (#36): an agent already in the log may
// carry a variant from a model it has since left. GLM, Kimi and the Grok
// models without reasoning effort reject the field, so it is left out for
// them whatever the request says.
func TestOnlyAVariantTheModelTakesIsSent(t *testing.T) {
	req := model.Request{Variant: "high", Messages: []model.Message{{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "x"}}}}}
	for _, tc := range []struct {
		provider, id string
		sent         bool
	}{
		{"zai", "glm-5.3", false},
		{"kimi", "kimi-for-coding", false},
		{"xai", "grok-4", false},
		{"xai", "grok-4-mini", true},
	} {
		p := NewWithToken(tc.provider, "http://unused", nil).(*provider)
		body, err := p.buildBody(tc.id, req)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(body), `"reasoning_effort":"high"`); got != tc.sent {
			t.Errorf("%s/%s: reasoning_effort sent = %v, want %v\n%s", tc.provider, tc.id, got, tc.sent, body)
		}
	}
	p := NewWithToken("xai", "http://unused", nil).(*provider)
	req.Variant = "medium" // ChatGPT's word, not one of Grok mini's
	if body, _ := p.buildBody("grok-4-mini", req); strings.Contains(string(body), "reasoning_effort") {
		t.Errorf("another provider's variant reached grok-4-mini: %s", body)
	}
}
