package discord

import (
	"context"
	"strings"
	"testing"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestQuestionRepliesRemainAgentConversation(t *testing.T) {
	p := protocol.PromptInfo{ID: "q", Channel: "channel", Kind: protocol.PromptQuestion, Escalated: true, From: "scout",
		Questions: []protocol.Question{{Question: "Music?", Options: []protocol.QuestionOption{{Label: "Rock"}, {Label: "Jazz"}}}}}
	var posts []string
	_, w, api := fixture(t, func(_ context.Context, method string, arg, out any) error {
		switch method {
		case protocol.MPromptList:
			return result(out, protocol.PromptListResult{Prompts: []protocol.PromptInfo{p}})
		case protocol.MChannelPost:
			posts = append(posts, arg.(protocol.ChannelPostParams).Text)
			return result(out, protocol.ChannelPostResult{})
		default:
			t.Fatalf("custom reply tried to claim/submit: %s", method)
		}
		return nil
	})
	ctx := context.Background()
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	id := api.snapshot()[0].ID
	reply := &dg.Message{ID: "reply-1", Content: "@scout Indie folk", MessageReference: &dg.MessageReference{MessageID: id, ChannelID: w.discord}}
	if err := w.post(ctx, reply); err != nil {
		t.Fatal(err)
	}
	if w.drafts[p.ID].text[0] != "" || len(posts) != 1 || posts[0] != "@scout Indie folk" {
		t.Fatal("question reply was captured as a custom answer")
	}
	if err := w.post(ctx, &dg.Message{ID: "normal", Content: "ordinary agent task"}); err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 || posts[1] != "ordinary agent task" {
		t.Fatal("ordinary chat stopped working")
	}
}

func TestQuestionOptionButtonsToggleIndependently(t *testing.T) {
	p := twoQuestions()
	d := newDraft(p)
	i := interaction(p.ID, "toggle")
	for _, arg := range []string{"0.0", "0.1", "0.0"} {
		if err := updateDraft(d, p, i, "toggle", arg, ""); err != nil {
			t.Fatal(err)
		}
	}
	if d.selected[0][0] || !d.selected[0][1] {
		t.Fatal("buttons did not independently toggle")
	}
	_, controls := questionView(p, d)
	first := controls[0].(dg.ActionsRow).Components[0].(dg.Button)
	second := controls[1].(dg.ActionsRow).Components[0].(dg.Button)
	if first.Emoji.Name != "⬜" || second.Emoji.Name != "✅" {
		t.Fatal("button markers do not match selections")
	}
	if !hasQuestionAction(controls, "text") {
		t.Fatal("question has no custom-answer checkbox")
	}
}

func customModal(id, index, value string) *dg.Interaction {
	i := interaction(id, "text-submit")
	i.Type = dg.InteractionModalSubmit
	i.Message = &dg.Message{ID: "card", ChannelID: "discord"}
	i.Data = dg.ModalSubmitInteractionData{CustomID: componentID(id, "text-submit", index), Components: []dg.MessageComponent{
		&dg.ActionsRow{Components: []dg.MessageComponent{&dg.TextInput{CustomID: "value", Value: value}}},
	}}
	return i
}

func questionButton(t *testing.T, cs []dg.MessageComponent, action string) dg.Button {
	t.Helper()
	for _, c := range cs {
		for _, c := range c.(dg.ActionsRow).Components {
			b := c.(dg.Button)
			if _, a, _ := parseID(b.CustomID); a == action {
				return b
			}
		}
	}
	t.Fatalf("button %s missing", action)
	return dg.Button{}
}

func TestCustomAnswerCheckboxPopupLifecycle(t *testing.T) {
	p := twoQuestions()
	p.Channel, p.Escalated, p.From = "channel", true, "scout"
	p.Questions = p.Questions[:1]
	pending, claims, replies := true, 0, 0
	var submitted protocol.PromptReplyParams
	b, w, api := fixture(t, func(_ context.Context, method string, arg, out any) error {
		switch method {
		case protocol.MPromptList:
			var ps []protocol.PromptInfo
			if pending {
				ps = []protocol.PromptInfo{p}
			}
			return result(out, protocol.PromptListResult{Prompts: ps})
		case protocol.MPromptClaim:
			claims++
		case protocol.MPromptReply:
			replies++
			submitted = arg.(protocol.PromptReplyParams)
			pending = false
		default:
			t.Fatalf("unexpected RPC: %s", method)
		}
		return result(out, protocol.None{})
	})
	if err := w.refreshPrompts(context.Background()); err != nil {
		t.Fatal(err)
	}
	card := api.snapshot()[0]
	click := func(action string) *dg.Interaction {
		button := questionButton(t, api.snapshot()[0].Components, action)
		i := interaction(p.ID, action)
		i.Message = &dg.Message{ID: card.ID, ChannelID: "discord"}
		i.Data = dg.MessageComponentInteractionData{CustomID: button.CustomID}
		return i
	}
	open := func() {
		t.Helper()
		button := questionButton(t, api.snapshot()[0].Components, "text")
		if button.Emoji.Name != "⬜" || button.Label != "Custom answer" {
			t.Fatalf("unchecked button: %+v", button)
		}
		b.Interaction(&dg.InteractionCreate{Interaction: click("text")})
		r := api.responses[len(api.responses)-1]
		if r.Type != dg.InteractionResponseModal || len(w.queue) != 0 {
			t.Fatal("popup was not the immediate response")
		}
		field := r.Data.Components[0].(dg.ActionsRow).Components[0].(dg.TextInput)
		if field.Value != "" || !field.Required || r.Data.CustomID != componentID(p.ID, "text-submit", "0") {
			t.Fatal("popup must start empty and target this question")
		}
	}
	run := func(i *dg.Interaction) {
		t.Helper()
		b.Interaction(&dg.InteractionCreate{Interaction: i})
		if api.responses[len(api.responses)-1].Type != dg.InteractionResponseDeferredMessageUpdate {
			t.Fatal("draft edit created another response")
		}
		if err := w.execute(<-w.queue); err != nil {
			t.Fatal(err)
		}
		if len(api.snapshot()) != 1 || api.snapshot()[0].ID != card.ID {
			t.Fatal("draft edit replaced the question message")
		}
	}
	open() // dismissing the popup sends no submission and changes nothing
	if w.drafts[p.ID].text[0] != "" || claims != 0 || replies != 0 {
		t.Fatal("opening a popup modified the answer")
	}
	open()
	run(customModal(p.ID, "0", "  Outdoors  "))
	checked := questionButton(t, api.snapshot()[0].Components, "clear-text")
	if checked.Emoji.Name != "✅" || checked.Label != "Outdoors" || checked.Style != dg.SuccessButton || claims != 0 || replies != 0 {
		t.Fatal("saving should check the custom option without submitting")
	}
	run(click("toggle")) // normal options remain independent
	run(click("clear-text"))
	if w.drafts[p.ID].text[0] != "" || !w.drafts[p.ID].selected[0][0] {
		t.Fatal("unchecking must clear only the custom answer")
	}
	open()
	run(customModal(p.ID, "0", " \n "))
	if w.drafts[p.ID].text[0] != "" || !hasQuestionAction(api.snapshot()[0].Components, "text") {
		t.Fatal("blank text checked the custom option")
	}
	run(customModal(p.ID, "0", "New value"))
	run(click("submit"))
	if claims != 1 || replies != 1 || len(submitted.Answers) != 1 || submitted.Answers[0] != "People, New value" || submitted.Details[0].Custom != "New value" {
		t.Fatalf("final submission: %+v", submitted)
	}
	if len(api.snapshot()[0].Components) != 0 {
		t.Fatal("submitted card kept its controls")
	}
	run(customModal(p.ID, "0", "late value"))
	if replies != 1 || w.drafts[p.ID] != nil {
		t.Fatal("late popup reopened the question")
	}
}

func TestCustomPopupRejectsStaleAndClaimedQuestions(t *testing.T) {
	p := twoQuestions()
	p.Channel, p.Escalated = "channel", true
	_, w, api := fixture(t, func(_ context.Context, method string, _, out any) error {
		if method != protocol.MPromptList {
			t.Fatalf("popup tried to submit: %s", method)
		}
		return result(out, protocol.PromptListResult{Prompts: []protocol.PromptInfo{p}})
	})
	ctx := context.Background()
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	d := w.drafts[p.ID]
	d.index = 1
	if err := w.execute(work{link: w.link, interaction: customModal(p.ID, "0", "wrong question")}); err != nil {
		t.Fatal(err)
	}
	if d.text[0] != "" || d.text[1] != "" {
		t.Fatal("stale popup changed a different question")
	}
	p.ClaimedBy = "terminal"
	if err := w.execute(work{link: w.link, interaction: customModal(p.ID, "1", "claimed question")}); err != nil {
		t.Fatal(err)
	}
	if d.text[1] != "" || !questionButton(t, api.snapshot()[0].Components, "text").Disabled {
		t.Fatal("popup changed a claimed question")
	}
}

func TestCustomAnswerPreviewKeepsFullValue(t *testing.T) {
	p := twoQuestions()
	d := newDraft(p)
	value := strings.Repeat("🌲 long answer\n", 200)
	if err := updateDraft(d, p, customModal(p.ID, "0", value), "text-submit", "0", value); err != nil {
		t.Fatal(err)
	}
	text, controls := questionView(p, d)
	b := questionButton(t, controls, "clear-text")
	if units(b.Label) > 80 || strings.Contains(b.Label, "\n") || b.Emoji.Name != "✅" || units(text) > 2000 || d.text[0] != strings.TrimSpace(value) {
		t.Fatal("preview exceeded Discord limits or truncated the stored answer")
	}
}
