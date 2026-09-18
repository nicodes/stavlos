package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/toolname"
)

// TestNestedInstructionsReachTheAgentOnce: the first read in a
// subdirectory with its own AGENTS.md carries that file in the result; the
// next read there does not repeat it.
func TestNestedInstructionsReachTheAgentOnce(t *testing.T) {
	var results []string
	capture := func(next model.Response) step {
		return func(_ context.Context, req model.Request) (model.Response, error) {
			results = append(results, lastUserText(req))
			return next, nil
		}
	}
	fm := &fakeModel{steps: []step{
		reply(call("r1", "read", `{"path":"svc/a.go"}`)),
		capture(call("r2", "read", `{"path":"svc/b.go"}`)),
		capture(text("done")),
	}}
	cfg, work := loadTestConfig(t, testConfig{})
	for name, body := range map[string]string{"svc/AGENTS.md": "Keep handlers thin.", "svc/a.go": "package svc", "svc/b.go": "package svc"} {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg.ProjectTrusted = true
	h := newFakeHost(fm)
	s := New(h, "s1", work, cfg, "", "")
	if err := s.Start(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	runTurn(t, s, h, "look at svc")
	if len(results) != 2 {
		t.Fatalf("results %q", results)
	}
	if !strings.Contains(results[0], "Keep handlers thin.") || !strings.Contains(results[0], "[Instructions for the directories") {
		t.Fatalf("the first read in svc carries its instructions:\n%s", results[0])
	}
	if strings.Contains(results[1], "Keep handlers thin.") {
		t.Fatalf("the second read does not repeat them:\n%s", results[1])
	}
}

// TestInstructionsAreControlFiles: an edit to an AGENTS.md or CLAUDE.md at
// any depth of a working directory asks, and the sandbox keeps the known
// ones read-only.
func TestInstructionsAreControlFiles(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"AGENTS.md", "svc/AGENTS.md", "svc/deep/CLAUDE.md"} {
		if got := controlFile(toolname.ApplyPatch, policy.Path(p), dir, []string{dir}); got != p {
			t.Fatalf("%s: control file %q", p, got)
		}
	}
	if got := controlFile(toolname.ApplyPatch, policy.Path("svc/main.go"), dir, []string{dir}); got != "" {
		t.Fatalf("an ordinary file is no control file: %q", got)
	}
	cfg, work := loadTestConfig(t, testConfig{})
	nested := filepath.Join(work, "svc", "AGENTS.md")
	cfg.InstructionFiles = []string{nested}
	s := New(newFakeHost(&fakeModel{}), "s1", work, cfg, "", "")
	if spec := s.sandboxSpec(cfg); spec == nil || !slices.Contains(spec.ReadOnly, nested) {
		t.Fatalf("the sandbox keeps the nested instructions read-only: %+v", spec)
	}
}

// TestChangedInstructionsAskForTrustAgain: an edit to a known instructions
// file since the config was loaded tells the host at the next turn.
func TestChangedInstructionsAskForTrustAgain(t *testing.T) {
	cfg, work := loadTestConfig(t, testConfig{})
	file := filepath.Join(work, "AGENTS.md")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.InstructionFiles, cfg.TrustFiles = []string{file}, []string{"AGENTS.md"}
	h := newFakeHost(&fakeModel{steps: []step{reply(text("a")), reply(text("b"))}})
	s := New(h, "s1", work, cfg, "", "")
	if err := s.Start(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	runTurn(t, s, h, "first")
	h.mu.Lock()
	n := len(h.changed)
	h.mu.Unlock()
	if n != 0 {
		t.Fatal("unchanged instructions tell the host nothing")
	}
	if err := os.WriteFile(file, []byte("two, and longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTurn(t, s, h, "second")
	h.mu.Lock()
	changed := append([]string(nil), h.changed...)
	h.mu.Unlock()
	if len(changed) != 1 || changed[0] != work {
		t.Fatalf("the edit tells the host once: %v", changed)
	}
}

// TestChangedProjectLayerAsksForTrustAgain: editing the project layer
// itself — a role, a command, stavlos.json — changes the trust hash just as
// an edited AGENTS.md does, so it has to ask again too. Watching only the
// instructions let a channel quietly lose its roles: the hash no longer
// matched, the project layer stopped loading, and nothing said so until the
// channel was resumed.
func TestChangedProjectLayerAsksForTrustAgain(t *testing.T) {
	cfg, work := loadTestConfig(t, testConfig{})
	role := filepath.Join(work, ".stavlos", "agents", "coder.md")
	if err := os.MkdirAll(filepath.Dir(role), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(role, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.TrustFiles = []string{filepath.Join(".stavlos", "agents", "coder.md")}
	h := newFakeHost(&fakeModel{steps: []step{reply(text("a")), reply(text("b"))}})
	s := New(h, "s1", work, cfg, "", "")
	if err := s.Start(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	runTurn(t, s, h, "first")
	h.mu.Lock()
	n := len(h.changed)
	h.mu.Unlock()
	if n != 0 {
		t.Fatal("an unchanged project layer tells the host nothing")
	}
	if err := os.WriteFile(role, []byte("two, and longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTurn(t, s, h, "second")
	h.mu.Lock()
	changed := append([]string(nil), h.changed...)
	h.mu.Unlock()
	if len(changed) != 1 || changed[0] != work {
		t.Fatalf("the edited role tells the host once: %v", changed)
	}
}
