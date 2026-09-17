package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

func TestDiscordPanelAndControls(t *testing.T) {
	m := channelModel()
	if cmd := m.openDiscord(""); cmd == nil || m.ov == nil || m.ov.kind != ovDiscord {
		t.Fatal("Discord panel did not open")
	}
	st := protocol.DiscordStatus{Configured: true, Enabled: true, State: "connected", Bot: "Stavlos", GuildName: "My Server", Channels: 5}
	if cmd := m.onDiscord(discordMsg{status: st, epoch: m.discordEpoch}); cmd == nil {
		t.Fatal("status polling not scheduled")
	}
	view := stripANSI(m.ov.view(100, ""))
	for _, want := range []string{"connected", "Stavlos", "My Server", "Bridged channels: 5", "Automatic connection: on", "Disconnect"} {
		if !strings.Contains(view, want) {
			t.Fatalf("panel lacks %q:\n%s", want, view)
		}
	}
	if m.ov.selected().id != "disconnect" {
		t.Fatal("connected panel should select disconnect")
	}
	before := m.discordEpoch
	if cmd := m.overlaySubmit(false); cmd == nil || m.discordEpoch <= before {
		t.Fatal("disconnect control did not issue a fresh command")
	}
	st.Enabled, st.State = false, "disconnected"
	m.onDiscord(discordMsg{status: st, epoch: m.discordEpoch})
	if m.ov.selected().id != "connect" {
		t.Fatal("disconnected panel should select connect")
	}
	if cmd := m.openDiscord("unknown"); cmd == nil || !strings.Contains(m.status, "usage") {
		t.Fatal("invalid Discord action was accepted")
	}
}

func TestDiscordPanelDropsLateResponses(t *testing.T) {
	m := channelModel()
	m.openDiscord("connect")
	old := m.discordEpoch
	m.openDiscord("disconnect")
	m.onDiscord(discordMsg{epoch: old, err: errors.New("stale failure")})
	if m.ov.title != "Discord" {
		t.Fatal("old response changed new panel")
	}
	m.onDiscord(discordMsg{epoch: m.discordEpoch, status: protocol.DiscordStatus{State: "error", Error: "bad token", ConfigPath: "/config/stavlos.json"}})
	if view := stripANSI(m.ov.view(100, "")); !strings.Contains(view, "bad token") || !strings.Contains(view, "/config/stavlos.json") {
		t.Fatalf("missing setup error: %s", view)
	}
	m.closeOverlay()
	if cmd := m.onDiscord(discordMsg{epoch: m.discordEpoch, status: protocol.DiscordStatus{Configured: true, State: "connected"}}); cmd == nil || m.ov != nil {
		t.Fatal("background polling must continue without reopening the panel")
	}
	next, cmd := m.Update(discordTickMsg{epoch: m.discordEpoch})
	if next.(Model).ov != nil {
		t.Fatal("tick reopened panel")
	}
	_ = cmd // ordinary cursor/spinner commands may still be scheduled by Update
}

func TestDiscordNavIndicatorStates(t *testing.T) {
	m := channelModel()
	if text := stripANSI(m.sidebarHeader(sidebarWidth - 1)[m.sidebarDiscordRow()]); !strings.Contains(text, "Discord checking") {
		t.Fatalf("initial state: %q", text)
	}
	for _, tc := range []struct {
		status protocol.DiscordStatus
		err    error
		want   string
	}{
		{status: protocol.DiscordStatus{Configured: true, Enabled: true, State: "connected"}, want: "● Discord connected"},
		{status: protocol.DiscordStatus{Configured: true, Enabled: true, State: "connecting"}, want: "◐ Discord connecting"},
		{status: protocol.DiscordStatus{Configured: true, State: "reconnecting"}, want: "◐ Discord reconnecting"},
		{status: protocol.DiscordStatus{Configured: true, State: "stopping"}, want: "◐ Discord stopping"},
		{status: protocol.DiscordStatus{Configured: true, Enabled: true, State: "disconnected"}, want: "○ Discord disconnected"},
		{status: protocol.DiscordStatus{State: "disconnected"}, want: "○ Discord not configured"},
		{status: protocol.DiscordStatus{State: "error", Error: "bad token"}, want: "! Discord error"},
		{err: errors.New("status request failed"), want: "! Discord unavailable"},
	} {
		cmd := m.onDiscord(discordMsg{status: tc.status, err: tc.err, epoch: m.discordEpoch})
		if cmd == nil || m.ov != nil {
			t.Fatal("background update stopped polling or opened a panel")
		}
		if text := stripANSI(m.sidebarHeader(sidebarWidth - 1)[m.sidebarDiscordRow()]); text != tc.want {
			t.Fatalf("want %q, got %q", tc.want, text)
		}
	}
}

func TestDiscordNavSurvivesChannelSwitchAndOpensControls(t *testing.T) {
	m := channelModel()
	m.onDiscord(discordMsg{epoch: m.discordEpoch, status: protocol.DiscordStatus{Configured: true, State: "connected"}})
	m.bindChannel(protocol.ChannelInfo{ID: "other", Dir: "/other"})
	if !m.discordKnown || m.discordStatus.State != "connected" {
		t.Fatal("channel switch discarded global Discord status")
	}
	m.showTree = true
	old := m.discordEpoch
	if cmd := m.sidebarClick(2, m.sidebarDiscordRow()); cmd == nil || m.ov == nil || m.ov.kind != ovDiscord {
		t.Fatal("indicator did not open controls")
	}
	if m.discordEpoch == old {
		t.Fatal("opening controls did not invalidate the previous poll")
	}
	m.onDiscord(discordMsg{epoch: old, err: errors.New("old failed request")})
	if m.discordStatus.State != "connected" {
		t.Fatal("stale poll overwrote the current indicator")
	}
	cmds, _ := m.update(discordTickMsg{epoch: old})
	if len(cmds) != 0 {
		t.Fatal("stale poll started another polling chain")
	}
}
