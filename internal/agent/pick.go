package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

// The harness chooses an agent's model, not the agent that creates it
// (docs/model-selection.md). A role lists the models good enough for it, in
// the order it prefers them; among those the harness takes the one whose
// subscription has the most allowance about to go to waste, and moves a
// running agent to the next when its model's plan runs out.
//
// What is unused when a window resets is lost, and how much is lost depends
// on the window: most of a week about to lapse is a great deal of allowance,
// most of five hours very little, and there is another five hours right
// behind it. A provider may limit by any mix of windows (five hours and a
// week, the week alone, five hours alone, a day, a month), so nothing here
// knows the mixes; it reads whatever windows a reading has.
//
//  1. A provider with any window used up is passed over until that window
//     resets: a short window above a long one is a throttle on spending it.
//  2. The rest are ranked by the allowance about to lapse in their budget,
//     their longest window: how far behind pace it is (the share of the
//     window gone by, less the share used), weighted by the window's length
//     so that a week outweighs five hours. 50% used with a day of the week
//     left is 36 points of a week going to waste; a five-hour window in the
//     same state is one point of a week.
//  3. Providers level on that are compared window by window, longest first,
//     on plain pace: the week first, then down to the five hours. A provider
//     without a window of some length has nothing lapsing there.
//  4. Then the sooner reset of the budget, then the role's order.

// limitedAt is the used percent at which a window counts as used up.
const limitedAt = 99.5

// paceBand is how many points count as level: readings are minutes old, and
// a choice that flips on a point of noise would scatter agents for no gain.
const paceBand = 5.0

// weekMinutes is the unit allowance at risk is measured in: points of a week.
const weekMinutes = 7 * 24 * 60

// pace is one window of a reading as the picker sees it.
type pace struct {
	minutes int       // the window's length, 0 when the provider did not say
	used    float64   // percent used
	elapsed float64   // percent of the window gone by
	surplus float64   // elapsed less used: points behind pace (negative: ahead)
	resets  time.Time // zero when unknown
}

// plan is what a provider's reading says to the picker.
type plan struct {
	limited bool      // a window is used up, or a call was refused
	until   time.Time // when it stops being limited (zero: unknown)
	windows []pace    // longest first; none when there is no reading
}

// budget is the plan's longest window, the allowance that lapses.
func (p plan) budget() (pace, bool) {
	if len(p.windows) == 0 {
		return pace{}, false
	}
	return p.windows[0], true
}

// atRisk is the allowance about to lapse in the budget, in points of a week.
func (p plan) atRisk() float64 {
	b, ok := p.budget()
	if !ok || b.minutes <= 0 {
		return 0
	}
	return b.surplus * float64(b.minutes) / weekMinutes
}

// surplusAt is the pace of the plan's window of that length, 0 when it has
// none: nothing of that length is lapsing.
func (p plan) surplusAt(minutes int) float64 {
	for _, w := range p.windows {
		if w.minutes == minutes {
			return w.surplus
		}
	}
	return 0
}

func readPlan(u model.PlanUsage, now time.Time) plan {
	var p plan
	if u.LimitedUntil.After(now) {
		p.limited, p.until = true, u.LimitedUntil
	}
	for _, w := range u.Windows {
		if !w.ResetsAt.IsZero() && !w.ResetsAt.After(now) {
			// it has reset since the reading: nothing used, nothing gone by
			p.windows = append(p.windows, pace{minutes: w.Minutes})
			continue
		}
		if w.UsedPercent >= limitedAt {
			p.limited = true
			if w.ResetsAt.After(p.until) {
				p.until = w.ResetsAt
			}
		}
		pc := pace{minutes: w.Minutes, used: w.UsedPercent, resets: w.ResetsAt}
		if w.Minutes > 0 && !w.ResetsAt.IsZero() {
			left := w.ResetsAt.Sub(now).Minutes() / float64(w.Minutes)
			pc.elapsed = 100 * (1 - min(max(left, 0), 1))
			pc.surplus = pc.elapsed - pc.used
		}
		p.windows = append(p.windows, pc)
	}
	sort.SliceStable(p.windows, func(i, j int) bool { return p.windows[i].minutes > p.windows[j].minutes })
	return p
}

// choice is the picker's answer.
type choice struct {
	model string
	why   string // for the human: what the readings said
}

