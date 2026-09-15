package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestModeStartsChannels: a new channel's permission mode is ask unless the
// global stavlos.json says auto or yolo; any other word is an error, and a
// repository may only say ask.
func TestModeStartsChannels(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{}`)
	if e, err := LoadGlobal(); err != nil || e.Mode != "ask" {
		t.Fatalf("default: %v %v", e, err)
	}
	write(`{"mode": "auto"}`)
	e, err := LoadGlobal()
	if err != nil || e.Mode != "auto" {
		t.Fatalf("auto: %v %v", e, err)
	}
	write(`{"mode": "sometimes"}`)
	if _, err := LoadGlobal(); err == nil {
		t.Fatal("an unknown mode is an error")
	}
	if err := e.checkRepositoryFile(File{Mode: "yolo"}); err == nil {
		t.Fatal("a repository may not start channels in yolo")
	}
	if err := e.checkRepositoryFile(File{Mode: "ask"}); err != nil {
		t.Fatalf("a repository may say ask: %v", err)
	}
}
