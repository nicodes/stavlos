package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	dg "github.com/bwmarrin/discordgo"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDiscordAdapterMentionsAndWebhookOwnership(t *testing.T) {
	s, err := dg.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	posts := 0
	s.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"id":"message"}`
		if r.Method == http.MethodGet {
			body = `[{"id":"other","name":"stavlos","user":{"id":"other-bot"},"token":"foreign"},{"id":"owned","name":"stavlos","user":{"id":"bot"},"token":"webhook-secret"}]`
		} else {
			posts++
			var payload map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			var mentions struct {
				Parse []string `json:"parse"`
			}
			if err := json.Unmarshal(payload["allowed_mentions"], &mentions); err != nil || len(mentions.Parse) != 0 {
				t.Fatal("outgoing text could ping users", err)
			}
			if !strings.HasSuffix(r.URL.Path, "/webhooks/owned/webhook-secret") && !strings.HasSuffix(r.URL.Path, "/channels/channel/messages") {
				t.Fatalf("wrong webhook owner or endpoint: %s", r.URL.Path)
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	g := &gateway{s: s, bot: "bot", hooks: map[string]*dg.Webhook{}}
	ctx := context.Background()
	if _, err := g.Send(ctx, "channel", "scout", "@everyone hi", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Send(ctx, "channel", "", "@everyone prompt", []dg.MessageComponent{row(button("p", "allow", "Allow", false))}); err != nil {
		t.Fatal(err)
	}
	if posts != 2 || !g.OwnWebhook(ctx, "channel", "owned") || g.OwnWebhook(ctx, "channel", "other") {
		t.Fatal("webhook ownership or send mismatch")
	}
}

func TestAPIErrorsNeverExposeWebhookTokens(t *testing.T) {
	for _, err := range []error{
		errors.New("Post https://discord.com/webhooks/id/secret-token failed"),
		&dg.RESTError{Response: &http.Response{StatusCode: 403}, ResponseBody: []byte("secret-token")},
	} {
		if e := apiError(err); e == nil || strings.Contains(e.Error(), "secret-token") {
			t.Fatalf("unsafe error: %v", e)
		}
	}
}
