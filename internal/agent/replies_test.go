package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

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

// TestReminderThenMissing: a turn that ends owing the human a reply gets one
// reminder turn; ignoring it too records the reply as missing, and nothing
// runs after that.
func TestReminderThenMissing(t *testing.T) {
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
	waitUntil(t, h, func() bool { return len(h.ofType(event.ReplyMissing, root.ID)) == 1 && root.StateOf() == StateIdle })
	if !strings.Contains(reminder, "without replying to user") || !strings.Contains(reminder, "message (to: user)") {
		t.Fatalf("reminder: %q", reminder)
	}
	if q, m := repliesOf(t, h, event.ReminderQueued, root.ID), repliesOf(t, h, event.ReplyMissing, root.ID); !reflect.DeepEqual(q, [][]string{{"user"}}) || !reflect.DeepEqual(m, [][]string{{"user"}}) {
		t.Fatalf("queued %v missing %v", q, m)
	}
	um := userMessages(h, root.ID)
	if in := root.Info(); in.Turn != 2 || len(um) != 2 || um[1].Kind != event.MsgReminder {
		t.Fatalf("turn %d, inputs %+v", in.Turn, um)
	}
	if owed := root.replyState(root.owed); len(owed) != 0 {
		t.Fatalf("a missing reply is dropped: %v", owed)
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
	if owed := root.replyState(root.owed); len(owed) != 0 || len(h.ofType(event.ReminderQueued, root.ID)) != 0 || root.Info().Turn != 1 {
		t.Fatalf("owed %v, log:\n%s", owed, h.dump())
	}
}

// TestChildRemindedOfItsParent: a child that ends its task without
// answering is reminded of its parent by name, and its answer then settles
// both sides.
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
				return call("k1", "message", `{"to":"main","text":"found it"}`), nil
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
	if len(h.ofType(event.ReplyMissing, child.ID)) != 0 || len(child.replyState(child.owed)) != 0 {
		t.Fatalf("the answer settles the child's debt:\n%s", h.dump())
	}
}

// TestReminderSurvivesRestart: a reminder queued when the daemon stopped
// starts its turn after recovery.
func TestReminderSurvivesRestart(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(text("notes only")),
		reply(text("notes again")),
	}}
	s, h := newTestSession(t, testConfig{reminders: true}, fm)
	root := s.Root()
	runTurn(t, s, h, "check it")
	waitUntil(t, h, func() bool { return len(h.ofType(event.ReminderQueued, root.ID)) == 1 })
	s.Stop()
	var cut []event.Event
	for _, e := range h.all() {
		cut = append(cut, e)
		if e.Type == event.ReminderQueued {
			break
		}
	}
	h2 := newFakeHost(&fakeModel{steps: []step{reply(text("after restart"))}})
	s2, err := Recover(context.Background(), h2, s.ID, s.Dir, s.Created, s.Config(), cut)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Stop)
	waitUntil(t, h2, func() bool { return len(h2.ofType(event.ReplyMissing, root.ID)) == 1 })
	if um := userMessages(h2, root.ID); len(um) != 1 || um[0].Kind != event.MsgReminder {
		t.Fatalf("recovered reminder turn inputs: %+v", um)
	}
}
