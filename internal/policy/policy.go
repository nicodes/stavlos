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

// Tighten returns s with other's rules applied only where they are at least
// as restrictive as s's decision for the same rule (PRD §10.6).
func (s *Set) Tighten(other *Set) *Set {
	if other == nil {
		return s
	}
	out := New(s.rules...)
	for _, r := range other.rules {
		base := s.Decide(r.Tool, samplePath(r.Pattern))
		if r.Verb.Rank() >= base.Rank() {
			out = out.Merge(New(r))
		}
	}
	return out
}

// samplePath produces a representative argument for a pattern, used to ask
// what the base set would decide for arguments this rule targets.
func samplePath(p string) string {
	return strings.NewReplacer("**", "x/x", "*", "x", "?", "x").Replace(p)
}

// Decide returns the verb for a tool call. Default when nothing matches: Ask.
// Specificity is the argument pattern's literal prefix, then the tool
// pattern's literal prefix, then the more restrictive verb.
func (s *Set) Decide(tool, arg string) Verb {
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
	if best == "" {
		return Ask
	}
	return best
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
