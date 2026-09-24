package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestQuestionWebhookIdentityComponentsAndReconnect(t *testing.T) {
	s, err := dg.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	created, edited, lookedUp := 0, 0, 0
	s.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"id":"message"}`
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/channels/channel/webhooks"):
			body = `[{"id":"hook","name":"stavlos","application_id":"app","user":{"id":"bot"},"token":"webhook-secret"}]`
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/webhooks/hook"):
			lookedUp++
			body = `{"id":"hook","name":"renamed","application_id":"app","user":{"id":"bot"},"token":"webhook-secret"}`
		case r.Method == http.MethodPost:
			created++
			if !strings.HasSuffix(r.URL.Path, "/webhooks/hook/webhook-secret") || r.URL.Query().Get("wait") != "true" || r.URL.Query().Get("with_components") != "true" {
				t.Fatal("question execution did not enable webhook components")
			}
			var p struct {
				Username        string                     `json:"username"`
				Content         *string                    `json:"content"`
				Flags           dg.MessageFlags            `json:"flags"`
				Components      []json.RawMessage          `json:"components"`
				AllowedMentions *dg.MessageAllowedMentions `json:"allowed_mentions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Username != "scout" || p.Content != nil || p.Flags != cardFlags || len(p.Components) != 7 || p.AllowedMentions == nil || len(p.AllowedMentions.Parse) != 0 {
				t.Fatalf("question payload: %+v", p)
			}
			assertCardText(t, p.Components[0], "❓ **Which?**")
			assertButtonRows(t, p.Components[1:])
		case r.Method == http.MethodPatch:
			edited++
			if !strings.HasSuffix(r.URL.Path, "/webhooks/hook/webhook-secret/messages/message") || r.URL.Query().Get("with_components") != "true" {
				t.Fatal("question edit used the wrong endpoint")
			}
			var p struct {
				Content         json.RawMessage            `json:"content"`
				Embeds          json.RawMessage            `json:"embeds"`
				Flags           dg.MessageFlags            `json:"flags"`
				Components      []json.RawMessage          `json:"components"`
				AllowedMentions *dg.MessageAllowedMentions `json:"allowed_mentions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Components == nil || p.AllowedMentions == nil || len(p.AllowedMentions.Parse) != 0 || p.Flags != cardFlags || string(p.Content) != "null" || string(p.Embeds) != "null" {
				t.Fatal("edit did not preserve explicit components/mention policy")
			}
			want, n := "updated", 7
			if edited == 2 {
				want, n = "answered", 1
			}
			if len(p.Components) != n {
				t.Fatal("edit retained stale controls or lost question rows")
			}
			assertCardText(t, p.Components[0], want)
			if edited == 1 {
				assertButtonRows(t, p.Components[1:])
			}
		default:
			t.Fatalf("unexpected %s request", r.Method)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	g := &gateway{s: s, bot: "bot", app: "app", hooks: map[string]*dg.Webhook{}}
	ctx := context.Background()
	p := protocol.PromptInfo{ID: "p", Questions: []protocol.Question{{Question: "Which?", Options: []protocol.QuestionOption{{Label: "One"}, {Label: "Two"}, {Label: "Three"}, {Label: "Four"}}}}}
	_, components := questionView(p, newDraft(p))
	if len(components) != 6 || hasQuestionAction(components, "page-next") {
		t.Fatal("four options should fit above Custom answer and Submit without pagination")
	}
	for _, c := range components {
		if len(c.(dg.ActionsRow).Components) != 1 {
			t.Fatal("buttons are not stacked vertically")
		}
	}
	id, hook, err := g.SendQuestion(ctx, "channel", "scout", "❓ **Which?**", components)
	if err != nil || id != "message" || hook != "hook" {
		t.Fatalf("send: %s %s %v", id, hook, err)
	}
	if err := g.EditWebhook(ctx, hook, id, "updated", components); err != nil {
		t.Fatal(err)
	}
	// A new gateway has no cached token, only the persisted webhook ID.
	g = &gateway{s: s, bot: "bot", app: "app", hooks: map[string]*dg.Webhook{}}
	if err := g.EditWebhook(ctx, hook, id, "answered", nil); err != nil {
		t.Fatal(err)
	}
	if created != 1 || edited != 2 || lookedUp != 1 {
		t.Fatalf("requests: %d %d %d", created, edited, lookedUp)
	}
}

func assertCardText(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var text struct {
		Type    dg.ComponentType `json:"type"`
		Content string           `json:"content"`
	}
	if err := json.Unmarshal(raw, &text); err != nil || text.Type != dg.TextDisplayComponent || text.Content != want {
		t.Fatalf("card text: %s, want %q", raw, want)
	}
}

func assertButtonRows(t *testing.T, rows []json.RawMessage) {
	t.Helper()
	for _, raw := range rows {
		var r struct {
			Type       dg.ComponentType `json:"type"`
			Components []dg.Button      `json:"components"`
		}
		if err := json.Unmarshal(raw, &r); err != nil || r.Type != dg.ActionsRowComponent || len(r.Components) != 1 {
			t.Fatalf("expected a standalone button row, without a container: %s", raw)
		}
	}
}

func TestUnboxedChoicesPreserveSelectedDisabledControls(t *testing.T) {
	p := twoQuestions()
	p.Questions = p.Questions[:1]
	p.ClaimedBy = "terminal"
	d := newDraft(p)
	d.selected[0][0], d.text[0] = true, "windy"
	text, controls := questionPrompt(p, d, "discord")
	card := cardComponents(text, controls)
	if len(card) != 5 || card[0].Type() != dg.TextDisplayComponent {
		t.Fatal("expected heading, two options, custom checkbox, then Submit")
	}
	for _, c := range card[1:] {
		if c.Type() != dg.ActionsRowComponent {
			t.Fatal("button still has a surrounding container")
		}
		if !c.(dg.ActionsRow).Components[0].(dg.Button).Disabled {
			t.Fatal("grouping re-enabled a claimed question")
		}
	}
	custom := card[3].(dg.ActionsRow).Components[0].(dg.Button)
	if _, action, _ := parseID(custom.CustomID); action != "clear-text" || custom.Label != "windy" || custom.Emoji.Name != "✅" {
		t.Fatal("grouping lost the selected custom checkbox")
	}
	if _, action, _ := parseID(card[4].(dg.ActionsRow).Components[0].(dg.Button).CustomID); action != "submit" {
		t.Fatal("Submit must follow the selectors")
	}
}

func TestLegacyBotCardAndInteractionFeedbackUseV2(t *testing.T) {
	s, err := dg.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		var p struct {
			Flags      dg.MessageFlags   `json:"flags"`
			Content    json.RawMessage   `json:"content"`
			Components []json.RawMessage `json:"components"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		if r.Method != "PATCH" || p.Flags != cardFlags || string(p.Content) != "null" || len(p.Components) != 1 {
			t.Fatal("legacy card/feedback did not retain V2 format")
		}
		assertCardText(t, p.Components[0], "updated")
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"message"}`)), Request: r}, nil
	})}
	g := &gateway{s: s}
	if err := g.Edit(context.Background(), "channel", "message", "updated", nil); err != nil {
		t.Fatal(err)
	}
	i := interaction("p", "submit")
	i.AppID, i.Token = "app", "interaction-token"
	if err := g.ResponseEdit(context.Background(), i, "updated", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("edits: %d", calls)
	}
}

func TestQuestionWebhookOwnershipIsEnforced(t *testing.T) {
	s, err := dg.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("wrote to another app's webhook")
		}
		body := `{"id":"foreign","application_id":"other-app","token":"foreign-secret"}`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	g := &gateway{s: s, bot: "bot", app: "app", hooks: map[string]*dg.Webhook{
		"channel:stavlos": {ID: "foreign", ApplicationID: "other-app", Token: "foreign-secret"},
	}}
	if _, _, err := g.SendQuestion(context.Background(), "channel", "scout", "question", nil); err == nil {
		t.Fatal("accepted a non-owned question webhook")
	}
	if err := g.EditWebhook(context.Background(), "foreign", "message", "changed", nil); err == nil {
		t.Fatal("edited a non-owned webhook")
	}
}

func TestQuestionWebhookLifecycleUsesStoredOwnership(t *testing.T) {
	p := protocol.PromptInfo{ID: "q", Channel: "channel", Kind: protocol.PromptQuestion, From: "scout", Escalated: true,
		Questions: []protocol.Question{{Question: "Which?", Options: []protocol.QuestionOption{{Label: "One"}}}}}
	pending := []protocol.PromptInfo{p}
	b, w, api := fixture(t, func(_ context.Context, _ string, _, out any) error {
		return result(out, protocol.PromptListResult{Prompts: pending})
	})
	ctx := context.Background()
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	first := api.snapshot()[0]
	saved, err := openStore(b.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.snapshot()[p.ID].Webhook != first.Webhook {
		t.Fatal("restart lost webhook ownership")
	}
	data, err := os.ReadFile(b.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "token") {
		t.Fatal("stored webhook credential")
	}
	b.store = saved
	b.disablePrompts(ctx)
	if m := api.snapshot()[0]; m.User != "scout" || len(m.Components) != 0 || !strings.Contains(m.Text, "disconnected") {
		t.Fatalf("disconnect failed to edit webhook card: %+v", m)
	}
	w.shown = map[string]string{}
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	if m := api.snapshot()[0]; m.ID != first.ID || m.User != "scout" || len(m.Components) == 0 {
		t.Fatal("reconnect changed the question identity")
	}
	pending = nil
	if err := w.refreshPrompts(ctx); err != nil {
		t.Fatal(err)
	}
	if m := api.snapshot()[0]; len(m.Components) != 0 || m.User != "scout" {
		t.Fatal("resolution failed to edit the webhook card")
	}
}
