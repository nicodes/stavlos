package agent

import (
	"context"
	"fmt"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

// runTurn is one run of the agent loop (PRD §6.2): model call, tool calls,
// model call, … until the model stops calling tools or the turn is
// cancelled. The loop itself lives here; what one step does with the
// model is step, what a tool call goes through is permission.go, what the
// model is told is prompt.go, and compaction is compact.go.
func (a *Agent) runTurn(inputs []event.UserMessagePayload) {
	turnCtx, cancel := context.WithCancel(a.ctx)
	a.mu.Lock()
	a.turn++
	turn := a.turn
	a.state = StateRunning
	a.cancelTurn = cancel
	a.lastError = ""
	a.mu.Unlock()
	a.disarmMCPIdle()
	defer func() {
		cancel()
		a.mu.Lock()
		a.cancelTurn = nil
		if a.state == StateRunning || a.state == StateBlocked {
			a.state = StateIdle
		}
		a.mu.Unlock()
	}()

	t := &turnRun{a: a, ctx: turnCtx, turn: turn, bg: context.Background()} // logging must not be cut short by cancellation
	if _, err := a.record(t.bg, event.TurnStarted, event.TurnPayload{Turn: turn}); err != nil {
		return
	}
	for _, in := range inputs {
		in.Turn = turn
		_, _ = a.record(t.bg, event.UserMessage, in)
		a.took(in)
	}

	// A subagent past its role's turn limit does not run: the turn ends at
	// once and every agent waiting on it is told, so nobody waits forever.
	if rv := a.role(); a.Parent != "" && rv.preset.MaxTurns > 0 && turn > rv.preset.MaxTurns {
		limit := rv.preset.MaxTurns
		t.end(event.ReasonError, fmt.Sprintf("turn limit reached: %s may take at most %d turns", rv.label, limit))
		a.reportTurnLimit(limit)
		return
	}

	for {
		if reason, errText, done := t.step(); done {
			t.end(reason, errText)
			a.endReplies(t.bg, reason)
			return
		}
	}
}

// turnRun is one turn in progress: the agent, the turn's context and
// number, and the background context its log writes use.
type turnRun struct {
	a    *Agent
	ctx  context.Context
	turn int
	bg   context.Context
}

// end flips the agent back to idle before logging TurnEnded, so a client
// that reacts to the event never observes a stale "running" state.
func (t *turnRun) end(reason event.TurnReason, errText string) {
	a := t.a
	a.mu.Lock()
	a.cancelTurn = nil
	if reason == event.ReasonError {
		a.lastError = errText
	}
	if a.state == StateRunning || a.state == StateBlocked {
		a.state = StateIdle
	}
	a.mu.Unlock()
	_, _ = a.record(t.bg, event.TurnEnded, event.TurnEndedPayload{Turn: t.turn, Reason: reason, Error: errText})
	a.armMCPIdle()
}

// step is one model call and the tool calls it asks for. It returns
// done=true with the reason when the turn is over.
func (t *turnRun) step() (reason event.TurnReason, errText string, done bool) {
	a := t.a
	if t.ctx.Err() != nil {
		return event.ReasonCancelled, "", true
	}
	if err := a.takeLogErr(); err != nil {
		return event.ReasonError, err.Error(), true
	}
	t.injectSteers()

	modelID := a.ModelID()
	if modelID == "" {
		return event.ReasonError, ErrNoModel, true
	}
	m, info, err := a.s.host.Resolve(modelID)
	if err != nil {
		return event.ReasonError, err.Error(), true
	}
	rv := a.role() // one consistent view of the role for this step (SetRole may run meanwhile)
	cfg := a.s.Config()
	// The role's MCP servers start before the prompt is built (their tools
	// are part of it); a server that fails is logged and skipped.
	if len(rv.preset.MCP) > 0 || a.hasMCP() {
		a.ensureMCP(a.ctx, cfg)
	}
	system, defs := a.buildContext(rv, cfg)
	history := a.prepareHistory(t.ctx, m, info, system, defs)

	resp, err := m.Complete(t.ctx, model.Request{Model: bareID(modelID), System: system, Messages: history, Tools: defs, Variant: a.Variant()},
		func(d model.Delta) {
			a.s.host.Stream(protocol.StreamNotification{Channel: a.s.ID, Agent: a.ID, Turn: t.turn, Text: d.Text, Thinking: d.Thinking, ToolName: d.ToolName})
		})
	if resp.Usage != (model.Usage{}) {
		cost := info.Cost(resp.Usage)
		a.mu.Lock()
		a.usage.tokens += resp.Usage.InputTokens + resp.Usage.OutputTokens
		a.usage.cost += cost
		a.mu.Unlock()
		_, _ = a.record(t.bg, event.Usage, event.UsagePayload{Turn: t.turn, Model: modelID, Usage: resp.Usage, CostUSD: cost})
	}
	if err != nil {
		if t.ctx.Err() != nil {
			if len(resp.Blocks) > 0 {
				_, _ = a.record(t.bg, event.AssistantMessage, event.AssistantMessagePayload{Turn: t.turn, Blocks: resp.Blocks, StopReason: "cancelled", Model: modelID})
			}
			return event.ReasonCancelled, "", true
		}
		return event.ReasonError, err.Error(), true
	}
	_, _ = a.record(t.bg, event.AssistantMessage, event.AssistantMessagePayload{Turn: t.turn, Blocks: resp.Blocks, StopReason: string(resp.StopReason), Model: modelID})

	var calls []model.Block
	for _, b := range resp.Blocks {
		if b.Type == model.BlockToolUse {
			calls = append(calls, b)
		}
	}
	if len(calls) == 0 {
		if resp.StopReason == model.StopMaxTokens {
			return event.ReasonMaxTokens, "", true
		}
		return event.ReasonEndTurn, "", true
	}
	for _, c := range calls {
		if t.ctx.Err() != nil {
			break
		}
		a.runTool(t.ctx, t.turn, c, defs, rv)
	}
	return "", "", false
}

// injectSteers logs the steers that arrived mid-turn as user messages, at
// the model-call boundary where the model will see them.
func (t *turnRun) injectSteers() {
	a := t.a
	a.mu.Lock()
	steers, notes := a.steers, a.notes
	a.steers, a.notes = nil, nil
	a.mu.Unlock()
	defer func() { // notes land after the steers, needing no reply
		for _, q := range notes {
			_, _ = a.record(t.bg, event.UserMessage, event.UserMessagePayload{Turn: t.turn, Kind: event.MsgNote, Text: q.text, From: a.s.senderLabel(q.source), FromID: senderID(q.source)})
		}
	}()
	for _, st := range steers {
		in := event.UserMessagePayload{Turn: t.turn, Kind: event.MsgSteer, Text: st.text, From: a.s.senderLabel(st.source), FromID: senderID(st.source), Post: st.post}
		_, _ = a.record(t.bg, event.UserMessage, in)
		a.took(in)
	}
}

func bareID(full string) string {
	_, id, err := model.Split(full)
	if err != nil {
		return full
	}
	return id
}

func (a *Agent) setState(st State) {
	a.mu.Lock()
	if a.state != StateKilled {
		a.state = st
	}
	a.mu.Unlock()
}
