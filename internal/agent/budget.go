package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
)

var errChannelBudget = errors.New("channel budget exhausted")

// budgetReasonLocked accounts for all agents, retries and compactions. Calls
// are reserved atomically before execution; usage/cost are observed limits,
// so already-running calls may finish beyond those thresholds.
func (c *Channel) budgetReasonLocked(now time.Time) string {
	limit := c.cfg.Limits
	switch {
	case limit.MaxChannelCalls > 0 && c.st.modelCalls >= limit.MaxChannelCalls:
		return "limits.maxChannelCalls"
	case limit.MaxChannelTokens > 0 && c.st.budgetTokens >= limit.MaxChannelTokens:
		return "limits.maxChannelTokens"
	case limit.MaxChannelCostUSD > 0 && c.st.budgetCost >= limit.MaxChannelCostUSD:
		return "limits.maxChannelCostUSD"
	}
	if d, _ := time.ParseDuration(limit.MaxChannelDuration); d > 0 && !now.Before(c.Created.Add(d)) {
		return "limits.maxChannelDuration"
	}
	return ""
}

func (a *Agent) beginBudgetCall(ctx context.Context, purpose string) (context.Context, context.CancelFunc, error) {
	c := a.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if why := c.budgetReasonLocked(time.Now()); why != "" {
		return nil, nil, fmt.Errorf("%w (%s); an operator must increase the limit or start a new channel", errChannelBudget, why)
	}
	if err := c.commitLocked(ctx, c.event(a.ID, event.ModelCallStarted, event.ModelCallPayload{Purpose: purpose})); err != nil {
		return nil, nil, err
	}
	if d, _ := time.ParseDuration(c.cfg.Limits.MaxChannelDuration); d > 0 {
		child, cancel := context.WithDeadline(ctx, c.Created.Add(d))
		return child, cancel, nil
	}
	child, cancel := context.WithCancel(ctx)
	return child, cancel, nil
}

func (a *Agent) finishBudgetCall(purpose string, usage model.Usage, cost float64) error {
	return a.recordFact(event.ModelCallCompleted, event.ModelCallPayload{Purpose: purpose, Usage: usage, CostUSD: cost})
}
