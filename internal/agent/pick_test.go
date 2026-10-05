package agent

import (
	"context"
	"errors"
	"github.com/nicodes/stavlos/internal/protocol"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/config"
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

// TestWeekBeforeFiveHours: what lapses with a week outweighs what lapses
// with five hours, whichever window ends first, and for whatever mix of
// windows a provider has. The five-hour pace only settles providers the
// longer windows leave level.
func TestWeekBeforeFiveHours(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	day := 24 * time.Hour
	win := func(used float64, minutes int, left time.Duration) model.UsageWindow {
		return model.UsageWindow{UsedPercent: used, Minutes: minutes, ResetsAt: now.Add(left)}
	}
	plan := func(ws ...model.UsageWindow) model.PlanUsage { return model.PlanUsage{Observed: now, Windows: ws} }
	grokWeek := plan(win(50, week, day)) // 50% used, a day left: 36 points of a week about to lapse

	for _, tc := range []struct {
		name  string
		order []string
		usage map[string]model.PlanUsage
		want  string
	}{
		{name: "grok's week lapses tomorrow; kimi's five hours end first, its week in five days",
			order: []string{"kimi/k2", "xai/grok"}, want: "xai/grok",
			usage: map[string]model.PlanUsage{"xai": grokWeek, "kimi": plan(win(50, 300, 30*time.Minute), win(10, week, 5*day))}},
		{name: "the same with kimi limited by five hours alone",
			order: []string{"kimi/k2", "xai/grok"}, want: "xai/grok",
			usage: map[string]model.PlanUsage{"xai": grokWeek, "kimi": plan(win(50, 300, 30*time.Minute))}},
		{name: "a whole five-hour window about to lapse is still no match for a third of a week",
			order: []string{"kimi/k2", "xai/grok"}, want: "xai/grok",
			usage: map[string]model.PlanUsage{"xai": grokWeek, "kimi": plan(win(0, 300, 10*time.Minute))}},
		{name: "a week ahead of its pace is spared for a plan with no week to protect",
			order: []string{"xai/grok", "kimi/k2"}, want: "kimi/k2",
			usage: map[string]model.PlanUsage{"xai": plan(win(80, week, 5*day)), "kimi": plan(win(50, 300, 2*time.Hour))}},
		{name: "weeks level: the five hours decide, down the cascade",
			order: []string{"zai/glm", "openai/gpt"}, want: "openai/gpt",
			usage: map[string]model.PlanUsage{
				"zai":    plan(win(90, 300, 4*time.Hour), win(40, week, 4*day)),     // five hours nearly spent
				"openai": plan(win(5, 300, 30*time.Minute), win(40, week, 4*day))}}, // five hours about to lapse unused
		{name: "windows nobody has seen yet: most of a month lapsing beats a little of a week",
			order: []string{"zai/glm", "kimi/k2"}, want: "kimi/k2",
			usage: map[string]model.PlanUsage{
				"zai":  plan(win(60, week, 2*day)),                                    // 71% gone, 60% used: 11 points of a week
				"kimi": plan(win(30, 30*24*60, 3*day), win(20, 24*60, 6*time.Hour))}}, // 90% gone, 30% used: 60 points of a month
		{name: "a throttle that is full passes the plan over, whatever its week says",
			order: []string{"xai/grok", "kimi/k2"}, want: "kimi/k2",
			usage: map[string]model.PlanUsage{"xai": plan(win(100, 300, time.Hour), win(50, week, day)), "kimi": plan(win(50, 300, 30*time.Minute))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := pickModel(tc.order, tc.usage, now, "")
			if !ok || got.model != tc.want {
				t.Fatalf("picked %q (%s), want %q", got.model, got.why, tc.want)
			}
		})
	}
}

// TestVariantDoesNotFollowAnAgentToAModelThatRejectsIt (#36): a variant is a
// provider's word. An agent moved off a model with reasoning efforts onto
// one that has none must not carry "medium" along, although the role's
// entry for the new model lists no variants and so allows any.
func TestVariantDoesNotFollowAnAgentToAModelThatRejectsIt(t *testing.T) {
	var sent []string
	fm := &fakeModel{steps: []step{
		func(_ context.Context, req model.Request) (model.Response, error) {
			sent = append(sent, req.Model+":"+req.Variant)
			return model.Response{}, &model.LimitError{Err: errors.New("fake: status 429: usage_limit_reached")}
		},
		func(_ context.Context, req model.Request) (model.Response, error) {
			sent = append(sent, req.Model+":"+req.Variant)
			return text("carried on"), nil
		},
	}}
	s, h := newTestChannel(t, testConfig{
		json:  `{"model":"fake/m1"}`,
		roles: map[string]string{"worker": "---\ndescription: works\nmodels:\n  - id: fake/m1\n    variants: [medium, high]\n  - other/plain\n  - third/efforts\n---\nwork\n"},
	}, fm)
	h.variants = map[string][]string{"fake/m1": {"low", "medium", "high"}, "third/efforts": {"low", "high"}} // other/plain takes none
	ctx := context.Background()
	if err := s.Root().SetRole(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if got := s.Root().Info().Variant; got != "medium" {
		t.Fatalf("the role's default variant for fake/m1: %q", got)
	}
	runTurn(t, s, h, "go")
	if strings.Join(sent, " ") != "m1:medium plain:" {
		t.Fatalf("calls carried %v, want the variant dropped on the model that takes none", sent)
	}
	if info := s.Root().Info(); info.Model != "other/plain" || info.Variant != "" {
		t.Fatalf("after the move: %s %q", info.Model, info.Variant)
	}
	// /models onto a model with other words for it: "medium" is not one of
	// them and the role names no default there, so the provider's own applies
	if err := s.Root().SetModel(ctx, "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Root().SetVariant(ctx, "medium"); err != nil {
		t.Fatal(err)
	}
	if err := s.Root().SetModel(ctx, "third/efforts"); err != nil {
		t.Fatal(err)
	}
	if got := s.Root().Info().Variant; got != "" {
		t.Fatalf("medium followed the agent to a model that takes low and high: %q", got)
	}
	// one both models take does carry over
	if err := s.Root().SetVariant(ctx, "high"); err != nil {
		t.Fatal(err)
	}
	if err := s.Root().SetModel(ctx, "fake/m1"); err != nil {
		t.Fatal(err)
	}
	if got := s.Root().Info().Variant; got != "high" {
		t.Fatalf("high is allowed and taken by both: %q", got)
	}
}

// TestFitVariant covers the fallbacks, among them the one the fix suggested
// in #36 would have broken: an agent arriving with no variant still gets the
// role's default for its model.
func TestFitVariant(t *testing.T) {
	role := config.Role{Models: []config.ModelSpec{{ID: "a/efforts", Variants: []string{"medium", "high"}}, {ID: "b/any"}, {ID: "c/odd", Variants: []string{"turbo"}}}}
	efforts := []string{"low", "medium", "high"}
	for _, tc := range []struct {
		name, id, want, got string
		offered             []string
	}{
		{"kept: allowed and taken", "a/efforts", "high", "high", efforts},
		{"no variant yet: the role's default, not none", "a/efforts", "", "medium", efforts},
		{"not allowed by the role: its default", "a/efforts", "low", "medium", efforts},
		{"the role allows any, the model takes none", "b/any", "medium", "", nil},
		{"the role allows any, the model takes it", "b/any", "medium", "medium", efforts},
		{"the role's own default is not one the model takes: the provider's", "c/odd", "high", "", efforts},
	} {
		if got := fitVariant(role, tc.offered, tc.id, tc.want); got != tc.got {
			t.Errorf("%s: fitVariant(%s, %q) = %q, want %q", tc.name, tc.id, tc.want, got, tc.got)
		}
	}
}

// TestAgentStoppedAtEveryLimitIsWokenWhenAModelIsBack: with nowhere to move,
// the turn ends saying when the agent will carry on; nothing wakes it before
// that, nor while every model is still limited; once one is back the harness
// queues its own input (not a human's), the agent moves to that model before
// calling, and the work goes on. Cancel means stop, and the switch turns it
// all off.
func TestAgentStoppedAtEveryLimitIsWokenWhenAModelIsBack(t *testing.T) {
	var calledWith []string
	refuse := func(_ context.Context, req model.Request) (model.Response, error) {
		calledWith = append(calledWith, req.Model)
		return model.Response{}, &model.LimitError{Err: errors.New("status 403: out of credits"), RetryAfter: time.Hour}
	}
	fm := &fakeModel{steps: []step{refuse, refuse}}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","models":["fake/m1","other/m2"]}`}, fm)
	ctx := context.Background()

	end := runTurn(t, s, h, "do the long job")
	if end.Reason != event.ReasonError || strings.Join(calledWith, ",") != "m1,m2" {
		t.Fatalf("both models refuse: %q after %v", end.Reason, calledWith)
	}
	if end.ResumeAt.IsZero() || time.Until(end.ResumeAt) < 50*time.Minute || !strings.Contains(end.Error, "carries on by itself") {
		t.Fatalf("the turn's end does not say when it resumes: %+v", end)
	}
	queued := func() int {
		n := 0
		for _, e := range h.ofType(event.InputQueued, s.Root().ID) {
			var in event.Input
			if e.Decode(&in) == nil && in.Kind == event.InputResume {
				n++
			}
		}
		return n
	}
	// too early, then on time but with every model still limited: nothing
	if err := s.MaybeResume(ctx, time.Now().Add(10*time.Minute)); err != nil || queued() != 0 {
		t.Fatalf("woken before its time: %v %d", err, queued())
	}
	if err := s.MaybeResume(ctx, end.ResumeAt.Add(time.Minute)); err != nil || queued() != 0 {
		t.Fatalf("woken with nothing to run on: %v %d", err, queued())
	}
	// other/m2's plan comes back
	h.mu.Lock()
	h.usage["other"] = model.PlanUsage{}
	h.mu.Unlock()
	calledWith = nil
	fm.mu.Lock()
	fm.steps = []step{func(_ context.Context, req model.Request) (model.Response, error) {
		calledWith = append(calledWith, req.Model)
		if got := lastUserText(req); !strings.Contains(got, "[from the harness]") || !strings.Contains(got, "carry on from where you stopped") {
			t.Errorf("the model is not told why it woke: %q", got)
		}
		return text("finished the long job"), nil
	}}
	fm.mu.Unlock()
	if err := s.MaybeResume(ctx, end.ResumeAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	woke := h.waitTurnEnd(t, s.Root().ID, 2)
	if woke.Reason != event.ReasonEndTurn || strings.Join(calledWith, ",") != "m2" || queued() != 1 {
		t.Fatalf("resumed turn: %q, calls %v, resume inputs %d", woke.Reason, calledWith, queued())
	}
	if got := s.Root().Info().Model; got != "other/m2" {
		t.Fatalf("it resumed on %q", got)
	}
	// nothing more to wake
	if err := s.MaybeResume(ctx, time.Now().Add(24*time.Hour)); err != nil || queued() != 1 {
		t.Fatalf("woken twice: %v %d", err, queued())
	}
}

func TestCancelAndTheSwitchStopTheWake(t *testing.T) {
	refuse := fail(&model.LimitError{Err: errors.New("status 429: quota")})
	ctx := context.Background()
	t.Run("cancel", func(t *testing.T) {
		s, h := newTestChannel(t, testConfig{}, &fakeModel{steps: []step{refuse}})
		end := runTurn(t, s, h, "go")
		if end.ResumeAt.IsZero() {
			t.Fatal("not parked")
		}
		s.Root().Cancel()
		if err := s.MaybeResume(ctx, end.ResumeAt.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		settle(t, s, h) // a wake, had there been one, has been taken by now
		if n := len(h.ofType(event.TurnStarted, s.Root().ID)); n != 1 {
			t.Fatalf("a cancelled agent was woken: %d turns", n)
		}
	})
	t.Run("resumeAfterLimit false", func(t *testing.T) {
		s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","resumeAfterLimit":false}`}, &fakeModel{steps: []step{refuse}})
		if end := runTurn(t, s, h, "go"); !end.ResumeAt.IsZero() || strings.Contains(end.Error, "carries on") {
			t.Fatalf("parked although turned off: %+v", end)
		}
	})
}

// A turn that has made limits.maxCallsPerTurn model calls ends, and says so.
func TestATurnStopsAtTheCallCap(t *testing.T) {
	forever := make([]step, 50) // more than the cap: the loop must be what stops it
	for i := range forever {
		forever[i] = reply(call("c", "read", `{"path":"go.mod"}`))
	}
	fm := &fakeModel{steps: forever}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","limits":{"maxCallsPerTurn":10}}`}, fm)
	root := s.Root()
	if err := s.SetMode(context.Background(), protocol.ModeYolo); err != nil {
		t.Fatal(err)
	}
	postTurn(t, s, h, "loop")
	waitUntil(t, h, func() bool { return len(h.ofType(event.TurnEnded, root.ID)) == 1 })
	var p event.TurnEndedPayload
	_ = h.ofType(event.TurnEnded, root.ID)[0].Decode(&p)
	if p.Reason != event.ReasonError || !strings.Contains(p.Error, "10 model calls") || !strings.Contains(p.Error, "until_changed") {
		t.Fatalf("turn ended: %+v", p)
	}
	if n := len(h.ofType(event.AssistantMessage, root.ID)); n != 10 {
		t.Fatalf("%d model calls, want 10", n)
	}
}

// A move to another provider compacts first when the history is large: the
// new provider has nothing of it cached.
func TestAMoveToAnotherProviderCompactsFirst(t *testing.T) {
	for _, big := range []bool{false, true} {
		tr := &turnRun{a: &Agent{c: &Channel{}}}
		tr.a.ctxTokens = 10_000
		if big {
			tr.a.ctxTokens = compactBeforeMove + 1
		}
		tr.a.compactNext = false
		// what movedOn does after a successful move, without a provider
		tr.a.c.mu.Lock()
		if tr.a.ctxTokens > compactBeforeMove {
			tr.a.compactNext = true
		}
		tr.a.c.mu.Unlock()
		if tr.a.compactNext != big {
			t.Fatalf("history %d tokens: compactNext %v", tr.a.ctxTokens, tr.a.compactNext)
		}
	}
}

// The window is a ceiling: what the reply needs is reserved first, and the
// threshold applies to the rest.
func TestUsableWindow(t *testing.T) {
	for _, c := range []struct {
		window, out, want int
	}{
		{1_000_000, 65_536, 980_000}, // Kimi, GLM: 20k reserved
		{200_000, 8_192, 191_808},    // a small output limit reserves less
		{100_000, 0, 90_000},         // no output limit known: a tenth
	} {
		if got := usableWindow(model.Info{ContextWindow: c.window, MaxOutput: c.out}); got != c.want {
			t.Errorf("usableWindow(%d, %d) = %d, want %d", c.window, c.out, got, c.want)
		}
	}
}

// TestAFaultParksAndResumes: a call that fails for a passing fault (the
// stream layer has retried it already) ends the turn, parks the agent with
// a backoff, and the next tick past it wakes the agent with a note saying a
// fault, not a limit, stopped it; the backoff doubles per wake in a row and
// resets once a model answers. Turned off with resumeAfterLimit.
func TestAFaultParksAndResumes(t *testing.T) {
	fault := fail(&model.TransientError{Err: errors.New("xai: stream error: The model is currently at capacity due to high demand")})
	ctx := context.Background()
	fm := &fakeModel{steps: []step{fault}}
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1"}`}, fm)
	end := runTurn(t, s, h, "go")
	if end.Reason != event.ReasonError || end.ResumeAt.IsZero() || end.Resume != event.ResumeFault || !strings.Contains(end.Error, "tries again by itself") {
		t.Fatalf("a fault did not park the agent: %+v", end)
	}
	if until := time.Until(end.ResumeAt); until < 20*time.Second || until > 40*time.Second {
		t.Fatalf("first backoff is about 30s, got %s", until)
	}
	if err := s.MaybeResume(ctx, time.Now()); err != nil || len(h.ofType(event.TurnStarted, s.Root().ID)) != 1 {
		t.Fatal("woken before its time")
	}
	// still faulting: parked again, twice as long
	fm.mu.Lock()
	fm.steps = []step{fault}
	fm.mu.Unlock()
	if err := s.MaybeResume(ctx, end.ResumeAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	end2 := h.waitTurnEnd(t, s.Root().ID, 2)
	if end2.Resume != event.ResumeFault || time.Until(end2.ResumeAt) < 50*time.Second {
		t.Fatalf("second backoff is about 60s: %+v", end2)
	}
	// the fault passes: the wake says what stopped it, and the agent finishes
	var woke string
	fm.mu.Lock()
	fm.steps = []step{func(_ context.Context, req model.Request) (model.Response, error) {
		woke = lastUserText(req)
		return text("done"), nil
	}}
	fm.mu.Unlock()
	if err := s.MaybeResume(ctx, end2.ResumeAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if end3 := h.waitTurnEnd(t, s.Root().ID, 3); end3.Reason != event.ReasonEndTurn || !strings.Contains(woke, "passing fault") || !end3.ResumeAt.IsZero() {
		t.Fatalf("resumed turn: %+v woke=%q", end3, woke)
	}
	t.Run("resumeAfterLimit false", func(t *testing.T) {
		s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","resumeAfterLimit":false}`}, &fakeModel{steps: []step{fault}})
		if end := runTurn(t, s, h, "go"); !end.ResumeAt.IsZero() || strings.Contains(end.Error, "tries again") {
			t.Fatalf("parked although turned off: %+v", end)
		}
	})
	t.Run("an ordinary error does not park", func(t *testing.T) {
		s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1"}`}, &fakeModel{steps: []step{fail(errors.New("status 400: bad request"))}})
		if end := runTurn(t, s, h, "go"); !end.ResumeAt.IsZero() || end.Resume != "" {
			t.Fatalf("parked on a final error: %+v", end)
		}
	})
}
