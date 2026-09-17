package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

func TestCustomCommandsUseCurrentAgentAndTrustedProject(t *testing.T) {
	setupConfig(t)
	work := t.TempDir()
	file := filepath.Join(work, ".stavlos", "commands", "cmd.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	const body = "Run the full test suite with coverage.\nShow the failures."
	if err := os.WriteFile(file, []byte("---\ndescription: Run tests with coverage\n---\n\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	ctx := context.Background()
	ch, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: work})
	if err != nil {
		t.Fatal(err)
	}
	list, err := rpc.Do(ctx, h.c, protocol.CommandList, protocol.ChannelRef{Channel: ch.ID})
	if err != nil || len(list.Commands) != 0 {
		t.Fatalf("untrusted commands exposed: %+v %v", list, err)
	}
	if _, err := rpc.Do(ctx, h.c, protocol.CommandRun, protocol.CommandRunParams{Channel: ch.ID, Name: "cmd"}); err == nil {
		t.Fatal("untrusted command executed")
	}
	_, hash, err := config.ProjectHash(work)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rpc.Do(ctx, h.c, protocol.TrustReply, protocol.TrustReplyParams{Dir: work, Hash: hash, Trust: true}); err != nil {
		t.Fatal(err)
	}
	list, err = rpc.Do(ctx, h.c, protocol.CommandList, protocol.ChannelRef{Channel: ch.ID})
	if err != nil || len(list.Commands) != 1 || list.Commands[0].Name != "cmd" || list.Commands[0].Description != "Run tests with coverage" {
		t.Fatalf("commands: %+v %v", list, err)
	}
	agents, err := tree(ctx, h.c, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	root := agents[0]
	for _, target := range []string{"", root.ID} {
		if _, err := rpc.Do(ctx, h.c, protocol.CommandRun, protocol.CommandRunParams{Channel: ch.ID, Name: "cmd", Agent: target}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := h.d.Log.Read(ctx, ch.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	posts, inputs := 0, 0
	for _, ev := range events {
		if ev.Type == event.ChatPosted {
			var p event.ChatPayload
			_ = ev.Decode(&p)
			if p.Text == body {
				posts++
			}
		}
		if ev.Type == event.InputQueued {
			var p event.Input
			_ = ev.Decode(&p)
			if p.Text == body && ev.Agent == root.ID {
				inputs++
			}
		}
	}
	if posts != 1 || inputs != 2 {
		t.Fatalf("command delivery: %d posts, %d inputs", posts, inputs)
	}
	agents, _ = tree(ctx, h.c, ch.ID)
	if agents[0].Model != root.Model || agents[0].Role != root.Role {
		t.Fatal("command changed the agent or model")
	}
	if _, err := rpc.Do(ctx, h.c, protocol.CommandRun, protocol.CommandRunParams{Channel: ch.ID, Name: "cmd", Agent: "foreign-agent"}); err == nil {
		t.Fatal("command accepted a target outside its channel")
	}
	if err := os.WriteFile(file, []byte("---\ndescription: Changed\n---\nChanged body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rpc.Do(ctx, h.c, protocol.CommandRun, protocol.CommandRunParams{Channel: ch.ID, Name: "cmd"}); err == nil {
		t.Fatal("edited command bypassed the project trust hash")
	}
}
