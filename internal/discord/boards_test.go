package discord

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/daemon"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/modelsdev"
	"github.com/nicodes/stavlos/internal/tools"
)

func TestBoardMirrorReplaysInitialAndOfflinePosts(t *testing.T) {
	ctx := context.Background()
	gdir, data, dir := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", gdir)
	t.Setenv("STAVLOS_DATA_DIR", data)
	if err := os.WriteFile(filepath.Join(gdir, "stavlos.json"), []byte(`{"model":"fake/test","reminders":false}`), 0o600); err != nil {
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
	dctx, dcancel := context.WithCancel(ctx)
	d, err := daemon.New(dctx, data, reg)
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(data, "b.sock")
	done := make(chan error, 1)
	go func() { done <- d.Serve(dctx, sock) }()
	defer func() { dcancel(); d.Close(); <-done }()
	home, err := d.CreateChannel(ctx, dir, "", "", "source")
	if err != nil {
		t.Fatal(err)
	}
	root := home.Root().ID
	if _, err := d.BoardCreate(ctx, home.ID, root, "team", nil); err != nil {
		t.Fatal(err)
	}
	boards, err := d.ChannelList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	boardID := ""
	for _, ch := range boards {
		if ch.Board != nil {
			boardID = ch.ID
		}
	}
	if _, err := d.BoardMessage(ctx, home.ID, root, boardID, nil, "Initial board post", tools.KindInfo, nil); err != nil {
		t.Fatal(err)
	}
	api := newAPI()
	cfg := config.Discord{Guild: "guild", Category: "stavlos", Approvers: []string{"operator"}, Dirs: []string{dir}}
	state := filepath.Join(data, "discord-state.json")
	start := func() (*Bridge, func()) {
		b, err := New(cfg, api, "bot", state)
		if err != nil {
			t.Fatal(err)
		}
		bctx, cancel := context.WithCancel(ctx)
		end := make(chan error, 1)
		go func() { end <- b.Run(bctx, sock) }()
		eventually(t, "board ready", func() bool {
			b.mu.RLock()
			defer b.mu.RUnlock()
			w := b.workers[boardID]
			return w != nil && w.ready != nil
		})
		return b, func() {
			cancel()
			select {
			case err := <-end:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("bridge did not stop")
			}
		}
	}
	count := func(text string) int {
		n := 0
		for _, m := range api.snapshot() {
			if strings.Contains(m.Text, text) {
				n++
			}
		}
		return n
	}
	_, stop := start()
	eventually(t, "initial post replayed", func() bool { return count("Initial board post") == 1 })
	stop()
	if _, err := d.BoardMessage(ctx, home.ID, root, boardID, nil, "Offline board post", tools.KindInfo, nil); err != nil {
		t.Fatal(err)
	}
	b, stop := start()
	defer stop()
	eventually(t, "offline post replayed", func() bool { return count("Offline board post") == 1 })
	if count("Initial board post") != 1 {
		t.Fatal("reconnect duplicated old posts")
	}
	b.mu.RLock()
	discordID := b.workers[boardID].discord
	b.mu.RUnlock()
	b.Message(&dg.MessageCreate{Message: &dg.Message{GuildID: "guild", ChannelID: discordID, Author: &dg.User{ID: "operator"}, Content: "Human shared update"}})
	eventually(t, "Discord post persisted", func() bool {
		evs, _ := d.Log.Read(ctx, boardID, 1, 0)
		for _, e := range evs {
			if e.Type == event.ChatPosted {
				var p event.ChatPayload
				_ = e.Decode(&p)
				if p.Text == "Human shared update" && p.From == "human:discord" {
					return true
				}
			}
		}
		return false
	})
	if home.Root().Info().Turn != 0 {
		t.Fatal("unaddressed human post woke the manager")
	}
}
