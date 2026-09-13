package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nicodes/stavlos/internal/policy"
)

type allTrust struct{}

func (allTrust) Trusted(string, string) bool { return true }

type noTrust struct{}

func (noTrust) Trusted(string, string) bool { return false }

func TestLoadLayersAndTrust(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{
  // comment
  "model": "anthropic/claude-sonnet-5",
  "policy": { "bash": { "*": "allow", "git push*": "ask" }, },
}`), 0o644)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".stavlos", "agents"), 0o755)
	os.WriteFile(filepath.Join(dir, ".stavlos", "stavlos.json"), []byte(`{"policy":{"bash":{"git push*":"allow","curl*":"deny"}}}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".stavlos", "agents", "reviewer.md"), []byte("---\ndescription: reviews\nmodel: openai/gpt-5-mini\ntools: [read]\n---\nYou review.\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Use light models."), 0o644)

	e, err := Load(dir, noTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if !e.TrustPending || e.TrustHash == "" || len(e.TrustFiles) != 3 {
		t.Fatalf("trust: pending=%v hash=%q files=%v", e.TrustPending, e.TrustHash, e.TrustFiles)
	}
	if _, ok := e.Presets["reviewer"]; ok {
		t.Fatal("untrusted preset loaded")
	}
	if e.AgentsMD != "" {
		t.Fatal("untrusted AGENTS.md loaded")
	}
	if e.Policy.Decide("bash", "curl x") != policy.Allow {
		t.Fatal("untrusted project policy applied")
	}

	e, err = Load(dir, allTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if e.TrustPending {
		t.Fatal("still pending")
	}
	if e.Model != "anthropic/claude-sonnet-5" || e.RootAgent != "coder" {
		t.Fatalf("model %q root %q", e.Model, e.RootAgent)
	}
	p, ok := e.Presets["reviewer"]
	if !ok || p.Model != "openai/gpt-5-mini" || p.Body != "You review." || p.Layer != "project" {
		t.Fatalf("preset %+v", p)
	}
	if e.AgentsMD != "Use light models." {
		t.Fatal("AGENTS.md")
	}
	if e.Policy.Decide("bash", "git push x") != policy.Ask {
		t.Fatal("project loosened git push")
	}
	if e.Policy.Decide("bash", "curl x") != policy.Deny {
		t.Fatal("project tighten lost")
	}
	if _, ok := e.Presets["coder"]; !ok {
		t.Fatal("builtin missing")
	}
}

func TestReadOnlyBashAllowedByDefault(t *testing.T) {
	t.Setenv("STAVLOS_CONFIG_DIR", t.TempDir())
	e, err := Load(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"grep -rn foo .", "rg foo", "find . -name '*.go'", "ls -la", "git status", "git log --oneline", "cat go.mod"} {
		if v := e.Policy.Decide("bash", cmd); v != policy.Allow {
			t.Errorf("%q: %s, want allow", cmd, v)
		}
	}
	for _, cmd := range []string{"rm -rf x", "git push origin main", "go test ./...", "grepx"} {
		if v := e.Policy.Decide("bash", cmd); v == policy.Allow {
			t.Errorf("%q should not be allowed by default", cmd)
		}
	}
}
