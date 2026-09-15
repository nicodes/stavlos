package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDirsFromEveryLayer: the global stavlos.json's dirs load with ~
// expanded; a trusted project's add to them, anywhere (/tmp, a relative
// sibling); an untrusted project's do not apply.
func TestDirsFromEveryLayer(t *testing.T) {
	g := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	if err := os.WriteFile(filepath.Join(g, "stavlos.json"), []byte(`{"dirs": ["~/shared", "/opt/data"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if len(e.Dirs) != 2 || e.Dirs[0] != filepath.Join(home, "shared") || e.Dirs[1] != "/opt/data" {
		t.Fatalf("global dirs: %v", e.Dirs)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".stavlos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".stavlos", "stavlos.json"), []byte(`{"dirs": ["/tmp", "../sibling"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if e, err := Load(dir, noTrust{}); err != nil || len(e.Dirs) != 2 {
		t.Fatalf("an untrusted project adds no dirs: %v %v", e.Dirs, err)
	}
	e, err = Load(dir, allTrust{})
	if err != nil || strings.Join(e.Dirs, ",") != filepath.Join(home, "shared")+",/opt/data,/tmp,../sibling" {
		t.Fatalf("a trusted project's dirs add to the global ones: %v %v", e.Dirs, err)
	}
}
