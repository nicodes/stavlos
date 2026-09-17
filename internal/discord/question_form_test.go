package discord

import (
	"context"
	"strings"
	"testing"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestQuestionUsesOneEditableMessage(t *testing.T) {
	p := protocol.PromptInfo{ID: "q", Channel: "channel", Kind: protocol.PromptQuestion, Escalated: true,
		From: "main", Question: "main asks: duplicate summary", Questions: []protocol.Question{
			{Question: "Favorite genre?", Options: []protocol.QuestionOption{{Label: "Rock", Description: "Guitar-driven."}, {Label: "Pop"}}},
			{Question: "What else?", Options: []protocol.QuestionOption{{Label: "Classical"}}},
		}}
	pending := true
	var answers []string
	replies := 0
	b, w, api := fixture(t, func(_ context.Context, method string, params, out any) error {
		switch method {
		case protocol.MPromptList:
			var ps []protocol.PromptInfo
			if pending {
				ps = []protocol.PromptInfo{p}
			}
			return result(out, protocol.PromptListResult{Prompts: ps})
		case protocol.MPromptReply:
			r := params.(protocol.PromptReplyParams)
			answers, pending = r.Answers, false
			replies++
		case protocol.MChannelPost:
			t.Fatal("custom answer was forwarded as an agent task")
		}
		return result(out, protocol.None{})
	})
	ctx := context.Background()
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	first := api.snapshot()[0]
	if strings.Contains(first.Text, "Custom answer:") {
		t.Fatal("empty custom-answer label appears above the dropdown")
	}
	if len(api.snapshot()) != 1 || strings.Contains(first.Text, "duplicate summary") || !strings.Contains(first.Text, "Favorite genre?") {
		t.Fatalf("initial card: %+v", first)
	}
	if first.User != "main" || first.Webhook == "" || strings.Contains(first.Text, "@main") {
		t.Fatalf("question identity belongs to the sender: %+v", first)
	}
	if b.store.snapshot()[p.ID].Webhook != first.Webhook {
		t.Fatal("question webhook ownership was not persisted")
	}
	if b, ok := first.Components[0].(dg.ActionsRow).Components[0].(dg.Button); !ok || b.Emoji == nil || b.Emoji.Name != "⬜" {
		t.Fatal("question does not show inline option toggles")
	}
	makeInteraction := func(action, arg string) *dg.Interaction {
		i := interaction("q", action)
		i.Data = dg.MessageComponentInteractionData{CustomID: componentID("q", action, arg)}
		i.Message = &dg.Message{ID: first.ID, ChannelID: "discord"}
		return i
	}
	run := func(i *dg.Interaction) {
		t.Helper()
		b.Interaction(&dg.InteractionCreate{Interaction: i})
		if response := api.responses[len(api.responses)-1]; response.Type != dg.InteractionResponseDeferredMessageUpdate {
			t.Fatalf("created a second response: %+v", response)
		}
		if err := w.execute(<-w.queue); err != nil {
			t.Fatal(err)
		}
		if len(api.snapshot()) != 1 || api.snapshot()[0].ID != first.ID {
			t.Fatal("question created another message")
		}
	}
	// An early submit keeps the form usable, with the error on the same card.
	run(makeInteraction("submit", "0"))
	if m := api.snapshot()[0]; !strings.Contains(m.Text, "needs an answer") || len(m.Components) == 0 {
		t.Fatalf("error destroyed form: %+v", m)
	}
	i := makeInteraction("toggle", "0.0")
	// Even an old private form must update the canonical public card.
	i.Message = &dg.Message{ID: "old-private-form", ChannelID: "discord", Flags: dg.MessageFlagsEphemeral}
	i.Data = dg.MessageComponentInteractionData{CustomID: componentID("q", "toggle", "0.0")}
	run(i)
	option := api.snapshot()[0].Components[0].(dg.ActionsRow).Components[0].(dg.Button)
	if option.Emoji.Name != "✅" {
		t.Fatal("selection not visible on the original card")
	}
	// A claim elsewhere disables this same form without losing draft choices.
	p.ClaimedBy = "terminal-client"
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	option = api.snapshot()[0].Components[0].(dg.ActionsRow).Components[0].(dg.Button)
	if !option.Disabled || option.Emoji.Name != "✅" {
		t.Fatal("claim refresh lost selections or left controls enabled")
	}
	p.ClaimedBy = ""
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	run(makeInteraction("next", "0"))
	if !strings.HasPrefix(api.snapshot()[0].Text, "❓ **What else?**") {
		t.Fatal("question navigation did not update the card")
	}
	b.Interaction(&dg.InteractionCreate{Interaction: makeInteraction("text", "1")})
	if api.responses[len(api.responses)-1].Type != dg.InteractionResponseModal || len(w.queue) != 0 {
		t.Fatal("custom button did not open a popup")
	}
	run(customModal("q", "1", "Jazz"))
	if !strings.Contains(api.snapshot()[0].Text, "Custom answer: Jazz") {
		t.Fatal("custom answer not displayed in place")
	}
	// The public card has one shared draft for the authorized operators.
	b.cfg.Approvers = append(b.cfg.Approvers, "second")
	submit := makeInteraction("submit", "1")
	submit.Member.User.ID = "second"
	run(submit)
	if strings.Join(answers, ";") != "Rock;Jazz" || replies != 1 {
		t.Fatalf("submitted %v (%d replies)", answers, replies)
	}
	resolved := api.snapshot()[0]
	if len(resolved.Components) != 0 || !strings.HasPrefix(resolved.Text, "❓ **Favorite genre?**") || strings.Contains(resolved.Text, "Questions answered") || strings.Contains(resolved.Text, "<@second>") {
		t.Fatalf("resolution: %+v", resolved)
	}
	for _, want := range []string{"❓ **Favorite genre?**", "✅ Rock — Guitar-driven.", "⬜ Pop", "❓ **What else?**", "⬜ Classical", "✅ Jazz"} {
		if !strings.Contains(resolved.Text, want) {
			t.Fatalf("submitted summary lacks %q: %s", want, resolved.Text)
		}
	}
	if strings.Contains(resolved.Text, "Custom answer:") || strings.Count(resolved.Text, "✅ Jazz") != 1 {
		t.Fatal("summary should show custom text once as a checked choice")
	}
	run(submit) // a late click cannot overwrite the confirmed resolution
	if api.snapshot()[0].Text != resolved.Text || replies != 1 {
		t.Fatal("late submit replaced or duplicated the answer")
	}
}

func TestLongQuestionStillUsesOneForm(t *testing.T) {
	p := protocol.PromptInfo{ID: "q", Channel: "channel", Kind: protocol.PromptQuestion, Escalated: true,
		Questions: []protocol.Question{{Question: strings.Repeat("music ", 400), Options: []protocol.QuestionOption{{Label: "Jazz"}}}}}
	_, w, api := fixture(t, func(_ context.Context, _ string, _, out any) error {
		return result(out, protocol.PromptListResult{Prompts: []protocol.PromptInfo{p}})
	})
	if err := w.refreshPrompts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ms := api.snapshot(); len(ms) != 1 || units(ms[0].Text) > 2000 || len(ms[0].Components) == 0 {
		t.Fatalf("question split into multiple messages: %+v", ms)
	}
	if text := strings.Split(api.snapshot()[0].Text, "\n")[0]; !strings.HasPrefix(text, "❓ **") || !strings.HasSuffix(text, "**") {
		t.Fatal("long question lost its bold heading")
	}
	if !hasQuestionAction(api.snapshot()[0].Components, "text") {
		t.Fatal("long question hid the custom-answer checkbox")
	}
}
