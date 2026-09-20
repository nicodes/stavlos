package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
)

// TestRecap: a channel that has worked but not spoken for the set minutes
// asks its main agent for a status report; one that has said nothing new,
// spoken recently, or has no recap set is left alone, and the ask is a
// request the agent owes a response to.
func TestRecap(t *testing.T) {
	fm := &fakeModel{steps: []step{reply(text("hello"))}}
	s, h := newTestChannel(t, testConfig{}, fm)
	root := s.Root()
	ctx := context.Background()
	runTurn(t, s, h, "go")

	now := time.Now()
	if err := s.MaybeRecap(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(recapInputs(h, root.ID)); n != 0 {
		t.Fatalf("no recap set: %d asks", n)
	}
	if err := s.SetRecap(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if s.Recap() != 10 || s.Info().Recap != 10 {
		t.Fatalf("recap: %d %d", s.Recap(), s.Info().Recap)
	}
	if err := s.MaybeRecap(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := len(recapInputs(h, root.ID)); n != 0 {
		t.Fatalf("still within the interval: %d asks", n)
	}
	if err := s.MaybeRecap(ctx, now.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	asks := recapInputs(h, root.ID)
	if len(asks) != 1 || asks[0].RequestID == "" || asks[0].Kind != event.InputPrompt || !strings.Contains(asks[0].Text, "status report") {
		t.Fatalf("recap ask: %+v", asks)
	}
	// one ask at a time: unanswered, it is chased by the nudges, not re-asked
	if err := s.MaybeRecap(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(recapInputs(h, root.ID)); n != 1 {
		t.Fatalf("an unanswered recap should not be asked again: %d asks", n)
	}
	// off again
	if err := s.SetRecap(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MaybeRecap(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(recapInputs(h, root.ID)); n != 1 {
		t.Fatalf("off: %d asks", n)
	}
	if err := s.SetRecap(ctx, 1000000); err == nil {
		t.Fatal("an absurd interval should be refused")
	}
}

// recapInputs are the recap asks queued for an agent.
func recapInputs(h *fakeHost, agent string) []event.Input {
	var out []event.Input
	for _, e := range h.ofType(event.InputQueued, agent) {
		var in event.Input
		if e.Decode(&in) == nil && strings.HasPrefix(in.RequestID, "recap") {
			out = append(out, in)
		}
	}
	return out
}

// TestAnOpenRecapSurvivesARestart (RT1): that a recap was asked for and is
// still unanswered was kept only in memory, set before the event that says
// so was written. After a restart the daemon asked again. It is now folded
// from the log like everything else.
func TestAnOpenRecapSurvivesARestart(t *testing.T) {
	fm := &fakeModel{steps: []step{reply(text("worked")), reply(text("noted, no report yet"))}}
	s, h := newTestChannel(t, testConfig{}, fm)
	ctx := context.Background()
	runTurn(t, s, h, "do something") // work worth a recap
	if err := s.SetRecap(ctx, 1); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(10 * time.Minute)
	if err := s.MaybeRecap(ctx, later); err != nil {
		t.Fatal(err)
	}
	h.waitTurnEnd(t, s.Root().ID, 2) // the agent took the ask and did not answer the human
	if n := len(recapInputs(h, s.Root().ID)); n != 1 {
		t.Fatalf("%d recap asks", n)
	}
	s.Stop()
	r, err := Recover(ctx, newFakeHost(fm), s.ID, s.Dir(), time.Now(), s.Config(), h.all())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	rh := r.host.(*fakeHost)
	if err := r.MaybeRecap(ctx, later.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	settle(t, r, rh)
	if n := len(recapInputs(rh, r.Root().ID)); n != 0 {
		t.Fatalf("the restarted daemon asked for a recap that was already open: %d new asks", n)
	}
}
