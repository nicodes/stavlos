package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestModeStartsChannels: a new channel's permission mode is ask unless a
// stavlos.json says auto or yolo; any other word is an error, and a trusted
// project's mode overrides the global one, while an untrusted project's
// does not apply.
func TestModeStartsChannels(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	write := func(p, body string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	global := filepath.Join(g, "stavlos.json")
	write(global, `{}`)
	if e, err := LoadGlobal(); err != nil || e.Mode != "ask" {
		t.Fatalf("default: %v %v", e, err)
	}
	write(global, `{"mode": "auto"}`)
	if e, err := LoadGlobal(); err != nil || e.Mode != "auto" {
		t.Fatalf("auto: %v %v", e, err)
	}
	write(global, `{"mode": "sometimes"}`)
	if _, err := LoadGlobal(); err == nil {
		t.Fatal("an unknown mode is an error")
	}
	write(global, `{"mode": "auto"}`)
	dir := t.TempDir()
	write(filepath.Join(dir, ".stavlos", "stavlos.json"), `{"mode": "yolo"}`)
	if e, err := Load(dir, noTrust{}); err != nil || e.Mode != "auto" {
		t.Fatalf("an untrusted project's mode does not apply: %v %v", e, err)
	}
	if e, err := Load(dir, allTrust{}); err != nil || e.Mode != "yolo" {
		t.Fatalf("a trusted project's mode overrides the global one: %v %v", e, err)
	}
}
