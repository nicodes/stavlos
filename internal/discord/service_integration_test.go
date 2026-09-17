package discord

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/daemon"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/modelsdev"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/pkg/client"
)

func TestDaemonOwnsDiscordBeyondClientLifetime(t *testing.T) {
	serviceConfig(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := t.TempDir()
	socket := filepath.Join(data, "s.sock")
	cat, err := modelsdev.Parse([]byte(`{"fake":{"id":"fake","models":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.New(ctx, data, registry.New(cat))
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(ctx, socket, data)
	s.run = func(ctx context.Context, _ config.Discord, publish func(*Bridge)) error {
		publish(fakeConnected(ctx))
		<-ctx.Done()
		return ctx.Err()
	}
	d.Discord = s
	served := make(chan error, 1)
	go func() { served <- d.Serve(ctx, socket) }()
	t.Cleanup(func() { cancel(); d.Close(); <-served })
	var c *client.Client
	eventually(t, "daemon ready", func() bool { c, err = client.Dial(socket); return err == nil })
	t.Cleanup(func() { _ = c.Close() })
	ch, err := client.Do(ctx, c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(ctx, c, protocol.DiscordConnect, protocol.None{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Discord connected", func() bool { return s.Status().State == "connected" })
	_ = c.Close() // closing the UI's connection must not own the service context
	second, err := client.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	status, err := client.Do(ctx, second, protocol.DiscordStatusMethod, protocol.None{})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "connected" || !status.Enabled {
		t.Fatalf("client detach stopped Discord: %+v", status)
	}
	b, _ := json.Marshal(status)
	if strings.Contains(string(b), "test-token") || strings.Contains(string(b), `"token"`) {
		t.Fatal("status exposed credentials")
	}
	if _, err := client.Do(ctx, second, protocol.DiscordDisconnect, protocol.None{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Discord disconnected", func() bool { return s.Status().State == "disconnected" })
	r, err := client.Do(ctx, second, protocol.AgentTree, protocol.AgentTreeParams{Channel: ch.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Agents) != 1 || r.Agents[0].State != protocol.AgentIdle {
		t.Fatal("disconnect affected agents")
	}
	if _, err := client.Do(ctx, second, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir()}); err != nil {
		t.Fatal("daemon stopped with Discord", err)
	}
}
