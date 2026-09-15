package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/config"
)

// TestInitWritesALoadableConfig: the starter config passes the strict
// loader, names a model only when one is given, comes with agents/ and
// skills/, and is never overwritten.
func TestInitWritesALoadableConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", dir)
	if err := initConfig(nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "stavlos.json")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v %v", st, err)
	}
	for _, sub := range []string{"agents", "skills"} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			t.Fatalf("%s: %v", sub, err)
		}
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), `"model"`) {
		t.Fatalf("without --model the model is left to /models:\n%s", b)
	}
	if _, err := config.LoadGlobal(); err != nil {
		t.Fatalf("the starter config does not load: %v\n%s", err, b)
	}
	if err := initConfig(nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a second init must not overwrite: %v", err)
	}

	t.Setenv("STAVLOS_CONFIG_DIR", t.TempDir())
	if err := initConfig([]string{"--model", "openai/gpt-5.4"}); err != nil {
		t.Fatal(err)
	}
	eff, err := config.LoadGlobal()
	if err != nil || eff.Model != "openai/gpt-5.4" {
		t.Fatalf("with --model: %v %v", eff, err)
	}
}
