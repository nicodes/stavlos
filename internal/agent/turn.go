package agent

import (
	"context"
	"fmt"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

// run is the agent's goroutine: it waits to be woken, then runs turns while
// its inbox holds something that starts one.
func (a *Agent) run() {
	defer a.s.wg.Done()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-a.wake:
		}
		for a.ctx.Err() == nil {
			turn, ctx, ok := a.beginTurn()
			if !ok {
				break
			}
			a.runTurn(ctx, turn)
		}
	}
}

// beginTurn starts a turn when the inbox holds something that starts one:
// it logs turn.started and takes the whole inbox in one transaction (PRD
// §6.3: queued prompts coalesce, answers and job results come along).
func (a *Agent) beginTurn() (int, context.Context, bool) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	st := a.state()
	if st.killed || st.inTurn || !st.startsTurn() {
		return 0, nil, false
	}
	turn := st.turn + 1
	ids := make([]string, len(st.inbox))
	for i, in := range st.inbox {
		ids[i] = in.ID
	}
	if _, err := s.commitLocked(context.Background(),
		s.event(a.ID, event.TurnStarted, event.TurnPayload{Turn: turn}),
		s.event(a.ID, event.InputTaken, event.InputTakenPayload{Turn: turn, IDs: ids})); err != nil {
		return 0, nil, false
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.cancelTurn, a.logErr = cancel, nil
	return turn, ctx, true
}

// runTurn is one run of the agent loop (PRD §6.2): model call, tool calls,
// model call, … until the model stops calling tools or the turn is
// cancelled. What a step does with the model is step; what a tool call
// goes through is permission.go; what the model is told is prompt.go.
func (a *Agent) runTurn(ctx context.Context, turn int) {
	a.disarmMCPIdle()
	t := &turnRun{a: a, ctx: ctx, turn: turn}
	defer func() {
		a.s.mu.Lock()
		cancel := a.cancelTurn
		a.cancelTurn = nil
		a.s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}()
	// A subagent past its role's turn limit does not run: the turn ends at
	// once and every agent waiting on it is told, so nobody waits forever.
	if rv := a.role(); a.Parent != "" && rv.preset.MaxTurns > 0 && turn > rv.preset.MaxTurns {
		t.end(event.ReasonError, fmt.Sprintf("turn limit reached: %s may take at most %d turns", rv.name, rv.preset.MaxTurns))
		a.reportTurnLimit(rv.preset.MaxTurns)
		return
	}
	for {
		if reason, errText, done := t.step(); done {
			t.end(reason, errText)
			a.endReplies(reason)
			return
		}
	}
}

// turnRun is one turn in progress.
type turnRun struct {
	a    *Agent
	ctx  context.Context
	turn int
}

// end logs the turn's end.
func (t *turnRun) end(reason event.TurnReason, errText string) {
	_ = t.a.record(event.TurnEnded, event.TurnEndedPayload{Turn: t.turn, Reason: reason, Error: errText})
	t.a.armMCPIdle()
}

// step is one model call and the tool calls it asks for. It returns
// done=true with the reason when the turn is over.
func (t *turnRun) step() (reason event.TurnReason, errText string, done bool) {
	a, s := t.a, t.a.s
	if t.ctx.Err() != nil {
		return event.ReasonCancelled, "", true
	}
	if err := t.takeMidTurn(); err != nil {
		return event.ReasonError, err.Error(), true
	}
	s.mu.Lock()
	st, cfg := a.state(), s.cfg
	rv, modelID, variant := s.roleLocked(st), st.model, st.variant
	s.mu.Unlock()
	if modelID == "" {
		return event.ReasonError, ErrNoModel, true
	}
	m, info, err := s.host.Resolve(modelID)
	if err != nil {
		return event.ReasonError, err.Error(), true
	}
	// The role's MCP servers start before the prompt is built (their tools
	// are part of it); a server that fails is logged and skipped.
	if len(rv.preset.MCP) > 0 || a.hasMCP() {
		a.ensureMCP(a.ctx, cfg, rv.preset.MCP)
	}
	system, defs := a.buildContext(rv, cfg)
	history := a.prepareHistory(t.ctx, m, info, system, defs)
	history = withNote(history, a.stateNote(rv, cfg))

	resp, err := m.Complete(t.ctx, model.Request{Model: bareID(modelID), System: system, Messages: history, Tools: defs, Variant: variant},
		func(d model.Delta) {
			s.host.Stream(protocol.StreamNotification{Channel: s.ID, Agent: a.ID, Turn: t.turn, Text: d.Text, Thinking: d.Thinking, ToolName: d.ToolName})
		})
	msg := event.AssistantMessagePayload{Turn: t.turn, Blocks: resp.Blocks, StopReason: string(resp.StopReason), Model: modelID, Usage: resp.Usage, CostUSD: info.Cost(resp.Usage)}
	if err != nil {
		cancelled := t.ctx.Err() != nil
		if len(resp.Blocks) > 0 || resp.Usage != (model.Usage{}) {
			if cancelled {
				msg.StopReason = "cancelled"
			}
			_ = a.record(event.AssistantMessage, msg) // what was produced and paid for stays on the record
		}
		if cancelled {
			return event.ReasonCancelled, "", true
		}
		return event.ReasonError, err.Error(), true
	}
	if err := a.record(event.AssistantMessage, msg); err != nil {
		return event.ReasonError, err.Error(), true
	}
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
		a.runTool(t.ctx, t.turn, c, defs, rv, cfg)
	}
	return "", "", false
}

// takeMidTurn hands the model, at this model call, the inputs that do not
// wait for the turn to end (steers, requests, info), and reports a log
// write that failed since the last step.
func (t *turnRun) takeMidTurn() error {
	a, s := t.a, t.a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := a.logErr; err != nil {
		a.logErr = nil
		return err
	}
	var ids []string
	for _, in := range a.state().inbox {
		if midTurn(in.Kind) {
			ids = append(ids, in.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := s.commitLocked(context.Background(), s.event(a.ID, event.InputTaken, event.InputTakenPayload{Turn: t.turn, IDs: ids}))
	return err
}

// withNote appends the per-call state note to the last user message, so it
// sits after everything a provider can cache.
func withNote(history []model.Message, note string) []model.Message {
	if len(history) == 0 || history[len(history)-1].Role != model.RoleUser {
		history = append(history, model.Message{Role: model.RoleUser})
	}
	if note == "" && len(history[len(history)-1].Blocks) > 0 {
		return history
	}
	if note == "" {
		note = "(continue)"
	}
	last := &history[len(history)-1]
	last.Blocks = append(append([]model.Block(nil), last.Blocks...), model.Block{Type: model.BlockText, Text: note})
	return history
}

func bareID(full string) string {
	_, id, err := model.Split(full)
	if err != nil {
		return full
	}
	return id
}
