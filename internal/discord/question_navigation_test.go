package discord

import (
	"context"
	"strings"
	"testing"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/protocol"
)

func twoQuestions() protocol.PromptInfo {
	return protocol.PromptInfo{ID: "q", Kind: protocol.PromptQuestion, Questions: []protocol.Question{
		{Question: "Best part of the day?", Options: []protocol.QuestionOption{{Label: "People"}, {Label: "Hobbies"}}},
		{Question: "Energy today?", Options: []protocol.QuestionOption{{Label: "High"}, {Label: "Low"}}},
	}}
}

func hasQuestionAction(components []dg.MessageComponent, action string) bool {
	for _, c := range components {
		for _, item := range c.(dg.ActionsRow).Components {
			if b, ok := item.(dg.Button); ok {
				_, a, _ := parseID(b.CustomID)
				if a == action {
					return true
				}
			}
		}
	}
	return false
}

func TestQuestionFormGuidesThroughTheBatch(t *testing.T) {
	p := twoQuestions()
	d := newDraft(p)
	_, components := questionView(p, d)
	if hasQuestionAction(components, "submit") || hasQuestionAction(components, "back") || !hasQuestionAction(components, "next") {
		t.Fatal("first page must offer Next, not Submit")
	}
	i := interaction(p.ID, "next")
	d.text[0] = " \n"
	if err := updateDraft(d, p, i, "next", "0", ""); err == nil || d.index != 0 {
		t.Fatal("Next skipped an unanswered question")
	}
	d.selected[0][0], d.selected[0][1] = true, true
	if err := updateDraft(d, p, i, "next", "0", ""); err != nil {
		t.Fatal(err)
	}
	text, components := questionView(p, d)
	if !strings.HasPrefix(text, "❓ **Energy today?**") || !hasQuestionAction(components, "submit") || !hasQuestionAction(components, "back") || hasQuestionAction(components, "next") {
		t.Fatal("last page must offer Submit")
	}
	option := components[0].(dg.ActionsRow).Components[0].(dg.Button)
	if option.Label != p.Questions[1].Options[0].Label || option.Emoji.Name != "⬜" {
		t.Fatal("current question should show its options as toggle buttons")
	}
	d.selected[1][0] = true
	if err := updateDraft(d, p, i, "back", "1", ""); err != nil {
		t.Fatal(err)
	}
	if !d.selected[0][0] || !d.selected[0][1] || !d.selected[1][0] {
		t.Fatal("navigation lost an answer")
	}
}

func TestEarlySubmitMovesToMissingQuestionWithoutSendingPartialAnswers(t *testing.T) {
	p := twoQuestions()
	var methods []string
	var submitted []string
	_, w, _ := fixture(t, func(_ context.Context, method string, params, out any) error {
		methods = append(methods, method)
		if method == protocol.MPromptReply {
			submitted = params.(protocol.PromptReplyParams).Answers
		}
		return result(out, protocol.None{})
	})
	d := w.questionDraft(p)
	d.selected[0][0], d.selected[0][1] = true, true
	// Reproduce the existing card's premature Submit on Question 1/2.
	text, controls, err := w.question(context.Background(), interaction(p.ID, "submit"), p, "submit", "0", "")
	if err != nil || d.index != 1 || !strings.HasPrefix(text, "❓ **Energy today?**") || !hasQuestionAction(controls, "submit") {
		t.Fatalf("did not advance to question 2: %q %v", text, err)
	}
	if len(methods) != 0 {
		t.Fatal("partial answers reached the daemon")
	}
	d.selected[1][0] = true
	if _, _, err := w.question(context.Background(), interaction(p.ID, "submit"), p, "submit", "1", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ";") != "prompt.claim;prompt.reply" || strings.Join(submitted, ";") != "People, Hobbies;High" {
		t.Fatalf("batch submitted incorrectly: %v %v", methods, submitted)
	}
}

func TestSubmitReturnsToAnEarlierUnansweredQuestion(t *testing.T) {
	p := twoQuestions()
	_, w, _ := fixture(t, func(context.Context, string, any, any) error {
		t.Fatal("must not submit incomplete answers")
		return nil
	})
	d := w.questionDraft(p)
	d.index = 1
	d.selected[1][1] = true // an older form allowed skipping question 1
	_, _, err := w.question(context.Background(), interaction(p.ID, "submit"), p, "submit", "1", "")
	if err == nil || d.index != 0 || !d.selected[1][1] {
		t.Fatal("did not return to the missing answer while retaining the second")
	}
}
