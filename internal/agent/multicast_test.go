package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/project"
	"github.com/nicodes/stavlos/internal/tools"
)

func TestMessageDeliversFullRecipientListOnce(t *testing.T) {
	s, h := newTestChannel(t, testConfig{}, &fakeModel{})
	ctx := context.Background()
	a, err := s.spawn(ctx, s.Root().ID, "general", "scout", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.spawn(ctx, s.Root().ID, "general", "reader", "", "")
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"to": []string{a.ID, "@human", b.Name(), "@SCOUT"}, "text": "shared body", "kind": "info"})
	r := tools.Builtin()["message"].Run(ctx, input, &tools.Env{Agent: s.Root().ID, Orch: orchestrator{s}})
	if r.IsError {
		t.Fatal(r.Output)
	}
	want := []string{"scout", "user", "reader"}
	for _, agent := range []*Agent{a, b} {
		evs := h.ofType(event.InputQueued, agent.ID)
		if len(evs) != 1 {
			t.Fatalf("duplicate/missing delivery: %d", len(evs))
		}
		var in event.Input
		_ = evs[0].Decode(&in)
		if in.Kind != event.InputInfo || in.Text != "shared body" || !reflect.DeepEqual(in.To, want) {
			t.Fatalf("delivery: %+v", in)
		}
		if text := project.InputText(in, event.JobFinishedPayload{}); !strings.Contains(text, "recipients: @scout @user @reader") || !strings.Contains(text, "another agent's output") {
			t.Fatalf("model context: %s", text)
		}
	}
	posts := h.ofType(event.ChatMessage, s.Root().ID)
	var post event.ChatPayload
	if len(posts) != 1 || posts[0].Decode(&post) != nil || !reflect.DeepEqual(post.To, want) {
		t.Fatalf("human delivery: %+v", posts)
	}
	before := len(h.all())
	for _, to := range [][]string{{"user", "scout", "missing"}, {"scout", s.Root().ID}, {}} {
		if _, err := (orchestrator{s}).Message(s.Root().ID, to, "must not send", tools.KindRequest); err == nil {
			t.Fatalf("invalid recipients accepted: %v", to)
		}
		if len(h.all()) != before {
			t.Fatal("failed send partially delivered")
		}
	}
	legacy := tools.Builtin()["message"].Run(ctx, json.RawMessage(`{"to":"user","text":"legacy string"}`), &tools.Env{Agent: s.Root().ID, Orch: orchestrator{s}})
	if legacy.IsError {
		t.Fatal("legacy calls stopped working: " + legacy.Output)
	}
	if err := s.Kill(b.ID); err != nil {
		t.Fatal(err)
	}
	before = len(h.all())
	if _, err := (orchestrator{s}).Message(s.Root().ID, []string{"user", a.Name(), b.Name()}, "must not send", tools.KindInfo); err == nil || len(h.all()) != before {
		t.Fatal("killed recipient allowed a partial delivery")
	}
}

func TestMulticastRequestsWaitForEachRecipient(t *testing.T) {
	s, h := newTestChannel(t, testConfig{}, &fakeModel{})
	ctx := context.Background()
	a, err := s.spawn(ctx, s.Root().ID, "general", "scout", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.spawn(ctx, s.Root().ID, "general", "reader", "", "")
	if err != nil {
		t.Fatal(err)
	}
	o := orchestrator{s}
	if _, err := o.Message(s.Root().ID, []string{a.Name(), b.Name()}, "report back", tools.KindRequest); err != nil {
		t.Fatal(err)
	}
	if len(s.Root().Info().Awaiting) != 2 {
		t.Fatal("sender must await both recipients")
	}
	waitUntil(t, h, func() bool { return len(a.Info().Due) > 0 && len(b.Info().Due) > 0 })
	if _, err := o.Message(a.ID, []string{s.Root().Name()}, "first result", tools.KindResponse, a.Info().PendingReplies[0].ID); err != nil {
		t.Fatal(err)
	}
	if waiting := s.Root().Info().Awaiting; len(waiting) != 1 || waiting[0] != b.ID {
		t.Fatalf("first reply cleared another agent's wait: %v", waiting)
	}
	if _, err := o.Message(b.ID, []string{s.Root().Name()}, "second result", tools.KindResponse, b.Info().PendingReplies[0].ID); err != nil {
		t.Fatal(err)
	}
	if len(s.Root().Info().Awaiting) != 0 {
		t.Fatal("last reply did not settle the send")
	}
}

func TestNoReplyAliasPersistsAsInfo(t *testing.T) {
	s, h := newTestChannel(t, testConfig{}, &fakeModel{})
	ctx := context.Background()
	r := tools.Builtin()["message"].Run(ctx, json.RawMessage(`{"to":["user"],"text":"fyi","kind":"no_reply"}`), &tools.Env{Agent: s.Root().ID, Orch: orchestrator{s}})
	if r.IsError {
		t.Fatal(r.Output)
	}
	posts := h.ofType(event.ChatMessage, s.Root().ID)
	var p event.ChatPayload
	if len(posts) != 1 || posts[0].Decode(&p) != nil || p.Kind != tools.KindInfo || p.Text != "fyi" {
		t.Fatalf("no_reply should persist as info: %+v", p)
	}
}
