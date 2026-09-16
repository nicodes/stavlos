package discord

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/daemon"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/modelsdev"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/pkg/client"
)

type bridgeModel struct{ step atomic.Int32 }

func (m *bridgeModel) Complete(context.Context, model.Request, func(model.Delta)) (model.Response, error) {
	var b model.Block
	switch m.step.Add(1) {
	case 1:
		b = model.Block{Type: model.BlockToolUse, ID: "shell", Name: "shell", Input: json.RawMessage(`{"command":"true"}`)}
	case 2:
		b = model.Block{Type: model.BlockToolUse, ID: "reply", Name: "message", Input: json.RawMessage(`{"to":"user","text":"Bridge task finished.","kind":"response"}`)}
	default:
		return model.Response{Blocks: []model.Block{{Type: model.BlockText, Text: "done"}}, StopReason: model.StopEndTurn}, nil
	}
	return model.Response{Blocks: []model.Block{b}, StopReason: model.StopToolUse}, nil
}

type bridgeProvider struct{ m *bridgeModel }

func (p bridgeProvider) Name() string                     { return "fake" }
func (p bridgeProvider) Open(string) (model.Model, error) { return p.m, nil }
func (p bridgeProvider) Variants(string) []string         { return nil }

func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out: " + what)
}

func TestBridgeRoundTripWithDaemon(t *testing.T) {
	gdir, data, dir := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", gdir)
	t.Setenv("STAVLOS_DATA_DIR", data)
	if err := os.WriteFile(filepath.Join(gdir, "stavlos.json"), []byte(`{"model":"fake/test","reminders":false,"sandbox":{"enabled":false},"escalation":{"claimTimeout":"20ms","answerTimeout":"10s"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := modelsdev.Parse([]byte(`{"fake":{"id":"fake","models":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(cat)
	if err := reg.Register(bridgeProvider{&bridgeModel{}}); err != nil {
		t.Fatal(err)
	}
	dctx, dcancel := context.WithCancel(context.Background())
	d, err := daemon.New(dctx, data, reg)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(data, "s.sock")
	served := make(chan error, 1)
	go func() { served <- d.Serve(dctx, socket) }()
	t.Cleanup(func() {
		dcancel()
		if d != nil {
			d.Close()
		}
		if served != nil {
			<-served
		}
	})
	var terminal *client.Client
	eventually(t, "daemon socket", func() bool { terminal, err = client.Dial(socket); return err == nil })
	t.Cleanup(func() { _ = terminal.Close() })
	ctx := context.Background()
	if _, err := client.Do(ctx, terminal, protocol.Attach, protocol.AttachParams{Client: "test-terminal", Tier: protocol.TierInteractive}); err != nil {
		t.Fatal(err)
	}
	ch, err := client.Do(ctx, terminal, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: dir, Name: "integration"})
	if err != nil {
		t.Fatal(err)
	}
	api := newAPI()
	cfg := config.Discord{Guild: "guild", Category: "stavlos", Approvers: []string{"operator"}, Dirs: []string{dir}}
	state := filepath.Join(data, "bridge-prompts.json")
	startBridge := func() (*Bridge, func()) {
		t.Helper()
		b, err := New(cfg, api, "bot", state)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- b.Run(ctx, socket) }()
		stop := func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("bridge shutdown hung")
			}
		}
		eventually(t, "bridge subscription", func() bool {
			b.mu.RLock()
			defer b.mu.RUnlock()
			w := b.workers[ch.ID]
			return w != nil && w.ready != nil
		})
		return b, stop
	}
	b, stop := startBridge()
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	b.mu.RLock()
	dc := b.workers[ch.ID].discord
	b.mu.RUnlock()
	b.Message(&dg.MessageCreate{Message: &dg.Message{GuildID: cfg.Guild, ChannelID: dc, Author: &dg.User{ID: "operator"}, Content: "Run the task"}})
	var promptID string
	eventually(t, "permission escalated to Discord", func() bool {
		for _, m := range api.snapshot() {
			if len(m.Components) == 0 {
				continue
			}
			r := m.Components[0].(dg.ActionsRow)
			if len(r.Components) > 0 {
				promptID, _, _ = parseID(r.Components[0].(dg.Button).CustomID)
				return promptID != ""
			}
		}
		return false
	})
	i := interaction(promptID, "allow")
	i.ChannelID = dc
	b.Interaction(&dg.InteractionCreate{Interaction: i})
	eventually(t, "agent reply via webhook", func() bool {
		for _, m := range api.snapshot() {
			if m.User == "main" && m.Text == "Bridge task finished." {
				return true
			}
		}
		return false
	})
	eventually(t, "prompt collapsed", func() bool { return len(b.store.snapshot()) == 0 })
	for _, m := range api.snapshot() {
		if strings.Contains(m.Text, "You (terminal)") && strings.Contains(m.Text, "Run the task") {
			t.Fatal("Discord post echoed back")
		}
	}
	// A bridge process restart reuses mappings and does not replay old chat.
	stop()
	stop = nil
	api.mu.Lock()
	count := len(api.sends)
	api.mu.Unlock()
	b, stop = startBridge()
	b.mu.RLock()
	dc2 := b.workers[ch.ID].discord
	b.mu.RUnlock()
	if dc2 != dc {
		t.Fatal("restart created another channel")
	}
	api.mu.Lock()
	count2 := len(api.sends)
	api.mu.Unlock()
	if count != count2 {
		t.Fatal("restart replayed historical chat")
	}
	if _, err := client.Do(ctx, terminal, protocol.ChannelPost, protocol.ChannelPostParams{Channel: ch.ID, Text: "terminal follow-up"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "terminal post mirrored", func() bool {
		for _, m := range api.snapshot() {
			if strings.Contains(m.Text, "terminal follow-up") {
				return true
			}
		}
		return false
	})
	// The bridge also survives a daemon restart, reattaches, and resubscribes
	// from a fresh snapshot without duplicating the existing Discord channel.
	b.mu.RLock()
	oldLink := b.live
	b.mu.RUnlock()
	dcancel()
	d.Close()
	<-served
	served = nil
	eventually(t, "old socket removed", func() bool { _, err := os.Stat(socket); return os.IsNotExist(err) })
	dctx, dcancel = context.WithCancel(context.Background())
	defer dcancel()
	d, err = daemon.New(dctx, data, reg)
	if err != nil {
		t.Fatal(err)
	}
	served = make(chan error, 1)
	go func() { served <- d.Serve(dctx, socket) }()
	eventually(t, "bridge reattached after daemon restart", func() bool {
		b.mu.RLock()
		defer b.mu.RUnlock()
		w := b.workers[ch.ID]
		return b.live != nil && b.live != oldLink && w != nil && w.ready == b.live && w.discord == dc
	})
}
