package escalation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/protocol"
)

type recSink struct {
	mu sync.Mutex
	n  []protocol.PromptNotification
	t  [][]protocol.Tier
}

func TestAcceptedQuestionWinsConcurrentCancellation(t *testing.T) {
	for range 50 {
		ctx, cancel := context.WithCancel(context.Background())
		m := New(Config{}, &recSink{})
		p := protocol.PromptInfo{ID: "q", Kind: protocol.PromptQuestion, QuestionNumber: 1, QuestionTotal: 2}
		answer := m.Request(ctx, p, func() {
			if err := m.Reply(p.ID, "client", Answer{Answers: []string{"accepted"}}); err != nil {
				t.Fatal(err)
			}
			cancel() // both select cases are ready before Request starts waiting
		})
		cancel()
		if answer.Withdrawn || len(answer.Answers) != 1 || answer.Answers[0] != "accepted" {
			t.Fatalf("accepted answer lost: %+v", answer)
		}
	}
}

func (s *recSink) Notify(n protocol.PromptNotification, tiers []protocol.Tier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n = append(s.n, n)
	s.t = append(s.t, tiers)
}

func TestPermissionIsImmediateThenDefaults(t *testing.T) {
	s := &recSink{}
	m := New(Config{ClaimTimeout: 30 * time.Millisecond, AnswerTimeout: 80 * time.Millisecond, Default: "deny"}, s)
	a := m.Request(context.Background(), protocol.PromptInfo{ID: "p1", Kind: "permission"}, nil)
	if !a.Defaulted || a.Value != "deny" {
		t.Fatalf("%+v", a)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.n) != 2 || s.n[0].Action != "requested" || s.n[1].Action != "defaulted" {
		t.Fatalf("%+v", s.n)
	}
	if len(s.t[0]) != 2 || len(s.t[1]) != 2 {
		t.Fatalf("tiers %+v", s.t)
	}
	if err := m.Reply("p1", "c", Answer{Value: "allow"}); err != ErrLate {
		t.Fatalf("late reply: %v", err)
	}
}

func TestClaimConflictAndWithdraw(t *testing.T) {
	s := &recSink{}
	m := New(Config{ClaimTimeout: time.Second, AnswerTimeout: time.Second, Default: "deny"}, s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Answer, 1)
	go func() { done <- m.Request(ctx, protocol.PromptInfo{ID: "p2"}, nil) }()
	time.Sleep(10 * time.Millisecond)
	if err := m.Claim("p2", "a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Claim("p2", "b"); err != ErrClaimed {
		t.Fatal(err)
	}
	if err := m.Reply("p2", "b", Answer{Value: "allow"}); err != ErrClaimed {
		t.Fatal(err)
	}
	cancel()
	a := <-done
	if !a.Withdrawn {
		t.Fatalf("%+v", a)
	}
	if len(m.Pending("")) != 0 {
		t.Fatal("still pending")
	}
}

func TestReply(t *testing.T) {
	s := &recSink{}
	m := New(Config{ClaimTimeout: time.Second, AnswerTimeout: time.Second}, s)
	done := make(chan Answer, 1)
	go func() { done <- m.Request(context.Background(), protocol.PromptInfo{ID: "p3"}, nil) }()
	time.Sleep(10 * time.Millisecond)
	if err := m.Reply("p3", "a", Answer{Value: "allow", Dir: "/x", Reason: "r", Answers: []string{"one"}}); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.Value != "allow" || a.Client != "a" || a.Dir != "/x" || a.Reason != "r" || len(a.Answers) != 1 {
		t.Fatalf("%+v", a)
	}
}

// TestResolveIgnoresClaims: a decision made elsewhere settles a prompt
// that a client had claimed.
func TestResolveIgnoresClaims(t *testing.T) {
	m := New(Config{ClaimTimeout: time.Second, AnswerTimeout: time.Second}, &recSink{})
	done := make(chan Answer, 1)
	go func() {
		done <- m.Request(context.Background(), protocol.PromptInfo{ID: "p4", Kind: protocol.PromptTrust}, nil)
	}()
	time.Sleep(10 * time.Millisecond)
	if err := m.Claim("p4", "tui"); err != nil {
		t.Fatal(err)
	}
	if err := m.Resolve("p4", "trust.reply", Answer{Value: "allow"}); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.Value != "allow" || a.Client != "trust.reply" {
		t.Fatalf("%+v", a)
	}
	if err := m.Resolve("p4", "trust.reply", Answer{Value: "deny"}); err != ErrLate {
		t.Fatalf("second resolve: %v", err)
	}
}

