package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/nicodes/stavlos/internal/clip"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/project"
)

// prepareHistory is the history a model call carries: compacted first when
// /compact asked for it mid-turn, or when it has grown past the threshold
// of the model's context window. It records the size the call will carry.
func (a *Agent) prepareHistory(turnCtx context.Context, m model.Model, info model.Info, system string, defs []model.ToolDef) []model.Message {
	s := a.c
	s.mu.Lock()
	st := a.state()
	history := st.hist.History()
	wanted, running := a.compactNext, st.compacting
	if !running {
		a.compactNext = false
	}
	threshold := s.cfg.Compaction.Threshold
	s.mu.Unlock()
	est := project.EstimateTokens(history, system, defs)
	if !running && (wanted || info.ContextWindow > 0 && est > int(float64(info.ContextWindow)*threshold)) {
		if err := a.compact(turnCtx, m, info, wanted); err == nil {
			history = a.history()
			est = project.EstimateTokens(history, system, defs)
		}
	}
	s.mu.Lock()
	a.ctxTokens, a.ctxWindow = est, info.ContextWindow
	s.mu.Unlock()
	return history
}

// summaryInputMax bounds the transcript a summariser reads: its tail.
const summaryInputMax = 400_000

// compact summarises older turns (PRD §4.3): everything up to the last turn
// that ended (all, for /compact) or the last one within the older two
// thirds of the history (automatic), replaced by the model's summary.
func (a *Agent) compact(ctx context.Context, m model.Model, info model.Info, all bool) error {
	s := a.c
	s.mu.Lock()
	st := a.state()
	if st.compacting {
		s.mu.Unlock()
		return errors.New("a compaction is already running")
	}
	cut, ok := st.hist.Cut(all)
	if !ok {
		s.mu.Unlock()
		return errors.New("nothing to compact")
	}
	modelID := st.model
	_, err := s.commitLocked(context.Background(), s.event(a.ID, event.CompactionStarted, event.CompactionPayload{Before: cut.Before}))
	s.mu.Unlock()
	if err != nil {
		return err
	}
	resp, err := m.Complete(ctx, summaryRequest(bareID(modelID), clip.Tail(project.Transcript(cut.Old), summaryInputMax), info), nil)
	var sb strings.Builder
	for _, b := range resp.Blocks {
		if b.Type == model.BlockText {
			sb.WriteString(b.Text)
		}
	}
	if err == nil && sb.Len() == 0 {
		err = errors.New("empty summary")
	}
	if err != nil {
		_ = a.record(event.CompactionFailed, event.CompactionPayload{Before: cut.Before, Error: err.Error()})
		return err
	}
	summary := sb.String()
	return a.record(event.CompactionDone, event.CompactionPayload{FromSeq: cut.FromSeq, ToSeq: cut.ToSeq, Summary: summary, Before: cut.Before, After: cut.After(summary)})
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
