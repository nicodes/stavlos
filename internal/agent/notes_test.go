package agent

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/nicodes/stavlos/internal/model"
)

// TestRequestsExtendThePreviousOne: every model call of a turn repeats the
// previous call's messages and adds to them. A provider that caches a
// conversation keeps the prefix of the request it last answered and reuses it
// only for a request that extends it, so a call that rewrites any earlier
// message pays for the whole history again. The state note is what used to
// break this: it was last in each request and gone from the next.
func TestRequestsExtendThePreviousOne(t *testing.T) {
	const turns = 6
	steps := make([]step, 0, turns+1)
	for i := range turns {
		steps = append(steps, reply(call(fmt.Sprintf("c%d", i), "read", `{"path":"f.txt"}`)))
	}
	steps = append(steps, reply(text("done")))
	fm := &fakeModel{steps: steps}
	s, h := newTestChannel(t, testConfig{}, fm)
	runTurn(t, s, h, "read the file a few times")

	reqs := fm.requests()
	if len(reqs) < 3 {
		t.Fatalf("want several calls, got %d", len(reqs))
	}
	blocks := func(m model.Message) string { b, _ := json.Marshal(m); return string(b) }
	for i := 1; i < len(reqs); i++ {
		prev, cur := reqs[i-1].Messages, reqs[i].Messages
		if len(cur) < len(prev) {
			t.Errorf("call %d has %d messages, fewer than call %d's %d", i, len(cur), i-1, len(prev))
			continue
		}
		for j := range prev {
			if blocks(prev[j]) != blocks(cur[j]) {
				t.Errorf("call %d rewrote message %d of call %d, so the cached prefix stops there:\n prev %s\n cur  %s",
					i, j, i-1, blocks(prev[j]), blocks(cur[j]))
				break
			}
		}
	}
}

// TestWithNotesReplaysAndGivesUp pins withNotes directly: notes come back
// where they were, and a history that no longer holds them starts over
// rather than inserting them somewhere they never were.
func TestWithNotesReplaysAndGivesUp(t *testing.T) {
	user := func(text string) model.Message {
		return model.Message{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: text}}}
	}
	assistant := func(text string) model.Message {
		return model.Message{Role: model.RoleAssistant, Blocks: []model.Block{{Type: model.BlockText, Text: text}}}
	}
	texts := func(ms []model.Message) []string {
		var out []string
		for _, m := range ms {
			for _, b := range m.Blocks {
				out = append(out, string(m.Role)+":"+b.Text)
			}
		}
		return out
	}
	eq := func(t *testing.T, got, want []string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}

	// First call: the note lands on the only user message.
	h1 := []model.Message{user("hello")}
	got1, notes1 := withNotes(h1, nil, "state A")
	eq(t, texts(got1), []string{"user:hello", "user:state A"})
	if len(notes1) != 1 {
		t.Fatalf("want one note recorded, got %d", len(notes1))
	}

	// Second call: the history grew, and the old note is still in its place.
	h2 := []model.Message{user("hello"), assistant("hi"), user("more")}
	got2, notes2 := withNotes(h2, notes1, "state B")
	eq(t, texts(got2), []string{"user:hello", "user:state A", "assistant:hi", "user:more", "user:state B"})
	if len(notes2) != 2 {
		t.Fatalf("want two notes recorded, got %d", len(notes2))
	}

	// The second call's messages extend the first call's.
	for i := range got1 {
		if fmt.Sprint(got1[i]) != fmt.Sprint(got2[i]) {
			t.Errorf("message %d changed between calls: %v then %v", i, got1[i], got2[i])
		}
	}

	// A compacted history no longer has the messages the notes sat on: the
	// chain is dropped and only this call's note is sent.
	got3, notes3 := withNotes([]model.Message{user("summary")}, notes2, "state C")
	eq(t, texts(got3), []string{"user:summary", "user:state C"})
	if len(notes3) != 1 {
		t.Errorf("want the chain restarted with one note, got %d", len(notes3))
	}
}
