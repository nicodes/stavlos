package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/model"
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
