package model

import (
	"errors"
	"math"
	"testing"
	"time"
)

// TestSplitWantsAProviderAndAnID: "provider/id" splits at the first slash
// (an id may hold more); a missing side or no slash is an error naming the
// input.
func TestSplitWantsAProviderAndAnID(t *testing.T) {
	for _, tc := range []struct{ in, provider, id string }{
		{"anthropic/claude-opus-5", "anthropic", "claude-opus-5"},
		{"openrouter/meta/llama-3", "openrouter", "meta/llama-3"},
		{"a/b", "a", "b"},
	} {
		provider, id, err := Split(tc.in)
		if err != nil || provider != tc.provider || id != tc.id {
			t.Errorf("Split(%q) = %q %q %v", tc.in, provider, id, err)
		}
	}
	for _, in := range []string{"/x", "x/", "x", "", "/"} {
		provider, id, err := Split(in)
		if err == nil || provider != "" || id != "" {
			t.Errorf("Split(%q) = %q %q %v, want an error", in, provider, id, err)
		}
	}
}

// TestInfoCostIsPerMillionTokens: each kind of token is priced by its own
// rate per million.
func TestInfoCostIsPerMillionTokens(t *testing.T) {
	i := Info{InputPrice: 3, OutputPrice: 15, CacheReadPrice: 0.3, CacheWritePrice: 3.75}
	u := Usage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 2_000_000, CacheWriteTokens: 400_000}
	if got, want := i.Cost(u), 3+1.5+0.6+1.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("Cost = %v, want %v", got, want)
	}
	if got := (Info{}).Cost(u); got != 0 {
		t.Errorf("no prices, no cost: %v", got)
	}
	if got := i.Cost(Usage{}); got != 0 {
		t.Errorf("no tokens, no cost: %v", got)
	}
}

// TestUsageFromTakesTheCachedPartOutOfInput: providers report cached
// tokens inside the input count; the record keeps them apart and never
// goes negative.
func TestUsageFromTakesTheCachedPartOutOfInput(t *testing.T) {
	if got, want := UsageFrom(1000, 50, 300), (Usage{InputTokens: 700, OutputTokens: 50, CacheReadTokens: 300}); got != want {
		t.Errorf("UsageFrom = %+v, want %+v", got, want)
	}
	if got, want := UsageFrom(100, 0, 250), (Usage{InputTokens: 0, CacheReadTokens: 250}); got != want {
		t.Errorf("cached over input = %+v, want %+v", got, want)
	}
}

// TestLimitErrorUnwraps: a limit error reads as the error it wraps and
// errors.Is/As see through it.
func TestLimitErrorUnwraps(t *testing.T) {
	inner := errors.New("429 too many requests")
	var err error = &LimitError{Err: inner, RetryAfter: 30 * time.Second}
	if err.Error() != inner.Error() || !errors.Is(err, inner) {
		t.Errorf("LimitError: %v", err)
	}
	var le *LimitError
	if !errors.As(err, &le) || le.RetryAfter != 30*time.Second {
		t.Errorf("errors.As: %v", le)
	}
}
