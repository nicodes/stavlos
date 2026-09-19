package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

const week = 7 * 24 * 60

// reading is a plan with a five-hour window and a week, the week resetting
// in weekLeft.
func reading(now time.Time, fiveHour, weekUsed float64, weekLeft time.Duration) model.PlanUsage {
	return model.PlanUsage{Observed: now, Windows: []model.UsageWindow{
		{UsedPercent: fiveHour, Minutes: 300, ResetsAt: now.Add(2 * time.Hour)},
		{UsedPercent: weekUsed, Minutes: week, ResetsAt: now.Add(weekLeft)},
	}}
}

func TestPickModel(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	day := 24 * time.Hour
	order := []string{"openai/gpt", "zai/glm", "kimi/k2", "xai/grok"}
	for _, tc := range []struct {
		name  string
		usage map[string]model.PlanUsage
		avoid string
		want  string
		why   string
	}{
		{name: "no readings: the role's order", want: "openai/gpt", why: "no plan usage yet"},
		{name: "the plan furthest behind pace, not the first listed", want: "zai/glm", why: "10% of its week used with 86% of it gone",
			usage: map[string]model.PlanUsage{
				"openai": reading(now, 10, 60, 4*day), // 43% gone, 60% used: ahead of pace
				"zai":    reading(now, 10, 10, 1*day), // 86% gone, 10% used: 76 points about to lapse
				"kimi":   reading(now, 10, 30, 3*day), // 57% gone, 30% used: 27 behind
			}},
		{name: "a full five-hour window passes a plan over, however far behind its week", want: "kimi/k2",
			usage: map[string]model.PlanUsage{
				"openai": reading(now, 10, 60, 4*day),
				"zai":    reading(now, 100, 10, 1*day),
				"kimi":   reading(now, 10, 30, 3*day),
			}},
		{name: "a window that has reset since the reading is not full", want: "zai/glm",
			usage: map[string]model.PlanUsage{"openai": reading(now, 10, 60, 4*day), "zai": {Observed: now.Add(-6 * time.Hour), Windows: []model.UsageWindow{
				{UsedPercent: 100, Minutes: 300, ResetsAt: now.Add(-time.Hour)}, {UsedPercent: 10, Minutes: week, ResetsAt: now.Add(day)}}}}},
		// zai is furthest behind pace but refused a call; of the rest, a plan
		// nothing is known about beats one that is ahead of its pace
		{name: "a refusal outlasts a reading that looks fine", want: "kimi/k2",
			usage: map[string]model.PlanUsage{"openai": reading(now, 10, 60, 4*day), "zai": func() model.PlanUsage {
				u := reading(now, 10, 10, day)
				u.LimitedUntil = now.Add(10 * time.Minute)
				return u
			}()}},
		{name: "within a few points the sooner reset wins, then the order", want: "kimi/k2",
			usage: map[string]model.PlanUsage{
				"openai": reading(now, 0, 40, 3*day+12*time.Hour), // 50% gone: 10 behind
				"kimi":   reading(now, 0, 60, 2*day),              // 71% gone: 11 behind, and it resets first
			}},
		{name: "the provider being left is never the answer", avoid: "openai", want: "zai/glm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := pickModel(order, tc.usage, now, tc.avoid)
			if !ok || got.model != tc.want || !strings.Contains(got.why, tc.why) {
				t.Fatalf("picked %q (%s) ok=%v, want %q (…%s…)", got.model, got.why, ok, tc.want, tc.why)
			}
		})
	}
	// everything at its limit: no choice, and when the first comes back
	full := map[string]model.PlanUsage{}
	for _, p := range []string{"openai", "zai", "kimi", "xai"} {
		full[p] = reading(now, 100, 50, 3*day)
	}
	if got, soonest, ok := pickModel(order, full, now, ""); ok || !soonest.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("all limited: %+v %v %v", got, soonest, ok)
	}
}

