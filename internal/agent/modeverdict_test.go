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

// judge, over every combination of its facts: the properties the stages are
// there to keep, rather than a table that restates the code.
func TestJudgeKeepsItsPromises(t *testing.T) {
	bools := []bool{false, true}
	for _, ruled := range []policy.Verb{policy.Allow, policy.Ask, policy.Deny} {
		for _, mode := range []string{protocol.ModeAsk, protocol.ModeAuto, protocol.ModeYolo} {
			for _, compound := range bools {
				for _, control := range bools {
					for _, permitted := range bools {
						for _, egress := range bools {
							for _, outside := range bools {
								f := facts{ruled: ruled, compound: compound, control: control, permitted: permitted, mode: mode, egress: egress, outside: outside}
								got := judge(f)
								if hid := f; true {
									hid.hidden = true
									if judge(hid) != policy.Deny {
										t.Errorf("%+v: a hidden path was opened", hid)
									}
								}
								if bare := f; ruled != policy.Deny {
									bare.bare = true
									if judge(bare) == policy.Allow {
										t.Errorf("%+v: a command ran with no sandbox and nobody asked", bare)
									}
								}
								switch {
								case ruled == policy.Deny && got != policy.Deny:
									t.Errorf("%+v: a deny was loosened to %s", f, got)
								case control && got == policy.Allow:
									t.Errorf("%+v: an edit to what steers the harness was allowed without a human", f)
								case outside && mode == protocol.ModeAuto && got != policy.Deny:
									t.Errorf("%+v: auto let a call outside the directories through as %s", f, got)
								case outside && mode == protocol.ModeAsk && got == policy.Allow:
									t.Errorf("%+v: ask mode allowed a call outside the directories unasked", f)
								case compound && ruled == policy.Allow && !permitted && mode == protocol.ModeAsk && got == policy.Allow:
									t.Errorf("%+v: an allow rule spoke for a compound command", f)
								case egress && mode == protocol.ModeAuto && ruled == policy.Ask && !permitted && got == policy.Allow:
									t.Errorf("%+v: auto sent data out unasked", f)
								case ruled == policy.Allow && !compound && !control && !outside && got != policy.Allow:
									t.Errorf("%+v: a plain allowed call inside the directories became %s", f, got)
								}
							}
						}
					}
				}
			}
		}
	}
}
