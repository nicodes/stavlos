package transcript

import (
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/transcript/evtest"
)

func TestQuestionResultReplacesOriginalMessage(t *testing.T) {
	for _, chat := range []bool{false, true} {
		tr := NewTranscript()
		if chat {
			tr = NewChat()
		}
		qs := []event.Question{{Question: "Pick a format?", Options: []event.QuestionOption{{Label: "A, B", Description: "both"}, {Label: "C"}}}}
		requested := evtest.Ev("agent", event.AskRequested, event.AskRequestedPayload{ID: "q", Kind: "question", From: "writer", Role: "reviewer", Questions: qs})
		tr.Apply(requested)
		item, ok := tr.QuestionItem("q")
		if !ok {
			t.Fatal("question missing")
		}
		if tr.Item(item)[0].Note {
			t.Fatal("pending question text should use the lighter message color")
		}
		tr.Apply(evtest.Ev("agent", event.ChatMessage, event.ChatPayload{From: "writer", Text: "another message"}))
		n := tr.Items()
		before := chatItem(tr, item)
		tr.Apply(evtest.Ev("agent", event.AskResolved, event.AskResolvedPayload{ID: "q", Outcome: event.AskAnswered, Answer: "A, B, C", Details: []event.QuestionAnswer{{Selected: []int{0}, Custom: "C"}}}))
		if tr.Items() != n {
			t.Fatal("answer appended instead of updating the question")
		}
		after := chatItem(tr, item)
		for _, line := range tr.Item(item) {
			if !line.Note {
				t.Fatalf("answered card should use grey message text: %+v", line)
			}
		}
		header, names := "@user Pick a format?", "user"
		if chat {
			header, names = "@writer: Pick a format?", "writer"
		}
		for _, want := range []string{header, ">>■ A, B — both", ">>□ C", ">>■ C", "Submitted"} {
			if !strings.Contains(after, want) {
				t.Fatalf("chat=%v missing %q: %s", chat, want, after)
			}
		}
		if head := tr.Item(item)[0]; head.Glyph != GlyphPrompt || head.Who != "writer" || strings.Join(head.Names, ",") != names {
			t.Fatalf("message header metadata: %+v", head)
		}
		if after == before {
			t.Fatal("result did not update")
		}
		tr.EnsureQuestion(protocol.PromptInfo{ID: "q", Questions: qs})
		tr.Apply(requested) // delayed notification/replay must not reopen a resolved card
		if chatItem(tr, item) != after {
			t.Fatal("late request erased the result")
		}
	}
}

func TestLegacyQuestionResultAndWithdrawal(t *testing.T) {
	for _, outcome := range []event.AskOutcome{event.AskAnswered, event.AskWithdrawn} {
		tr := NewChat()
		tr.Apply(evtest.Ev("a", event.AskRequested, event.AskRequestedPayload{ID: "old", Kind: "question", Question: "Old question?"}))
		tr.Apply(evtest.Ev("a", event.AskResolved, event.AskResolvedPayload{ID: "old", Outcome: outcome, Answer: "Old answer"}))
		want := "Answer: Old answer"
		if outcome == event.AskWithdrawn {
			want = "Question withdrawn"
		}
		if tr.Items() != 1 || !strings.Contains(chatItem(tr, 0), want) || !strings.Contains(chatItem(tr, 0), "Old question?") {
			t.Fatal(chatItem(tr, 0))
		}
	}
}
