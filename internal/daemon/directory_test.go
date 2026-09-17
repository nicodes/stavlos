package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

func TestChannelDirectoryPersists(t *testing.T) {
	setupConfig(t)
	ctx := context.Background()
	data, old, target, other := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(target, ".stavlos"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".stavlos", "stavlos.json"), []byte(`{"model":"fake/m2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, data, &fakeModel{})
	closed := false
	defer func() {
		if !closed {
			h.close()
		}
	}()
	a, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: old, Name: "first"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: other, Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := config.ProjectHash(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.d.Trust(ctx, target, hash, true); err != nil {
		t.Fatal(err)
	}
	changed, err := rpc.Do(ctx, h.c, protocol.ChannelSetDir, protocol.ChannelDirParams{Channel: a.ID, Dir: target})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Dir != target || changed.ID != a.ID || changed.Name != a.Name || changed.Mode != protocol.ModeAsk {
		t.Fatalf("changed: %+v", changed)
	}
	s, err := h.d.channel(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config().Model != "fake/m2" || !s.Config().ProjectTrusted {
		t.Fatal("target project config was not loaded")
	}
	for dir, value := range map[string]string{old: "OLD_MARKER", target: "NEW_MARKER"} {
		if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.fm.mu.Lock()
	h.fm.steps = []func(model.Request) model.Response{
		func(req model.Request) model.Response {
			if !strings.Contains(req.System, "Working directory: "+target) {
				t.Error("model still saw the old directory")
			}
			return call("read-marker", "read", `{"path":"marker.txt"}`)
		},
		func(req model.Request) model.Response {
			last := req.Messages[len(req.Messages)-1]
			if len(last.Blocks) == 0 || !strings.Contains(last.Blocks[0].Content, "NEW_MARKER") {
				t.Error("relative read did not use the new default directory")
			}
			return text("done")
		},
	}
	h.fm.mu.Unlock()
	if _, err := rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: s.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, s.Root().ID, "read marker", "human:test"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.TurnEnded, s.Root().ID)
	list, err := rpc.Do(ctx, h.c, protocol.ChannelList, protocol.ChannelListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Channels) != 2 {
		t.Fatal("catalog is not global")
	}
	for _, ch := range list.Channels {
		if ch.ID == b.ID && ch.Dir != other {
			t.Fatal("directory change affected another channel")
		}
	}
	rows, err := h.d.Log.Channels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == a.ID && r.Dir != target {
			t.Fatal("index has stale directory")
		}
	}
	before, err := h.d.Log.Read(ctx, a.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) < 3 {
		t.Fatal("directory change was not logged")
	}
	var found bool
	for _, e := range before {
		if e.Type == event.ChannelUpdated {
			var p event.ChannelUpdatedPayload
			_ = e.Decode(&p)
			found = p.Dir != nil && *p.Dir == target
		}
	}
	if !found {
		t.Fatal("missing directory event")
	}
	h.close()
	closed = true
	h2 := newHarness(t, data, &fakeModel{})
	defer h2.close()
	restored, err := rpc.Do(ctx, h2.c, protocol.ChannelResume, protocol.ChannelRef{Channel: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Dir != target || restored.Name != "first" || restored.ID != a.ID {
		t.Fatalf("recovery: %+v", restored)
	}
	// Losing the directory keeps the channel visible and never reassigns it.
	if err := os.Rename(target, target+"-moved"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(target+"-moved", target)
	list, err = rpc.Do(ctx, h2.c, protocol.ChannelList, protocol.ChannelListParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range list.Channels {
		if ch.ID == a.ID && (ch.Dir != target || ch.DirError == "") {
			t.Fatalf("unavailable directory: %+v", ch)
		}
	}
}

func TestDirectoryChangeReplacesTrustPrompt(t *testing.T) {
	setupConfig(t)
	old, target := t.TempDir(), t.TempDir()
	for _, dir := range []string{old, target} {
		if err := os.Mkdir(filepath.Join(dir, ".stavlos"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".stavlos", "stavlos.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx := context.Background()
	ch, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: old})
	if err != nil {
		t.Fatal(err)
	}
	oldPrompts := h.d.esc.Pending(ch.ID)
	if len(oldPrompts) != 1 {
		t.Fatal("initial trust prompt not published")
	}
	changed, err := rpc.Do(ctx, h.c, protocol.ChannelSetDir, protocol.ChannelDirParams{Channel: ch.ID, Dir: target})
	if err != nil {
		t.Fatal(err)
	}
	if !changed.TrustPending {
		t.Fatal("new project bypassed trust")
	}
	ps := h.d.esc.Pending(ch.ID)
	if len(ps) != 1 || ps[0].ID == oldPrompts[0].ID {
		t.Fatalf("stale trust prompt: %+v", ps)
	}
}
