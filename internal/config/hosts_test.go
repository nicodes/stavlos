package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostsFromEveryLayer: hosts load lower-cased from the global
// stavlos.json, a URL is not a host, and a trusted project's add to them
// (* included) while an untrusted project's do not.
func TestHostsFromEveryLayer(t *testing.T) {
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
	write(global, `{"hosts": ["GitHub.com", "*.golang.org"]}`)
	if e, err := LoadGlobal(); err != nil || strings.Join(e.Hosts, ",") != "github.com,*.golang.org" {
		t.Fatalf("global hosts: %v %v", e, err)
	}
	write(global, `{"hosts": ["https://github.com/x"]}`)
	if _, err := LoadGlobal(); err == nil || !strings.Contains(err.Error(), "hosts") {
		t.Fatalf("a URL is not a host: %v", err)
	}
	write(global, `{"hosts": ["github.com"]}`)
	dir := t.TempDir()
	write(filepath.Join(dir, ".stavlos", "stavlos.json"), `{"hosts": ["*"]}`)
	if e, err := Load(dir, noTrust{}); err != nil || strings.Join(e.Hosts, ",") != "github.com" {
		t.Fatalf("an untrusted project adds no hosts: %v %v", e, err)
	}
	if e, err := Load(dir, allTrust{}); err != nil || strings.Join(e.Hosts, ",") != "github.com,*" {
		t.Fatalf("a trusted project's hosts add to the global ones: %v %v", e, err)
	}
}
