package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

func sentRequestID(t *testing.T, result string) string {
	t.Helper()
	_, id, ok := strings.Cut(result, "; request_id: ")
	if !ok || id == "" {
		t.Fatalf("request result lacks ID: %s", result)
	}
	return id
}

func TestExplicitRepliesSettleOnlyReferencedRequests(t *testing.T) {
	s, h := newTestChannel(t, testConfig{}, &fakeModel{})
	a, err := s.spawn(context.Background(), s.Root().ID, "general", "scout", "", "")
	if err != nil {
		t.Fatal(err)
	}
	o, root := orchestrator{s}, s.Root()
	request := func(text string) string {
		t.Helper()
		out, err := o.Message(root.ID, []string{a.Name()}, text, tools.KindRequest)
		if err != nil {
			t.Fatal(err)
		}
		return sentRequestID(t, out)
	}
	first, second := request("inspect tests"), request("inspect docs")
	waitUntil(t, h, func() bool { return len(a.Info().PendingReplies) == 2 })
	if _, err := o.Message(a.ID, []string{root.Name()}, "working on it", tools.KindInfo); err != nil {
		t.Fatal(err)
	}
	clarification, err := o.Message(a.ID, []string{root.Name()}, "which version?", tools.KindRequest)
	if err != nil {
		t.Fatal(err)
	}
	clarificationID := sentRequestID(t, clarification)
	waitUntil(t, h, func() bool { return len(root.Info().PendingReplies) == 1 })
	if len(a.Info().PendingReplies) != 2 || len(root.Info().AwaitingReplies) != 2 {
		t.Fatal("ordinary messages cleared outstanding requests")
	}
	for _, ids := range [][]string{nil, {"missing"}, {first, "missing"}, {first, first}} {
		if _, err := o.Message(a.ID, []string{root.Name()}, "invalid", tools.KindResponse, ids...); err == nil {
			t.Fatalf("invalid response accepted: %v", ids)
		}
		if len(a.Info().PendingReplies) != 2 {
			t.Fatal("failed validation partially settled a request")
		}
	}
	if _, err := o.Message(a.ID, []string{"user"}, "wrong recipient", tools.KindResponse, first); err == nil {
		t.Fatal("request answered to the wrong recipient")
	}
	if _, err := o.Message(a.ID, []string{root.Name()}, "tests done", tools.KindResponse, first); err != nil {
		t.Fatal(err)
	}
	if due := a.Info().PendingReplies; len(due) != 1 || due[0].ID != second {
		t.Fatalf("partial response: %+v", due)
	}
	if len(root.Info().AwaitingReplies) != 1 || len(a.Info().AwaitingReplies) != 1 {
		t.Fatal("response cleared an unrelated wait")
	}
	if _, err := o.Message(a.ID, []string{root.Name()}, "duplicate", tools.KindResponse, first); err == nil {
		t.Fatal("already answered ID accepted")
	}
	if _, err := o.Message(root.ID, []string{a.Name()}, "latest version", tools.KindResponse, clarificationID); err != nil {
		t.Fatal(err)
	}
	third := request("inspect build")
	waitUntil(t, h, func() bool { return len(a.Info().PendingReplies) == 2 })
	if _, err := o.Message(a.ID, []string{root.Name()}, "docs and build done", tools.KindResponse, second, third); err != nil {
		t.Fatal(err)
	}
	if len(a.Info().PendingReplies) != 0 || len(root.Info().AwaitingReplies) != 0 {
		t.Fatal("batched explicit response did not settle both requests")
	}
}

