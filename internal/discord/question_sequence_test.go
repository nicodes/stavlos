package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

func TestIndividualQuestionsKeepSeparateResultCards(t *testing.T) {
	first := protocol.PromptInfo{ID: "first", Channel: "channel", From: "scout", Kind: protocol.PromptQuestion, Escalated: true, QuestionNumber: 1, QuestionTotal: 2,
		Questions: []protocol.Question{{Question: "Genre?", Options: []protocol.QuestionOption{{Label: "Rock"}}}}}
	second := first
	second.ID, second.QuestionNumber = "second", 2
	second.Questions = []protocol.Question{{Question: "Energy?", Options: []protocol.QuestionOption{{Label: "High"}}}}
	pending := []protocol.PromptInfo{first}
	_, w, api := fixture(t, func(_ context.Context, method string, params, out any) error {
		switch method {
		case protocol.MPromptList:
			return result(out, protocol.PromptListResult{Prompts: pending})
		case protocol.MPromptReply:
			p := params.(protocol.PromptReplyParams)
			if len(p.Answers) != 1 {
				t.Fatal("sent a batch rather than one answer")
			}
			if len(p.Details) != 1 || len(p.Details[0].Selected) != 1 || p.Details[0].Selected[0] != 0 {
				t.Fatalf("structured answer missing for TUI/history: %+v", p.Details)
			}
			if p.ID == first.ID {
				pending = []protocol.PromptInfo{second}
			} else {
				pending = nil
			}
		}
		return result(out, protocol.None{})
	})
	ctx := context.Background()
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	old := api.snapshot()[0]
	if hasQuestionAction(old.Components, "next") || hasQuestionAction(old.Components, "back") || !hasQuestionAction(old.Components, "submit") {
		t.Fatal("individual question has batch navigation")
	}
	w.questionDraft(first).selected[0][0] = true
	if err := w.execute(work{link: w.link, interaction: interaction(first.ID, "submit")}); err != nil {
		t.Fatal(err)
	}
	answered := api.snapshot()[0]
	if len(answered.Components) != 0 || !strings.Contains(answered.Text, "✅ Rock") || !strings.HasPrefix(answered.Text, "❓ **Genre?**") {
		t.Fatal("first message was not turned into its result")
	}
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	if len(api.snapshot()) != 2 {
		t.Fatal("second question did not get its own message")
	}
	for _, m := range api.snapshot() {
		if m.ID == old.ID {
			if m.Text != answered.Text {
				t.Fatal("next question overwrote the first result")
			}
		} else if !strings.HasPrefix(m.Text, "❓ **Energy?**") || len(m.Components) == 0 {
			t.Fatalf("second form: %+v", m)
		}
	}
	w.questionDraft(second).selected[0][0] = true
	if err := w.execute(work{link: w.link, interaction: interaction(second.ID, "submit")}); err != nil {
		t.Fatal(err)
	}
	for _, m := range api.snapshot() {
		if len(m.Components) != 0 {
			t.Fatal("submitted question still has controls")
		}
		if m.ID != old.ID && (!strings.HasPrefix(m.Text, "❓ **Energy?**") || !strings.Contains(m.Text, "✅ High")) {
			t.Fatal("second result lost its question or answer")
		}
	}
}