// pickModel takes the candidate (in the role's order) whose provider has the
// most allowance about to lapse, passing over limited providers and avoid.
// ok is false when every candidate is limited; soonest is then when the
// first of them comes back (zero when none says).
func pickModel(candidates []string, usage map[string]model.PlanUsage, now time.Time, avoid string) (c choice, soonest time.Time, ok bool) {
	type ranked struct {
		model string
		plan  plan
		at    int
	}
	var open []ranked
	lengths := map[int]bool{}
	for i, id := range candidates {
		provider, _, err := model.Split(id)
		if err != nil {
			continue
		}
		p := readPlan(usage[provider], now)
		if provider == avoid {
			p.limited = true
		}
		if p.limited {
			if !p.until.IsZero() && (soonest.IsZero() || p.until.Before(soonest)) {
				soonest = p.until
			}
			continue
		}
		for _, w := range p.windows {
			if w.minutes > 0 {
				lengths[w.minutes] = true
			}
		}
		open = append(open, ranked{id, p, i})
	}
	if len(open) == 0 {
		return choice{}, soonest, false
	}
	tiers := make([]int, 0, len(lengths)) // every window length among them, longest first
	for m := range lengths {
		tiers = append(tiers, m)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(tiers)))
	band := func(points float64) float64 { return math.Round(points / paceBand) }
	sort.SliceStable(open, func(i, j int) bool {
		a, b := open[i].plan, open[j].plan
		if x, y := band(a.atRisk()), band(b.atRisk()); x != y {
			return x > y
		}
		for _, m := range tiers {
			if x, y := band(a.surplusAt(m)), band(b.surplusAt(m)); x != y {
				return x > y
			}
		}
		ab, aok := a.budget()
		bb, bok := b.budget()
		if aok && bok && !ab.resets.IsZero() && !bb.resets.IsZero() && !ab.resets.Equal(bb.resets) {
			return ab.resets.Before(bb.resets)
		}
		return open[i].at < open[j].at
	})
	best := open[0]
	return choice{model: best.model, why: describePlan(best.model, best.plan)}, soonest, true
}

// describePlan says what made a model the choice.
func describePlan(id string, p plan) string {
	provider, _, _ := model.Split(id)
	b, ok := p.budget()
	switch {
	case !ok:
		return provider + " has reported no plan usage yet"
	case b.minutes <= 0 || b.resets.IsZero():
		return fmt.Sprintf("%s: %.0f%% used", provider, b.used)
	}
	return fmt.Sprintf("%s: %.0f%% of its %s used with %.0f%% of it gone", provider, b.used, spanName(b.minutes), b.elapsed)
}

func spanName(minutes int) string {
	switch {
	case minutes == 7*24*60:
		return "week"
	case minutes >= 28*24*60 && minutes <= 31*24*60:
		return "month"
	case minutes%(24*60) == 0:
		return fmt.Sprintf("%d days", minutes/(24*60))
	case minutes%60 == 0:
		return fmt.Sprintf("%d hours", minutes/60)
	}
	return fmt.Sprintf("%d minutes", minutes)
}

