package discord

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
			var flags dg.MessageFlags
			if err := json.Unmarshal(payload["flags"], &flags); err != nil || flags&dg.MessageFlagsSuppressEmbeds == 0 {
				t.Fatal("outgoing text could unfurl links", err)
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

// TestWebhookLookupDoesNotStallOtherChannels: one channel's slow webhook
// lookup must not hold the gateway lock, so a post to a channel whose webhook
// is already cached goes out meanwhile. Concurrent misses for the same channel
// share one lookup rather than create duplicate webhooks.
func TestWebhookLookupDoesNotStallOtherChannels(t *testing.T) {
	s, err := dg.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	release, slowStarted := make(chan struct{}), make(chan struct{}, 2)
	lookups := 0
	var mu sync.Mutex
	s.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"id":"message"}`
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/channels/slow/webhooks") {
			mu.Lock()
			lookups++
			mu.Unlock()
			slowStarted <- struct{}{}
			<-release
			body = `[{"id":"slow-hook","name":"stavlos","user":{"id":"bot"},"token":"slow-secret"}]`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	g := &gateway{s: s, bot: "bot", hooks: map[string]*dg.Webhook{
		"fast:stavlos": {ID: "fast-hook", Token: "fast-secret", User: &dg.User{ID: "bot"}},
	}}
	ctx := context.Background()
	slow := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := g.Send(ctx, "slow", "scout", "hello", nil)
			slow <- err
		}()
	}
	<-slowStarted // the lookup is in flight and blocked
	fast := make(chan error, 1)
	go func() {
		_, err := g.Send(ctx, "fast", "scout", "meanwhile", nil)
		fast <- err
	}()
	select {
	case err := <-fast:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cached channel waited on another channel's webhook lookup")
	}
	close(release)
	for range 2 {
		if err := <-slow; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if lookups != 1 || g.hooks["slow:stavlos"] == nil || len(g.filling) != 0 {
		t.Fatalf("lookups %d, cached %v, in flight %d", lookups, g.hooks["slow:stavlos"] != nil, len(g.filling))
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
