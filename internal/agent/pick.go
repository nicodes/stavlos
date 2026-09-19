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
// A subscription's long window (the week) is its budget: what is unused when
// it resets is lost. Its short window (five hours) is a throttle on spending
// that budget. So a provider with any window used up is passed over until
// that window resets, and the rest are ranked by how far behind pace their
// budget is: the share of the window that has gone by, less the share used.
// 17% used with 60% of the week gone is 43 points of allowance that will
// lapse unless something uses it; 80% used on day two is a plan to spare.

// limitedAt is the used percent at which a window counts as used up.
const limitedAt = 99.5

// paceBand is how many points of pace count as a tie, which the sooner
// reset, then the role's order, break: readings are minutes old, and a
// choice that flips on a point of noise would scatter agents for no gain.
const paceBand = 5.0

// plan is what a provider's reading says to the picker.
type plan struct {
	known   bool      // a reading exists
	limited bool      // a window is used up, or a call was refused
	until   time.Time // when it stops being limited (zero: unknown)
	surplus float64   // points behind pace in its longest window (negative: ahead)
	used    float64   // percent of that window used
	elapsed float64   // percent of that window gone by
	span    int       // that window's length in minutes
	resets  time.Time // when that window resets
}

func readPlan(u model.PlanUsage, now time.Time) plan {
	var p plan
	if u.LimitedUntil.After(now) {
		p.limited, p.until = true, u.LimitedUntil
	}
	longest := -1
	for i, w := range u.Windows {
		live := w.ResetsAt.IsZero() || w.ResetsAt.After(now)
		if live && w.UsedPercent >= limitedAt {
			p.limited = true
			if w.ResetsAt.After(p.until) {
				p.until = w.ResetsAt
			}
		}
		if longest < 0 || w.Minutes > u.Windows[longest].Minutes {
			longest = i
		}
	}
	if longest < 0 {
		return p
	}
	p.known = true
	w := u.Windows[longest]
	p.span, p.resets = w.Minutes, w.ResetsAt
	if !w.ResetsAt.IsZero() && !w.ResetsAt.After(now) {
		return p // it has reset since the reading: nothing used, nothing gone by
	}
	p.used = w.UsedPercent
	if w.Minutes > 0 && !w.ResetsAt.IsZero() {
		left := w.ResetsAt.Sub(now).Minutes() / float64(w.Minutes)
		p.elapsed = 100 * (1 - min(max(left, 0), 1))
		p.surplus = p.elapsed - p.used
	}
	return p
}

// choice is the picker's answer.
type choice struct {
	model string
	why   string // for the human: what the readings said
}

// pickModel takes the candidate (in the role's order) whose provider has the
// most allowance at risk, passing over limited providers and avoid. ok is
// false when every candidate is limited; soonest is then when the first of
// them comes back (zero when none says).
func pickModel(candidates []string, usage map[string]model.PlanUsage, now time.Time, avoid string) (c choice, soonest time.Time, ok bool) {
	type ranked struct {
		model string
		plan  plan
		at    int
	}
	var open []ranked
	for i, id := range candidates {
		provider, _, err := model.Split(id)
		if err != nil {
			continue
		}
		p := readPlan(usage[provider], now)
		if provider == avoid && !p.limited {
			p.limited = true
		}
		if p.limited {
			if !p.until.IsZero() && (soonest.IsZero() || p.until.Before(soonest)) {
				soonest = p.until
			}
			continue
		}
		open = append(open, ranked{id, p, i})
	}
	if len(open) == 0 {
		return choice{}, soonest, false
	}
	band := func(p plan) float64 { return math.Round(p.surplus / paceBand) }
	sort.SliceStable(open, func(i, j int) bool {
		a, b := open[i].plan, open[j].plan
		if band(a) != band(b) {
			return band(a) > band(b)
		}
		if a.known && b.known && !a.resets.IsZero() && !b.resets.IsZero() && !a.resets.Equal(b.resets) {
			return a.resets.Before(b.resets)
		}
		return open[i].at < open[j].at
	})
	best := open[0]
	return choice{model: best.model, why: describePlan(best.model, best.plan)}, soonest, true
}

// describePlan says what made a model the choice.
func describePlan(id string, p plan) string {
	provider, _, _ := model.Split(id)
	switch {
	case !p.known:
		return provider + " has reported no plan usage yet"
	case p.span <= 0 || p.resets.IsZero():
		return fmt.Sprintf("%s: %.0f%% used", provider, p.used)
	}
	return fmt.Sprintf("%s: %.0f%% of its %s used with %.0f%% of it gone", provider, p.used, spanName(p.span), p.elapsed)
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
	moved, _ := t.a.moveOff(provider, until)
	if moved {
		t.moves++
	}
	return moved
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
	up := changed(st, "", pick.model, fitVariant(preset, pick.model, st.variant))
	up.Reason = reason + "; " + pick.why
	_, err := c.commitLocked(context.Background(), c.event(a.ID, event.AgentUpdated, up))
	return err == nil, soonest
}

// limitHint is a turn's error text: a refusal for a limit that the harness
// could not answer by moving says so, since "status 429" alone reads as a
// fault.
func limitHint(err error) string {
	var le *model.LimitError
	if !errors.As(err, &le) {
		return err.Error()
	}
	return err.Error() + " (this model's plan is at its limit, and no other model this agent's role may use is available: list more under models in the role or in stavlos.json, or pick one with /models)"
}