// candidatesLocked are the models the harness may choose from for a role, in
// preferred order: the role's list, or, for a role that lists none (or only
// patterns), the models stavlos.json lists that the role allows. Only models
// that can be called now (their provider signed in) count.
func (c *Channel) candidatesLocked(preset config.Preset) []string {
	var ids []string
	for _, m := range preset.Models {
		if !strings.ContainsAny(m.ID, "*?[") {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		for _, id := range c.cfg.Models {
			if preset.AllowsModel(id) {
				ids = append(ids, id)
			}
		}
	}
	out := ids[:0:0]
	for _, id := range ids {
		if c.host.CheckModel(id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// chooseLocked is the harness's choice for a role, passing over the provider
// avoid ("" for none). ok is false when there is nothing to choose from or
// every candidate is at its limit.
func (c *Channel) chooseLocked(preset config.Preset, avoid string) (choice, time.Time, bool) {
	cands := c.candidatesLocked(preset)
	if len(cands) == 0 {
		return choice{}, time.Time{}, false
	}
	return pickModel(cands, c.host.PlanUsage(), time.Now(), avoid)
}

// --- a running agent whose model's plan runs out ---

// maxMoves bounds the models one turn may be moved through: every candidate
// refusing in a row ends the turn rather than going round.
const maxMoves = 4

// limitedFor is how long a provider is passed over after refusing a call,
// when neither the refusal nor a reading says when it comes back.
const limitedFor = 15 * time.Minute

// leaveLimitedModel moves the agent off a model whose plan the readings say
// is used up, before it is called. With nowhere to go the call is made
// anyway: a reading is minutes old and the provider has the last word.
func (a *Agent) leaveLimitedModel() {
	c := a.c
	c.mu.Lock()
	provider, _, err := model.Split(a.state().model)
	c.mu.Unlock()
	if err != nil {
		return
	}
	p := readPlan(c.host.PlanUsage()[provider], time.Now())
	if p.limited {
		_, _ = a.moveOff(provider, p.until)
	}
}

// movedOn answers a model call refused for a limit: the provider is marked
// so nothing else chooses it, and the agent moves to the next model its role
// allows. It reports whether the step should run again.
func (t *turnRun) movedOn(err error, modelID string) bool {
	var le *model.LimitError
	if !errors.As(err, &le) || t.moves >= maxMoves {
		return false
	}
	provider, _, serr := model.Split(modelID)
	if serr != nil {
		return false
	}
	host := t.a.c.host
	now := time.Now()
	until := now.Add(limitedFor)
	if le.RetryAfter > 0 {
		until = now.Add(le.RetryAfter)
	} else if p := readPlan(host.PlanUsage()[provider], now); p.limited && p.until.After(now) {
		until = p.until
	}
	host.MarkLimited(provider, until)
	moved, soonest := t.a.moveOff(provider, until)
	if moved {
		t.moves++
		return true
	}
	t.resumeAt = t.a.parkUntil(soonest, until)
	return false
}

// moveOff moves the agent to the harness's choice among its role's models
// other than provider's, and says why in its chat. It reports false, and
// when the first limited candidate comes back, if there is nowhere to go.
func (a *Agent) moveOff(provider string, until time.Time) (bool, time.Time) {
	c := a.c
	c.mu.Lock()
	defer c.mu.Unlock()
	st := a.state()
	preset := c.roleLocked(st).preset
	pick, soonest, ok := c.chooseLocked(preset, provider)
	if !ok || pick.model == st.model {
		return false, soonest
	}
	reason := provider + " is at its limit"
	if !until.IsZero() {
		reason += " until " + until.Local().Format("15:04")
	}
	up := changed(st, "", pick.model, fitVariant(preset, c.host.Variants(pick.model), pick.model, st.variant))
	up.Reason = reason + "; " + pick.why
	err := c.commitLocked(context.Background(), c.event(a.ID, event.AgentUpdated, up))
	return err == nil, soonest
}

// limitHint is a turn's error text: a refusal for a limit that the harness
// could not answer by moving says so, since "status 429" alone reads as a
// fault, and says whether the agent will carry on by itself.
func limitHint(err error, resumeAt time.Time) string {
	var le *model.LimitError
	if !errors.As(err, &le) {
		return err.Error()
	}
	hint := " (this model's plan is at its limit, and no other model this agent's role may use is available: list more under models in the role or in stavlos.json, or pick one with /models"
	if !resumeAt.IsZero() {
		hint += "; the agent carries on by itself once a model is back, about " + resumeAt.Local().Format("Mon 15:04")
	}
	return err.Error() + hint + ")"
}

// --- waking an agent that stopped at every plan's limit ---

// maxResumes bounds the wakes in a row that end in another refusal: a plan
// that never comes back is not asked for ever.
const maxResumes = 12

// resumeText is what the model is told when it is woken.
const resumeText = "Your last turn stopped because every model you may use was at its plan's limit. One is available again: carry on from where you stopped. Nothing else has changed, and nobody has written to you since."

// parkUntil is when to wake an agent whose turn is ending with nowhere to
// move: when the first limited candidate comes back, else when its own
// provider does; zero when the configuration turns this off or the agent has
// been woken too many times to no effect.
func (a *Agent) parkUntil(soonest, own time.Time) time.Time {
	c := a.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.cfg.ResumeAfterLimit || a.state().resumes >= maxResumes {
		return time.Time{}
	}
	if soonest.IsZero() || (!own.IsZero() && own.Before(soonest)) {
		soonest = own
	}
	if soonest.IsZero() {
		soonest = time.Now().Add(limitedFor)
	}
	return soonest
}

// MaybeResume wakes the agents whose turn stopped at every plan's limit and
// whose time has come, provided a model really is available now: the wake is
// an input from the harness, not a message put in the human's mouth, and the
// turn it starts moves the agent to the available model before it calls.
func (c *Channel) MaybeResume(ctx context.Context, now time.Time) error {
	c.mu.Lock()
	var evs []event.Event
	for _, id := range c.st.order {
		st := c.st.agents[id]
		if st.killed || st.inTurn || st.resumeAt.IsZero() || st.resumeAt.After(now) || st.startsTurn() {
			continue
		}
		if !c.modelAvailableLocked(st, now) {
			continue // still nothing to run on: look again at the next tick
		}
		evs = append(evs, c.event(id, event.InputQueued, event.Input{ID: NewID("i"), Kind: event.InputResume, Text: resumeText}))
	}
	if len(evs) == 0 || c.reconfiguring {
		c.mu.Unlock()
		return nil
	}
	err := c.commitLocked(ctx, evs...)
	c.mu.Unlock()
	return err
}

// modelAvailableLocked reports whether the agent has a model to run on now:
// one of its role's candidates, or, for a role with nothing to choose from,
// its own.
func (c *Channel) modelAvailableLocked(st *agentState, now time.Time) bool {
	preset := c.roleLocked(st).preset
	if len(c.candidatesLocked(preset)) > 0 {
		_, _, ok := c.chooseLocked(preset, "")
		return ok
	}
	provider, _, err := model.Split(st.model)
	return err == nil && !readPlan(c.host.PlanUsage()[provider], now).limited
}