func TestBroadcastReplyIsIndependentPerAgent(t *testing.T) {
	s, h := newTestChannel(t, testConfig{}, &fakeModel{})
	ctx := context.Background()
	a, err := s.spawn(ctx, s.Root().ID, "general", "scout", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.spawn(ctx, s.Root().ID, "general", "reviewer", "", "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.spawn(ctx, s.Root().ID, "general", "outsider", "", "")
	if err != nil {
		t.Fatal(err)
	}
	o := orchestrator{s}
	out, err := o.Message(s.Root().ID, []string{a.Name(), b.Name()}, "review", tools.KindRequest)
	if err != nil {
		t.Fatal(err)
	}
	id := sentRequestID(t, out)
	waitUntil(t, h, func() bool { return len(a.Info().PendingReplies) == 1 && len(b.Info().PendingReplies) == 1 })
	if a.Info().PendingReplies[0].ID != id || b.Info().PendingReplies[0].ID != id {
		t.Fatal("broadcast request ID was not shared")
	}
	if _, err := o.Message(c.ID, []string{s.Root().Name()}, "not mine", tools.KindResponse, id); err == nil {
		t.Fatal("another agent answered a foreign request")
	}
	if _, err := o.Message(a.ID, []string{s.Root().Name()}, "done", tools.KindResponse, id); err != nil {
		t.Fatal(err)
	}
	if len(a.Info().PendingReplies) != 0 || len(b.Info().PendingReplies) != 1 || len(s.Root().Info().AwaitingReplies) != 1 {
		t.Fatal("broadcast response settled another recipient")
	}
	if err := s.Kill(b.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.Root().Info().AwaitingReplies) != 0 {
		t.Fatal("killed recipient left an impossible wait")
	}
}

func TestHumanRequestsAreIndependentAndInfoDoesNotAnswer(t *testing.T) {
	s, h := newTestChannel(t, testConfig{}, &fakeModel{})
	root := s.Root()
	for _, text := range []string{"first", "second"} {
		if _, err := s.Post(context.Background(), text, "human:test"); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, h, func() bool { return len(root.Info().PendingReplies) == 2 })
	requests := root.Info().PendingReplies
	o := orchestrator{s}
	if _, err := o.Message(root.ID, []string{"user"}, "an update", tools.KindInfo); err != nil {
		t.Fatal(err)
	}
	if len(root.Info().PendingReplies) != 2 {
		t.Fatal("info settled human requests")
	}
	if _, err := o.Message(root.ID, []string{"user"}, "second answer", tools.KindResponse, requests[1].ID); err != nil {
		t.Fatal(err)
	}
	if due := root.Info().PendingReplies; len(due) != 1 || due[0].ID != requests[0].ID {
		t.Fatal("human requests collapsed by sender")
	}
	var p event.ChatPayload
	posts := h.ofType(event.ChatMessage, root.ID)
	_ = posts[len(posts)-1].Decode(&p)
	if p.Kind != tools.KindResponse || len(p.ReplyTo) != 1 || p.Post != requests[1].Post {
		t.Fatalf("response associations not logged: %+v", p)
	}
}

func TestLegacyResponsesOnlySettleLegacyRequests(t *testing.T) {
	cs := newChannelState("m", "general")
	apply := func(agent string, typ event.Type, payload any) {
		cs.apply(event.Event{Agent: agent, Type: typ, Payload: event.MustPayload(payload)}, &effects{})
	}
	apply("main", event.AgentSpawned, event.AgentSpawnedPayload{ID: "main", Name: "main"})
	apply("scout", event.AgentSpawned, event.AgentSpawnedPayload{ID: "scout", Name: "scout"})
	apply("scout", event.InputQueued, event.Input{ID: "old", Kind: event.InputRequest, From: "main", FromName: "main", Text: "old request"})
	apply("scout", event.InputQueued, event.Input{ID: "new-input", RequestID: "new", Kind: event.InputRequest, From: "main", FromName: "main", Text: "new request"})
	apply("scout", event.InputTaken, event.InputTakenPayload{IDs: []string{"old", "new-input"}})
	apply("main", event.InputQueued, event.Input{ID: "legacy-response", Kind: event.InputResponse, From: "scout", FromName: "scout", Text: "old response"})
	if due := cs.agents["scout"].pendingReplies(); len(due) != 1 || due[0].ID != "new" || cs.agents["main"].awaiting["scout"] != 1 {
		t.Fatalf("legacy replay changed new obligations: %+v", due)
	}
	apply("main", event.InputQueued, event.Input{ID: "explicit-response", Kind: event.InputResponse, From: "scout", ReplyTo: []string{"new"}})
	if len(cs.agents["scout"].owed) != 0 || len(cs.agents["main"].awaiting) != 0 {
		t.Fatal("explicit replay failed to settle its request")
	}
}
