package discord

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestPermissionUsesAgentCardAndInlineDecision(t *testing.T) {
	p := protocol.PromptInfo{ID: "p", Channel: "channel", From: "main", Kind: protocol.PromptPermission, Tool: "shell", Input: json.RawMessage(`{"command":"go test ./..."}`), Prefix: "go test", Escalated: true}
	var sent protocol.PromptReplyParams
	var methods []string
	b, w, api := fixture(t, func(_ context.Context, method string, arg, out any) error {
		if method == protocol.MPromptList {
			return result(out, protocol.PromptListResult{Prompts: []protocol.PromptInfo{p}})
		}
		methods = append(methods, method)
		if method == protocol.MPromptReply {
			sent = arg.(protocol.PromptReplyParams)
		}
		return result(out, protocol.None{})
	})
	if err := w.refreshPrompts(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := api.snapshot()[0]
	if original.User != "main" || original.Webhook == "" || !strings.Contains(original.Text, "go test ./...") || len(original.Components) != 1 || len(original.Components[0].(dg.ActionsRow).Components) != 5 {
		t.Fatalf("permission card: %+v", original)
	}
	i := interaction(p.ID, "prefix")
	b.Interaction(&dg.InteractionCreate{Interaction: i})
	if api.responses[0].Type != dg.InteractionResponseDeferredMessageUpdate {
		t.Fatal("permission answer created a separate private response")
	}
	if err := w.execute(<-w.queue); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "prompt.claim,prompt.reply" || sent.ID != p.ID || sent.Answer != protocol.AnswerAllowPrefix {
		t.Fatalf("decision: %+v %v", sent, methods)
	}
	after := api.snapshot()[0]
	if after.ID != original.ID || len(after.Components) != 0 || !strings.Contains(after.Text, "go test ./...") || !strings.Contains(after.Text, "Allowed prefix: go test") {
		t.Fatalf("result: %+v", after)
	}
}

func TestTerminalPermissionResultSurvivesReconnect(t *testing.T) {
	p := protocol.PromptInfo{ID: "p", Channel: "channel", From: "main", Kind: protocol.PromptPermission, Tool: "shell", Input: json.RawMessage(`{"command":"rm cache"}`), Escalated: true}
	pending := true
	b, w, api := fixture(t, func(_ context.Context, _ string, _, out any) error {
		var ps []protocol.PromptInfo
		if pending {
			ps = []protocol.PromptInfo{p}
		}
		return result(out, protocol.PromptListResult{Prompts: ps})
	})
	ctx := context.Background()
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	original := api.snapshot()[0]
	pending = false
	if err := w.syncPrompts(ctx, &protocol.PromptNotification{Action: protocol.ActionAnswered, Prompt: p}); err != nil {
		t.Fatal(err)
	}
	if b.store.snapshot()[p.ID].Permission == nil {
		t.Fatal("missing prompt discarded the permission's recorded context")
	}
	stored, err := openStore(b.store.path)
	if err != nil {
		t.Fatal(err)
	}
	b.store = stored
	if err := w.execute(work{link: w.link, snapshot: &protocol.ReconcileResult{Seq: 100}}); err != nil {
		t.Fatal(err)
	}
	if b.questionReplayFrom(w.discord, 100) != 1 {
		t.Fatal("permission result was not scheduled for recovery")
	}
	if err := w.mirror(ctx, event.Event{Channel: p.Channel, Seq: 12, Type: event.AskResolved, Payload: event.MustPayload(event.AskResolvedPayload{ID: p.ID, Outcome: event.AskAnswered, Answer: protocol.AnswerDeny, Reason: "keep the files"})}); err != nil {
		t.Fatal(err)
	}
	after := api.snapshot()[0]
	if len(api.snapshot()) != 1 || after.ID != original.ID || after.User != "main" || len(after.Components) != 0 || !strings.Contains(after.Text, "rm cache") || !strings.Contains(after.Text, "Denied · keep the files") || len(b.store.snapshot()) != 0 {
		t.Fatalf("terminal decision: %+v", after)
	}
}

func TestPermissionButtonsAndReadableCommand(t *testing.T) {
	for _, boundary := range []bool{false, true} {
		p := protocol.PromptInfo{ID: "p", Kind: protocol.PromptPermission, Tool: "shell", Prefix: "pwd", Input: json.RawMessage(`{"command":"pwd\nls -la","timeout":120}`)}
		want := "❗ **Shell**\n```sh\npwd\nls -la\n```"
		count := 5
		if boundary {
			p.Dir = "/outside"
			want = "❗ **Shell** /outside\n```sh\npwd\nls -la\n```"
			count = 4
		}
		text, controls := promptView(p, "client")
		if text != want || len(controls) != 1 {
			t.Fatalf("permission layout: %q %+v", text, controls)
		}
		buttons := controls[0].(dg.ActionsRow).Components
		if len(buttons) != count {
			t.Fatalf("button count: %d", len(buttons))
		}
		if buttons[count-2].(dg.Button).Label != "Deny" || buttons[count-1].(dg.Button).Label != "Deny with reason" {
			t.Fatal("separate denial choices missing")
		}
		if hasQuestionAction(controls, "dir") {
			t.Fatal("alternate-directory button still present")
		}
		if boundary {
			if buttons[1].(dg.Button).Label != "Allow and add directory" || hasQuestionAction(controls, "prefix") {
				t.Fatal("boundary approval choices changed")
			}
		}
	}
}

func TestDiscordDenyImmediatelyOrWithReason(t *testing.T) {
	for _, reason := range []string{"", "keep the files"} {
		t.Run("reason="+reason, func(t *testing.T) {
			p := protocol.PromptInfo{ID: "p", Channel: "channel", From: "main", Kind: protocol.PromptPermission, Tool: "shell", Input: json.RawMessage(`{"command":"rm cache"}`), Escalated: true}
			var sent protocol.PromptReplyParams
			var calls []string
			b, w, api := fixture(t, func(_ context.Context, method string, arg, out any) error {
				if method == protocol.MPromptList {
					return result(out, protocol.PromptListResult{Prompts: []protocol.PromptInfo{p}})
				}
				calls = append(calls, method)
				if method == protocol.MPromptReply {
					sent = arg.(protocol.PromptReplyParams)
				}
				return result(out, protocol.None{})
			})
			if err := w.refreshPrompts(context.Background()); err != nil {
				t.Fatal(err)
			}
			original := api.snapshot()[0]
			i := interaction(p.ID, "deny-now")
			if reason != "" {
				b.Interaction(&dg.InteractionCreate{Interaction: interaction(p.ID, "deny")})
				r := api.responses[len(api.responses)-1]
				if r.Type != dg.InteractionResponseModal || len(w.queue) != 0 || len(calls) != 0 {
					t.Fatal("Deny with reason must open a popup without claiming or denying")
				}
				if !r.Data.Components[0].(dg.ActionsRow).Components[0].(dg.TextInput).Required {
					t.Fatal("reason field must be required")
				}
				i.Type = dg.InteractionModalSubmit
				i.Data = dg.ModalSubmitInteractionData{CustomID: componentID(p.ID, "deny-submit", ""), Components: []dg.MessageComponent{&dg.ActionsRow{Components: []dg.MessageComponent{&dg.TextInput{CustomID: "value", Value: "  " + reason + "  "}}}}}
			}
			b.Interaction(&dg.InteractionCreate{Interaction: i})
			if api.responses[len(api.responses)-1].Type != dg.InteractionResponseDeferredMessageUpdate {
				t.Fatal("denial should update the permission message")
			}
			if err := w.execute(<-w.queue); err != nil {
				t.Fatal(err)
			}
			if sent.ID != p.ID || sent.Answer != protocol.AnswerDeny || sent.Reason != reason || strings.Join(calls, ",") != "prompt.claim,prompt.reply" {
				t.Fatalf("denial: %+v %v", sent, calls)
			}
			after := api.snapshot()[0]
			if after.ID != original.ID || len(after.Components) != 0 || !strings.Contains(after.Text, "\nrm cache") || !strings.Contains(after.Text, "Denied") || reason != "" && !strings.Contains(after.Text, reason) {
				t.Fatalf("result: %+v", after)
			}
		})
	}
	if _, err := permissionAnswer(protocol.PromptInfo{Kind: protocol.PromptPermission}, "deny-submit", " \n "); err == nil {
		t.Fatal("empty reason should use the immediate Deny button")
	}
}

func TestLongPermissionCommandKeepsDecisionOutsideCode(t *testing.T) {
	input, err := json.Marshal(map[string]string{"command": "echo " + strings.Repeat("🌬", 1500)})
	if err != nil {
		t.Fatal(err)
	}
	p := protocol.PromptInfo{Kind: protocol.PromptPermission, Tool: "shell", Input: input}
	text := recordedPermissionResult(p, event.AskResolvedPayload{Outcome: event.AskAnswered, Answer: protocol.AnswerDeny, Reason: "keep the files"})
	if units(text) > 2000 || !strings.HasPrefix(text, "❗ **Shell** ❌ **Denied · keep the files**\n```sh\necho ") || !strings.HasSuffix(text, "\n```") || strings.Count(text, "```") != 2 {
		t.Fatalf("permission code/result formatting: %s", text)
	}
}
