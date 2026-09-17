package discord

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

func TestAnswerSummaryDistinguishesCustomTextFromSelections(t *testing.T) {
	p := protocol.PromptInfo{ID: "q", Questions: []protocol.Question{{Question: "Music?", Options: []protocol.QuestionOption{{Label: "Rock"}, {Label: "Jazz"}, {Label: "Pop"}}}}}
	d := newDraft(p)
	d.selected[0][0], d.selected[0][2] = true, true
	d.text[0] = "Jazz" // typed text is not a selected option, even with the same label
	s := answeredQuestions(p, d)
	if !strings.HasPrefix(s, "❓ **Music?**\n") || strings.Contains(s, "Questions answered") {
		t.Fatalf("unexpected result heading: %s", s)
	}
	for _, want := range []string{"✅ Rock", "⬜ Jazz", "✅ Pop", "✅ Jazz"} {
		if !strings.Contains(s, "\n\u00a0\u00a0\u00a0\u00a0"+want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
	if strings.Contains(s, "Custom answer:") {
		t.Fatal("custom answer should look like the other checked choices")
	}
	d.text[0] = ""
	if strings.Contains(answeredQuestions(p, d), "✅ Jazz") {
		t.Fatal("empty custom answer displayed")
	}
}

func TestLongAnswerSummaryKeepsEveryChoiceVisible(t *testing.T) {
	p := protocol.PromptInfo{ID: "q"}
	for i := 0; i < 4; i++ {
		q := protocol.Question{Question: fmt.Sprintf("Question %d? %s", i+1, strings.Repeat("🎵", 1500))}
		for n := 0; n < 4; n++ {
			q.Options = append(q.Options, protocol.QuestionOption{Label: fmt.Sprintf("choice-%d-%d", i+1, n+1), Description: strings.Repeat("🎶", 300)})
		}
		p.Questions = append(p.Questions, q)
	}
	d := newDraft(p)
	for i := range p.Questions {
		d.selected[i][3] = true
		d.text[i] = "my-answer-" + fmt.Sprint(i+1) + strings.Repeat("🎶", 1500)
	}
	s := answeredQuestions(p, d)
	if units(s) > 2000 || !strings.HasPrefix(s, "❓ **Question 1? ") {
		t.Fatalf("summary length: %d", units(s))
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "❓") && !strings.HasSuffix(line, "**") {
			t.Fatal("long question lost its bold formatting")
		}
		if line != "" && !strings.HasPrefix(line, "❓") && !strings.HasPrefix(line, "\u00a0\u00a0\u00a0\u00a0") {
			t.Fatal("long answer lost its checkbox indentation")
		}
	}
	for i := range p.Questions {
		for n := range p.Questions[i].Options {
			mark := "⬜"
			if n == 3 {
				mark = "✅"
			}
			if !strings.Contains(s, fmt.Sprintf("%s choice-%d-%d", mark, i+1, n+1)) {
				t.Fatal("long question hid a choice")
			}
		}
		if !strings.Contains(s, fmt.Sprintf("✅ my-answer-%d", i+1)) {
			t.Fatal("long question hid a custom answer")
		}
	}
}
