package eventlog

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

// TestUsageBuckets: model calls sum into equal time buckets, for every
// channel, one channel or one agent; other events and calls outside the
// span are left out, and a zero From starts at the first call.
func TestUsageBuckets(t *testing.T) {
	l := open(t, filepath.Join(t.TempDir(), "e.db"), nil)
	defer l.Close()
	ctx := context.Background()
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	call := func(channel, agent string, at time.Duration, in, out int, cost float64) {
		appendOne(t, l, event.Event{Channel: channel, Agent: agent, Type: event.AssistantMessage, Time: t0.Add(at),
			Payload: event.MustPayload(event.AssistantMessagePayload{Usage: model.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: 999}, CostUSD: cost})})
	}
	appendOne(t, l, created("a", "a"))
	appendOne(t, l, created("b", "b"))
	call("a", "a1", 0, 100, 10, 0.5)
	call("a", "a2", 30*time.Minute, 200, 20, 1)
	call("b", "b1", 90*time.Minute, 1000, 0, 2)
	call("a", "a1", 3*time.Hour, 7, 0, 9) // after the span
	appendOne(t, l, prompt("a", "human:x", "not a call"))

	span := UsageQuery{From: t0, To: t0.Add(2 * time.Hour), Buckets: 2}
	for _, c := range []struct {
		name           string
		channel, agent string
		tokens         []int
		cost           []float64
	}{
		{"system", "", "", []int{330, 1000}, []float64{1.5, 2}},
		{"channel", "a", "", []int{330, 0}, []float64{1.5, 0}},
		{"agent", "a", "a2", []int{220, 0}, []float64{1, 0}},
	} {
		q := span
		q.Channel, q.Agent = c.channel, c.agent
		s, err := l.Usage(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Tokens) != 2 || s.Tokens[0] != c.tokens[0] || s.Tokens[1] != c.tokens[1] || s.Cost[0] != c.cost[0] || s.Cost[1] != c.cost[1] {
			t.Errorf("%s: tokens %v cost %v, want %v %v", c.name, s.Tokens, s.Cost, c.tokens, c.cost)
		}
	}
	// a zero From starts at the first call, spanning at least an hour
	s, err := l.Usage(ctx, UsageQuery{To: t0.Add(2 * time.Hour), Buckets: 4})
	if err != nil || !s.From.Equal(t0) || s.Tokens[0] != 110 || s.Tokens[1] != 220 || s.Tokens[3] != 1000 {
		t.Fatalf("from the first call: %v %v %v", s.From, s.Tokens, err)
	}
	s, err = l.Usage(ctx, UsageQuery{Channel: "b", To: t0.Add(2 * time.Hour), Buckets: 4})
	if err != nil || !s.From.Equal(t0.Add(time.Hour)) || s.Tokens[2] != 1000 {
		t.Fatalf("at least an hour: %v %v %v", s.From, s.Tokens, err)
	}
}

// TestCacheUsage sums what recent calls carried: new input and what the
// provider served from its prompt cache; older calls and other events are
// left out.
func TestCacheUsage(t *testing.T) {
	l := open(t, filepath.Join(t.TempDir(), "e.db"), nil)
	defer l.Close()
	ctx := context.Background()
	appendOne(t, l, created("a", "a"))
	call := func(at time.Duration, in, cached int) {
		appendOne(t, l, event.Event{Channel: "a", Agent: "a1", Type: event.AssistantMessage, Time: time.Now().Add(at),
			Payload: event.MustPayload(event.AssistantMessagePayload{Usage: model.Usage{InputTokens: in, OutputTokens: 5, CacheReadTokens: cached}})})
	}
	call(-90*time.Minute, 1_000, 0) // before the window
	call(-30*time.Minute, 100, 900)
	call(-time.Minute, 200, 800)
	appendOne(t, l, prompt("a", "human:x", "not a call"))
	fresh, cached, err := l.CacheUsage(ctx, time.Now().Add(-time.Hour))
	if err != nil || fresh != 300 || cached != 1700 {
		t.Fatalf("fresh %d cached %d: %v", fresh, cached, err)
	}
}
