package policy

import "testing"

func TestDecide(t *testing.T) {
	s := New(
		Rule{"bash", "git push*", Ask},
		Rule{"bash", "rm -rf*", Deny},
		Rule{"bash", "*", Allow},
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
		{"bash", "ls -la", Allow},
		{"bash", "git push origin main", Ask},
		{"bash", "rm -rf /", Deny},
		{"bash", "rm -r -f /", Allow}, // documented limitation
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

func TestTighten(t *testing.T) {
	g := New(Rule{"bash", "*", Allow}, Rule{"bash", "git push*", Ask})
	p := New(Rule{"bash", "git push*", Allow}, Rule{"bash", "curl*", Deny})
	s := g.Tighten(p)
	if s.Decide("bash", "git push x") != Ask {
		t.Error("project loosened global")
	}
	if s.Decide("bash", "curl x") != Deny {
		t.Error("project tighten ignored")
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
