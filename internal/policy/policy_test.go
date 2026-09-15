package policy

import "testing"

func TestDecide(t *testing.T) {
	s := New(
		Rule{"shell", "git push*", Ask},
		Rule{"shell", "rm -rf*", Deny},
		Rule{"shell", "*", Allow},
		Rule{"apply_patch", "src/**", Allow},
		Rule{"apply_patch", "**", Ask},
		Rule{"read", "*", Allow},
		Rule{"mcp__github__*", "*", Ask},
		Rule{"mcp__github__get_*", "*", Allow},
	)
	cases := []struct {
		tool string
		sub  Subject
		want Verb
	}{
		{"shell", Command("ls -la"), Allow},
		{"shell", Command("git push origin main"), Ask},
		{"shell", Command("rm -rf /"), Deny},
		{"shell", Command("rm -r -f /"), Allow}, // globs see words, not meaning: the sandbox is the boundary
		{"shell", Command("cd x && rm -rf /"), Deny},
		{"shell", Command("FOO=1 sudo /bin/rm  -rf /"), Deny},
		{"shell", Command("bash -c 'rm -rf ~'"), Deny},
		{"apply_patch", Path("src/a/b.go"), Allow},
		{"apply_patch", Path("docs/x.md"), Ask},
		{"apply_patch", Path("src/a.go", "docs/x.md"), Ask}, // every path is judged
		{"read", Path("/etc/passwd"), Allow},
		{"mcp__github__get_issue", Text(""), Allow},
		{"mcp__github__create_issue", Text(""), Ask},
		{"write", Text("anything"), Ask}, // default
	}
	for _, c := range cases {
		if got := s.Decide(c.tool, c.sub); got != c.want {
			t.Errorf("%s %q: got %s want %s", c.tool, c.sub.Values, got, c.want)
		}
	}
}

// TestWildcardsByKind: in a command "*" crosses "/" (a deny on a push with
// any ref must see origin/main); in a path it stops at "/" and "**" crosses.
// Every pattern compiles: "[" is literal, not a broken character class.
func TestWildcardsByKind(t *testing.T) {
	s := New(Rule{"shell", "git push * --force", Deny}, Rule{"shell", "rm -rf [", Deny}, Rule{"p", "*.md", Deny}, Rule{"p", "**/*.go", Deny})
	for _, c := range []struct {
		tool string
		sub  Subject
		want bool
	}{
		{"shell", Command("git push origin/main --force"), true},
		{"shell", Command("rm -rf ["), true},
		{"shell", Command("rm -rf x"), false},
		{"p", Path("x.md"), true},
		{"p", Path("a/x.md"), false},
		{"p", Path("a/b/c.go"), true},
		{"p", Text("a/x.md"), true},
	} {
		if got := s.Decide(c.tool, c.sub) == Deny; got != c.want {
			t.Errorf("%s %v: deny=%v want %v", c.tool, c.sub.Values, got, c.want)
		}
	}
}

func TestLayered(t *testing.T) {
	g := New(Rule{"shell", "*", Allow}, Rule{"shell", "git push*", Ask})
	p := New(Rule{"shell", "git push*", Allow}, Rule{"shell", "curl*", Deny})
	s := Layer(g, p)
	decide := func(l *Layered, tool, arg string) Verb {
		v, _ := l.Decide(tool, Command(arg))
		return v
	}
	if decide(s, "shell", "git push x") != Ask {
		t.Error("project loosened global")
	}
	if decide(s, "shell", "curl x") != Deny {
		t.Error("project tighten ignored")
	}
	if decide(s, "shell", "ls") != Allow || decide(s, "read", "x") != Ask {
		t.Error("base decision lost where the overlay says nothing")
	}
	// An overlay rule with a longer literal prefix than a base deny still
	// cannot win: the decision is the most restrictive across layers.
	g = New(Rule{"shell", "*", Allow}, Rule{"shell", "*--force*", Deny})
	if decide(Layer(g, New(Rule{"shell", "git*", Allow})), "shell", "git push --force") != Deny {
		t.Error("overlay shadowed a base deny")
	}
	// Overlays stack; empty ones are dropped.
	l := Layer(g).With(New()).With(nil).With(New(Rule{"shell", "ls*", Ask})).With(New(Rule{"shell", "ls -la", Deny}))
	if len(l.Overlays()) != 2 || decide(l, "shell", "ls -la") != Deny || decide(l, "shell", "ls") != Ask || decide(l, "shell", "pwd") != Allow {
		t.Errorf("stacked overlays: %v", l.Overlays())
	}
	if v, ok := New().Lookup("shell", KindCommand, "x"); ok || v != "" {
		t.Error("Lookup on an empty set")
	}
	// The decision names what it is about: the worst path of several; the
	// whole line for a command, whichever part decided.
	pl := Layer(New(Rule{"apply_patch", "*", Allow}, Rule{"apply_patch", ".env", Deny}, Rule{"shell", "rm *", Deny}, Rule{"shell", "*", Allow}))
	if v, about := pl.Decide("apply_patch", Path("a.go", ".env")); v != Deny || about != ".env" {
		t.Errorf("patch: %s %q", v, about)
	}
	if v, about := pl.Decide("shell", Command("ls; rm x")); v != Deny || about != "ls; rm x" {
		t.Errorf("command: %s %q", v, about)
	}
}
