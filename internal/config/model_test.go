package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetGlobalModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", dir)
	p := filepath.Join(dir, "stavlos.json")

	if err := SetGlobalModel("anthropic/claude-sonnet-5"); err != nil {
		t.Fatal(err)
	}
	if e, err := Load(t.TempDir(), nil); err != nil || e.Model != "anthropic/claude-sonnet-5" {
		t.Fatalf("%v %+v", err, e)
	}
	os.WriteFile(p, []byte("{\n  // hi\n  \"model\": \"openai/gpt-5\",\n  \"rootAgent\": \"coder\",\n}\n"), 0o644)
	if err := SetGlobalModel("ollama/llama3.1"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "// hi") {
		t.Fatalf("lost comment: %s", b)
	}
	if e, _ := Load(t.TempDir(), nil); e.Model != "ollama/llama3.1" || e.RootAgent != "coder" {
		t.Fatalf("%+v", e)
	}
	os.WriteFile(p, []byte("{\n  \"rootAgent\": \"coder\"\n}\n"), 0o644)
	if err := SetGlobalModel("x/y"); err != nil {
		t.Fatal(err)
	}
	if e, _ := Load(t.TempDir(), nil); e.Model != "x/y" || e.RootAgent != "coder" {
		t.Fatalf("%+v", e)
	}
}

// TestSetGlobalModelTouchesOneFieldAndNothingElse (1B.7): the first model a
// user picks is saved as the default. That was a regular expression over the
// whole file: every "model" key at any depth was rewritten, comments
// included, and a "$" in the id was expanded.
func TestSetGlobalModelTouchesOneFieldAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", dir)
	p := filepath.Join(dir, "stavlos.json")
	const before = `{
  // "model": "commented/out"
  "model": "old/one",
  "mcp": {"srv": {"command": "x", "env": {"model": "not-the-default"}}},
  "search": {"provider": "brave", "apiKey": "k"}, // keep me
}
`
	if err := os.WriteFile(p, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetGlobalModel("prov/weird$1-id"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := strings.Replace(before, `"model": "old/one"`, `"model": "prov/weird$1-id"`, 1)
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v: the file may hold a key", st.Mode().Perm())
	}
	// no file yet: one is made
	other := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", filepath.Join(other, "new"))
	if err := SetGlobalModel("a/b"); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadGlobal(); err != nil || cfg.Model != "a/b" {
		t.Fatalf("a fresh config: %v %+v", err, cfg)
	}
}
