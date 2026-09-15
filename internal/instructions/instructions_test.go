package instructions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func texts(files []File) string {
	var out []string
	for _, f := range files {
		out = append(out, f.Text)
	}
	return strings.Join(out, "|")
}

// TestChainRunsFromTheRepositoryRoot: the files from the git root down to
// the directory, root first, AGENTS.md before CLAUDE.md in a directory; a
// directory in no repository reads only its own, never the home directory's.
func TestChainRunsFromTheRepositoryRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "repo")
	api := filepath.Join(repo, "svc", "api")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "AGENTS.md"), "home")
	write(t, filepath.Join(repo, "AGENTS.md"), "root")
	write(t, filepath.Join(repo, "svc", "CLAUDE.md"), "svc")
	write(t, filepath.Join(api, "AGENTS.md"), "api")
	write(t, filepath.Join(api, "CLAUDE.md"), "not this one")
	if got := Root(api); got != repo {
		t.Fatalf("root %s, want %s", got, repo)
	}
	if got := texts(Chain(api)); got != "root|svc|api" {
		t.Fatalf("chain %q", got)
	}
	loose := filepath.Join(home, "loose")
	write(t, filepath.Join(loose, "x.go"), "")
	if got := texts(Chain(loose)); got != "" || Root(loose) != loose {
		t.Fatalf("outside a repository: %q (root %s)", got, Root(loose))
	}
}

// TestNestedAndBetween: nested files are found below a directory, not in
// hidden or dependency trees; Between gives the ones on the way down to a
// path, top first.
func TestNestedAndBetween(t *testing.T) {
	top := t.TempDir()
	write(t, filepath.Join(top, "AGENTS.md"), "top")
	write(t, filepath.Join(top, "a", "AGENTS.md"), "a")
	write(t, filepath.Join(top, "a", "b", "CLAUDE.md"), "b")
	write(t, filepath.Join(top, "a", "b", "file.go"), "")
	write(t, filepath.Join(top, "node_modules", "x", "AGENTS.md"), "dep")
	write(t, filepath.Join(top, ".cache", "AGENTS.md"), "hidden")
	nested := Nested(top)
	if len(nested) != 2 || nested[0] != filepath.Join(top, "a", "AGENTS.md") || nested[1] != filepath.Join(top, "a", "b", "CLAUDE.md") {
		t.Fatalf("nested %v", nested)
	}
	if got := texts(Between(top, filepath.Join(top, "a", "b", "file.go"))); got != "a|b" {
		t.Fatalf("between %q", got)
	}
	if got := texts(Between(top, filepath.Join(top, "a", "b"))); got != "a|b" {
		t.Fatalf("between a directory %q", got)
	}
	for _, p := range []string{filepath.Join(top, "main.go"), filepath.Dir(top), filepath.Join(top, "node_modules", "x", "y.js")} {
		if got := Between(top, p); len(got) != 0 {
			t.Fatalf("between %s: %v", p, got)
		}
	}
}

// TestSectionKeepsTheBudget: headings name each file, the one that crosses
// the budget is cut with a note, and later ones are left out.
func TestSectionKeepsTheBudget(t *testing.T) {
	files := []File{{Path: "/r/AGENTS.md", Text: "one two three"}, {Path: "/r/a/AGENTS.md", Text: "four five six"}, {Path: "/r/a/b/AGENTS.md", Text: "seven"}}
	s := Section(files, 20)
	if !strings.HasPrefix(s, "# Instructions (AGENTS.md)") || !strings.Contains(s, "## /r/AGENTS.md\n\none two three") || !strings.Contains(s, "## /r/a/AGENTS.md\n\nfour f") ||
		!strings.Contains(s, "[cut here") || strings.Contains(s, "seven") {
		t.Fatalf("section:\n%s", s)
	}
	if Section(nil, Budget) != "" || Note([]File{{Path: "/x", Text: "  "}}, Budget) != "" {
		t.Fatal("no instructions, no section")
	}
}

// TestGlobal: the user's file lives in the config directory.
func TestGlobal(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", cfg)
	if len(Global()) != 0 {
		t.Fatal("no global file yet")
	}
	write(t, filepath.Join(cfg, "AGENTS.md"), "mine")
	if got := texts(Global()); got != "mine" {
		t.Fatalf("global %q", got)
	}
}
