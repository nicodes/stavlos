package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func inlineQuestion(id, channel, agent string) protocol.PromptInfo {
	return protocol.PromptInfo{ID: id, Channel: channel, Agent: agent, From: "writer", Role: "reviewer", Kind: protocol.PromptQuestion, QuestionNumber: 1, QuestionTotal: 1,
		Questions: []protocol.Question{{Question: "Which format for " + id + "?", Options: []protocol.QuestionOption{{Label: "Markdown"}, {Label: "Text"}}}}}
}

func TestInlineQuestionsCanBeAnsweredLastFirst(t *testing.T) {
	m := channelModel()
	m.openChat()
	first := inlineQuestion("first", m.channelID, "a")
	second := inlineQuestion("second", m.channelID, "a")
	first.QuestionTotal, second.QuestionTotal, second.QuestionNumber = 2, 2, 2
	for _, p := range []protocol.PromptInfo{first, second} {
		m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionRequested, Prompt: p})
	}
	m.refreshViewport()
	if view := stripANSI(m.View()); !strings.Contains(view, "for first") || !strings.Contains(view, "for second") {
		t.Fatal("not all questions are visible")
	}
	m.openQuestion(&second)
	press(&m, tea.KeyMsg{Type: tea.KeySpace}, tea.KeyMsg{Type: tea.KeyEnter})
	if m.promptBusy != second.ID {
		t.Fatal("could not submit the second question first")
	}
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: second})
	if len(m.prompts) != 1 || m.currentQuestion().ID != first.ID {
		t.Fatal("second answer removed or changed the first question")
	}
	m.openQuestion(m.currentQuestion())
	if m.q.marks[0] {
		t.Fatal("second answer leaked into first question")
	}
	press(&m, tea.KeyMsg{Type: tea.KeySpace}, tea.KeyMsg{Type: tea.KeyEnter})
	if m.promptBusy != first.ID {
		t.Fatal("remaining first question could not be submitted")
	}
}

func TestInlineQuestionsStayInTheirChannelAndKeepDrafts(t *testing.T) {
	m := channelModel()
	m.openChat()
	first := inlineQuestion("first", m.channelID, "a")
	second := inlineQuestion("second", m.channelID, "b")
	away := inlineQuestion("away", "other", "remote")
	m.input.SetValue("unfinished chat post")
	for _, p := range []protocol.PromptInfo{first, second, away} {
		m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionRequested, Prompt: p})
	}
	m.refreshViewport()
	view := stripANSI(m.View())
	if m.focus != focusInput || strings.Contains(view, "╭") || strings.Contains(view, "for away") || !strings.Contains(view, "for first") || !strings.Contains(view, "for second") {
		t.Fatalf("channel-local inline messages:\n%s", view)
	}
	for _, f := range m.tabOrder() {
		if f == focusQuestions {
			t.Fatal("global questions tab remains")
		}
	}
	m.openQuestion(&m.prompts[0])
	press(&m, tea.KeyMsg{Type: tea.KeySpace}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("with diagrams")})
	m.openQuestion(&m.prompts[1])
	if m.q.custom != "" || m.q.marks[0] {
		t.Fatal("draft leaked into another card")
	}
	m.openQuestion(&m.prompts[0])
	if m.q.custom != "with diagrams" || !m.q.marks[0] {
		t.Fatal("card draft was lost")
	}
	m.bindChannel(protocol.ChannelInfo{ID: "other"})
	m.loading = false
	m.refreshViewport()
	if m.currentQuestion().ID != "away" || strings.Contains(stripANSI(m.View()), "for first") {
		t.Fatal("channel switch leaked questions")
	}
	m.bindChannel(protocol.ChannelInfo{ID: first.Channel})
	m.loading = false
	m.openQuestion(m.currentQuestion())
	if m.q.id != first.ID || m.q.custom != "with diagrams" || !m.q.marks[0] || m.input.Value() != "unfinished chat post" {
		t.Fatal("switch lost drafts")
	}
}

