package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicodes/stavlos/internal/protocol"
)

func TestQuestionCardUsesMessageHeader(t *testing.T) {
	m := channelModel()
	p := protocol.PromptInfo{ID: "q", Channel: m.channelID, Agent: "a", From: "writer", Role: "reviewer", Kind: protocol.PromptQuestion,
		Questions: []protocol.Question{{Question: "Which format?", Options: []protocol.QuestionOption{{Label: "Markdown"}}}, {Question: "Which audience?", Options: []protocol.QuestionOption{{Label: "Developers"}}}}}
	m.prompts = []protocol.PromptInfo{p}
	m.setFocus(focusQuestions)
	card, _, _ := m.questionCard(&p)
	if title := stripANSI(card[0]); title != "? @user Which format?" {
		t.Fatalf("header: %q", title)
	}
	lines, _, _ := m.questionLines(&p, 80)
	if body := stripANSI(strings.Join(lines, "\n")); strings.Contains(body, "writer") || strings.Contains(body, "reviewer") || strings.Contains(body, "Which format?") || !strings.Contains(body, "□ Markdown") {
		t.Fatalf("body: %q", body)
	}
	if option := stripANSI(card[1]); option != "  ▸ □ Markdown" {
		t.Fatalf("option indentation: %q", option)
	}
	if custom := stripANSI(card[2]); custom != "    □ Reply with a custom answer…" {
		t.Fatalf("custom checkbox: %q", custom)
	}
	m.q.custom = "windy"
	card, _, _ = m.questionCard(&p)
	if custom := stripANSI(card[2]); custom != "    ■ windy" {
		t.Fatalf("saved custom answer should align with the options: %q", custom)
	}
	m.q.idx = 1
	card, _, _ = m.questionCard(&p)
	if title := stripANSI(card[0]); title != "? @user Which audience?" {
		t.Fatalf("second question header: %q", title)
	}
}

func TestIndividualQuestionSubmitsAndFollowsTheNextPrompt(t *testing.T) {
	m := channelModel()
	p := protocol.PromptInfo{ID: "first", Channel: m.channelID, Agent: "a", Kind: protocol.PromptQuestion, QuestionNumber: 1, QuestionTotal: 2,
		Questions: []protocol.Question{{Question: "First?", Options: []protocol.QuestionOption{{Label: "One"}}}}}
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionRequested, Prompt: p})
	if m.focus != focusInput {
		t.Fatal("question arrival stole focus")
	}
	m.openQuestion(m.currentQuestion())
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.promptBusy != p.ID {
		t.Fatal("first answer was held instead of submitted")
	}
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: p})
	m.onPromptReply(promptReplyMsg{id: p.ID}) // duplicate acknowledgement must not close the sequence
	if m.focus != focusInput {
		t.Fatal("submission did not release the keyboard")
	}
	p.ID, p.QuestionNumber = "second", 2
	p.Questions = []protocol.Question{{Question: "Second?", Options: []protocol.QuestionOption{{Label: "Two"}}}}
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionRequested, Prompt: p})
	m.openQuestion(m.currentQuestion())
	card, _, _ := m.questionCard(&p)
	if m.q.id != p.ID || !strings.Contains(stripANSI(card[0]), "Second?") || strings.Contains(stripANSI(card[0]), "2/2") {
		t.Fatal("independent question should show its text without a counter")
	}
	if m.q.marks[0] {
		t.Fatal("first question's selection leaked into second")
	}
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.promptBusy != p.ID {
		t.Fatal("second answer was not submitted")
	}
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: p})
	if m.focus == focusQuestions {
		t.Fatal("dialog stayed open after the last answer")
	}
}
