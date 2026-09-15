package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstructionsFollowTheRepository: the user's AGENTS.md loads whatever
// the trust; the repository's, from its root down and below the channel
// directory, count toward the trust hash and load only once trusted.
func TestInstructionsFollowTheRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	g := filepath.Join(home, "config")
	t.Setenv("STAVLOS_CONFIG_DIR", g)
	repo := filepath.Join(home, "repo")
	svc := filepath.Join(repo, "svc")
	for p, text := range map[string]string{
		filepath.Join(g, "AGENTS.md"):                   "mine",
		filepath.Join(repo, "AGENTS.md"):                "root",
		filepath.Join(svc, "pkg", "AGENTS.md"):          "pkg",
		filepath.Join(repo, ".git", "HEAD"):             "ref: main",
		filepath.Join(svc, "node_modules", "AGENTS.md"): "dep",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e, err := Load(svc, noTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(e.TrustFiles, ",") != "../AGENTS.md,pkg/AGENTS.md" || !e.TrustPending || e.ProjectTrusted {
		t.Fatalf("untrusted: files %v pending %v", e.TrustFiles, e.TrustPending)
	}
	if len(e.Instructions) != 1 || e.Instructions[0].Text != "mine" {
		t.Fatalf("untrusted instructions: %+v", e.Instructions)
	}
	e, err = Load(svc, allTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Instructions) != 2 || e.Instructions[1].Text != "root" || !e.ProjectTrusted ||
		strings.Join(e.InstructionFiles, ",") != filepath.Join(repo, "AGENTS.md")+","+filepath.Join(svc, "pkg", "AGENTS.md") {
		t.Fatalf("trusted: %+v %v", e.Instructions, e.InstructionFiles)
	}
}
