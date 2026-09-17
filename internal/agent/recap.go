package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// A recap keeps a channel from going quiet: when the human has not heard
// from it for the set minutes, and its agents have done something since the
// last one, the main agent is asked for a status report. The ask is an
// ordinary request from the human, so it owes an explicit response and the
// nudges tab lists it until it answers.

// recapBounds are the shortest and longest interval a recap may be set to.
const (
	recapMin = 1 * time.Minute
	recapMax = 24 * time.Hour
)

// recapText is what the main agent is asked for.
const recapText = "Send the human a short status report: what you are working on now, what is done, what is blocked, and what comes next. Two or three sentences, no preamble. This request came from the channel's recap timer, not from the human typing."

// SetRecap sets how many minutes of silence may pass before the main agent
// is asked for a status report; 0 turns it off.
func (c *Channel) SetRecap(ctx context.Context, minutes int) error {
	switch d := time.Duration(minutes) * time.Minute; {
	case minutes < 0:
		return fmt.Errorf("recap %d: minutes, or 0 to turn it off", minutes)
	case minutes > 0 && d < recapMin:
		return fmt.Errorf("recap %d: at least %s", minutes, recapMin)
	case d > recapMax:
		return fmt.Errorf("recap %d: at most %s", minutes, recapMax)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.recap == minutes {
		return nil
	}
	_, err := c.commitLocked(ctx, c.event("", event.ChannelUpdated, event.ChannelUpdatedPayload{Recap: &minutes}))
	return err
}

// Recap is the channel's recap interval in minutes; 0 when it is off.
func (c *Channel) Recap() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.recap
}

// MaybeRecap asks the main agent for a status report when the channel has
// been quiet for the set interval and its agents have worked since the last
// one. A channel that has done nothing new says nothing new, so it is left
// alone, and an unanswered recap is never asked twice: it is owed like any
// request, and the nudges chase it. The daemon calls this on a timer.
func (c *Channel) MaybeRecap(ctx context.Context, now time.Time) error {
	c.mu.Lock()
	st := c.st
	every := time.Duration(st.recap) * time.Minute
	main := ""
	if len(st.order) > 0 {
		main = st.order[0]
	}
	if every == 0 || main == "" || st.agents[main].killed {
		c.mu.Unlock()
		return nil
	}
	// the clock runs from whatever the human last heard, and from the last
	// recap asked for; a channel that never spoke starts at its last work
	since := latest(st.lastHeard, st.lastRecap, c.Created)
	if st.recapOpen || now.Sub(since) < every || !st.lastWork.After(st.lastRecap) {
		c.mu.Unlock()
		return nil // already asked, quiet for less than the interval, or nothing new to report
	}
	st.lastRecap, st.recapOpen = now, true
	id := NewID("recap")
	wake, err := c.commitLocked(ctx, c.event(main, event.InputQueued, event.Input{
		ID: NewID("i"), RequestID: id, Kind: event.InputPrompt, Text: recapText, To: []string{st.agents[main].name}, From: "", FromName: tools.User,
	}))
	c.mu.Unlock()
	signal(wake)
	return err
}

// latest is the most recent of the times given, zero times ignored.
func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}
