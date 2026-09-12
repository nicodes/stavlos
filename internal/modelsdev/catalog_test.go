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

	if got, want := c.Providers(), []string{"anthropic", "ollama-cloud"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Providers() = %v, want %v", got, want)
	}

	p, ok := c.Provider("ollama-cloud")
	if !ok {
		t.Fatal("ollama-cloud missing")
	}
	if p.API != "https://ollama.com/v1" || p.NPM != "@ai-sdk/openai-compatible" || !reflect.DeepEqual(p.EnvVars, []string{"OLLAMA_API_KEY"}) {
		t.Errorf("ProviderInfo = %+v", p)
	}
	if _, ok := c.Provider("nope"); ok {
		t.Error("unknown provider reported present")
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
	for _, p := range []string{"anthropic", "openai"} {
		if _, ok := c.Provider(p); !ok {
			t.Errorf("fallback lacks %s", p)
		}
	}
	if info, ok := c.Model("anthropic", "claude-sonnet-5"); !ok || info.InputPrice == 0 {
		t.Errorf("fallback claude-sonnet-5 = %+v ok=%v", info, ok)
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
