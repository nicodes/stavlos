package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/toolname"
)

// askTool is ask_user: one to four clarifying questions with constrained
// answers, raised to the human as one prompt. The turn waits for the
// answers; there is no default, so the model should ask only when guessing
// would waste work.
type askTool struct{}

const (
	askMaxQuestions = 4
	askMaxOptions   = 4
)

func (askTool) Def() model.ToolDef {
	return model.ToolDef{Name: toolname.AskUser, Description: "Ask the human one to four short questions when several valid approaches exist and guessing would waste work: which backend, which of two designs, whether to keep going. Each question has its text and one to four options with a label and a one-line description; put the option you would pick first. Every question is a checklist: the human may pick several options and always has a last entry for typing something else, so never add an 'Other' or 'all of the above' option. The turn waits for the answers. Do not ask what you can find out yourself, and do not ask more than once for the same thing.",
		Schema: schemaOf(askInput{})}
}

type askInput struct {
	Questions []askQuestion `json:"questions" req:"true" min:"1" max:"4"`
}

type askQuestion struct {
	Question string      `json:"question" desc:"The question, ending with ?" req:"true"`
	Options  []askOption `json:"options" req:"true" min:"1" max:"4"`
}

type askOption struct {
	Label       string `json:"label" desc:"The choice, one to five words" req:"true"`
	Description string `json:"description" desc:"What picking it means (one line)"`
}

func (askTool) Subject(in json.RawMessage) policy.Subject {
	qs, _ := parseQuestions(in)
	texts := make([]string, 0, len(qs))
	for _, q := range qs {
		texts = append(texts, q.Question)
	}
	return policy.Text(strings.Join(texts, " | "))
}

func (askTool) Run(ctx context.Context, in json.RawMessage, env *Env) Result {
	if env.Ask == nil {
		return errf("asking the human is not available here")
	}
	qs, err := parseQuestions(in)
	if err != nil {
		return errf("%v", err)
	}
	answers, err := env.Ask.Ask(ctx, qs)
	if err != nil {
		return errf("%v", err)
	}
	return Result{Output: FormatAnswers(qs, answers)}
}

// parseQuestions decodes and checks an ask_user input.
func parseQuestions(in json.RawMessage) ([]protocol.Question, error) {
	var a struct {
		Questions []protocol.Question `json:"questions"`
	}
	if err := decode(in, &a); err != nil {
		return nil, fmt.Errorf("bad input: %v", err)
	}
	if len(a.Questions) == 0 {
		return nil, fmt.Errorf("at least one question is required")
	}
	if len(a.Questions) > askMaxQuestions {
		return nil, fmt.Errorf("at most %d questions per call", askMaxQuestions)
	}
	for i := range a.Questions {
		q := &a.Questions[i]
		q.Question = strings.TrimSpace(q.Question)
		if q.Question == "" {
			return nil, fmt.Errorf("question %d needs its text", i+1)
		}
		if len(q.Options) == 0 {
			return nil, fmt.Errorf("question %d needs at least one option (the human can always type something else)", i+1)
		}
		if len(q.Options) > askMaxOptions {
			return nil, fmt.Errorf("question %d: at most %d options", i+1, askMaxOptions)
		}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" {
				return nil, fmt.Errorf("question %d: every option needs a label", i+1)
			}
		}
	}
	return a.Questions, nil
}

// FormatAnswers is the tool result the model reads: one "question → answer"
// line per question ("(no answer)" where the human left one blank).
func FormatAnswers(qs []protocol.Question, answers []string) string {
	var b strings.Builder
	for i, q := range qs {
		ans := "(no answer)"
		if i < len(answers) && strings.TrimSpace(answers[i]) != "" {
			ans = strings.TrimSpace(answers[i])
		}
		fmt.Fprintf(&b, "%s → %s\n", q.Question, ans)
	}
	return strings.TrimRight(b.String(), "\n")
}
