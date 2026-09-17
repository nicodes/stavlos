package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestDirectoryTransitionPersistsAndResetsEnvironment(t *testing.T) {
	s, h := newTestChannel(t, testConfig{}, &fakeModel{})
	old, next, extra := s.Dir(), t.TempDir(), t.TempDir()
	ctx := context.Background()
	if err := s.SetMode(ctx, protocol.ModeYolo); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDir(ctx, extra); err != nil {
		t.Fatal(err)
	}
	if err := s.commit(ctx, s.event("", event.PermitGranted, event.PermitPayload{Tool: "shell", Prefix: "go test"})); err != nil {
		t.Fatal(err)
	}
	root := s.Root()
	root.mcp.mu.Lock()
	root.mcp.servers = map[string]*mcpServer{"old-server": {name: "old-server", state: protocol.MCPConnected}}
	root.mcp.mu.Unlock()
	s.mu.Lock()
	root.prefix = promptPrefix{key: "old"}
	root.instructed = map[string]bool{"old": true}
	s.mu.Unlock()
	load := func(dir string) (*config.Effective, error) { cfg := *s.Config(); cfg.Dir = dir; return &cfg, nil }
	if err := s.SetDir(ctx, next, load); err != nil {
		t.Fatal(err)
	}
	if s.Dir() != next || s.Config().Dir != next || s.Mode() != protocol.ModeAsk {
		t.Fatalf("directory/config/mode: %s %s %s", s.Dir(), s.Config().Dir, s.Mode())
	}
	if root.hasMCP() {
		t.Fatal("MCP server from the old directory survived")
	}
	if dirs := s.dirPaths(); len(dirs) != 2 || dirs[0] != next || dirs[1] != extra {
		t.Fatal(dirs)
	}
	s.mu.Lock()
	if s.st.permits.covers("shell", policy.Command("go test ./...")) || root.prefix.key != "" || len(root.instructed) != 0 {
		t.Error("old environment leaked")
	}
	if len(root.state().inbox) != 1 || root.state().inbox[0].Kind != event.InputInfo || !strings.Contains(root.state().inbox[0].Text, old) {
		t.Error("no non-waking directory notice")
	}
	s.mu.Unlock()
	// An old config reload completing after the move must not put old policy back.
	stale := *s.Config()
	stale.Dir = old
	s.SetConfig(&stale)
	if s.Config().Dir != next {
		t.Fatal("stale config crossed a directory change")
	}
	s.Stop()
	r, err := Recover(ctx, newFakeHost(&fakeModel{}), s.ID, next, s.Created, s.Config(), h.all())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	if r.Dir() != next || r.Mode() != protocol.ModeAsk || r.Info().Name != s.Info().Name {
		t.Fatal("recovery lost channel settings")
	}
	r.mu.Lock()
	allowed := r.st.permits.covers("shell", policy.Command("go test ./..."))
	r.mu.Unlock()
	if allowed {
		t.Fatal("replay restored a permit from the old directory")
	}
}

func TestDirectoryTransitionFailureAndBusy(t *testing.T) {
	s, h := newTestChannel(t, testConfig{}, &fakeModel{})
	old, count := s.Dir(), len(h.all())
	bad := func(string) (*config.Effective, error) { return nil, errors.New("bad target config") }
	if err := s.SetDir(context.Background(), t.TempDir(), bad); err == nil {
		t.Fatal("bad config accepted")
	}
	if s.Dir() != old || len(h.all()) != count {
		t.Fatal("failed change modified durable state")
	}
	if err := s.SetDir(context.Background(), filepath.Join(old, "missing"), bad); err == nil {
		t.Fatal("missing directory accepted")
	}
	root := s.Root()
	for _, kind := range []string{"turn", "job", "prompt", "compaction"} {
		s.mu.Lock()
		st := root.state()
		switch kind {
		case "turn":
			st.inTurn = true
		case "job":
			st.jobs["j"] = event.JobStartedPayload{}
		case "prompt":
			st.asks["p"] = true
		case "compaction":
			root.maintenance++
		}
		s.mu.Unlock()
		if err := s.SetDir(context.Background(), t.TempDir(), bad); err == nil || !strings.Contains(err.Error(), "idle") {
			t.Fatalf("%s: %v", kind, err)
		}
		s.mu.Lock()
		st.inTurn = false
		delete(st.jobs, "j")
		delete(st.asks, "p")
		root.maintenance = 0
		s.mu.Unlock()
	}
}

func TestDirectoryTransitionGatesNewTurns(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	next := t.TempDir()
	fm := &fakeModel{steps: []step{func(_ context.Context, req model.Request) (model.Response, error) {
		if !strings.Contains(req.System, "Working directory: "+next) {
			t.Error("queued turn used old working directory")
		}
		return text("done"), nil
	}}}
	s, h := newTestChannel(t, testConfig{}, fm)
	done := make(chan error, 1)
	go func() {
		done <- s.SetDir(context.Background(), next, func(dir string) (*config.Effective, error) {
			close(entered)
			<-release
			cfg := *s.Config()
			cfg.Dir = dir
			return &cfg, nil
		})
	}()
	<-entered
	if err := s.Root().Prompt(context.Background(), "run after move", "human:test"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Root().beginTurn(); ok {
		t.Fatal("a turn started during environment replacement")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, event.TurnEnded, s.Root().ID)
}
