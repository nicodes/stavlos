package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func inlinePermit(id, channel string) protocol.PromptInfo {
	return protocol.PromptInfo{ID: id, Channel: channel, Agent: "a", From: "main", Kind: protocol.PromptPermission, Tool: "shell", Input: json.RawMessage(`{"command":"echo ` + id + `"}`), Prefix: "echo"}
}

func TestInlinePermissionsAreChannelLocalAndResolveInPlace(t *testing.T) {
	m := channelModel()
	m.openChat()
	first, second, away := inlinePermit("first", m.channelID), inlinePermit("second", m.channelID), inlinePermit("away", "other")
	m.input.SetValue("draft task")
	for _, p := range []protocol.PromptInfo{first, second, away} {
		m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionRequested, Prompt: p})
	}
	m.refreshViewport()
	view := stripANSI(m.View())
	if m.focus != focusInput || strings.Contains(view, "╭") || !strings.Contains(view, "echo first") || !strings.Contains(view, "echo second") || strings.Contains(view, "echo away") {
		t.Fatalf("inline permissions: %s", view)
	}
	tr := m.transcript(chatView)
	item, _ := tr.PermissionItem(second.ID)
	m.mouseHover(5, m.itemRows[item].First-m.vp.YOffset)
	_, start, _ := m.permissionCard(&second)
	m.mouseClick(5, m.itemRows[item].First+start+2-m.vp.YOffset) // prefix approval
	if m.promptBusy != second.ID || m.permSel != 2 || m.focus != focusChat {
		t.Fatal("hover did not directly approve the selected permission")
	}
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: second})
	tr.Apply(event.Event{Type: event.AskResolved, Payload: event.MustPayload(event.AskResolvedPayload{ID: second.ID, Outcome: event.AskAnswered, Answer: protocol.AnswerAllowPrefix})})
	m.refreshViewport()
	if tr.Items() != 2 || !strings.Contains(stripANSI(m.View()), "Allowed prefix: echo") || m.findPrompt(first.ID) < 0 || m.input.Value() != "draft task" {
		t.Fatal("resolution replaced another card or lost the chat draft")
	}
}

func TestInlinePermissionReasonAndBoundaryDrafts(t *testing.T) {
	m := channelModel()
	m.openChat()
	p := inlinePermit("boundary", m.channelID)
	p.Dir = "/outside"
	m.upsertPrompt(p)
	m.openPermission(m.inlinePermission())
	m.permSel = 3 // deny
	press(&m, tea.KeyMsg{Type: tea.KeySpace}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("keep these files")})
	if m.permEdit != "deny" || m.promptBusy != "" || !strings.Contains(stripANSI(m.View()), "outside the channel's directories") {
		t.Fatal("denial should open an inline reason before submitting")
	}
	m.bindChannel(protocol.ChannelInfo{ID: "other"})
	m.bindChannel(protocol.ChannelInfo{ID: p.Channel})
	m.loading = false
	m.openPermission(m.inlinePermission())
	m.permSel = 3
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if m.dirInput.Value() != "keep these files" {
		t.Fatal("switching channels lost the denial draft")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	m.permSel = 2 // allow and add another directory
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if m.permEdit != "dir" || m.dirInput.Value() != "/outside" || m.promptBusy != "" {
		t.Fatal("directory edit reused the denial reason or approved prematurely")
	}
	m.dirInput.SetValue("/different")
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.promptBusy != p.ID {
		t.Fatal("directory approval was not sent")
	}
}

func TestInlinePermissionClaimsAndReplay(t *testing.T) {
	m := channelModel()
	p := inlinePermit("claimed", m.channelID)
	p.ClaimedBy = "discord"
	m.upsertPrompt(p)
	m.refreshViewport()
	item, _ := m.transcript("a").PermissionItem(p.ID)
	if r := m.itemRows[item]; r.First != r.Last {
		t.Fatal("agent permission should fold when not highlighted")
	}
	m.setFocus(focusChat)
	if !strings.Contains(stripANSI(m.View()), "Allow once") {
		t.Fatal("highlighted permission did not show its controls")
	}
	m.openPermission(m.inlinePermission())
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if m.promptBusy != "" {
		t.Fatal("another client's claim was ignored")
	}
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: p})
	m.transcript("a").Apply(event.Event{Type: event.AskResolved, Payload: event.MustPayload(event.AskResolvedPayload{ID: p.ID, Outcome: event.AskAnswered, Answer: protocol.AnswerDeny, Reason: "use a safer command"})})
	m.details = true
	m.refreshViewport()
	if view := stripANSI(m.View()); !strings.Contains(view, "Denied · use a safer command") || strings.Contains(view, "Allow once") {
		t.Fatalf("recorded decision: %s", view)
	}
}
