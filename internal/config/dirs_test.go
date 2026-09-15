package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDirsAreGlobalOnly: the global stavlos.json's dirs load with ~
// expanded; a repository's config may not add any.
func TestDirsAreGlobalOnly(t *testing.T) {
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
		t.Fatalf("dirs: %v", e.Dirs)
	}
	if err := e.checkRepositoryFile(File{Dirs: []string{"/opt/data"}}); err == nil {
		t.Fatal("a repository may not add dirs")
	}
}
