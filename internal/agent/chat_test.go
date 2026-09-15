package agent

import (
	"context"
	"errors"
	"github.com/nicodes/stavlos/internal/model"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
)

// TestPostDeliversByMention: the @names at the front of a chat message say
// who gets it, and what follows is delivered to each exactly as written; a
// message with none goes to the root; a leading name that is nobody refuses
// it whole.
func TestPostDeliversByMention(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"look"}`)),
		reply(call("c2", "agent_create", `{"archetype":"general","label":"lookout","task":"watch"}`)),
		reply(text("delegated")),
	}}
	s, h := newTestChannel(t, testConfig{}, fm)
	root := s.Root()
	runTurn(t, s, h, "delegate")
	waitUntil(t, h, func() bool { return len(s.Agents()) == 3 && busy(s) == 0 })
	scout, lookout := s.Agents()[1], s.Agents()[2]
	ctx := context.Background()

	steers := func(agent string) (out []string) {
		for _, in := range inputsOf(h, event.InputSteer, agent) {
			if in.Post == "" {
				t.Fatalf("a post's steer names its post: %+v", in)
			}
			out = append(out, in.Text)
		}
		return out
	}
	to, err := s.Post(ctx, "@Scout @lookout @scout check the tests, then ping @main", "human:test")
	if err != nil || !reflect.DeepEqual(to, []string{"scout", "lookout"}) {
		t.Fatalf("to %v err %v", to, err)
	}
	if got := steers(scout.ID); len(got) != 1 || got[0] != "check the tests, then ping @main" {
		t.Fatalf("scout steers %q", got)
	}
	if len(steers(lookout.ID)) != 1 || len(steers(root.ID)) != 0 {
		t.Fatalf("lookout %q root %q", steers(lookout.ID), steers(root.ID))
	}
	if to, err := s.Post(ctx, "hi @scout, mail me@example.com", "human:test"); err != nil || !reflect.DeepEqual(to, []string{"main"}) || steers(root.ID)[0] != "hi @scout, mail me@example.com" {
		t.Fatalf("no leading name goes to the root, untouched: %v %v %q", to, err, steers(root.ID))
	}
	if _, err := s.Post(ctx, "@scout @ghost hi", "human:test"); err == nil || !strings.Contains(err.Error(), "@ghost") || len(steers(scout.ID)) != 1 {
		t.Fatalf("an unknown leading name refuses the message: %v", err)
	}
	if _, err := s.Post(ctx, "@scout", "human:test"); err == nil {
		t.Fatal("names with no message are refused")
	}
	var posts []string
	for _, e := range h.ofType(event.ChatPosted, "") {
		var p event.ChatPayload
		_ = e.Decode(&p)
		posts = append(posts, strings.Join(p.To, ",")+": "+p.Text)
	}
	if strings.Join(posts, " | ") != "scout,lookout: check the tests, then ping @main | main: hi @scout, mail me@example.com" {
		t.Fatalf("chat.posted log: %q", posts)
	}
}

// TestMessageAnswersTheLatestPost: a message to the human names the chat
// post the agent last took in, and none after the human wrote to it
// directly.
func TestMessageAnswersTheLatestPost(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(call("c1", "message", `{"to":"user","text":"one"}`)), reply(text("n")),
		reply(call("c2", "message", `{"to":"user","text":"two"}`)), reply(text("n")),
		reply(call("c3", "message", `{"to":"user","text":"three"}`)), reply(text("n")),
	}}
	s, h := newTestChannel(t, testConfig{}, fm)
	root := s.Root()
	ctx := context.Background()
	sent := func(n int) bool {
		return len(h.ofType(event.ChatMessage, root.ID)) == n && stateOf(root) == StateIdle
	}
	if _, err := s.Post(ctx, "first", "human:test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, h, func() bool { return sent(1) })
	if _, err := s.Post(ctx, "second", "human:test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, h, func() bool { return sent(2) })
	if err := root.Steer(ctx, "direct", "human:test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, h, func() bool { return sent(3) })

	var posts []string
	for _, e := range h.ofType(event.ChatPosted, "") {
		var p event.ChatPayload
		_ = e.Decode(&p)
		posts = append(posts, p.ID)
	}
	var answers []string
	for _, e := range h.ofType(event.ChatMessage, root.ID) {
		var p event.ChatPayload
		_ = e.Decode(&p)
		answers = append(answers, p.Post)
	}
	if len(posts) != 2 || posts[0] == "" || !reflect.DeepEqual(answers, []string{posts[0], posts[1], ""}) {
		t.Fatalf("posts %q answers %q", posts, answers)
	}
}

// TestNoReplyNote: a message of kind info leaves no debt and no wait,
// does not wake an idle recipient, survives a restart, and reaches the
// recipient with its next turn, marked as needing no reply.
func TestNoReplyNote(t *testing.T) {
	fm := &fakeModel{
		steps: []step{
			reply(call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"look"}`)),
			reply(text("delegated")),
			reply(call("c2", "message", `{"to":"scout","text":"thanks","kind":"info"}`)), // woken by scout's answer
			reply(text("noted")),
		},
		childSteps: []step{
			reply(call("k1", "message", `{"to":"main","text":"found it","kind":"response"}`)),
			reply(text("done")),
		},
	}
	s, h := newTestChannel(t, testConfig{}, fm)
	root := s.Root()
	runTurn(t, s, h, "delegate")
	waitUntil(t, h, func() bool { return len(s.Agents()) == 2 })
	child := s.Agents()[1]
	waitUntil(t, h, func() bool {
		return len(inputsOf(h, event.InputInfo, child.ID)) == 1 && stateOf(root) == StateIdle && stateOf(child) == StateIdle
	})
	time.Sleep(50 * time.Millisecond) // the note must not start a turn
	if in := child.Info(); in.Turn != 1 || len(in.Due) != 0 || in.Queued != 1 || len(root.Info().Awaiting) != 0 {
		t.Fatalf("child %+v root awaiting %v", in, root.Info().Awaiting)
	}
	fin := finished(h, root.ID)
	if out := fin[len(fin)-1].Output; !strings.HasPrefix(out, "info delivered to scout") {
		t.Fatalf("tool result %q", out)
	}
	s.Stop()

	h2 := newFakeHost(&fakeModel{childSteps: []step{func(_ context.Context, req model.Request) (model.Response, error) {
		for _, m := range req.Messages {
			for _, b := range m.Blocks {
				if strings.Contains(b.Text, "[message from agent main, no reply needed") && strings.Contains(b.Text, "thanks") {
					return text("ok"), nil
				}
			}
		}
		return text(""), errors.New("the note should reach the next turn")
	}}})
	s2, err := Recover(context.Background(), h2, s.ID, s.Dir, s.Created, s.Config(), h.all())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Stop)
	c2, _ := s2.Agent(child.ID)
	if c2.Info().Queued != 1 {
		t.Fatalf("the note should survive a restart: %+v", c2.Info())
	}
	if err := c2.Prompt(context.Background(), "anything else?", "human:test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, h2, func() bool { return c2.Info().Turn == 2 && stateOf(c2) == StateIdle })
	var kinds []string
	for _, m := range takenIn(append(h.all(), h2.all()...), child.ID) {
		if m.Turn == 2 {
			kinds = append(kinds, string(m.Kind))
		}
	}
	if strings.Join(kinds, ",") != "info,prompt" || c2.Info().Queued != 0 || c2.Info().LastError != "" {
		t.Fatalf("turn inputs %v, info %+v", kinds, c2.Info())
	}
}
