package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/project"
)

// prepareHistory projects the agent's events into the model's history,
// compacts first when asked (/compact while busy) or when the history is
// past the threshold, records the size the call will carry, and makes
// sure the history ends with a user message.
func (a *Agent) prepareHistory(turnCtx context.Context, m model.Model, info model.Info, system string, defs []model.ToolDef) []model.Message {
	history := a.history()
	est := project.EstimateTokens(history, system, defs)
	a.mu.Lock()
	wanted, inFlight := a.compactNext, a.compacting
	if !inFlight {
		a.compactNext = false
		a.compacting = true // released below; a /compact arriving meanwhile queues instead of running alongside
	}
	a.mu.Unlock()
	if !inFlight {
		n := len(a.eventsCopy())
		target := -1
		switch {
		case wanted:
			target = n // every completed turn
		case info.ContextWindow > 0 && est > int(float64(info.ContextWindow)*a.s.Config().Compaction.Threshold):
			target = n * 2 / 3 // the older two thirds
		}
		if target >= 0 {
			if err := a.compact(turnCtx, m, info, target); err == nil {
				history = a.history()
				est = project.EstimateTokens(history, system, defs)
			}
		}
		a.endCompacting()
	}
	a.mu.Lock()
	a.ctxTokens, a.ctxWindow = est, info.ContextWindow
	a.mu.Unlock()
	if len(history) == 0 || history[len(history)-1].Role != model.RoleUser {
		history = append(history, model.Message{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText, Text: "(continue)"}}})
	}
	return history
}

// compact summarises older turns (PRD §4.3). It picks the last turn
// boundary at or before index target in the agent's events (two thirds of
// the way for auto-compaction, the end for /compact), summarises everything
// up to it with the model, and logs a Compacted event.
func (a *Agent) compact(ctx context.Context, m model.Model, info model.Info, target int) error {
	evs := a.eventsCopy()
	cut := -1
	for i, e := range evs {
		if e.Type == event.TurnEnded && i <= target {
			cut = i
		}
	}
	if cut < 0 {
		return errors.New("nothing to compact")
	}
	before := project.EstimateTokens(project.Project(evs), "", nil)
	old := project.Project(evs[:cut+1])
	transcript := project.Transcript(old)
	if len(transcript) > 400_000 {
		transcript = transcript[len(transcript)-400_000:]
	}
	// Clients draw a bar while the summariser runs.
	_, _ = a.record(context.Background(), event.CompactionStarted, event.CompactionPayload{Before: before})
	fail := func(err error) error {
		_, _ = a.record(context.Background(), event.CompactionFailed, event.CompactionPayload{Before: before, Error: err.Error()})
		return err
	}
	resp, err := m.Complete(ctx, summaryRequest(bareID(a.ModelID()), transcript, info), nil)
	if err != nil {
		return fail(err)
	}
	var sb strings.Builder
	for _, b := range resp.Blocks {
		if b.Type == model.BlockText {
			sb.WriteString(b.Text)
		}
	}
	if sb.Len() == 0 {
		return fail(errors.New("empty summary"))
	}
	payload := event.CompactedPayload{FromSeq: evs[0].Seq, ToSeq: evs[cut].Seq, Summary: sb.String(), Before: before}
	// After: the history as the next call will see it, summary included.
	kept := append([]event.Event{}, evs[cut+1:]...)
	kept = append(kept, event.Event{Type: event.Compacted, Seq: evs[len(evs)-1].Seq + 1, Payload: event.MustPayload(payload)})
	payload.After = project.EstimateTokens(project.Project(append(append([]event.Event{}, evs[:cut+1]...), kept...)), "", nil)
	_, err = a.record(context.Background(), event.Compacted, payload)
	return err
}

// summaryMaxTokens bounds the summary; summaryWords is the same bound in
// words for models that ignore max tokens.
const (
	summaryMaxTokens = 4000
	summaryWords     = "2,500"
)

// summaryRequest is the summariser call for transcript.
func summaryRequest(modelID, transcript string, info model.Info) model.Request {
	system := "You summarise an AI coding agent's conversation so it can continue with less context. Preserve: the task and its current status, decisions made and why, files touched with paths, commands run and their outcomes, open problems, and anything the user asked for. Be concrete and complete; omit pleasantries."
	req := model.Request{
		Model: modelID,
		Messages: []model.Message{{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText,
			Text: "Summarise this transcript:\n\n" + transcript}}}},
	}
	if info.IgnoresMaxTokens {
		system += " Keep the summary under " + summaryWords + " words."
	} else {
		req.MaxTokens = summaryMaxTokens
	}
	req.System = system
	return req
}
