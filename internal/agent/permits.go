package agent

import (
	"net/url"
	"slices"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/shellcmd"
)

// permits are the allows a human granted for the rest of a channel: exact
// calls ("Allow for this channel") and prefixes ("Allow go test for this
// channel": a command prefix, a host). They answer a policy Ask; they
// never override a Deny, and they last for the channel, across daemon
// restarts (each is a permit.granted event). They are channel state,
// guarded by the channel's lock.
type permits struct {
	calls    map[string]bool     // tool + "\x00" + one subject value
	prefixes map[string][]string // tool → remembered prefixes
}

// covers reports whether remembered allows answer a call of tool with this
// subject: every value of it (each path a patch touches) must be covered,
// or a patch allowed for one file would carry any other along.
func (p *permits) covers(tool string, sub policy.Subject) bool {
	if len(sub.Values) == 0 {
		return false
	}
	for _, v := range sub.Values {
		if !p.coversOne(tool, sub.Kind, v) {
			return false
		}
	}
	return true
}

func (p *permits) coversOne(tool string, kind policy.Kind, v string) bool {
	if p.calls[tool+"\x00"+v] {
		return true
	}
	for _, pre := range p.prefixes[tool] {
		if prefixCovers(kind, pre, v) {
			return true
		}
	}
	return false
}

// apply installs a granted permit.
func (p *permits) apply(g event.PermitPayload) {
	switch {
	case g.Prefix != "":
		if p.prefixes == nil {
			p.prefixes = map[string][]string{}
		}
		if !contains(p.prefixes[g.Tool], g.Prefix) {
			p.prefixes[g.Tool] = append(p.prefixes[g.Tool], g.Prefix)
		}
	case g.Call != "":
		if p.calls == nil {
			p.calls = map[string]bool{}
		}
		p.calls[g.Tool+"\x00"+g.Call] = true
	}
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

// hostsAllow reports whether the host of every URL is in hosts
// (stavlos.json's hosts): "*" is every host, "*.example.com" each subdomain
// of example.com, anything else that host itself.
func hostsAllow(hosts, urls []string) bool {
	if len(hosts) == 0 || len(urls) == 0 {
		return false
	}
	for _, raw := range urls {
		host := urlHost(raw)
		listed := host != "" && slices.ContainsFunc(hosts, func(h string) bool {
			if h == "*" {
				return true
			}
			if domain, ok := strings.CutPrefix(h, "*."); ok {
				return strings.HasSuffix(host, "."+domain)
			}
			return host == h
		})
		if !listed {
			return false
		}
	}
	return true
}
