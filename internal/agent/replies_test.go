package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

// postTurn posts text in the channel chat (to the main agent) and waits for
// the turn it starts to end: the human's post is owed a reply.
func postTurn(t *testing.T, s *Channel, h *fakeHost, text string) {
	t.Helper()
	if _, err := s.Post(context.Background(), text, "human:test"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, event.TurnEnded, s.Root().ID)
}

// TestNudgesUntilReplyOrCap: every turn that ends owing the human a reply (for
// a channel chat post) is
// followed by a reminder turn, up to maxNudges in a row; the reply stays
// due, nothing is injected into the system prompt, and a new message resets
// the count.
func TestNudgesUntilReplyOrCap(t *testing.T) {
	var reminder string
	fm := &fakeModel{steps: []step{
		reply(text("I looked; all good")),
		func(_ context.Context, req model.Request) (model.Response, error) {
			reminder = lastUserText(req)
			return text("still just notes"), nil
		},
	}}
	s, h := newTestChannel(t, testConfig{reminders: true}, fm)
	root := s.Root()
	postTurn(t, s, h, "check it")
	waitUntil(t, h, func() bool { return root.Info().Turn == 1+maxNudges && stateOf(root) == StateIdle })
	time.Sleep(50 * time.Millisecond) // nothing follows the cap
	if in := root.Info(); in.Turn != 1+maxNudges || !reflect.DeepEqual(in.Due, []string{"user"}) {
		t.Fatalf("turn %d due %v", in.Turn, in.Due)
	}
	if q := reminders(h, root.ID); len(q) != maxNudges || !reflect.DeepEqual(q[0], []string{"user"}) {
		t.Fatalf("reminders %v", q)
	}
	if !strings.Contains(reminder, "without replying to user") || !strings.Contains(reminder, "message (to: user, kind: response)") {
		t.Fatalf("reminder: %q", reminder)
	}
	reqs := fm.requests()
	if strings.Contains(reqs[len(reqs)-1].System, "Replies due") || strings.Contains(reqs[len(reqs)-1].System, "owe a reply to") {
		t.Fatal("what is owed is not injected into the system prompt")
	}
	if _, err := s.Post(context.Background(), "again", "human:test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, h, func() bool { return root.Info().Turn == 2+2*maxNudges && stateOf(root) == StateIdle })
	time.Sleep(50 * time.Millisecond)
	if q := reminders(h, root.ID); len(q) != 2*maxNudges || root.Info().Turn != 2+2*maxNudges {
		t.Fatalf("a new message resets the count: reminders %d turn %d", len(q), root.Info().Turn)
	}
}

// TestNudgeGetsAReply: a reminder turn that replies ends the nudging.
func TestNudgeGetsAReply(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(text("notes")),
		reply(call("c1", "message", `{"to":"user","text":"all good"}`)),
		reply(text("done")),
	}}
	s, h := newTestChannel(t, testConfig{reminders: true}, fm)
	root := s.Root()
	postTurn(t, s, h, "check it")
	waitUntil(t, h, func() bool { return len(h.ofType(event.ChatMessage, root.ID)) == 1 && stateOf(root) == StateIdle })
	time.Sleep(50 * time.Millisecond)
	if in := root.Info(); in.Turn != 2 || len(in.Due) != 0 || len(reminders(h, root.ID)) != 1 {
		t.Fatalf("turn %d due %v log:\n%s", in.Turn, in.Due, h.dump())
	}
}

// TestReplyNeedsNoReminder: a message to the human settles its prompt.
func TestReplyNeedsNoReminder(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(call("c1", "message", `{"to":"user","text":"all good"}`)),
		reply(text("notes")),
	}}
	s, h := newTestChannel(t, testConfig{reminders: true}, fm)
	root := s.Root()
	postTurn(t, s, h, "check it")
	waitUntil(t, h, func() bool { return stateOf(root) == StateIdle })
	if due := root.Info().Due; len(due) != 0 || len(reminders(h, root.ID)) != 0 || root.Info().Turn != 1 {
		t.Fatalf("due %v, log:\n%s", due, h.dump())
	}
}

