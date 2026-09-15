package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/toolname"
)

// askAPI raises an ask_user batch as one question prompt through the path
// permissions use, and blocks the turn until it is answered or withdrawn
// (a cancel). Questions never fall to the headless default.
type askAPI struct{ a *Agent }

func (k askAPI) Ask(ctx context.Context, qs []protocol.Question) ([]string, error) {
	a := k.a
	rv := a.role()
	texts := make([]string, 0, len(qs))
	for _, q := range qs {
		texts = append(texts, q.Question)
	}
	ans := a.ask(ctx, protocol.PromptInfo{
		ID: NewID("p"), Channel: a.c.ID, ChannelName: a.c.Name(), Agent: a.ID, From: rv.name, Kind: protocol.PromptQuestion, Tool: toolname.AskUser,
		Question: fmt.Sprintf("%s asks: %s", rv.name, strings.Join(texts, " | ")), Questions: qs,
	}, "")
	if ans.Withdrawn {
		return nil, fmt.Errorf("the question was withdrawn before it was answered")
	}
	if len(ans.Answers) == 0 && ans.Value != "" && ans.Value != protocol.AnswerAnswered {
		return []string{ans.Value}, nil // a single free-text reply answers the first question
	}
	return ans.Answers, nil
}
