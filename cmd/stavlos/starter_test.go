package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/policy"
)

// TestStarterConfigLoads: the file stavlos init writes loads as the global
// layer and says what it shows.
func TestStarterConfigLoads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", dir)
	b, err := json.MarshalIndent(starterConfig("openai/gpt-5"), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stavlos.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := config.LoadGlobal()
	if err != nil {
		t.Fatalf("the starter config does not load: %v\n%s", err, b)
	}
	if e.Model != "openai/gpt-5" || e.Limits.MaxDepth != 3 {
		t.Fatalf("model %q, limits %+v", e.Model, e.Limits)
	}
	for cmd, want := range map[string]policy.Verb{"rm -rf /tmp/x": policy.Deny, "git push": policy.Ask, "ls": policy.Ask} {
		if got, _ := e.Policy.Decide("shell", policy.Command(cmd)); got != want {
			t.Errorf("shell %q: %s, want %s", cmd, got, want)
		}
	}
}
