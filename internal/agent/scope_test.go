package agent

import (
	"context"
	"github.com/nicodes/stavlos/internal/event"
	"path/filepath"
	"testing"
)

func TestChildDirectoryScopeCannotWidenAndSurvivesReplay(t *testing.T) {
	c, h := newTestChannel(t, testConfig{}, &fakeModel{})
	root := c.Root().ID
	extra := t.TempDir()
	if err := c.AddDir(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defaultScope, err := c.childScopeLocked(root, nil)
	if err != nil || len(defaultScope) != 1 || defaultScope[0] != c.st.dir {
		t.Fatalf("default scope: %v %v", defaultScope, err)
	}
	scope, err := c.childScopeLocked(root, []string{extra})
	if err != nil {
		t.Fatal(err)
	}
	err = c.commitLocked(context.Background(), c.event("scoped", event.AgentSpawned, event.AgentSpawnedPayload{ID: "scoped", Parent: root, Role: "general", Name: "scoped", Directories: scope}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.childScopeLocked("scoped", []string{c.st.dir}); err == nil {
		t.Fatal("descendant widened its parent scope")
	}
	if _, err = c.childScopeLocked(root, []string{filepath.Dir(c.st.dir)}); err == nil {
		t.Fatal("parent directory widened scope")
	}
	c.mu.Unlock()
	replay := newChannelState("", "")
	for _, e := range h.all() {
		replay.apply(e, &effects{})
	}
	if got := replay.agents["scoped"].directories; len(got) != 1 || got[0] != extra {
		t.Fatalf("scope lost on replay: %v", got)
	}
	if err := c.RemoveDir(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.agentDirsLocked("scoped")) != 0 {
		t.Fatal("revoked scope retained")
	}
	if _, err := c.childScopeLocked("scoped", []string{extra}); err == nil {
		t.Fatal("revoked scope could delegate")
	}
}
