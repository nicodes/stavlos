package modelsdev

import (
	"os"
	"reflect"
	"testing"
)

func TestParseFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/small.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	if got := c.Models("ollama-cloud"); !reflect.DeepEqual(got, []string{"free-model", "priced"}) {
		t.Errorf("Models(ollama-cloud) = %v", got)
	}
	if got := c.Models("nope"); got != nil {
		t.Errorf("unknown provider has models: %v", got)
	}

	info, ok := c.Model("anthropic", "claude-sonnet-5")
	if !ok {
		t.Fatal("claude-sonnet-5 missing")
	}
	if info.ContextWindow != 1000000 || info.MaxOutput != 128000 ||
		info.InputPrice != 2 || info.OutputPrice != 10 || info.CacheReadPrice != 0.2 || info.CacheWritePrice != 2.5 {
		t.Errorf("Info = %+v", info)
	}

	free, ok := c.Model("ollama-cloud", "free-model")
	if !ok || free.InputPrice != 0 || free.ContextWindow != 32000 {
		t.Errorf("free-model = %+v ok=%v", free, ok)
	}
	priced, _ := c.Model("ollama-cloud", "priced")
	if priced.InputPrice != 1 || priced.CacheReadPrice != 0 {
		t.Errorf("priced = %+v", priced)
	}
	if _, ok := c.Model("anthropic", "missing"); ok {
		t.Error("unknown model reported present")
	}
}

func TestFallbackParses(t *testing.T) {
	c, err := Parse(fallbackJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"openai", "xai"} {
		if len(c.Models(p)) == 0 {
			t.Errorf("fallback lacks %s", p)
		}
	}
	if info, ok := c.Model("openai", "gpt-5.4"); !ok || info.ContextWindow == 0 {
		t.Errorf("fallback gpt-5.4 = %+v ok=%v", info, ok)
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, err := Parse([]byte(`{}`)); err == nil {
		t.Error("empty catalog accepted")
	}
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Error("garbage accepted")
	}
}