// TestNoNudgeWhileWaiting: a turn that ends waiting on a child is not
// nudged; the child's answer wakes the agent, and a turn that then ends
// without replying is.
func TestNoNudgeWhileWaiting(t *testing.T) {
	release := make(chan struct{})
	fm := &fakeModel{
		steps: []step{
			reply(call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"look"}`)),
			reply(text("waiting on scout")),
			reply(text("got the answer, forgot to reply")),
			reply(call("c2", "message", `{"to":"user","text":"scout found it"}`)),
		},
		childSteps: []step{func(context.Context, model.Request) (model.Response, error) {
			<-release
			return call("k1", "message", `{"to":"main","text":"found it","kind":"response"}`), nil
		}},
	}
	s, h := newTestChannel(t, testConfig{reminders: true}, fm)
	root := s.Root()
	postTurn(t, s, h, "delegate")
	time.Sleep(50 * time.Millisecond)
	if n := len(reminders(h, root.ID)); n != 0 || root.Info().Turn != 1 {
		t.Fatalf("no nudge while waiting on the child: reminders %d turn %d", n, root.Info().Turn)
	}
	close(release)
	waitUntil(t, h, func() bool { return len(h.ofType(event.ChatMessage, root.ID)) == 1 && stateOf(root) == StateIdle })
	if q := reminders(h, root.ID); !reflect.DeepEqual(q, [][]string{{"user"}}) || root.Info().Turn != 3 {
		t.Fatalf("reminders %v turn %d", q, root.Info().Turn)
	}
}

// TestChildRemindedOfItsParent: a child that ends its task without
// answering is reminded of its parent by name, and its answer then settles
// what it owes.
func TestChildRemindedOfItsParent(t *testing.T) {
	fm := &fakeModel{
		steps: []step{
			reply(call("c1", "agent_create", `{"archetype":"general","label":"scout","task":"look"}`)),
			reply(call("c2", "message", `{"to":"user","text":"delegated to scout"}`)),
		},
		childSteps: []step{
			reply(text("found it, but forgot to say")),
			func(_ context.Context, req model.Request) (model.Response, error) {
				if !strings.Contains(lastUserText(req), "without replying to main") {
					return text(""), errors.New("reminder: " + lastUserText(req))
				}
				return call("k1", "message", `{"to":"main","text":"found it","kind":"response"}`), nil
			},
		},
	}
	s, h := newTestChannel(t, testConfig{reminders: true}, fm)
	root := s.Root()
	runTurn(t, s, h, "delegate")
	waitUntil(t, h, func() bool { return len(s.Agents()) == 2 })
	child := s.Agents()[1]
	waitUntil(t, h, func() bool {
		for _, m := range userMessages(h, root.ID) {
			if m.Kind == event.InputResponse && m.FromName == "scout" {
				return stateOf(child) == StateIdle
			}
		}
		return false
	})
	if q := reminders(h, child.ID); !reflect.DeepEqual(q, [][]string{{"main"}}) {
		t.Fatalf("child reminders %v\n%s", q, h.dump())
	}
	if due := child.Info().Due; len(due) != 0 {
		t.Fatalf("the answer settles the child's debt: %v", due)
	}
}

// TestReminderSurvivesRestart: a reminder queued when the daemon stopped
// starts its turn after recovery.
func TestReminderSurvivesRestart(t *testing.T) {
	fm := &fakeModel{steps: []step{reply(text("notes only")), reply(text("notes again"))}}
	s, h := newTestChannel(t, testConfig{reminders: true}, fm)
	root := s.Root()
	postTurn(t, s, h, "check it")
	waitUntil(t, h, func() bool { return len(reminders(h, root.ID)) >= 1 })
	s.Stop()
	var cut []event.Event
	for _, e := range h.all() {
		cut = append(cut, e)
		var in event.Input
		if e.Type == event.InputQueued && e.Decode(&in) == nil && in.Kind == event.InputReminder {
			break
		}
	}
	h2 := newFakeHost(&fakeModel{steps: []step{reply(call("c1", "message", `{"to":"user","text":"after restart"}`))}})
	s2, err := Recover(context.Background(), h2, s.ID, s.Dir, s.Created, s.Config(), cut)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Stop)
	r2, _ := s2.Agent(root.ID)
	waitUntil(t, h2, func() bool { return len(h2.ofType(event.ChatMessage, root.ID)) == 1 && stateOf(r2) == StateIdle })
	if um := takenIn(append(cut, h2.all()...), root.ID); len(um) != 2 || um[1].Kind != event.InputReminder || um[1].Turn != 2 {
		t.Fatalf("recovered reminder turn inputs: %+v", um)
	}
}

// TestDirectMessageOwesNothing: a message the human types in the agent's own
// chat is answered in that chat, by the text the turn ends with, so no reply
// is owed and no reminder follows.
func TestDirectMessageOwesNothing(t *testing.T) {
	fm := &fakeModel{steps: []step{reply(text("answered right here")), reply(text("a reminder turn would land here"))}}
	s, h := newTestChannel(t, testConfig{reminders: true}, fm)
	root := s.Root()
	runTurn(t, s, h, "check it")
	time.Sleep(50 * time.Millisecond)
	if in := root.Info(); len(in.Due) != 0 || len(reminders(h, root.ID)) != 0 || in.Turn != 1 {
		t.Fatalf("due %v, turn %d, log:\n%s", in.Due, in.Turn, h.dump())
	}
}
