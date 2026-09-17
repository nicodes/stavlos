package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/toolname"
)

// askAPI publishes every question without waiting for earlier answers. Each
// resolves independently; the tool collects results in the original order.
// Interrupted results keep empty slots so later answers never shift questions.
type askAPI struct{ a *Agent }

func (k askAPI) Ask(ctx context.Context, qs []protocol.Question) ([]string, error) {
	a := k.a
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		index  int
		answer escalation.Answer
	}
	results := make(chan result, len(qs))
	answers := make([]string, len(qs))
	started := 0
	var firstErr error
	for i, q := range qs {
		if err := ctx.Err(); err != nil {
			firstErr = err
			break
		}
		rv := a.role()
		info := protocol.PromptInfo{
			ID: NewID("p"), Channel: a.c.ID, ChannelName: a.c.Name(), Agent: a.ID, From: rv.name, Role: rv.role, Kind: protocol.PromptQuestion, Tool: toolname.AskUser,
			Question: fmt.Sprintf("%s asks: %s", rv.name, q.Question), Questions: []protocol.Question{q}, QuestionNumber: i + 1, QuestionTotal: len(qs),
		}
		opened := make(chan struct{})
		started++
		go func() {
			ans := a.askOpened(ctx, info, "", func() { close(opened) })
			results <- result{i, ans}
		}()
		// Preserve publication order, but wait only for the request to open,
		// never for the human's answer. Cancellation stops further publication.
		select {
		case <-opened:
		case <-ctx.Done():
		}
	}
	for range started {
		r := <-results // join every request, including its withdrawal/log write
		ans := r.answer
		var err error
		if ans.Withdrawn {
			err = fmt.Errorf("question %d was withdrawn before it was answered", r.index+1)
		} else if len(ans.Answers) > 0 && strings.TrimSpace(ans.Answers[0]) != "" {
			answers[r.index] = ans.Answers[0]
		} else if ans.Value != "" && ans.Value != protocol.AnswerAnswered {
			answers[r.index] = ans.Value // legacy free-text replies
		} else {
			err = fmt.Errorf("question %d received no answer", r.index+1)
		}
		if err != nil && firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	return answers, firstErr
}
