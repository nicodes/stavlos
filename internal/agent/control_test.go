package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestControlFilesChangedByACommandAreReported: a command that edits a git
// hook, a control file the sandbox cannot always protect, is said so in
// its result, for the agent and the human; one that does not is not.
func TestControlFilesChangedByACommandAreReported(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(call("c1", "shell", `{"command":"echo untouched"}`)),
		reply(call("c2", "shell", `{"command":"mkdir -p .git/hooks && printf '#!/bin/sh\\necho hi\\n' > .git/hooks/pre-commit"}`)),
		reply(text("done")),
	}}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","mode":"yolo","sandbox":{"enabled":false}}`}, fm)
	_ = os.MkdirAll(filepath.Join(s.Dir(), ".git"), 0o755)
	runTurn(t, s, h, "go")
	fin := finished(h, s.Root().ID)
	if len(fin) != 2 || strings.Contains(fin[0].Output, "changed") {
		t.Fatalf("a harmless command was flagged: %+v", fin)
	}
	if !strings.Contains(fin[1].Output, "changed "+filepath.Join(s.Dir(), ".git/hooks")) || !strings.Contains(fin[1].Output, "review") {
		t.Fatalf("a hook written by a command went unreported: %q", fin[1].Output)
	}
	// the stamp itself: a directory's files count, a missing path is ""
	cfg := s.Config()
	before := s.stampControl(cfg)
	if before[filepath.Join(s.Dir(), ".envrc")] != "" || before[filepath.Join(s.Dir(), ".git/hooks")] == "" {
		t.Fatalf("stamp: %v", before)
	}
	if note := s.controlNote(before, cfg); note != "" {
		t.Fatalf("nothing changed, yet: %q", note)
	}
	_ = os.WriteFile(filepath.Join(s.Dir(), ".envrc"), []byte("export X=1\n"), 0o644)
	if note := s.controlNote(before, cfg); !strings.Contains(note, ".envrc") {
		t.Fatalf("a new .envrc went unreported: %q", note)
	}
	if note := s.controlNote(nil, cfg); note != "" {
		t.Fatal("a nil stamp compared")
	}
}
