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

func repliesOf(t *testing.T, h *fakeHost, typ event.Type, agent string) [][]string {
	t.Helper()
	var out [][]string
	for _, e := range h.ofType(typ, agent) {
		var p event.RepliesPayload
		if err := e.Decode(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Names)
	}
	return out
}

// TestNudgesUntilReplyOrCap: every turn that ends owing the human a reply is
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
	s, h := newTestSession(t, testConfig{reminders: true}, fm)
	root := s.Root()
	runTurn(t, s, h, "check it")
	waitUntil(t, h, func() bool { return root.Info().Turn == 1+maxNudges && root.StateOf() == StateIdle })
	time.Sleep(50 * time.Millisecond) // nothing follows the cap
	if in := root.Info(); in.Turn != 1+maxNudges || !reflect.DeepEqual(in.Due, []string{"user"}) {
		t.Fatalf("turn %d due %v", in.Turn, in.Due)
	}
	if q := repliesOf(t, h, event.ReminderQueued, root.ID); len(q) != maxNudges || !reflect.DeepEqual(q[0], []string{"user"}) {
		t.Fatalf("reminders %v", q)
	}
	if !strings.Contains(reminder, "without replying to user") || !strings.Contains(reminder, "message (to: user, kind: response)") {
		t.Fatalf("reminder: %q", reminder)
	}
	reqs := fm.requests()
	if strings.Contains(reqs[len(reqs)-1].System, "Replies due") || strings.Contains(reqs[len(reqs)-1].System, "owe a reply to") {
		t.Fatal("what is owed is not injected into the system prompt")
	}
	if err := root.Prompt(context.Background(), "again", "human:test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, h, func() bool { return root.Info().Turn == 2+2*maxNudges && root.StateOf() == StateIdle })
	time.Sleep(50 * time.Millisecond)
	if q := repliesOf(t, h, event.ReminderQueued, root.ID); len(q) != 2*maxNudges || root.Info().Turn != 2+2*maxNudges {
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
	s, h := newTestSession(t, testConfig{reminders: true}, fm)
	root := s.Root()
	runTurn(t, s, h, "check it")
	waitUntil(t, h, func() bool { return len(h.ofType(event.MessageToUser, root.ID)) == 1 && root.StateOf() == StateIdle })
	time.Sleep(50 * time.Millisecond)
	if in := root.Info(); in.Turn != 2 || len(in.Due) != 0 || len(h.ofType(event.ReminderQueued, root.ID)) != 1 {
		t.Fatalf("turn %d due %v log:\n%s", in.Turn, in.Due, h.dump())
	}
}

// TestReplyNeedsNoReminder: a message to the human settles its prompt.
func TestReplyNeedsNoReminder(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(call("c1", "message", `{"to":"user","text":"all good"}`)),
		reply(text("notes")),
	}}
	s, h := newTestSession(t, testConfig{reminders: true}, fm)
	root := s.Root()
	runTurn(t, s, h, "check it")
	waitUntil(t, h, func() bool { return root.StateOf() == StateIdle })
	if due := root.Info().Due; len(due) != 0 || len(h.ofType(event.ReminderQueued, root.ID)) != 0 || root.Info().Turn != 1 {
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
	s, h := newTestSession(t, testConfig{reminders: true}, fm)
	root := s.Root()
	runTurn(t, s, h, "delegate")
	time.Sleep(50 * time.Millisecond)
	if n := len(h.ofType(event.ReminderQueued, root.ID)); n != 0 || root.Info().Turn != 1 {
		t.Fatalf("no nudge while waiting on the child: reminders %d turn %d", n, root.Info().Turn)
	}
	close(release)
	waitUntil(t, h, func() bool { return len(h.ofType(event.MessageToUser, root.ID)) == 1 && root.StateOf() == StateIdle })
	if q := repliesOf(t, h, event.ReminderQueued, root.ID); !reflect.DeepEqual(q, [][]string{{"user"}}) || root.Info().Turn != 3 {
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
	s, h := newTestSession(t, testConfig{reminders: true}, fm)
	root := s.Root()
	runTurn(t, s, h, "delegate")
	waitUntil(t, h, func() bool { return len(s.Agents()) == 2 })
	child := s.Agents()[1]
	waitUntil(t, h, func() bool {
		for _, m := range userMessages(h, root.ID) {
			if m.Kind == event.MsgAgentResponse && m.From == "scout" {
				return child.StateOf() == StateIdle
			}
		}
		return false
	})
	if q := repliesOf(t, h, event.ReminderQueued, child.ID); !reflect.DeepEqual(q, [][]string{{"main"}}) {
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
	s, h := newTestSession(t, testConfig{reminders: true}, fm)
	root := s.Root()
	runTurn(t, s, h, "check it")
	waitUntil(t, h, func() bool { return len(h.ofType(event.ReminderQueued, root.ID)) >= 1 })
	s.Stop()
	var cut []event.Event
	for _, e := range h.all() {
		cut = append(cut, e)
		if e.Type == event.ReminderQueued {
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
	waitUntil(t, h2, func() bool { return len(h2.ofType(event.MessageToUser, root.ID)) == 1 && r2.StateOf() == StateIdle })
	if um := userMessages(h2, root.ID); len(um) != 1 || um[0].Kind != event.MsgReminder {
		t.Fatalf("recovered reminder turn inputs: %+v", um)
	}
}
