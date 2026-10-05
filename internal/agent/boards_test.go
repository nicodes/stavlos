package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

func TestBoardVisibilityDeliveryAndReplyScope(t *testing.T) {
	ctx := context.Background()
	home, h := newTestChannel(t, testConfig{}, &fakeModel{})
	root := home.Root()
	first, err := home.spawn(ctx, root.ID, "general", "first", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := home.spawn(ctx, root.ID, "general", "second", "", "")
	if err != nil {
		t.Fatal(err)
	}
	board := New(h, "board", home.Dir(), home.Config(), "", "")
	defer board.Stop()
	members, err := home.BoardMembers(root.ID, []string{"self", "first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.StartBoard(ctx, "work", event.BoardInfo{Source: home.ID, Owner: root.ID, Members: members}); err != nil {
		t.Fatal(err)
	}
	if board.Root() != nil || len(board.Agents()) != 0 || len(home.Agents()) != 3 {
		t.Fatal("board changed agent ownership")
	}
	// Passive board posts stay queued without starting idle workers.
	if _, err := home.DeliverBoard(ctx, board, root.ID, nil, "announcement", tools.KindInfo, nil, ""); err != nil {
		t.Fatal(err)
	}
	if first.Info().Turn != 0 || second.Info().Turn != 0 {
		t.Fatal("unaddressed post woke an agent")
	}
	// A targeted steer wakes the recipient without opening request debt.
	if _, err := home.DeliverBoard(ctx, board, root.ID, []string{"first"}, "adjust approach", tools.KindSteer, nil, ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, h, func() bool { return first.Info().Turn > 0 && first.Info().State != protocol.AgentRunning })
	if len(root.Info().AwaitingReplies) != 0 || len(first.Info().PendingReplies) != 0 || second.Info().Turn != 0 {
		t.Fatal("steer created debt or woke a bystander")
	}
	if _, err := home.DeliverBoard(ctx, board, root.ID, []string{"first"}, "review", tools.KindRequest, nil, ""); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, h, func() bool { return len(first.Info().PendingReplies) == 1 })
	req := first.Info().PendingReplies[0]
	if req.Channel != board.ID || len(root.Info().AwaitingReplies) != 1 {
		t.Fatalf("public request: %+v", req)
	}
	// Neither a private answer nor an answer in a different board can settle it.
	if _, err := (orchestrator{home}).Message(first.ID, []string{"main"}, "answer", tools.KindResponse, req.ID); err == nil {
		t.Fatal("public debt settled privately")
	}
	if _, err := home.DeliverBoard(ctx, board, first.ID, []string{"main"}, "approved", tools.KindResponse, []string{req.ID}, ""); err != nil {
		t.Fatal(err)
	}
	if len(first.Info().PendingReplies) != 0 || len(root.Info().AwaitingReplies) != 0 {
		t.Fatal("reply did not settle debt")
	}
	// Recovery reconstructs membership without spawning a root.
	var boardEvents []event.Event
	for _, e := range h.all() {
		if e.Channel == board.ID {
			boardEvents = append(boardEvents, e)
		}
	}
	recovered, err := Recover(ctx, h, board.ID, home.Dir(), board.Created, home.Config(), boardEvents)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Stop()
	if recovered.Board() == nil || recovered.Root() != nil || len(recovered.Board().Members) != 3 {
		t.Fatal("board recovery lost metadata")
	}
	if _, err := home.BoardMembers(first.ID, []string{"second"}); err == nil || !strings.Contains(err.Error(), "subtree") {
		t.Fatalf("sibling admitted: %v", err)
	}
}

func TestBoardAppendFailureHasNoDeliveries(t *testing.T) {
	ctx := context.Background()
	home, h := newTestChannel(t, testConfig{}, &fakeModel{})
	root := home.Root()
	child, err := home.spawn(ctx, root.ID, "general", "worker", "", "")
	if err != nil {
		t.Fatal(err)
	}
	board := New(h, "board", home.Dir(), home.Config(), "", "")
	defer board.Stop()
	if err := board.StartBoard(ctx, "work", event.BoardInfo{Source: home.ID, Owner: root.ID, Members: []string{root.ID, child.ID}}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.failType = event.ChatMessage
	h.mu.Unlock()
	if _, err := home.DeliverBoard(ctx, board, root.ID, []string{"worker"}, "request", tools.KindRequest, nil, ""); err == nil {
		t.Fatal("failed append succeeded")
	}
	if child.Info().Queued != 0 || len(root.Info().AwaitingReplies) != 0 {
		t.Fatal("failed append delivered or opened debt")
	}
}
