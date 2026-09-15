package modelsdev

import (
	"strings"
	"testing"
)

// TestParseKeepsOnlyWhatStavlosServes: with keep, other providers are
// neither decoded nor kept, and the encoded catalog parses back to the same
// metadata without them or the fields Stavlos does not read.
func TestParseKeepsOnlyWhatStavlosServes(t *testing.T) {
	doc := `{
		"openai": {"id": "openai", "env": ["OPENAI_API_KEY"], "models": {"gpt-5": {"id": "gpt-5", "name": "GPT-5", "attachment": true, "limit": {"context": 400000, "output": 128000}, "cost": {"input": 1.25, "output": 10}}}},
		"other": {"models": {"x": {"id": "x", "limit": "not an object: never decoded"}}}
	}`
	c, err := Parse([]byte(doc), "openai", "xai")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Models("other"); len(got) != 0 {
		t.Fatalf("a provider not kept: %v", got)
	}
	b, err := c.encode()
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "other") || strings.Contains(s, "attachment") || strings.Contains(s, "OPENAI_API_KEY") {
		t.Fatalf("the cache keeps only kept providers and read fields: %s", s)
	}
	again, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := c.Model("openai", "gpt-5")
	got, ok := again.Model("openai", "gpt-5")
	if !ok || got != want {
		t.Fatalf("round trip: %+v, want %+v", got, want)
	}
	if _, err := Parse([]byte(doc), "xai"); err == nil {
		t.Fatal("nothing kept is an empty catalog")
	}
}
