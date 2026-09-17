package agent

import (
	"context"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/escalation"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestQuestionsAreAcceptedIndividuallyBeforeCancellation(t *testing.T) {
	s, h := newTestChannel(t, testConfig{}, &fakeModel{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan protocol.PromptInfo, 3)
	replies := []chan escalation.Answer{make(chan escalation.Answer, 1), make(chan escalation.Answer, 1), make(chan escalation.Answer, 1)}
	h.answer = func(ctx context.Context, p protocol.PromptInfo) escalation.Answer {
		opened <- p
		select {
		case answer := <-replies[p.QuestionNumber-1]:
			return answer
		case <-ctx.Done():
			return escalation.Answer{Withdrawn: true}
		}
	}
	type result struct {
		answers []string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		a, err := (askAPI{a: s.Root()}).Ask(ctx, []protocol.Question{{Question: "First?", Options: []protocol.QuestionOption{{Label: "One", Description: "First choice"}}}, {Question: "Second?"}, {Question: "Third?"}})
		done <- result{a, err}
	}()
	await := func() protocol.PromptInfo {
		t.Helper()
		select {
		case p := <-opened:
			return p
		case <-time.After(time.Second):
			t.Fatal("question not opened")
			return protocol.PromptInfo{}
		}
	}
	var prompts [3]protocol.PromptInfo
	for range 3 {
		p := await()
		prompts[p.QuestionNumber-1] = p
	}
	first, second, third := prompts[0], prompts[1], prompts[2]
	if len(first.Questions) != 1 || first.QuestionNumber != 1 || first.QuestionTotal != 3 {
		t.Fatalf("first: %+v", first)
	}
	if second.QuestionNumber != 2 || third.QuestionNumber != 3 || first.ID == second.ID || second.ID == third.ID || first.ID == third.ID {
		t.Fatal("questions must all open independently in input order")
	}
	var requested event.AskRequestedPayload
	_ = h.ofType(event.AskRequested, s.Root().ID)[0].Decode(&requested)
	if requested.From != first.From || requested.Role != first.Role || len(requested.Questions) != 1 || len(requested.Questions[0].Options) != 1 || requested.Questions[0].Options[0].Description != "First choice" {
		t.Fatalf("question card not persisted: %+v", requested)
	}
	replies[2] <- escalation.Answer{Value: protocol.AnswerAnswered, Answers: []string{"three"}, Details: []protocol.QuestionAnswer{{Custom: "three"}}}
	h.waitFor(t, event.AskResolved, s.Root().ID)
	resolved := h.ofType(event.AskResolved, s.Root().ID)
	if len(resolved) != 1 {
		t.Fatal("first answer was not logged immediately")
	}
	var saved event.AskResolvedPayload
	_ = resolved[0].Decode(&saved)
	if saved.ID != third.ID || saved.Answer != "three" || len(saved.Details) != 1 || saved.Details[0].Custom != "three" {
		t.Fatalf("saved answer: %+v", saved)
	}
	cancel()
	select {
	case r := <-done:
		if r.err == nil || len(r.answers) != 3 || r.answers[0] != "" || r.answers[1] != "" || r.answers[2] != "three" {
			t.Fatalf("partial answers lost: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled sequence did not finish")
	}
	if len(h.ofType(event.AskResolved, s.Root().ID)) != 3 {
		t.Fatal("Ask returned before all outstanding questions were withdrawn")
	}
}

func TestQuestionsAnsweredOutOfOrderReturnInInputOrder(t *testing.T) {
	s, h := newTestChannel(t, testConfig{}, &fakeModel{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan protocol.PromptInfo, 3)
	replies := []chan string{make(chan string, 1), make(chan string, 1), make(chan string, 1)}
	h.answer = func(ctx context.Context, p protocol.PromptInfo) escalation.Answer {
		opened <- p
		select {
		case answer := <-replies[p.QuestionNumber-1]:
			return escalation.Answer{Answers: []string{answer}}
		case <-ctx.Done():
			return escalation.Answer{Withdrawn: true}
		}
	}
	type result struct {
		answers []string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		answers, err := (askAPI{a: s.Root()}).Ask(ctx, []protocol.Question{{Question: "First?"}, {Question: "Second?"}, {Question: "Third?"}})
		done <- result{answers, err}
	}()
	for range 3 {
		select {
		case <-opened:
		case <-time.After(time.Second):
			t.Fatal("later question waited on an earlier answer")
		}
	}
	for n, ev := range h.ofType(event.AskRequested, s.Root().ID) {
		var p event.AskRequestedPayload
		_ = ev.Decode(&p)
		if p.QuestionNumber != n+1 {
			t.Fatal("request log not in publication order")
		}
	}
	for i, n := range []int{2, 0, 1} {
		replies[n] <- []string{"one", "two", "three"}[n]
		waitUntil(t, h, func() bool { return len(h.ofType(event.AskResolved, s.Root().ID)) == i+1 })
		if n != 1 {
			select {
			case <-done:
				t.Fatal("tool returned with unanswered questions")
			default:
			}
		}
	}
	select {
	case r := <-done:
		if r.err != nil || len(r.answers) != 3 || r.answers[0] != "one" || r.answers[1] != "two" || r.answers[2] != "three" {
			t.Fatalf("ordered answers: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("answered questions did not finish")
	}
}
