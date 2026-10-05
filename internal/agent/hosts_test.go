package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
)

// TestHostsAnswerFetches: a fetch from a listed host (exact, a subdomain of
// *.domain, or anything for *) is allowed without asking, in ask and in auto
// mode; an unlisted host still asks, and a deny rule still wins.
func TestHostsAnswerFetches(t *testing.T) {
	s, _ := newTestChannel(t, testConfig{json: `{"model":"fake/m1","hosts":["github.com","*.golang.org"]}`}, &fakeModel{})
	a := s.Root()
	rv := a.role()
	fetch := s.tools["web"]
	verdict := func(cfg *config.Effective, u string) policy.Verb {
		return a.decide(model.Block{ID: "c1", Name: "web", Input: json.RawMessage(`{"action":"fetch","url":` + strconv.Quote(u) + `}`)}, fetch, rv, cfg).verb
	}
	cfg := s.Config()
	for u, want := range map[string]policy.Verb{
		"https://github.com/nicodes/stavlos": policy.Allow,
		"https://go.golang.org/doc":          policy.Allow,
		"https://golang.org/doc":             policy.Ask,
		"https://example.com/":               policy.Ask,
	} {
		if got := verdict(cfg, u); got != want {
			t.Fatalf("ask mode, %s: %s, want %s", u, got, want)
		}
	}
	if err := s.SetMode(context.Background(), protocol.ModeAuto); err != nil {
		t.Fatal(err)
	}
	if verdict(cfg, "https://example.com/") != policy.Ask || verdict(cfg, "https://github.com/x") != policy.Allow {
		t.Fatal("auto mode asks for an unlisted host and allows a listed one")
	}
	every := *cfg
	every.Hosts = []string{"*"}
	if got := verdict(&every, "https://example.com/"); got != policy.Allow {
		t.Fatalf("* allows every host: %s", got)
	}
	deny, err := config.ParsePolicy(map[string]any{"fetch": "deny"})
	if err != nil {
		t.Fatal(err)
	}
	denied := *cfg
	denied.Policy = cfg.Policy.With(deny)
	if got := verdict(&denied, "https://github.com/x"); got != policy.Deny {
		t.Fatalf("a deny rule wins over hosts: %s", got)
	}
}

// A job cancellation allowance must never approve a shell command named kill.
func TestShellKillAllowanceDoesNotApproveCommands(t *testing.T) {
	s, _ := newTestChannel(t, testConfig{}, &fakeModel{})
	a := s.Root()
	call := model.Block{Name: "shell", Input: json.RawMessage(`{"command":"kill 1234"}`)}
	if got := a.decide(call, s.tools["shell"], a.role(), s.Config()).verb; got != policy.Ask {
		t.Fatalf("kill command admitted: %s", got)
	}
	call.Input = json.RawMessage(`{"action":"kill","id":"job1"}`)
	if got := a.decide(call, s.tools["shell"], a.role(), s.Config()).verb; got != policy.Allow {
		t.Fatalf("job cancellation: %s", got)
	}
}
