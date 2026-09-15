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
