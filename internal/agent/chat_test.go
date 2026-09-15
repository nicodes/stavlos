package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
)

// TestPostDeliversByMention: a chat message reaches each agent it
// mentions once, the root when it mentions none, and is refused whole when
// a mention names nobody.
func TestPostDeliversByMention(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"look"}`)),
		reply(call("c2", "agent_create", `{"archetype":"general","label":"lookout","task":"watch"}`)),
		reply(text("delegated")),
	}}
	s, h := newTestSession(t, testConfig{}, fm)
	root := s.Root()
	runTurn(t, s, h, "delegate")
	waitUntil(t, h, func() bool { return len(s.Agents()) == 3 && s.Busy() == 0 })
	scout, lookout := s.Agents()[1], s.Agents()[2]
	ctx := context.Background()

	steers := func(agent string) (out []string) {
		for _, e := range h.ofType(event.SteerReceived, agent) {
			var p event.TextPayload
			_ = e.Decode(&p)
			out = append(out, p.Source+" "+p.Text)
		}
		return out
	}
	to, err := s.Post(ctx, "@Scout and @lookout, then @scout again", "human:test")
	if err != nil || !reflect.DeepEqual(to, []string{"scout", "lookout"}) {
		t.Fatalf("to %v err %v", to, err)
	}
	if got := steers(scout.ID); len(got) != 1 || got[0] != "human:test @Scout and @lookout, then @scout again" {
		t.Fatalf("scout steers %q", got)
	}
	if len(steers(lookout.ID)) != 1 || len(steers(root.ID)) != 0 {
		t.Fatalf("lookout %q root %q", steers(lookout.ID), steers(root.ID))
	}
	if to, err := s.Post(ctx, "mail me@example.com", "human:test"); err != nil || !reflect.DeepEqual(to, []string{"main"}) || len(steers(root.ID)) != 1 {
		t.Fatalf("no mention goes to the root: %v %v", to, err)
	}
	if _, err := s.Post(ctx, "@scout and @ghost", "human:test"); err == nil || !strings.Contains(err.Error(), "@ghost") || len(steers(scout.ID)) != 1 {
		t.Fatalf("an unknown mention refuses the message: %v", err)
	}
	var posts [][]string
	for _, e := range h.ofType(event.ChatPosted, "") {
		var p event.ChatPayload
		_ = e.Decode(&p)
		posts = append(posts, p.To)
	}
	if !reflect.DeepEqual(posts, [][]string{{"scout", "lookout"}, {"main"}}) {
		t.Fatalf("chat.posted log: %v", posts)
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
	s, h := newTestSession(t, testConfig{}, fm)
	root := s.Root()
	ctx := context.Background()
	sent := func(n int) bool {
		return len(h.ofType(event.MessageToUser, root.ID)) == n && root.StateOf() == StateIdle
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
	for _, e := range h.ofType(event.MessageToUser, root.ID) {
		var p event.ChatPayload
		_ = e.Decode(&p)
		answers = append(answers, p.Post)
	}
	if len(posts) != 2 || posts[0] == "" || !reflect.DeepEqual(answers, []string{posts[0], posts[1], ""}) {
		t.Fatalf("posts %q answers %q", posts, answers)
	}
}
