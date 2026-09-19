package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

func TestWebNavRowStatesAndClick(t *testing.T) {
	m := channelModel()
	row := func() string { return stripANSI(m.sidebarHeader(sidebarWidth - 1)[sidebarWebRow]) }
	if row() != "○ Web UI" {
		t.Fatalf("before the first status: %q", row())
	}
	m.onWeb(webMsg{action: "status", status: protocol.WebStatus{URL: "http://127.0.0.1:4999/"}})
	if row() != "○ Web UI off" {
		t.Fatalf("off: %q", row())
	}
	// off: a click turns it on, with no dialog in between
	m.showTree = true
	if cmd := m.sidebarClick(2, sidebarWebRow); cmd == nil || m.ov != nil {
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
	if m.sidebarClick(2, sidebarWebRow); m.ov == nil || m.ov.kind != ovWeb || m.ov.shown[0].id != "open" || m.ov.shown[1].id != "off" {
		t.Fatalf("controls: %+v", m.ov)
	}
	// a failed enable says why and leaves it off
	m.onWeb(webMsg{action: "on", err: errors.New("listen tcp4 127.0.0.1:4999: bind: address already in use")})
	if row() != "! Web UI error" || !m.statusErr {
		t.Fatalf("port taken: %q", row())
	}
}
