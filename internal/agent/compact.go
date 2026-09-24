package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/nicodes/stavlos/internal/clip"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/project"
	"github.com/nicodes/stavlos/internal/toolname"
)

// prepareHistory is the history a model call carries: compacted first when
// /compact asked for it mid-turn, or when it has grown past the threshold
// of the model's context window (or past compaction.maxTokens, whatever the
// window). It records the size the call will carry.
func (a *Agent) prepareHistory(turnCtx context.Context, m model.Model, info model.Info, system string, defs []model.ToolDef) []model.Message {
	s := a.c
	s.mu.Lock()
	st := a.state()
	history := st.hist.History()
	wanted, running := a.compactNext, st.compacting
	if !running {
		a.compactNext = false
	}
	cc := s.cfg.Compaction
	last := st.lastContext
	s.mu.Unlock()
	// Old tool results go first: lossless, free, and most of what a history
	// holds. Compaction below is for what is left.
	_, gone := project.ClearOld(history, cc.ClearTokens, project.ClearMinimum, conversationTools)
	est := project.EstimateTokens(history, system, defs)
	// the provider's own count of the last call, when it is larger than the
	// estimate: the estimate drifts, and the backend's number is what fills
	// the window
	size := max(est, last)
	full := info.ContextWindow > 0 && size > int(float64(usableWindow(info))*cc.Threshold)
	over := cc.MaxTokens > 0 && size > cc.MaxTokens
	if !running && (wanted || full || over) {
		if err := a.compact(turnCtx, m, info, wanted); err == nil {
			history = a.history()
			_, gone = project.ClearOld(history, cc.ClearTokens, project.ClearMinimum, conversationTools)
			est = project.EstimateTokens(history, system, defs)
		}
	}
	s.mu.Lock()
	a.ctxTokens, a.ctxWindow = est, info.ContextWindow
	a.forgetReads(gone)
	s.mu.Unlock()
	return history
}

// usableWindow is the part of a model's window a history may fill: the
// window less what the reply needs, as OpenCode reserves min(20k, the max
// output) and Codex takes 95% then compacts at 90% of that. The threshold
// applies to this, so the window is a ceiling and not a target.
func usableWindow(info model.Info) int {
	reserve := min(reserveOutput, info.ContextWindow/10)
	if info.MaxOutput > 0 {
		reserve = min(reserve, info.MaxOutput)
	}
	return info.ContextWindow - reserve
}

// reserveOutput is the most of a window kept free for the reply.
const reserveOutput = 20_000

// conversationTools are the tools whose results are the conversation, never
// cleared: they cannot be asked for again.
var conversationTools = map[string]bool{toolname.AgentCreate: true, toolname.Message: true, toolname.AskUser: true}

// summaryInputMax bounds the transcript a summariser reads. Tool results
// are already cut to their first 800 characters (project.Transcript), so a
// transcript reaching this is a long conversation; its middle is dropped,
// keeping the task at the start and the recent work at the end.
const summaryInputMax = 600_000

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
	keep := s.cfg.Compaction.KeepTokens
	if all {
		keep = 0 // /compact summarises every turn that ended
	}
	cut, ok := st.hist.Cut(keep)
	if !ok {
		s.mu.Unlock()
		return errors.New("nothing to compact")
	}
	modelID := st.model
	err := s.commitLocked(context.Background(), s.event(a.ID, event.CompactionStarted, event.CompactionPayload{Before: cut.Before}))
	s.mu.Unlock()
	if err != nil {
		return err
	}
	req := summaryRequest(bareID(modelID), clip.Middle(project.Transcript(cut.Old), summaryInputMax), cut.Prev, info)
	req.CacheKey = a.ID + ":compact" // a summary shares no prefix with the agent's turns
	resp, err := m.Complete(ctx, req, nil)
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
		_ = a.recordFact(event.CompactionFailed, event.CompactionPayload{Before: cut.Before, Error: err.Error()})
		return err
	}
	summary := sb.String()
	if err := a.record(event.CompactionDone, event.CompactionPayload{FromSeq: cut.FromSeq, ToSeq: cut.ToSeq, Summary: summary, Before: cut.Before, After: cut.After(summary)}); err != nil {
		// The summary was not recorded, so it is not used; but the compaction
		// is over, and the state must say so or no other ever starts.
		_ = a.recordFact(event.CompactionFailed, event.CompactionPayload{Before: cut.Before, Error: err.Error()})
		return err
	}
	a.c.mu.Lock()
	a.instructed = nil // the summary replaced the results that carried them: they are attached again
	a.reads = nil      // and the reads: a file is read in full again
	a.c.mu.Unlock()
	return nil
}

// summaryMaxTokens bounds the summary; summaryWords is the same bound in
// words for models that ignore max tokens.
const (
	summaryMaxTokens = 4000
	summaryWords     = "2,500"
)

// summaryTemplate is the shape every summary takes: what another agent
// needs to carry the work on, section by section, so nothing important
// depends on the summariser's own sense of what matters.
const summaryTemplate = `Write the summary as this Markdown, keeping every heading in this order, and "(none)" under a heading with nothing to say:

## Task
- what the human asked for, and the current status

## Decisions
- what was decided and why, and constraints the human gave

## Work so far
- what is done and verified
- what is in progress, and where it stands
- what is blocked, with the error or unknown

## Files and commands
- paths touched and why they matter; commands run and their outcomes

## Next
- the next concrete step, then the one after it

Rules: terse bullets, not prose. Keep exact paths, identifiers, commands and error text. Do not mention this summary or that the conversation was compacted.`

// summaryRequest is the summariser call for transcript. prev is the
// previous compaction's summary ("" for none): the new one replaces it, so
// whatever it does not carry forward is lost.
func summaryRequest(modelID, transcript, prev string, info model.Info) model.Request {
	system := "You summarise an AI coding agent's conversation so another agent can continue the work with less context. Be concrete and complete; omit pleasantries. " + summaryTemplate
	text := "Summarise this transcript:\n\n" + transcript
	if prev != "" {
		system += "\n\nThe transcript follows an earlier summary of everything before it. Merge both: carry forward every still-relevant task, decision and constraint from the earlier summary even when the transcript does not mention it, drop what is finished, and where they disagree the transcript is newer and wins."
		text = "Earlier summary of the conversation before this transcript:\n\n" + prev + "\n\n---\n\nSummarise this transcript:\n\n" + transcript
	}
	req := model.Request{
		Model: modelID,
		Messages: []model.Message{{Role: model.RoleUser, Blocks: []model.Block{{Type: model.BlockText,
			Text: text}}}},
	}
	if info.IgnoresMaxTokens {
		system += " Keep the summary under " + summaryWords + " words."
	} else {
		req.MaxTokens = summaryMaxTokens
	}
	req.System = system
	return req
}
