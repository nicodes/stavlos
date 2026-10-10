package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

func TestChannelCallBudgetReservesAcrossConcurrentAgents(t *testing.T) {
	c, _ := newTestChannel(t, testConfig{}, &fakeModel{})
	c.mu.Lock()
	c.cfg.Limits.MaxChannelCalls = 7
	c.mu.Unlock()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, cancel, err := c.Root().beginBudgetCall(context.Background(), "turn")
			if err == nil {
				accepted.Add(1)
				cancel()
			} else if !errors.Is(err, errChannelBudget) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 7 {
		t.Fatalf("reserved %d calls, wanted 7", accepted.Load())
	}
}

func TestChannelBudgetIncludesCompactionUsageAndRecovery(t *testing.T) {
	c, h := newTestChannel(t, testConfig{}, &fakeModel{})
	c.mu.Lock()
	c.cfg.Limits.MaxChannelCalls = 1
	c.cfg.Limits.MaxChannelTokens = 100
	c.mu.Unlock()
	_, cancel, err := c.Root().beginBudgetCall(context.Background(), "compaction")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	usage := model.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40}
	if err := c.Root().finishBudgetCall("compaction", usage, 1.25); err != nil {
		t.Fatal(err)
	}
	restored := newChannelState("", "")
	for _, e := range h.all() {
		restored.apply(e, &effects{})
	}
	if restored.modelCalls != 1 || restored.budgetTokens != 100 || restored.budgetCost != 1.25 {
		t.Fatalf("recovered budget differs: %d %d %f", restored.modelCalls, restored.budgetTokens, restored.budgetCost)
	}
	if _, _, err := c.Root().beginBudgetCall(context.Background(), "turn"); !errors.Is(err, errChannelBudget) {
		t.Fatal("compaction did not exhaust shared call budget")
	}
}

func TestObservedUsageCostAndElapsedLimitsPreventAnotherCall(t *testing.T) {
	for _, kind := range []string{"tokens", "cost", "duration"} {
		t.Run(kind, func(t *testing.T) {
			c, _ := newTestChannel(t, testConfig{}, &fakeModel{})
			c.mu.Lock()
			switch kind {
			case "tokens":
				c.cfg.Limits.MaxChannelTokens = 50
				c.st.budgetTokens = 50
			case "cost":
				c.cfg.Limits.MaxChannelCostUSD = 1
				c.st.budgetCost = 1
			case "duration":
				c.cfg.Limits.MaxChannelDuration = "1m"
				c.Created = time.Now().Add(-2 * time.Minute)
			}
			c.mu.Unlock()
			if _, _, err := c.Root().beginBudgetCall(context.Background(), "turn"); !errors.Is(err, errChannelBudget) {
				t.Fatal("exhausted budget admitted call")
			}
		})
	}
}

func TestReservationFailureDoesNotSpendACall(t *testing.T) {
	c, h := newTestChannel(t, testConfig{}, &fakeModel{})
	h.failType = event.ModelCallStarted
	if _, _, err := c.Root().beginBudgetCall(context.Background(), "turn"); err == nil {
		t.Fatal("reservation proceeded without durable evidence")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.modelCalls != 0 {
		t.Fatal("failed transaction changed budget")
	}
}
