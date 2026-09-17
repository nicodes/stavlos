package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

// sidebarY translates a selectable row to its drawn position, accounting for
// the directory metadata beneath channel rows.
func sidebarY(m Model, index int) int {
	_, items := m.sidebarLines(m.vp.Height)
	for y, i := range items {
		if i == index {
			return y
		}
	}
	return -1
}

func TestGlobalNavigationKeepsPerChannelEditor(t *testing.T) {
	m := channelModel()
	a := m.channel
	a.ID = m.channelID
	m.input.SetValue("draft for project A")
	m.history, m.histIdx, m.historySeq = []string{"A command"}, 1, 100
	m.bindChannel(protocol.ChannelInfo{ID: "b", Name: "other", Dir: "/different/repo"})
	if m.input.Value() != "" || len(m.history) != 0 {
		t.Fatal("editor leaked from A to B")
	}
	m.input.SetValue("draft for B")
	m.history = []string{"B command"}
	m.bindChannel(a)
	if m.input.Value() != "draft for project A" || strings.Join(m.history, ",") != "A command" {
		t.Fatal("A editor was not restored")
	}
	m.loading = true
	m.rememberPrompt(event.Event{Seq: 10, Type: event.InputQueued, Payload: event.MustPayload(event.Input{Kind: event.InputPrompt, Text: "old A command"})})
	if len(m.history) != 1 {
		t.Fatal("replay duplicated old history after cache eviction")
	}
	m.bindChannel(protocol.ChannelInfo{ID: "b", Dir: "/different/repo"})
	if m.input.Value() != "draft for B" || m.history[0] != "B command" {
		t.Fatal("B editor was not restored")
	}
}

func TestStaleResponsesCannotReplaceCurrentChannel(t *testing.T) {
	m := channelModel()
	a, old := m.channelID, m.requestScope()
	m.bindChannel(protocol.ChannelInfo{ID: "b", Dir: "/b"})
	// Include A → B → A: channel IDs alone cannot detect stale A responses.
	m.bindChannel(protocol.ChannelInfo{ID: a, Name: "current", Dir: "/a"})
	for _, msg := range []tea.Msg{
		reconcileMsg{scope: old, res: protocol.ReconcileResult{Channel: protocol.ChannelInfo{ID: a, Name: "stale", Dir: "/wrong"}}},
		reconcileMsg{scope: old, err: errors.New("old failed request")},
		subscribedMsg{scope: old, err: errors.New("old subscribe failed")},
		rolesMsg{scope: old, roles: []protocol.PresetInfo{{Name: "wrong-project-role"}}, quiet: true},
		variantsMsg{scope: old, model: "wrong-model"},
		modelsMsg{scope: old, models: []protocol.ModelInfo{{ID: "wrong-model"}}},
		channelsMsg{scope: old, purpose: channelsPicker, channels: []protocol.ChannelInfo{{ID: "other"}}},
	} {
		next, _ := m.Update(msg)
		m = next.(Model)
	}
	if m.channel.Name != "current" || m.channel.Dir != "/a" || m.fatal != nil || len(m.presets) != 0 || m.ov != nil {
		t.Fatalf("stale response applied: %+v", m.channel)
	}
}

func TestCatalogIncludesOtherDirectoriesWhileIdle(t *testing.T) {
	m := channelModel()
	next, cmd := m.Update(catalogTickMsg{})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("idle catalog polling is not scheduled")
	}
	m.onChannelsListed(channelsMsg{purpose: channelsNav, channels: []protocol.ChannelInfo{
		{ID: m.channelID, Dir: m.channel.Dir}, {ID: "other", Name: "other", Dir: "/other/repo"},
	}})
	if len(m.navChannels) != 1 || m.navChannels[0].ID != "other" {
		t.Fatal("cross-directory channel was hidden")
	}
	m.onChannels(channelsMsg{purpose: channelsPicker, channels: m.navChannels})
	if m.ov.title != "All channels" || !strings.Contains(m.ov.items[0].hint, "/other/repo") {
		t.Fatal("picker lost channel directory")
	}
	if len(filterItems(m.ov.items, "/other/repo")) != 1 {
		t.Fatal("picker cannot search by directory")
	}
}

func TestCatalogKeepsSelectionByChannelID(t *testing.T) {
	m := channelModel()
	m.onChannelsListed(channelsMsg{purpose: channelsNav, channels: []protocol.ChannelInfo{{ID: "b", Name: "b", Dir: "/b"}}})
	m.sbCursor = m.sidebarIndex(sidebarRow{kind: sbOther, k: 0})
	m.onChannelsListed(channelsMsg{purpose: channelsNav, channels: []protocol.ChannelInfo{{ID: "a", Name: "a", Dir: "/a"}, {ID: "b", Name: "b", Dir: "/b"}}})
	r, ok := m.sidebarAt(m.sbCursor)
	if !ok || r.kind != sbOther || m.navChannels[r.k].ID != "b" {
		t.Fatal("catalog insertion moved selection to another channel")
	}
	m.onChannelsListed(channelsMsg{purpose: channelsNav, channels: []protocol.ChannelInfo{{ID: "a", Name: "a", Dir: "/a"}}})
	r, ok = m.sidebarAt(m.sbCursor)
	if !ok || r.kind != sbHere {
		t.Fatal("removed channel selection did not fall back to current channel")
	}
}

func TestChannelFooterShowsDefaultDirectory(t *testing.T) {
	m := channelModel()
	m.superChat = true
	m.channel.Name, m.channel.Dir = "backend", "/work/backend"
	text, spans := m.metaLeft()
	if !strings.Contains(text, "#backend") || !strings.Contains(text, "/work/backend") || len(spans) != 0 {
		t.Fatalf("footer: %q %v", text, spans)
	}
}
