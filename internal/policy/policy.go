// Package policy implements declarative allow/ask/deny rules (PRD §13).
//
// A rule is a tool pattern, an argument pattern and a verb. Patterns are
// compiled once, when the set is built, and have one small syntax: "*"
// matches any run of characters, "?" any one character, and everything
// else is literal. What "any" means depends on what the argument is: for a
// path "*" and "?" stop at "/" and "**" crosses it; for a command, a URL or
// text nothing is special about "/" ("git push * --force" matches
// "git push origin/main --force"). There is no invalid pattern, so a rule
// can never be silently inert.
//
// When several rules match, the longest literal prefix before the first
// wildcard wins, then the tool pattern's literal prefix; ties fall to the
// more restrictive verb (deny > ask > allow).
package policy

import (
	"regexp"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/shellcmd"
)

// Verb is a policy decision.
type Verb string

const (
	Allow Verb = "allow"
	Ask   Verb = "ask"
	Deny  Verb = "deny"
)

// Rank orders verbs by restrictiveness.
func (v Verb) Rank() int {
	switch v {
	case Deny:
		return 2
	case Ask:
		return 1
	default:
		return 0
	}
}

// Valid reports whether v is a known verb.
func (v Verb) Valid() bool { return v == Allow || v == Ask || v == Deny }

// Rule is one pattern → verb.
type Rule struct {
	Tool    string // "shell", "mcp__github__get_*" …
	Pattern string // argument pattern; "*" = any
	Verb    Verb
}

// rule is a Rule with its patterns compiled.
type rule struct {
	Rule
	tool     *regexp.Regexp
	flat     *regexp.Regexp // the argument pattern where "/" is an ordinary character
	path     *regexp.Regexp // the argument pattern where "*" and "?" stop at "/"
	lit, tlt int            // literal prefix lengths of the argument and tool patterns
}

// Set is an ordered rule set.
type Set struct {
	rules []rule
}

// New builds a Set from rules.
func New(rules ...Rule) *Set {
	s := &Set{rules: make([]rule, 0, len(rules))}
	for _, r := range rules {
		s.rules = append(s.rules, compile(r))
	}
	return s
}

func compile(r Rule) rule {
	c := rule{Rule: r, tool: Glob(r.Tool, false), flat: Glob(r.Pattern, false), path: Glob(r.Pattern, true),
		lit: literalPrefix(r.Pattern), tlt: literalPrefix(r.Tool)}
	if r.Pattern == "*" {
		c.path = c.flat // a bare "*" is the whole-tool rule: it matches every path, however deep
	}
	return c
}

// Glob compiles a pattern to an anchored regexp. With path set, "*" and "?"
// stop at "/" and "**" crosses it; otherwise "*" and "**" both match any run.
func Glob(p string, path bool) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`(?s)\A`)
	for i := 0; i < len(p); {
		switch {
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(`.*`)
			i += 2
		case p[i] == '*' && path:
			b.WriteString(`[^/]*`)
			i++
		case p[i] == '*':
			b.WriteString(`.*`)
			i++
		case p[i] == '?' && path:
			b.WriteString(`[^/]`)
			i++
		case p[i] == '?':
			b.WriteString(`.`)
			i++
		default:
			j := i
			for j < len(p) && p[j] != '*' && p[j] != '?' {
				j++
			}
			b.WriteString(regexp.QuoteMeta(p[i:j]))
			i = j
		}
	}
	b.WriteString(`\z`)
	return regexp.MustCompile(b.String()) // every character is quoted or a wildcard: it always compiles
}

func literalPrefix(p string) int {
	if i := strings.IndexAny(p, "*?"); i >= 0 {
		return i
	}
	return len(p)
}

// Rules returns a copy.
func (s *Set) Rules() []Rule {
	var out []Rule
	for _, r := range s.rules {
		out = append(out, r.Rule)
	}
	return out
}

// Merge overlays other onto s: a rule with the same tool and pattern
// replaces the earlier one (later layers win, PRD §10); new rules append.
func (s *Set) Merge(other *Set) *Set {
	if other == nil {
		return s
	}
	out := &Set{rules: append([]rule(nil), s.rules...)}
	for _, r := range other.rules {
		i := indexOf(out.rules, r.Tool, r.Pattern)
		if i >= 0 {
			out.rules[i] = r
		} else {
			out.rules = append(out.rules, r)
		}
	}
	return out
}