// TestTrustAndQuestionsNeverDefault: the answer timer does not apply.
func TestTrustAndQuestionsNeverDefault(t *testing.T) {
	for _, kind := range []protocol.PromptKind{protocol.PromptTrust, protocol.PromptQuestion} {
		m := New(Config{ClaimTimeout: 10 * time.Millisecond, AnswerTimeout: 30 * time.Millisecond, Default: "deny"}, &recSink{})
		done := make(chan Answer, 1)
		go func() { done <- m.Request(context.Background(), protocol.PromptInfo{ID: "p5", Kind: kind}, nil) }()
		select {
		case a := <-done:
			t.Fatalf("%s defaulted: %+v", kind, a)
		case <-time.After(120 * time.Millisecond):
		}
		if err := m.Reply("p5", "a", Answer{Value: "allow"}); err != nil {
			t.Fatal(err)
		}
		<-done
	}
}

func TestQuestionVisibilityIsImmediateAndSurvivesReconcile(t *testing.T) {
	for _, kind := range []protocol.PromptKind{protocol.PromptQuestion, protocol.PromptPermission, protocol.PromptTrust} {
		t.Run(string(kind), func(t *testing.T) {
			sink := &recSink{}
			m := New(Config{ClaimTimeout: time.Hour, AnswerTimeout: time.Hour}, sink)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opened := make(chan struct{})
			done := make(chan Answer, 1)
			go func() {
				done <- m.Request(ctx, protocol.PromptInfo{ID: "p", Channel: "channel", Kind: kind}, func() { close(opened) })
			}()
			<-opened
			visible := kind == protocol.PromptQuestion || kind == protocol.PromptPermission
			pending := m.Pending("channel")
			if len(pending) != 1 || pending[0].Escalated != visible {
				t.Fatalf("snapshot visibility: %+v", pending)
			}
			sink.mu.Lock()
			first, tiers := sink.n[0], append([]protocol.Tier(nil), sink.t[0]...)
			sink.mu.Unlock()
			want := 1
			if visible {
				want = 2
			}
			if first.Action != protocol.ActionRequested || first.Prompt.Escalated != visible || len(tiers) != want {
				t.Fatalf("notification: %+v, tiers: %v", first, tiers)
			}
			if visible && tiers[1] != protocol.TierFallback {
				t.Fatal("question not delivered to fallback client")
			}
			if err := m.Claim("p", "discord"); err != nil {
				t.Fatal(err)
			}
			reply := Answer{Value: protocol.AnswerAllow}
			if kind == protocol.PromptQuestion {
				reply = Answer{Value: protocol.AnswerAnswered, Answers: []string{"SQLite"}}
			}
			if err := m.Reply("p", "discord", reply); err != nil {
				t.Fatal(err)
			}
			answer := <-done
			if answer.Value != reply.Value || answer.Client != "discord" {
				t.Fatalf("answer lost: %+v", answer)
			}
			if kind == protocol.PromptQuestion && (len(answer.Answers) != 1 || answer.Answers[0] != "SQLite") {
				t.Fatalf("question answer lost: %+v", answer)
			}
			sink.mu.Lock()
			defer sink.mu.Unlock()
			if sink.n[len(sink.n)-1].Action != protocol.ActionAnswered || len(sink.t[len(sink.t)-1]) != want {
				t.Fatal("resolution did not reach the same clients")
			}
		})
	}
}

func TestAcceptedPermissionWinsDefaultTimer(t *testing.T) {
	for range 50 {
		m := New(Config{AnswerTimeout: time.Nanosecond}, &recSink{})
		p := protocol.PromptInfo{ID: "p", Kind: protocol.PromptPermission}
		a := m.Request(context.Background(), p, func() {
			if err := m.Reply(p.ID, "client", Answer{Value: protocol.AnswerAllow}); err != nil {
				t.Fatal(err)
			}
		})
		if a.Defaulted || a.Value != protocol.AnswerAllow {
			t.Fatalf("accepted permission lost to timeout: %+v", a)
		}
	}
}

func TestIndividualQuestionRejectsEmptyOrBatchReplies(t *testing.T) {
	m := New(Config{ClaimTimeout: time.Hour, AnswerTimeout: time.Hour}, &recSink{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan struct{})
	done := make(chan Answer, 1)
	go func() {
		done <- m.Request(ctx, protocol.PromptInfo{ID: "q", Kind: protocol.PromptQuestion, QuestionNumber: 1, QuestionTotal: 2}, func() { close(opened) })
	}()
	<-opened
	for _, answer := range []Answer{{Value: protocol.AnswerAnswered}, {Value: protocol.AnswerAnswered, Answers: []string{" "}}, {Value: protocol.AnswerAnswered, Answers: []string{"one", "two"}}} {
		if err := m.Reply("q", "user", answer); err == nil {
			t.Fatal("invalid answer accepted")
		}
		if ps := m.Pending(""); len(ps) != 1 || ps[0].ClaimedBy != "" {
			t.Fatal("invalid answer resolved or claimed the question")
		}
	}
	if err := m.Reply("q", "user", Answer{Value: protocol.AnswerAnswered, Answers: []string{"one"}}); err != nil {
		t.Fatal(err)
	}
	if a := <-done; len(a.Answers) != 1 || a.Answers[0] != "one" {
		t.Fatalf("answer: %+v", a)
	}
}