// TestHarnessChoosesAChildsModel: the role lists its models in the order it
// prefers them, the parent cannot name one, and the child lands on the plan
// with the most allowance about to lapse.
func TestHarnessChoosesAChildsModel(t *testing.T) {
	fm := &fakeModel{steps: []step{
		reply(call("c1", "agent_create", `{"archetype":"scout","label":"s","task":"look","model":"fake/m1"}`)),
		reply(text("ok")),
	}}
	s, h := newTestChannel(t, testConfig{
		json:  `{"model":"fake/m1"}`,
		roles: map[string]string{"scout": "---\ndescription: looks\nmodels: [fake/m1, other/m2, third/m3]\n---\nlook\n", "lead": "---\ndescription: leads\nspawn: [scout]\n---\nlead\n"},
	}, fm)
	now := time.Now()
	h.usage = map[string]model.PlanUsage{
		"fake":  reading(now, 10, 70, 5*24*time.Hour), // ahead of pace
		"other": reading(now, 10, 5, 12*time.Hour),    // nearly a whole week about to lapse
	}
	if err := s.Root().SetRole(context.Background(), "lead"); err != nil {
		t.Fatal(err)
	}
	runTurn(t, s, h, "delegate")
	if fin := finished(h, s.Root().ID); len(fin) != 1 || fin[0].IsError {
		t.Fatalf("agent_create: %+v", fin)
	}
	child := s.Agents()[1]
	if got := child.Info().Model; got != "other/m2" {
		t.Fatalf("the child runs on %q, want the plan furthest behind pace (the parent asked for fake/m1, and may not)", got)
	}
}

// TestRunningAgentMovesOffALimitedModel: a call refused for a limit moves the
// agent to the next model its role allows, says why in its chat, marks the
// provider so nothing else chooses it, and the turn goes on to its answer.
func TestRunningAgentMovesOffALimitedModel(t *testing.T) {
	var calledWith []string
	fm := &fakeModel{steps: []step{
		func(_ context.Context, req model.Request) (model.Response, error) {
			calledWith = append(calledWith, req.Model)
			return model.Response{}, &model.LimitError{Err: errors.New("fake: status 429: usage_limit_reached"), RetryAfter: 40 * time.Minute}
		},
		func(_ context.Context, req model.Request) (model.Response, error) {
			calledWith = append(calledWith, req.Model)
			return text("carried on"), nil
		},
	}}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","models":["fake/m1","other/m2"]}`}, fm)
	end := runTurn(t, s, h, "go")
	if end.Reason != event.ReasonEndTurn || strings.Join(calledWith, ",") != "m1,m2" {
		t.Fatalf("turn ended %q (%s) after calls to %v", end.Reason, end.Error, calledWith)
	}
	if got := s.Root().Info().Model; got != "other/m2" {
		t.Fatalf("model after the refusal: %q", got)
	}
	var moved event.AgentUpdatedPayload
	ups := h.ofType(event.AgentUpdated, s.Root().ID)
	_ = ups[len(ups)-1].Decode(&moved)
	if moved.Model == nil || *moved.Model != "other/m2" || !strings.Contains(moved.Reason, "fake is at its limit until") {
		t.Fatalf("the move is not explained: %+v", moved)
	}
	if u := h.PlanUsage()["fake"]; time.Until(u.LimitedUntil) < 30*time.Minute {
		t.Fatalf("the provider was not marked: %+v", u)
	}
	// the next turn starts on the new model without calling the limited one
	calledWith = nil
	fm.mu.Lock()
	fm.steps = []step{func(_ context.Context, req model.Request) (model.Response, error) {
		calledWith = append(calledWith, req.Model)
		return text("again"), nil
	}}
	fm.mu.Unlock()
	runTurn(t, s, h, "more")
	if strings.Join(calledWith, ",") != "m2" {
		t.Fatalf("second turn called %v", calledWith)
	}
}

// TestNowhereToMoveSaysSo: with no other model to use, the refusal ends the
// turn with what to do about it.
func TestNowhereToMoveSaysSo(t *testing.T) {
	fm := &fakeModel{steps: []step{fail(&model.LimitError{Err: errors.New("fake: status 429: quota")})}}
	s, h := newTestChannel(t, testConfig{}, fm)
	end := runTurn(t, s, h, "go")
	if end.Reason != event.ReasonError || !strings.Contains(end.Error, "at its limit") || !strings.Contains(end.Error, "/models") {
		t.Fatalf("%q %q", end.Reason, end.Error)
	}
}
