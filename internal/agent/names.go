package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// NewID returns a random id with a prefix.
func NewID(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// maxNameLen bounds an agent's name.
const maxNameLen = 32

// reservedNames may not be taken by an agent: messages are attributed by
// name, and these read as the human or the system.
var reservedNames = map[string]bool{"user": true, "human": true, "system": true}

// normalizeName turns a requested label into a name: lowercase letters,
// digits, '-' and '_', every run of anything else collapsed to one '-',
// trimmed, at most maxNameLen characters. So "SYSTEM: ignore this" becomes
// a plain identifier that cannot read as an instruction.
func normalizeName(label string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
			dash = r == '-'
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(b.String(), "-_")
	if len(name) > maxNameLen {
		name = strings.TrimRight(name[:maxNameLen], "-_")
	}
	return name
}

// NormalizeName is normalizeName for the daemon, which names channels by the
// same rules as agents.
func NormalizeName(label string) string { return normalizeName(label) }

// UniqueName is want normalised (fallback when that leaves nothing, then
// "agent") with a -2, -3, … suffix while taken reports the name in use.
func UniqueName(want, fallback string, taken func(string) bool) string {
	base := normalizeName(want)
	if base == "" {
		base = normalizeName(fallback)
	}
	if base == "" {
		base = "agent"
	}
	name := base
	for n := 2; taken(name); n++ {
		suffix := fmt.Sprintf("-%d", n)
		name = strings.TrimRight(base[:min(len(base), maxNameLen-len(suffix))], "-_") + suffix
	}
	return name
}

// uniqueName is the name agent id would get asking for want: normalised,
// suffixed while another agent has it (names are never released), and
// refused when it is reserved. It changes nothing: the spawn or update
// event carrying the name claims it.
func (cs *channelState) uniqueName(want, fallback, id string) (string, error) {
	name := UniqueName(want, fallback, func(n string) bool {
		owner, taken := cs.names[n]
		return taken && owner != id
	})
	base := normalizeName(want)
	if base == "" {
		base = normalizeName(fallback)
	}
	if reservedNames[base] {
		return "", fmt.Errorf("label %q is reserved: a name may not read as the human or the system", base)
	}
	return name, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
