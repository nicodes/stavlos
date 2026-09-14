package policy

import "testing"

func TestDecide(t *testing.T) {
	s := New(
		Rule{"shell", "git push*", Ask},
		Rule{"shell", "rm -rf*", Deny},
		Rule{"shell", "*", Allow},
		Rule{"edit", "src/**", Allow},
		Rule{"edit", "**", Ask},
		Rule{"read", "*", Allow},
		Rule{"mcp:github/*", "*", Ask},
		Rule{"mcp:github/get_*", "*", Allow},
	)
	cases := []struct {
		tool, arg string
		want      Verb
	}{
		{"shell", "ls -la", Allow},
		{"shell", "git push origin main", Ask},
		{"shell", "rm -rf /", Deny},
		{"shell", "rm -r -f /", Allow}, // documented limitation
		{"edit", "src/a/b.go", Allow},
		{"edit", "docs/x.md", Ask},
		{"read", "/etc/passwd", Allow},
		{"mcp:github/get_issue", "", Allow},
		{"mcp:github/create_issue", "", Ask},
		{"write", "anything", Ask}, // default
	}
	for _, c := range cases {
		if got := s.Decide(c.tool, c.arg); got != c.want {
			t.Errorf("%s %q: got %s want %s", c.tool, c.arg, got, c.want)
		}
	}
}

func TestLayered(t *testing.T) {
	g := New(Rule{"shell", "*", Allow}, Rule{"shell", "git push*", Ask})
	p := New(Rule{"shell", "git push*", Allow}, Rule{"shell", "curl*", Deny})
	s := Layer(g, p)
	if s.Decide("shell", "git push x") != Ask {
		t.Error("project loosened global")
	}
	if s.Decide("shell", "curl x") != Deny {
		t.Error("project tighten ignored")
	}
	if s.Decide("shell", "ls") != Allow || s.Decide("read", "x") != Ask {
		t.Error("base decision lost where the overlay says nothing")
	}
	// An overlay rule with a longer literal prefix than a base deny still
	// cannot win: the decision is the most restrictive across layers.
	g = New(Rule{"shell", "*", Allow}, Rule{"shell", "*--force*", Deny})
	if Layer(g, New(Rule{"shell", "git*", Allow})).Decide("shell", "git push --force") != Deny {
		t.Error("overlay shadowed a base deny")
	}
	// Overlays stack; empty ones are dropped.
	l := Layer(g).With(New()).With(nil).With(New(Rule{"shell", "ls*", Ask})).With(New(Rule{"shell", "ls -la", Deny}))
	if len(l.Overlays()) != 2 || l.Decide("shell", "ls -la") != Deny || l.Decide("shell", "ls") != Ask || l.Decide("shell", "pwd") != Allow {
		t.Errorf("stacked overlays: %v", l.Overlays())
	}
	if v, ok := New().Lookup("shell", "x"); ok || v != "" {
		t.Error("Lookup on an empty set")
	}
}

func TestMatch(t *testing.T) {
	yes := [][2]string{{"src/**", "src/a/b/c.go"}, {"**", "x"}, {"**/*.go", "a/b.go"}, {"git push*", "git push"}, {"*.md", "x.md"}}
	no := [][2]string{{"src/**", "lib/a.go"}, {"*.md", "a/x.md"}}
	for _, y := range yes {
		if !Match(y[0], y[1]) {
			t.Errorf("%q should match %q", y[0], y[1])
		}
	}
	for _, n := range no {
		if Match(n[0], n[1]) {
			t.Errorf("%q should not match %q", n[0], n[1])
		}
	}
}