func indexOf(rules []rule, tool, pattern string) int {
	for i, r := range rules {
		if r.Tool == tool && r.Pattern == pattern {
			return i
		}
	}
	return -1
}

// Decide is the verb for a call of tool on sub: the most restrictive
// decision over everything the subject names (see texts). Nothing matching
// means Ask.
func (s *Set) Decide(tool string, sub Subject) Verb {
	v, _ := judge(sub, func(text string) Verb {
		if v, ok := s.Lookup(tool, sub.Kind, text); ok {
			return v
		}
		return Ask
	})
	return v
}

// Lookup is the verb of the most specific rule matching one text of the
// given kind; ok is false when no rule matches.
func (s *Set) Lookup(tool string, kind Kind, text string) (Verb, bool) {
	best := Verb("")
	bestArg, bestTool := -1, -1
	for _, r := range s.rules {
		m := r.flat
		if kind == KindPath {
			m = r.path
		}
		if !r.tool.MatchString(tool) || !m.MatchString(text) {
			continue
		}
		switch {
		case r.lit > bestArg, r.lit == bestArg && r.tlt > bestTool:
			best, bestArg, bestTool = r.Verb, r.lit, r.tlt
		case r.lit == bestArg && r.tlt == bestTool && r.Verb.Rank() > best.Rank():
			best = r.Verb
		}
	}
	return best, best != ""
}

// Layered is a base Set tightened by overlays: a trusted project's policy,
// a role's rules. The decision is the most restrictive of the base's and of
// every overlay that has a matching rule, so an overlay can only tighten
// (PRD §10.6) — by construction, not by inspection of its patterns.
type Layered struct {
	base     *Set
	overlays []*Set
}

// Layer builds a layered policy over base.
func Layer(base *Set, overlays ...*Set) *Layered {
	l := &Layered{base: base}
	for _, o := range overlays {
		l = l.With(o)
	}
	return l
}

// With returns l plus one more overlay (an empty or nil one changes nothing).
func (l *Layered) With(overlay *Set) *Layered {
	if overlay == nil || len(overlay.rules) == 0 {
		return l
	}
	return &Layered{base: l.base, overlays: append(append([]*Set(nil), l.overlays...), overlay)}
}

// Base is the merged base set.
func (l *Layered) Base() *Set { return l.base }

// Overlays lists the tightening layers in order.
func (l *Layered) Overlays() []*Set { return append([]*Set(nil), l.overlays...) }

// Decide is the verb for a call of tool on sub and the text it is about:
// for each text the subject names, the base's decision raised to any
// overlay's more restrictive match; over the texts, the most restrictive.
func (l *Layered) Decide(tool string, sub Subject) (Verb, string) {
	return judge(sub, func(text string) Verb {
		v, ok := l.base.Lookup(tool, sub.Kind, text)
		if !ok {
			v = Ask
		}
		for _, o := range l.overlays {
			if ov, ok := o.Lookup(tool, sub.Kind, text); ok && ov.Rank() > v.Rank() {
				v = ov
			}
		}
		return v
	})
}

// judge applies one decision to every text of a subject and keeps the most
// restrictive, with the text it was about (the primary on a tie).
func judge(sub Subject, one func(string) Verb) (Verb, string) {
	texts := sub.texts()
	if len(texts) == 0 {
		texts = []string{""}
	}
	verb, about := one(texts[0]), texts[0]
	for _, t := range texts[1:] {
		if v := one(t); v.Rank() > verb.Rank() {
			verb, about = v, t
		}
	}
	if sub.Kind == KindCommand && verb.Rank() > Allow.Rank() && about != sub.Primary() {
		about = sub.Primary() // a prompt shows the whole command line, not the part that decided
	}
	return verb, about
}

// texts is everything rules judge for a subject: its values and, for a
// command line, each command inside it in normalised form, so a deny on
// "rm -rf *" also speaks for "cd x && rm  -rf /".
func (s Subject) texts() []string {
	if s.Kind != KindCommand {
		return s.Values
	}
	out := append([]string(nil), s.Values...)
	for _, v := range s.Values {
		for _, c := range shellcmd.Commands(v) {
			if c != v {
				out = append(out, c)
			}
		}
	}
	return out
}

// Sorted returns rules sorted for display.
func (s *Set) Sorted() []Rule {
	r := s.Rules()
	sort.SliceStable(r, func(i, j int) bool {
		if r[i].Tool != r[j].Tool {
			return r[i].Tool < r[j].Tool
		}
		return r[i].Pattern < r[j].Pattern
	})
	return r
}
