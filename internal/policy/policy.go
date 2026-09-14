// Package policy implements declarative allow/ask/deny rules (PRD §13).
//
// Patterns are globs over the tool's full argument string. When several
// patterns match, the longest literal prefix before the first wildcard wins;
// ties fall to the more restrictive verb (deny > ask > allow).
package policy

import (
	"path"
	"sort"
	"strings"
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
	Tool    string // "bash", "edit", "mcp:github/get_*" …
	Pattern string // argument glob; "*" = any
	Verb    Verb
}

// Set is a merged, ordered rule set.
type Set struct {
	rules []Rule
}

// New builds a Set from rules.
func New(rules ...Rule) *Set { return &Set{rules: append([]Rule(nil), rules...)} }

// Rules returns a copy.
func (s *Set) Rules() []Rule { return append([]Rule(nil), s.rules...) }

// Merge overlays other onto s: a rule with the same tool and pattern
// replaces the earlier one (later layers win, PRD §10); new rules append.
func (s *Set) Merge(other *Set) *Set {
	if other == nil {
		return s
	}
	out := New(s.rules...)
	for _, r := range other.rules {
		replaced := false
		for i := range out.rules {
			if out.rules[i].Tool == r.Tool && out.rules[i].Pattern == r.Pattern {
				out.rules[i] = r
				replaced = true
				break
			}
		}
		if !replaced {
			out.rules = append(out.rules, r)
		}
	}
	return out
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

// Decide is the base's decision, raised to any overlay's more restrictive
// matching decision. Where no overlay matches, the base alone decides.
func (l *Layered) Decide(tool, arg string) Verb {
	v := l.base.Decide(tool, arg)
	for _, o := range l.overlays {
		if ov, ok := o.Lookup(tool, arg); ok && ov.Rank() > v.Rank() {
			v = ov
		}
	}
	return v
}

// Decide returns the verb for a tool call. Default when nothing matches: Ask.
func (s *Set) Decide(tool, arg string) Verb {
	if v, ok := s.Lookup(tool, arg); ok {
		return v
	}
	return Ask
}

// Lookup is Decide without the default: ok is false when no rule matches.
// Specificity is the argument pattern's literal prefix, then the tool
// pattern's literal prefix, then the more restrictive verb.
func (s *Set) Lookup(tool, arg string) (Verb, bool) {
	best := Verb("")
	bestArg, bestTool := -1, -1
	for _, r := range s.rules {
		if !toolMatch(r.Tool, tool) || !Match(r.Pattern, arg) {
			continue
		}
		la, lt := literalPrefix(r.Pattern), literalPrefix(r.Tool)
		switch {
		case la > bestArg, la == bestArg && lt > bestTool:
			best, bestArg, bestTool = r.Verb, la, lt
		case la == bestArg && lt == bestTool && r.Verb.Rank() > best.Rank():
			best = r.Verb
		}
	}
	return best, best != ""
}

func toolMatch(pattern, tool string) bool {
	if pattern == tool {
		return true
	}
	if strings.ContainsAny(pattern, "*?") {
		ok, _ := path.Match(pattern, tool)
		return ok
	}
	return false
}

func literalPrefix(p string) int {
	i := strings.IndexAny(p, "*?[")
	if i < 0 {
		return len(p)
	}
	return i
}

// Match is a glob match where "*" and "?" do not cross "/" but "**" matches
// any run including separators. For bash command strings, which have no
// meaningful separators, callers may prefer patterns ending in "*"; a
// trailing "*" is treated as "**" so `git push*` matches `git push origin/x`.
func Match(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	if pattern == "*" || pattern == "**" {
		return true
	}
	if strings.HasSuffix(pattern, "*") && !strings.HasSuffix(pattern, "**") {
		pattern += "*"
	}
	return matchSegments(pattern, s)
}

func matchSegments(p, s string) bool {
	// Expand "**" into a regexp-free recursive matcher.
	for {
		i := strings.Index(p, "**")
		if i < 0 {
			ok, _ := path.Match(p, s)
			return ok
		}
		head := p[:i]
		tail := strings.TrimPrefix(p[i+2:], "/")
		// head must match a prefix of s ending at a boundary
		if head != "" {
			hl := literalPrefix(head)
			if hl == len(head) {
				if !strings.HasPrefix(s, head) {
					return false
				}
				s = s[len(head):]
			} else {
				// head contains single-char wildcards: try all split points
				for j := 0; j <= len(s); j++ {
					if ok, _ := path.Match(head, s[:j]); ok && matchSegments("**/"+tail, s[j:]) {
						return true
					}
				}
				return false
			}
		}
		if tail == "" {
			return true
		}
		// try every suffix of s for the tail
		for j := 0; j <= len(s); j++ {
			if matchSegments(tail, s[j:]) {
				return true
			}
		}
		return false
	}
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
