package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/config"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

// run is the agent's goroutine: it waits to be woken, then runs turns while
// its inbox holds something that starts one.
func (a *Agent) run() {
	defer a.c.wg.Done()
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
	s := a.c
	s.mu.Lock()
	defer s.mu.Unlock()
	st := a.state()
	if s.reconfiguring || st.killed || st.inTurn || !st.startsTurn() {
		return 0, nil, false
	}
	turn := st.turn + 1
	ids := make([]string, len(st.inbox))
	for i, in := range st.inbox {
		ids[i] = in.ID
	}
	if err := s.commitLocked(context.Background(),
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
	a.c.checkProject() // an edited AGENTS.md or role is trusted again before a turn uses it
	t := &turnRun{a: a, ctx: ctx, turn: turn}
	defer func() {
		a.c.mu.Lock()
		cancel := a.cancelTurn
		a.cancelTurn = nil
		a.c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}()
	// A subagent past its role's turn limit does not run: the turn ends at
	// once and every agent waiting on it is told, so nobody waits forever.
	if rv := a.role(); a.Parent != "" && rv.def.MaxTurns > 0 && turn > rv.def.MaxTurns {
		t.end(event.ReasonError, fmt.Sprintf("turn limit reached: %s may take at most %d turns", rv.name, rv.def.MaxTurns))
		a.reportTurnLimit(rv.def.MaxTurns)
		return
	}
	for {
		if cap := a.c.Config().Limits.MaxCallsPerTurn; cap > 0 && t.calls >= cap {
			// A turn that never ends by itself is a turn that is waiting for
			// something the wrong way. The human sees why it stopped, and
			// the agent is told how to wait next time.
			t.end(event.ReasonError, fmt.Sprintf("stopped after %d model calls in one turn (limits.maxCallsPerTurn). To wait for something, run the command with until_changed: true, or end the turn: a job or a child wakes you.", cap))
			a.endReplies(event.ReasonError)
			return
		}
		if reason, errText, done := t.step(); done {
			t.end(reason, errText)
			a.endReplies(reason)
			return
		}
	}
}

// turnRun is one turn in progress.
type turnRun struct {
	a        *Agent
	ctx      context.Context
	turn     int
	moves    int       // models the harness moved the agent to within this turn
	calls    int       // model calls made in this turn (limits.maxCallsPerTurn)
	resumeAt time.Time // the turn is ending at every plan's limit: when to wake the agent (movedOn)
}

// end logs the turn's end.
func (t *turnRun) end(reason event.TurnReason, errText string) {
	p := event.TurnEndedPayload{Turn: t.turn, Reason: reason, Error: errText}
	if reason == event.ReasonError {
		p.ResumeAt = t.resumeAt
	}
	if err := t.a.recordFact(event.TurnEnded, p); err != nil && !errors.Is(err, errStopped) {
		t.a.turnEndLost(err)
	}
	t.a.armMCPIdle()
}

// step is one model call and the tool calls it asks for. It returns
// done=true with the reason when the turn is over.
func (t *turnRun) step() (reason event.TurnReason, errText string, done bool) {
	if t.ctx.Err() != nil {
		return event.ReasonCancelled, "", true
	}
	p, errText := t.prepare()
	if errText != "" {
		return event.ReasonError, errText, true
	}
	resp, err := t.call(p)
	t.calls++
	msg := event.AssistantMessagePayload{Turn: t.turn, Blocks: resp.Blocks, StopReason: string(resp.StopReason), Model: p.modelID, Usage: resp.Usage, CostUSD: p.info.Cost(resp.Usage)}
	if err != nil {
		return t.onError(err, p, resp, msg)
	}
	if err := t.a.record(event.AssistantMessage, msg); err != nil {
		return event.ReasonError, err.Error(), true
	}
	if !t.runTools(p, resp) {
		if resp.StopReason == model.StopMaxTokens {
			return event.ReasonMaxTokens, "", true
		}
		return event.ReasonEndTurn, "", true
	}
	return "", "", false
}

// stepPlan is everything one model call is made from, read once: the role and
// configuration as they were when the step began, the model, and the prompt.
type stepPlan struct {
	rv      roleView
	cfg     *config.Effective
	modelID string
	variant string
	m       model.Model
	info    model.Info
	system  string
	defs    []model.ToolDef
	history []model.Message
}

// prepare takes what arrived mid-turn, settles which model the step runs on
// and builds the prompt. errText is why the turn cannot go on.
func (t *turnRun) prepare() (p stepPlan, errText string) {
	a, s := t.a, t.a.c
	if msg := config.DirectoryError(s.Dir()); msg != "" {
		return p, msg
	}
	if err := t.takeMidTurn(); err != nil {
		return p, err.Error()
	}
	a.leaveLimitedModel() // a plan known to be used up is not called just to be refused
	s.mu.Lock()
	st := a.state()
	p.cfg, p.rv, p.modelID, p.variant = s.cfg, s.roleLocked(st), st.model, st.variant
	s.mu.Unlock()
	if p.modelID == "" {
		return p, ErrNoModel
	}
	if !p.rv.def.AllowsModel(p.modelID) || !p.rv.def.AllowsVariant(p.modelID, p.variant) {
		return p, "the current model or variant is not allowed by this project's role; use /models or /variants to select one"
	}
	var err error
	if p.m, p.info, err = s.host.Resolve(p.modelID); err != nil {
		return p, err.Error()
	}
	// The role's MCP servers start before the prompt is built (their tools
	// are part of it); a server that fails is logged and skipped.
	if len(p.rv.def.MCP) > 0 || a.hasMCP() {
		a.ensureMCP(a.ctx, p.cfg, p.rv.def.MCP)
	}
	p.system, p.defs = a.buildContext(p.rv, p.cfg)
	p.history = withNote(a.prepareHistory(t.ctx, p.m, p.info, p.system, p.defs), a.stateNote(p.rv, p.cfg))
	return p, ""
}

// call makes the model call, streaming what it says to the channel's clients.
func (t *turnRun) call(p stepPlan) (model.Response, error) {
	a, s := t.a, t.a.c
	return p.m.Complete(t.ctx, model.Request{Model: bareID(p.modelID), System: p.system, Messages: p.history, Tools: p.defs, Variant: p.variant, CacheKey: a.ID},
		func(d model.Delta) {
			s.host.Stream(protocol.StreamNotification{Channel: s.ID, Agent: a.ID, Turn: t.turn, Text: d.Text, Thinking: d.Thinking, ToolName: d.ToolName, Reset: d.Reset})
		})
}

// onError is a model call that failed: the step runs again when the harness
// moved the agent to another model, and otherwise the turn ends, with what
// was produced and paid for kept on the record.
func (t *turnRun) onError(err error, p stepPlan, resp model.Response, msg event.AssistantMessagePayload) (event.TurnReason, string, bool) {
	cancelled := t.ctx.Err() != nil
	if !cancelled && t.movedOn(err, p.modelID) {
		return "", "", false // the same step again, on the model the harness moved the agent to
	}
	if len(resp.Blocks) > 0 || resp.Usage != (model.Usage{}) {
		if cancelled {
			msg.StopReason = "cancelled"
		}
		_ = t.a.record(event.AssistantMessage, msg)
	}
	if cancelled {
		return event.ReasonCancelled, "", true
	}
	return event.ReasonError, limitHint(err, t.resumeAt), true
}

// runTools runs the calls a response asks for, in order, and reports whether
// there were any: a response with none ends the turn.
func (t *turnRun) runTools(p stepPlan, resp model.Response) bool {
	any := false
	for _, b := range resp.Blocks {
		if b.Type != model.BlockToolUse {
			continue
		}
		any = true
		if t.ctx.Err() == nil {
			t.a.runTool(t.ctx, t.turn, b, p.defs, p.rv, p.cfg)
		}
	}
	return any
}

// takeMidTurn hands the model, at this model call, the inputs that do not
// wait for the turn to end (steers, requests, info), and reports a log
// write that failed since the last step.
func (t *turnRun) takeMidTurn() error {
	a, s := t.a, t.a.c
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
	err := s.commitLocked(context.Background(), s.event(a.ID, event.InputTaken, event.InputTakenPayload{Turn: t.turn, IDs: ids}))
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

// turnEndLost says what happened when a turn's end could not be written. The
// turn has ended in memory all the same (commitFactLocked), so the agent
// takes the next; this is where the human learns why the log is short.
func (a *Agent) turnEndLost(err error) {
	c := a.c
	c.mu.Lock()
	a.state().lastError = "the turn ended but could not be logged: " + strings.TrimPrefix(err.Error(), "event log: ")
	a.logErr = nil // reported here; the next turn starts clean
	c.mu.Unlock()
}
