package present

import (
	"slices"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
)

// Change is one thing about an agent that changed, as every client says it:
// what, what it is now, and why when the harness did it and not the human.
type Change struct {
	What string // "Name", "Role", "Model", "Variant"
	To   string
	Why  string
}

// AgentChanges reads an agent.updated event. The terminal drew a model move
// with its reason and Discord dropped the event altogether, so the one place
// a human was told "openai is at its limit until 14:10" was the one client
// they might not have open.
func AgentChanges(p event.AgentUpdatedPayload) []Change {
	var out []Change
	if p.Name != nil {
		out = append(out, Change{What: "Name", To: *p.Name})
	}
	if p.Role != nil {
		out = append(out, Change{What: "Role", To: *p.Role})
	}
	if p.Model != nil {
		out = append(out, Change{What: "Model", To: *p.Model, Why: p.Reason})
	}
	if p.Variant != nil {
		out = append(out, Change{What: "Variant", To: cmpOr(*p.Variant, "default")})
	}
	return out
}

func cmpOr(s, or string) string {
	if s == "" {
		return or
	}
	return s
}

// String is a change in one line: "model → zai/glm-5.3 · openai is at its limit".
func (c Change) String() string {
	s := strings.ToLower(c.What) + " → " + c.To
	if c.Why != "" {
		s += " · " + c.Why
	}
	return s
}

// Addressed puts who a message is for in front of it: "@scout @lint look at
// the parser". Nobody named is nothing added.
func Addressed(to []string, text string) string {
	if len(to) == 0 {
		return text
	}
	address := "@" + strings.Join(to, " @")
	if text == "" {
		return address
	}
	return address + " " + text
}

// Without drops recipients that go without saying where a message is shown
// ("user" in a chat the user is reading).
func Without(to []string, implicit ...string) []string {
	return slices.DeleteFunc(slices.Clone(to), func(name string) bool { return slices.Contains(implicit, name) })
}

// UnlessOnly is to, or nothing when its one recipient is the one that goes
// without saying (a post to the main agent alone); with anybody else named,
// everybody is.
func UnlessOnly(to []string, implicit string) []string {
	if len(to) == 1 && to[0] == implicit {
		return nil
	}
	return to
}
