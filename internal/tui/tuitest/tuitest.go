// Package tuitest holds what the tests of the TUI packages share.
package tuitest

import (
	"regexp"

	"github.com/nicodes/stavlos/internal/event"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// StripANSI removes the escape sequences from s.
func StripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// Event is a log event of the test channel s1.
func Event(seq int64, agent string, typ event.Type, payload any) event.Event {
	return event.Event{Seq: seq, Channel: "s1", Agent: agent, Type: typ, Payload: event.MustPayload(payload)}
}
