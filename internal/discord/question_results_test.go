package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func resultQuestion() protocol.PromptInfo {
	return protocol.PromptInfo{ID: "q", Channel: "channel", Agent: "a", From: "main", Kind: protocol.PromptQuestion, Escalated: true,
		Questions: []protocol.Question{{Question: "Which style?", Options: []protocol.QuestionOption{{Label: "A, B", Description: "combined"}, {Label: "Jazz"}}}}}
}

func resolvedQuestionEvent(p protocol.PromptInfo) event.Event {
	return event.Event{Channel: p.Channel, Seq: 12, Type: event.AskResolved, Payload: event.MustPayload(event.AskResolvedPayload{
		ID: p.ID, Outcome: event.AskAnswered, Answer: "A, B, Jazz", Details: []event.QuestionAnswer{{Selected: []int{0}, Custom: "Jazz"}},
	})}
}

func assertRecordedCard(t *testing.T, api *fakeAPI, original sentMessage) {
	t.Helper()
	ms := api.snapshot()
	if len(ms) != 1 || ms[0].ID != original.ID || ms[0].Webhook != original.Webhook || ms[0].User != "main" || len(ms[0].Components) != 0 {
		t.Fatalf("original question replaced: %+v", ms)
	}
	for _, want := range []string{"❓ **Which style?**", "✅ A, B — combined", "⬜ Jazz", "✅ Jazz"} {
		if !strings.Contains(ms[0].Text, want) {
			t.Fatalf("result lacks %q: %s", want, ms[0].Text)
		}
	}
}

func TestTerminalQuestionResultSurvivesNotificationOrdering(t *testing.T) {
	for _, order := range []string{"notification-first", "poll-first", "event-first"} {
		t.Run(order, func(t *testing.T) {
			p := resultQuestion()
			pending := true
			b, w, api := fixture(t, func(_ context.Context, method string, _, out any) error {
				if method != protocol.MPromptList {
					t.Fatalf("unexpected call: %s", method)
				}
				var ps []protocol.PromptInfo
				if pending {
					ps = []protocol.PromptInfo{p}
				}
				return result(out, protocol.PromptListResult{Prompts: ps})
			})
			ctx := context.Background()
			w.seq = 10
			if err := w.refreshPrompts(ctx); err != nil {
				t.Fatal(err)
			}
			original := api.snapshot()[0]
			// These unsent Discord edits must not replace the terminal's answer.
			w.drafts[p.ID].selected[0][1], w.drafts[p.ID].text[0] = true, "discard this draft"
			pending = false
			n := &protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: p}
			ev := resolvedQuestionEvent(p)
			switch order {
			case "notification-first":
				if err := w.syncPrompts(ctx, n); err != nil {
					t.Fatal(err)
				}
			case "poll-first":
				if err := w.refreshPrompts(ctx); err != nil {
					t.Fatal(err)
				}
			case "event-first":
				if err := w.mirror(ctx, ev); err != nil {
					t.Fatal(err)
				}
			}
			if order != "event-first" && b.store.snapshot()[p.ID].Message != original.ID {
				t.Fatal("discarded destination before the answer arrived")
			}
			if err := w.mirror(ctx, ev); err != nil {
				t.Fatal(err)
			}
			if err := w.syncPrompts(ctx, n); err != nil {
				t.Fatal(err)
			}
			assertRecordedCard(t, api, original)
			if len(b.store.snapshot()) != 0 {
				t.Fatal("completed result kept a pending mapping")
			}
		})
	}
}

func TestQuestionResultReplaysAfterRestartWithoutRepostingChat(t *testing.T) {
	for _, version := range []string{"checkpoint", "legacy-index"} {
		t.Run(version, func(t *testing.T) {
			p := resultQuestion()
			pending := true
			b, w, api := fixture(t, func(_ context.Context, _ string, _, out any) error {
				var ps []protocol.PromptInfo
				if pending {
					ps = []protocol.PromptInfo{p}
				}
				return result(out, protocol.PromptListResult{Prompts: ps})
			})
			ctx := context.Background()
			w.seq = 10
			if err := w.refreshPrompts(ctx); err != nil {
				t.Fatal(err)
			}
			original := api.snapshot()[0]
			if err := w.mirror(ctx, event.Event{Channel: p.Channel, Seq: 11, Type: event.AskRequested, Payload: event.MustPayload(event.AskRequestedPayload{ID: p.ID, Kind: "question", Questions: p.Questions})}); err != nil {
				t.Fatal(err)
			}
			wantFrom := int64(12)
			if version == "legacy-index" {
				m := b.store.snapshot()[p.ID]
				m.Questions, m.Seq = nil, 0
				if err := b.store.set(p.ID, m); err != nil {
					t.Fatal(err)
				}
				wantFrom = 1
			}
			stored, err := openStore(b.store.path)
			if err != nil {
				t.Fatal(err)
			}
			b.store = stored
			pending = false
			if err := w.execute(work{link: w.link, snapshot: &protocol.ReconcileResult{Seq: 100}}); err != nil {
				t.Fatal(err)
			}
			if from := b.questionReplayFrom(w.discord, 100); from != wantFrom {
				t.Fatalf("replay starts at %d, want %d", from, wantFrom)
			}
			if version == "legacy-index" {
				if err := w.mirror(ctx, event.Event{Channel: p.Channel, Seq: 11, Type: event.AskRequested, Payload: event.MustPayload(event.AskRequestedPayload{ID: p.ID, Kind: "question", Questions: p.Questions})}); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.mirror(ctx, event.Event{Channel: p.Channel, Seq: 99, Type: event.ChatMessage, Payload: event.MustPayload(event.ChatPayload{Text: "old chat"})}); err != nil {
				t.Fatal(err)
			}
			if err := w.mirror(ctx, resolvedQuestionEvent(p)); err != nil {
				t.Fatal(err)
			}
			assertRecordedCard(t, api, original)
			if w.seq != 100 || b.questionReplayFrom(w.discord, 100) != 101 {
				t.Fatal("question replay moved the ordinary chat cutoff backwards")
			}
		})
	}
}

func TestQuestionOutcomeWithoutStructuredDetails(t *testing.T) {
	p := resultQuestion()
	text := recordedQuestionResult(p, event.AskResolvedPayload{Outcome: event.AskAnswered, Answer: "A, B, Jazz"})
	if !strings.Contains(text, "Which style?") || !strings.Contains(text, "Answer: A, B, Jazz") || strings.Contains(text, "✅") {
		t.Fatal("legacy answer guessed option selections: " + text)
	}
	text = recordedQuestionResult(p, event.AskResolvedPayload{Outcome: event.AskWithdrawn})
	if !strings.Contains(text, "Which style?") || !strings.Contains(text, "Question withdrawn.") || strings.Contains(text, "Answer:") {
		t.Fatal("withdrawal lost the question or fabricated an answer: " + text)
	}
}
