package agent

import (
	"sync"

	"github.com/nicodes/stavlos/internal/protocol"
)

// permits are the allows a human granted for the rest of a session: exact
// calls ("Allow for this session") and command prefixes or hosts ("Allow
// go test for this session"). They answer a policy Ask; they never
// override a Deny, and they end with the session (PRD §10.3).
type permits struct {
	mu       sync.Mutex
	calls    map[string]bool     // tool + "\x00" + policy argument
	prefixes map[string][]string // tool → remembered prefixes
}

// covers reports whether a remembered allow answers a call of tool with
// the given policy argument.
func (p *permits) covers(tool, arg string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls[tool+"\x00"+arg] {
		return true
	}
	for _, pre := range p.prefixes[tool] {
		if protocol.ToolPrefixCovers(tool, pre, arg) {
			return true
		}
	}
	return false
}

// rememberCall allows this exact call for the session.
func (p *permits) rememberCall(tool, arg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == nil {
		p.calls = map[string]bool{}
	}
	p.calls[tool+"\x00"+arg] = true
}

// rememberPrefix allows every call of tool the prefix covers for the
// session. The prefix is the daemon's own (protocol.ToolPrefix of the call
// being answered), never a client's.
func (p *permits) rememberPrefix(tool, prefix string) {
	if prefix == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prefixes == nil {
		p.prefixes = map[string][]string{}
	}
	for _, have := range p.prefixes[tool] {
		if have == prefix {
			return
		}
	}
	p.prefixes[tool] = append(p.prefixes[tool], prefix)
}
