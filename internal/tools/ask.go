package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

// askTool is ask_user: one to four clarifying questions with constrained
// answers, raised to the human as one prompt. The turn waits for the
// answers; there is no default, so the model should ask only when guessing
// would waste work.
type askTool struct{}

const (
	askMaxQuestions = 4
	askMaxOptions   = 4
	askMaxHeader    = 16
)

func (askTool) Def() model.ToolDef {
	return model.ToolDef{Name: "ask_user", Description: "Ask the human one to four short questions when several valid approaches exist and guessing would waste work: which backend, which of two designs, whether to keep going. Each question has a header (a few words), the question text and one to four options with a label and a one-line description; put the option you would pick first. Every question is a checklist: the human may pick several options and always has a last entry for typing something else, so never add an 'Other' or 'all of the above' option. The turn waits for the answers. Do not ask what you can find out yourself, and do not ask more than once for the same thing.",
		Schema: schema(map[string]any{
			"questions": map[string]any{
				"type": "array", "minItems": 1, "maxItems": askMaxQuestions,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"header":   prop("string", "A label of a few words, shown as the question's title"),
						"question": prop("string", "The question, ending with ?"),
						"options": map[string]any{
							"type": "array", "minItems": 1, "maxItems": askMaxOptions,
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"label":       prop("string", "The choice, one to five words"),
									"description": prop("string", "What picking it means (one line)"),
								},
								"required": []string{"label"},
							},
						},
					},
					"required": []string{"header", "question", "options"},
				},
			},
		}, "questions")}
}

func (askTool) PolicyArg(in json.RawMessage) string {
	qs, _ := parseQuestions(in)
	heads := make([]string, 0, len(qs))
	for _, q := range qs {
		heads = append(heads, q.Header)
	}
	return strings.Join(heads, ", ")
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
		q.Header = strings.TrimSpace(q.Header)
		q.Question = strings.TrimSpace(q.Question)
		if q.Header == "" || q.Question == "" {
			return nil, fmt.Errorf("question %d needs a header and a question", i+1)
		}
		if len([]rune(q.Header)) > askMaxHeader {
			q.Header = string([]rune(q.Header)[:askMaxHeader])
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

// FormatAnswers is the tool result the model reads: one "header: answer"
// line per question ("(no answer)" where the human left one blank).
func FormatAnswers(qs []protocol.Question, answers []string) string {
	var b strings.Builder
	for i, q := range qs {
		ans := "(no answer)"
		if i < len(answers) && strings.TrimSpace(answers[i]) != "" {
			ans = strings.TrimSpace(answers[i])
		}
		fmt.Fprintf(&b, "%s: %s\n", q.Header, ans)
	}
	return strings.TrimRight(b.String(), "\n")
}
