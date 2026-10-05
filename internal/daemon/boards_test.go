package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
	rpc "github.com/nicodes/stavlos/pkg/client"
)

func TestAgentCreatedBoardIsListedRoutesPostsAndRecovers(t *testing.T) {
	setupConfig(t)
	ctx := context.Background()
	data := t.TempDir()
	fm := &fakeModel{steps: []func(model.Request) model.Response{
		func(model.Request) model.Response {
			return call("c1", "agent", `{"action":"create","archetype":"general","label":"first","task":"prepare"}`)
		},
		func(model.Request) model.Response {
			return call("c2", "agent", `{"action":"create","archetype":"general","label":"second","task":"prepare"}`)
		},
		func(model.Request) model.Response {
			return call("c3", "channel", `{"action":"create","name":"project-board","members":["first","second"]}`)
		},
		func(model.Request) model.Response {
			return call("c4", "message", `{"channel":"project-board","to":["first"],"text":"Use the new contract","expect_response":false}`)
		},
		func(model.Request) model.Response {
			return call("c5", "message", `{"channel":"project-board","text":"Shared progress"}`)
		},
		func(model.Request) model.Response {
			return call("c6", "channel", `{"action":"read","channel":"project-board"}`)
		},
		func(model.Request) model.Response { return text("done") },
	}}
	h := newHarness(t, data, fm)
	closed := false
	defer func() {
		if !closed {
			h.close()
		}
	}()
	home, err := rpc.Do(ctx, h.c, protocol.ChannelCreate, protocol.ChannelCreateParams{Dir: t.TempDir(), Model: "fake/m1"})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := h.d.channel(home.ID)
	if err != nil {
		t.Fatal(err)
	}
	root := runtime.Root()
	_, err = rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: home.ID, From: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Send(ctx, root.ID, "create a board", "human:test"); err != nil {
		t.Fatal(err)
	}
	for {
		e := h.waitFor(event.ToolFinished, root.ID)
		var p event.ToolFinishedPayload
		_ = e.Decode(&p)
		if p.IsError {
			t.Fatalf("%s: %s", p.CallID, p.Output)
		}
		if p.CallID == "c6" {
			if !strings.Contains(p.Output, "Shared progress") || !strings.Contains(p.Output, "Use the new contract") {
				t.Fatalf("public history: %s", p.Output)
			}
			break
		}
	}
	list, err := rpc.Do(ctx, h.c, protocol.ChannelList, protocol.ChannelListParams{})
	if err != nil {
		t.Fatal(err)
	}
	var board protocol.ChannelInfo
	for _, ch := range list.Channels {
		if ch.Name == "project-board" {
			board = ch
		}
	}
	if board.Board == nil || board.Board.Source != home.ID || len(board.Board.Members) != 3 {
		t.Fatalf("listed board: %+v", board)
	}
	members, err := rpc.Do(ctx, h.c, protocol.AgentTree, protocol.AgentTreeParams{Channel: board.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(members.Agents) != 3 || members.Agents[0].ID != root.ID || members.Agents[1].Channel != home.ID {
		t.Fatalf("members changed contexts: %+v", members)
	}
	public, err := h.d.Log.Read(ctx, board.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(public) != 3 {
		t.Fatalf("board events: %+v", public)
	}
	// Public human posts use the same route from the browser and Discord.
	post, err := rpc.Do(ctx, h.c, protocol.ChannelPost, protocol.ChannelPostParams{Channel: board.ID, Text: "@first Review this"})
	if err != nil || len(post.To) != 1 {
		t.Fatalf("human request %v %+v", err, post)
	}
	if _, err := h.d.BoardMessage(ctx, home.ID, root.ID, board.ID, []string{"missing"}, "bad", tools.KindRequest, nil); err == nil {
		t.Fatal("non-member recipient admitted")
	}
	if _, err := h.d.BoardMessage(ctx, home.ID, "intruder", board.ID, nil, "bad", tools.KindInfo, nil); err == nil {
		t.Fatal("non-member sender admitted")
	}
	h.close()
	closed = true
	recovered := newHarness(t, data, &fakeModel{})
	defer recovered.close()
	ch, err := recovered.d.channel(board.ID)
	if err != nil || ch.Board() == nil || ch.Root() != nil {
		t.Fatalf("recovered board: %v", err)
	}
	history, err := recovered.d.BoardRead(ctx, home.ID, root.ID, board.ID, 1, 100)
	if err != nil || !strings.Contains(history, "Shared progress") {
		t.Fatalf("recovered history: %v %s", err, history)
	}
	if _, err := recovered.d.BoardCreate(ctx, home.ID, root.ID, "project-board", nil); err == nil {
		t.Fatal("duplicate channel name admitted")
	}
}

func TestBoardTurnLimitReportsRemainPublic(t *testing.T) {
	setupConfig(t)
	ctx := context.Background()
	roles := filepath.Join(os.Getenv("STAVLOS_CONFIG_DIR"), "agents")
	if err := os.MkdirAll(roles, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"manager": "---\ndescription: Manager\ntype: primary\nspawn: [limited]\n---\nManage.\n",
		"limited": "---\ndescription: Limited\ntype: subagent\nmax_turns: 1\n---\nYou are a subagent.\n",
	} {
		if err := os.WriteFile(filepath.Join(roles, name+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, t.TempDir(), &fakeModel{})
	defer h.close()
	home, err := h.d.CreateChannel(ctx, t.TempDir(), "fake/m1", "manager", "source")
	if err != nil {
		t.Fatal(err)
	}
	root := home.Root().ID
	worker, err := home.SpawnFromClient(ctx, root, "limited", "worker", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.BoardCreate(ctx, home.ID, root, "limits", []string{"worker"}); err != nil {
		t.Fatal(err)
	}
	boards, _ := h.d.ChannelList(ctx, "", false)
	id := ""
	for _, ch := range boards {
		if ch.Board != nil {
			id = ch.ID
		}
	}
	_, err = rpc.Do(ctx, h.c, protocol.Subscribe, protocol.SubscribeParams{Channel: home.ID, From: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.BoardMessage(ctx, home.ID, root, id, []string{"worker"}, "first", tools.KindRequest, nil); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.TurnEnded, worker)
	if _, err := h.d.BoardMessage(ctx, home.ID, root, id, []string{"worker"}, "second", tools.KindRequest, nil); err != nil {
		t.Fatal(err)
	}
	h.waitFor(event.TurnEnded, worker)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		events, _ := h.d.Log.Read(ctx, id, 1, 0)
		answers := 0
		for _, e := range events {
			var p event.ChatPayload
			_ = e.Decode(&p)
			if e.Type == event.ChatMessage && p.Kind == tools.KindResponse && strings.Contains(p.Text, "turn limit") {
				answers++
			}
		}
		if answers == 2 && len(home.Root().Info().AwaitingReplies) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("turn-limit answers were not posted and settled on the shared board")
}