func TestInlineQuestionKeyboardAndScrolledMouse(t *testing.T) {
	m := channelModel()
	m.openChat()
	m.width, m.height = 50, 17
	p := inlineQuestion("long", m.channelID, "a")
	p.Questions[0].Question = strings.Repeat("A long wrapped question. ", 24)
	m.upsertPrompt(p)
	m.layout()
	m.refreshViewport()
	m.setFocus(focusChat)
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.focus != focusQuestions {
		t.Fatal("chat selection did not open inline controls")
	}
	press(&m, tea.KeyMsg{Type: tea.KeySpace}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("custom")}, tea.KeyMsg{Type: tea.KeyEnter})
	if m.promptBusy != "" || m.q.typing || m.q.custom != "custom" {
		t.Fatal("custom reply should be staged before Submit")
	}
	item, _ := m.transcript(chatView).QuestionItem(p.ID)
	_, start, count := m.questionCard(&p)
	y := m.itemRows[item].First + start + count - 1 - m.vp.YOffset
	if y < 0 || y >= m.vp.Height || m.vp.YOffset == 0 {
		t.Fatalf("Submit is off screen: y=%d offset=%d", y, m.vp.YOffset)
	}
	m.mouseClick(5, y)
	if m.promptBusy != p.ID || len(m.q.details) != 1 || len(m.q.details[0].Selected) != 1 || m.q.details[0].Custom != "custom" {
		t.Fatal("scrolled Submit did not preserve picks and custom reply")
	}
}

func TestQuestionReplayKeepsPositionAndRemoteResult(t *testing.T) {
	m := channelModel()
	m.openChat()
	m.loading = true
	m.replayTo = 3
	p := inlineQuestion("q", m.channelID, "a")
	m.upsertPrompt(p)
	m.refreshViewport() // pending snapshot must not put the card ahead of history
	ev := func(seq int64, typ event.Type, payload any) {
		m.applyEvent(event.Event{Seq: seq, Channel: m.channelID, Agent: "a", Type: typ, Payload: event.MustPayload(payload)})
	}
	ev(1, event.ChatMessage, event.ChatPayload{From: "writer", Text: "before question"})
	ev(2, event.AskRequested, event.AskRequestedPayload{ID: p.ID, Kind: "question", From: p.From, Role: p.Role, Questions: p.Questions})
	ev(3, event.ChatMessage, event.ChatPayload{From: "writer", Text: "after question"})
	m.refreshViewport()
	item, _ := m.transcript(chatView).QuestionItem(p.ID)
	if item != 1 {
		t.Fatalf("snapshot reordered history: item=%d", item)
	}
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: p})
	ev(4, event.AskResolved, event.AskResolvedPayload{ID: p.ID, Outcome: event.AskAnswered, Answer: "Text", Details: []event.QuestionAnswer{{Selected: []int{1}}}})
	m.refreshViewport()
	view := stripANSI(m.View())
	if m.transcript(chatView).Items() != 3 || !strings.Contains(view, "    ■ Text") || !strings.Contains(view, "? @writer: Which format") || strings.Contains(view, "Submit answer") || !inOrder(view, "before question", "Which format", "after question") {
		t.Fatalf("remote result:\n%s", view)
	}
}

