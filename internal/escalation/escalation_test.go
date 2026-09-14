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

func (s *recSink) Notify(n protocol.PromptNotification, tiers []protocol.Tier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n = append(s.n, n)
	s.t = append(s.t, tiers)
}

func TestEscalateThenDefault(t *testing.T) {
	s := &recSink{}
	m := New(Config{ClaimTimeout: 30 * time.Millisecond, AnswerTimeout: 80 * time.Millisecond, Default: "deny"}, s)
	a := m.Request(context.Background(), protocol.PromptInfo{ID: "p1", Kind: "permission"})
	if !a.Defaulted || a.Value != "deny" {
		t.Fatalf("%+v", a)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.n) != 3 || s.n[0].Action != "requested" || s.n[1].Action != "escalated" || s.n[2].Action != "defaulted" {
		t.Fatalf("%+v", s.n)
	}
	if len(s.t[0]) != 1 || len(s.t[1]) != 2 {
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
	go func() { done <- m.Request(ctx, protocol.PromptInfo{ID: "p2"}) }()
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
	go func() { done <- m.Request(context.Background(), protocol.PromptInfo{ID: "p3"}) }()
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
		done <- m.Request(context.Background(), protocol.PromptInfo{ID: "p4", Kind: protocol.PromptTrust})
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
		go func() { done <- m.Request(context.Background(), protocol.PromptInfo{ID: "p5", Kind: kind}) }()
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
