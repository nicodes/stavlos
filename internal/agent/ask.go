package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

// askAPI raises an ask_user batch as one question prompt through the
// escalation path permissions use, and blocks the turn until it is
// answered or withdrawn (a cancel). Questions never fall to the headless
// default.
type askAPI struct{ a *Agent }

func (a *Agent) askAPI() tools.Asker { return askAPI{a: a} }

func (k askAPI) Ask(ctx context.Context, qs []protocol.Question) ([]string, error) {
	a := k.a
	texts := make([]string, 0, len(qs))
	for _, q := range qs {
		texts = append(texts, q.Question)
	}
	a.setState(StateBlocked)
	ans := a.s.host.Prompt(ctx, protocol.PromptInfo{
		ID: NewID("p"), Session: a.s.ID, Agent: a.ID, Kind: "question", Tool: "ask_user",
		Question:  fmt.Sprintf("%s asks: %s", a.Label, strings.Join(texts, " | ")),
		Questions: qs,
	})
	a.setState(StateRunning)
	if ans.Withdrawn {
		return nil, fmt.Errorf("the question was withdrawn before it was answered")
	}
	if len(ans.Answers) == 0 && ans.Value != "" && ans.Value != "answered" {
		// A single free-text reply (an older client): it answers the first question.
		return []string{ans.Value}, nil
	}
	return ans.Answers, nil
}