func TestClaimedQuestionCannotBeEditedOrSubmitted(t *testing.T) {
	m := channelModel()
	p := inlineQuestion("claimed", m.channelID, "a")
	p.ClaimedBy = "discord"
	m.upsertPrompt(p)
	m.openQuestion(m.currentQuestion())
	press(&m, tea.KeyMsg{Type: tea.KeySpace}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ignored")}, tea.KeyMsg{Type: tea.KeyEnter})
	if m.q.marks[0] || m.q.typing || m.promptBusy != "" {
		t.Fatal("another client's claimed question was edited")
	}
	if !strings.Contains(stripANSI(m.View()), "Claimed by another client") {
		t.Fatal("claim not shown")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusInput {
		t.Fatal("cannot leave claimed question")
	}
}

func TestAgentQuestionsFoldLikeOtherTranscriptItems(t *testing.T) {
	m := channelModel()
	p := inlineQuestion("folded", m.channelID, "a")
	p.Questions[0].Question = strings.Repeat("A long question with context.\n", 8)
	m.upsertPrompt(p)
	m.refreshViewport()
	item, _ := m.transcript("a").QuestionItem(p.ID)
	if r := m.itemRows[item]; r.Last != r.First {
		t.Fatalf("unfocused agent question should be one line: %+v", r)
	}
	if view := stripANSI(m.View()); strings.Contains(view, "□ Markdown") || strings.Contains(view, "Submit answer") {
		t.Fatal("folded question exposes its controls")
	}
	m.setFocus(focusChat)
	if view := stripANSI(m.View()); !strings.Contains(view, "Submit answer") || !strings.Contains(view, "□ Markdown") {
		t.Fatal("highlighted pending question must show all controls without expansion")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if view := stripANSI(m.View()); !strings.Contains(view, "□ Markdown") || !strings.Contains(view, "Submit answer") || m.focus != focusQuestions {
		t.Fatal("expanding did not expose the interactive question")
	}
	press(&m, tea.KeyMsg{Type: tea.KeySpace}, tea.KeyMsg{Type: tea.KeyEsc})
	if r := m.itemRows[item]; r.Last != r.First {
		t.Fatal("leaving question controls did not fold the agent item")
	}
	m.openQuestion(m.currentQuestion())
	if !m.q.marks[0] {
		t.Fatal("folding lost the answer draft")
	}
	m.setFocus(focusInput)
	m.openChat()
	m.refreshViewport()
	if view := stripANSI(m.View()); !strings.Contains(view, "■ Markdown") || !strings.Contains(view, "Submit answer") {
		t.Fatal("channel chat should retain full inline questions")
	}
	m.openAgent(0)
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: p})
	m.transcript("a").Apply(event.Event{Type: event.AskResolved, Payload: event.MustPayload(event.AskResolvedPayload{ID: p.ID, Outcome: event.AskAnswered, Details: []event.QuestionAnswer{{Selected: []int{0}}}})})
	m.refreshViewport()
	if r := m.itemRows[item]; r.Last != r.First {
		t.Fatal("recorded answer did not fold to one line")
	}
	m.setFocus(focusChat)
	m.toggleItem()
	if !strings.Contains(stripANSI(m.View()), "■ Markdown") {
		t.Fatal("expanded result lost the submitted answer")
	}
}

func TestPendingQuestionHoverIsInteractiveAndResultUsesNormalPreview(t *testing.T) {
	m := channelModel()
	m.width, m.height = 100, 40
	m.layout()
	p := inlineQuestion("hover", m.channelID, "a")
	p.Questions[0].Options = append(p.Questions[0].Options, protocol.QuestionOption{Label: "HTML"}, protocol.QuestionOption{Label: "PDF"})
	m.upsertPrompt(p)
	m.refreshViewport()
	item, _ := m.transcript("a").QuestionItem(p.ID)
	height := func() int { r := m.itemRows[item]; return r.Last - r.First + 1 }
	if height() != 1 {
		t.Fatal("idle question should be folded")
	}
	m.mouseHover(5, m.itemRows[item].First-m.vp.YOffset)
	if m.focus != focusChat || !m.hoverFocus || !strings.Contains(stripANSI(m.View()), "Submit answer") || height() <= 3 {
		t.Fatal("hover should show the complete pending question")
	}
	m.mouseClick(5, m.itemRows[item].First-m.vp.YOffset)
	if m.focus != focusChat || m.q.typing || m.promptBusy != "" {
		t.Fatal("question header click should have no action")
	}
	clickControl := func(n int) {
		t.Helper()
		_, start, _ := m.questionCard(&p)
		m.mouseClick(5, m.itemRows[item].First+start+n-m.vp.YOffset)
	}
	offset := m.vp.YOffset
	clickControl(0)
	if !m.q.marks[0] || m.focus != focusChat || !m.hoverFocus || m.vp.YOffset != offset {
		t.Fatal("first option click should toggle without pinning or scrolling the card")
	}
	clickControl(4) // custom answer
	if !m.q.typing || !m.hoverFocus {
		t.Fatal("custom field must retain its hover origin")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("custom")}, tea.KeyMsg{Type: tea.KeyEnter})
	m.mouseHover(5, m.vp.Height+1)
	if m.focus != focusInput || m.hoverFocus || height() != 1 || m.q.custom != "custom" || !m.q.marks[0] {
		t.Fatal("leaving hover should fold and preserve the draft")
	}
	m.mouseHover(5, m.itemRows[item].First-m.vp.YOffset)
	clickControl(5) // submit
	if m.promptBusy != p.ID || m.focus != focusChat || !m.hoverFocus {
		t.Fatal("Submit should work directly from hover")
	}
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: p})
	m.transcript("a").Apply(event.Event{Type: event.AskResolved, Payload: event.MustPayload(event.AskResolvedPayload{ID: p.ID, Outcome: event.AskAnswered, Details: []event.QuestionAnswer{{Selected: []int{0}, Custom: "custom"}}})})
	m.refreshViewport()
	if height() != 3 || strings.Contains(stripANSI(m.View()), "Submit answer") {
		t.Fatalf("answered hover should be a normal three-line preview, got %d rows", height())
	}
	m.mouseClick(5, m.itemRows[item].First-m.vp.YOffset)
	if height() <= 3 || !strings.Contains(stripANSI(m.View()), "    ■ custom") {
		t.Fatal("click should expand the full response")
	}
	m.mouseClick(5, m.itemRows[item].First-m.vp.YOffset)
	if height() != 3 {
		t.Fatal("second click should return to the three-line preview")
	}
	m.mouseHover(5, m.vp.Height+1)
	if height() != 1 {
		t.Fatal("response should fold back to one line when hover leaves")
	}
}
