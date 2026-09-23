package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

func TestWebNavRowStatesAndClick(t *testing.T) {
	m := channelModel()
	row := func() string { return stripANSI(m.sidebarHeader(sidebarWidth - 1)[m.navRowIndex(navWeb)]) }
	if row() != "○ Web UI" {
		t.Fatalf("before the first status: %q", row())
	}
	m.onWeb(webMsg{action: "status", status: protocol.WebStatus{URL: "http://127.0.0.1:4999/"}})
	if row() != "○ Web UI off" {
		t.Fatalf("off: %q", row())
	}
	// off: a click turns it on, with no dialog in between
	m.showTree = true
	if cmd := m.sidebarClick(2, m.navRowIndex(navWeb)); cmd == nil || m.ov != nil {
		t.Fatal("a click on the off row opened a dialog instead of enabling")
	}
	m.onWeb(webMsg{action: "on", status: protocol.WebStatus{Enabled: true, URL: "http://127.0.0.1:4999/", OpenURL: "http://127.0.0.1:4999/#code=SECRET"}})
	if row() != "● Web UI 127.0.0.1:4999" {
		t.Fatalf("on: %q", row())
	}
	if strings.Contains(m.status, "SECRET") {
		t.Fatalf("the one-time code reached the screen: %q", m.status)
	}
	// on: a click opens the controls
	if m.sidebarClick(2, m.navRowIndex(navWeb)); m.ov == nil || m.ov.kind != ovWeb || m.ov.shown[0].id != "open" || m.ov.shown[1].id != "off" {
		t.Fatalf("controls: %+v", m.ov)
	}
	// a failed enable says why and leaves it off
	m.onWeb(webMsg{action: "on", err: errors.New("listen tcp4 127.0.0.1:4999: bind: address already in use")})
	if row() != "! Web UI error" || !m.statusErr {
		t.Fatalf("port taken: %q", row())
	}
}

// A pushed change asks once and does not start a second chain of polls: the
// poll armed before it finds itself stale.
func TestAPushedChangeKeepsOneChainOfPolls(t *testing.T) {
	m := channelModel()
	armedWeb, armedDiscord := webTickMsg{m.webEpoch}, discordTickMsg{m.discordEpoch}
	if m.onChanged(changedMsg{protocol.ChangedWeb}) == nil || m.onChanged(changedMsg{protocol.ChangedDiscord}) == nil {
		t.Fatal("a change was not asked about")
	}
	if armedWeb.epoch == m.webEpoch || armedDiscord.epoch == m.discordEpoch {
		t.Fatal("the polls armed before the change are still current")
	}
	if m.onChanged(changedMsg{"something newer"}) != nil {
		t.Fatal("an unknown change was acted on")
	}
}

// The nav says what bounds the channel's commands only when it is less than
// a full sandbox.
func TestTheNavSaysWhenTheSandboxIsNotFull(t *testing.T) {
	m := channelModel()
	has := func() string {
		if i := m.navRowIndex(navSandbox); i >= 0 {
			return stripANSI(m.sidebarHeader(sidebarWidth - 1)[i])
		}
		return ""
	}
	for level, want := range map[string]string{"": "", "full": "", "limited": "limited", "none": "none · asks", "off": "off"} {
		m.channel.Sandbox = level
		if got := has(); (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("sandbox %q: row %q, want it to say %q", level, got, want)
		}
	}
}

// The sandbox dialog: a full sandbox offers only the switch; a limited one
// says what is missing, why, and the fix; off offers turning it on.
func TestTheSandboxDialog(t *testing.T) {
	m := channelModel()
	m.openSandbox("")
	ids := func() string {
		var out []string
		for _, it := range m.ov.shown {
			out = append(out, it.id)
		}
		return strings.Join(out, " ")
	}
	m.onSandbox(sandboxMsg{status: protocol.SandboxStatus{Enabled: true, Level: "full"}})
	if m.ov == nil || m.ov.kind != ovSandbox || ids() != "off" || m.ov.title != "Sandbox · on" {
		t.Fatalf("full: %q %q", m.ov.title, ids())
	}
	m.onSandbox(sandboxMsg{status: protocol.SandboxStatus{Enabled: true, Level: "limited", Missing: []string{"a", "b"}, Why: "blocked", Fix: "sudo sysctl -w x=1"}})
	if ids() != "info info info fix off" || m.ov.title != "Sandbox · on · limited" {
		t.Fatalf("limited: %q %q", m.ov.title, ids())
	}
	m.onSandbox(sandboxMsg{status: protocol.SandboxStatus{Enabled: false, Level: "full"}})
	if ids() != "on" || m.ov.title != "Sandbox · off" {
		t.Fatalf("off: %q %q", m.ov.title, ids())
	}
	if m.openSandbox("sideways") == nil || m.openSandbox("off") == nil {
		t.Fatal("/sandbox off asks the daemon; anything else says how it is used")
	}
	// the nav's row opens it
	m.ov, m.channel.Sandbox, m.showTree = nil, "limited", true
	if m.sidebarClick(2, m.navRowIndex(navSandbox)); m.ov == nil || m.ov.kind != ovSandbox {
		t.Fatal("a click on the nav's sandbox row did not open the dialog")
	}
}
