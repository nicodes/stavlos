package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

func TestConfigEditorScopesPersistsAndReloads(t *testing.T) {
	setupConfig(t)
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx := context.Background()
	ch, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	scope := protocol.ConfigScope{Scope: "project", Channel: ch.ID}
	tree, err := rpc.Do(ctx, h.c, protocol.ConfigList, scope)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Root != filepath.Join(ch.Dir, ".stavlos") {
		t.Fatalf("project root: %s", tree.Root)
	}
	p := protocol.ConfigEditParams{ConfigFileParams: protocol.ConfigFileParams{ConfigScope: scope, Root: tree.Root, Path: "stavlos.json"}, Revision: tree.Revision, Action: "write", FieldPath: []string{"policy", "shell", "echo *"}, FieldValue: json.RawMessage(`"deny"`)}
	saved, err := rpc.Do(ctx, h.c, protocol.ConfigEdit, p)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Document == nil || !strings.Contains(saved.Notice, "Reloaded") {
		t.Fatalf("save result: %+v", saved)
	}
	if _, err := os.Stat(filepath.Join(tree.Root, "stavlos.json")); err != nil {
		t.Fatal("save did not write the project file")
	}
	s, _ := h.d.channel(ch.ID)
	if decision, _ := s.Config().Policy.Decide("shell", policy.Command("echo hello")); decision != policy.Deny {
		t.Fatalf("live policy was not reloaded: %s; pending=%v; file=%s; notice=%s", decision, s.Config().TrustPending, saved.Document.Content, saved.Notice)
	}
	_, hash, _ := config.ProjectHash(ch.Dir)
	if !h.d.trust.Trusted(ch.Dir, hash) {
		t.Fatal("human edit did not carry trust forward")
	}
	if _, err := rpc.Do(ctx, h.c, protocol.ConfigEdit, p); err == nil {
		t.Fatal("stale UI edit accepted")
	}
	p.Revision, p.Path = saved.Tree.Revision, "../outside"
	if _, err := rpc.Do(ctx, h.c, protocol.ConfigEdit, p); err == nil {
		t.Fatal("scope escape accepted")
	}
	global, err := rpc.Do(ctx, h.c, protocol.ConfigList, protocol.ConfigScope{Scope: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if global.Root == tree.Root {
		t.Fatal("system and project scopes overlap")
	}
	p = protocol.ConfigEditParams{ConfigFileParams: protocol.ConfigFileParams{ConfigScope: protocol.ConfigScope{Scope: "system"}, Root: global.Root, Path: "stavlos.json"}, Revision: global.Revision, Action: "write", FieldPath: []string{"limits", "maxAgents"}, FieldValue: json.RawMessage("12")}
	if _, err := rpc.Do(ctx, h.c, protocol.ConfigEdit, p); err != nil {
		t.Fatal(err)
	}
	if s.Config().Limits.MaxAgents != 12 {
		t.Fatal("system edit was not applied to the channel")
	}
	if decision, _ := s.Config().Policy.Decide("shell", policy.Command("echo hello")); decision != policy.Deny {
		t.Fatal("system reload lost project overrides")
	}
}
