package agent

import (
	"testing"

	"github.com/nicodes/stavlos/internal/policy"
	"github.com/nicodes/stavlos/internal/protocol"
)

// The whole meaning of the three modes, as a table.
func TestModeVerdict(t *testing.T) {
	for _, c := range []struct {
		mode                    string
		sticky, egress, outside bool
		want                    policy.Verb
	}{
		{protocol.ModeAsk, false, false, false, policy.Ask},
		{protocol.ModeAsk, false, false, true, policy.Ask},
		{protocol.ModeYolo, false, false, false, policy.Allow},
		{protocol.ModeYolo, false, true, true, policy.Allow},
		{protocol.ModeYolo, true, false, false, policy.Ask}, // what steers the harness is a human's to allow
		{protocol.ModeYolo, true, false, true, policy.Ask},
		{protocol.ModeAuto, false, false, false, policy.Allow},
		{protocol.ModeAuto, false, true, false, policy.Ask}, // sends data out
		{protocol.ModeAuto, true, false, false, policy.Ask},
		{protocol.ModeAuto, false, false, true, policy.Deny},
		{protocol.ModeAuto, true, true, true, policy.Deny}, // outside is outside
		{"a mode from the future", false, false, false, policy.Ask},
	} {
		if got := ModeVerdict(c.mode, c.sticky, c.egress, c.outside); got != c.want {
			t.Errorf("%s sticky=%v egress=%v outside=%v: %s, want %s", c.mode, c.sticky, c.egress, c.outside, got, c.want)
		}
	}
}
