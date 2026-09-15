package agent

import (
	"net/url"
	"strings"
	"sync"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/shellcmd"
)

// permits are the allows a human granted for the rest of a channel: exact
// calls ("Allow for this channel") and prefixes ("Allow go test for this
// channel": a command prefix, a host). They answer a policy Ask; they
// never override a Deny, and they last for the channel, across daemon
// restarts (each is logged as permit.granted and replayed) (PRD §10.3).
type permits struct {
	mu       sync.Mutex
	calls    map[string]bool     // tool + "\x00" + primary subject
	prefixes map[string][]string // tool → remembered prefixes
}

// covers reports whether a remembered allow answers a call of tool with
// this subject.
func (p *permits) covers(tool string, sub policy.Subject) bool {
	arg := sub.Primary()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls[tool+"\x00"+arg] {
		return true
	}
	for _, pre := range p.prefixes[tool] {
		if prefixCovers(sub.Kind, pre, arg) {
			return true
		}
	}
	return false
}

// apply installs a granted permit (live or replayed).
func (p *permits) apply(g event.PermitPayload) {
	if g.Prefix != "" {
		p.rememberPrefix(g.Tool, g.Prefix)
	} else if g.Call != "" {
		p.rememberCall(g.Tool, g.Call)
	}
}

// rememberCall allows this exact call for the channel.
func (p *permits) rememberCall(tool, arg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == nil {
		p.calls = map[string]bool{}
	}
	p.calls[tool+"\x00"+arg] = true
}

// rememberPrefix allows every call of tool the prefix covers for the
// channel. The prefix is the daemon's own (prefixFor of the call being
// answered), never a client's.
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

// prefixFor is what "allow … for this channel" may remember for a call:
// the command prefix for a command (shellcmd.Prefix), the host for a URL,
// "" for subjects without a sensible prefix.
func prefixFor(kind policy.Kind, arg string) string {
	switch kind {
	case policy.KindCommand:
		return shellcmd.Prefix(arg)
	case policy.KindURL:
		return urlHost(arg)
	case policy.KindText, policy.KindPath, policy.KindID:
	}
	return ""
}

// prefixCovers reports whether a remembered prefix covers a call: whole-word
// command prefixes for commands, the host for URLs.
func prefixCovers(kind policy.Kind, prefix, arg string) bool {
	switch kind {
	case policy.KindCommand:
		return shellcmd.Covers(prefix, arg)
	case policy.KindURL:
		return prefix != "" && urlHost(arg) == prefix
	case policy.KindText, policy.KindPath, policy.KindID:
	}
	return false
}

// urlHost is the lower-cased host of a URL ("" when it has none); a bare
// "host/path" counts as https.
func urlHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
