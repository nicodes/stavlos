package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	dg "github.com/bwmarrin/discordgo"
)

func TestTerminalWebhookProfileAndReplyIdentity(t *testing.T) {
	s, err := dg.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	profileRequests := 0
	var posts []dg.WebhookParams
	s.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"id":"message"}`
		switch {
		case strings.HasSuffix(r.URL.Path, "/guilds/guild/members/123"):
			profileRequests++
			body = `{"nick":"Server Nick","avatar":"server-avatar","user":{"id":"123","username":"account","global_name":"Global Name","avatar":"global-avatar"}}`
		case strings.HasSuffix(r.URL.Path, "/channels/channel/webhooks"):
			body = `[{"id":"agent-hook","name":"stavlos","user":{"id":"bot"},"token":"agent-secret"},{"id":"human-hook","name":"stavlos-terminal","user":{"id":"bot"},"token":"human-secret"}]`
		case r.Method == http.MethodPost:
			if !strings.HasSuffix(r.URL.Path, "/webhooks/human-hook/human-secret") {
				t.Fatalf("terminal post used wrong webhook: %s", r.URL.Path)
			}
			var p dg.WebhookParams
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			posts = append(posts, p)
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	g := &gateway{s: s, bot: "bot", hooks: map[string]*dg.Webhook{}}
	ctx := context.Background()
	for range 2 {
		if _, err := g.SendUser(ctx, "channel", "guild", "123", "@everyone original text"); err != nil {
			t.Fatal(err)
		}
	}
	if profileRequests != 1 || len(posts) != 2 {
		t.Fatalf("profile requests: %d, posts: %d", profileRequests, len(posts))
	}
	for _, p := range posts {
		if p.Username != "Server Nick" || !strings.Contains(p.AvatarURL, "/guilds/guild/users/123/avatars/server-avatar") {
			t.Fatalf("wrong identity: %+v", p)
		}
		if p.Content != "@everyone original text" || p.AllowedMentions == nil || len(p.AllowedMentions.Parse) != 0 {
			t.Fatalf("text or mentions changed: %+v", p)
		}
	}
	if g.OwnWebhook(ctx, "channel", "human-hook") || !g.OwnWebhook(ctx, "channel", "agent-hook") {
		t.Fatal("human replies were treated as agent replies")
	}
	g.mu.Lock()
	p := g.profiles["guild:123"]
	p.expires = time.Now().Add(-time.Second)
	g.profiles["guild:123"] = p
	g.mu.Unlock()
	if _, err := g.SendUser(ctx, "channel", "guild", "123", "refresh"); err != nil {
		t.Fatal(err)
	}
	if profileRequests != 2 {
		t.Fatal("expired profile was not refreshed")
	}
}

func TestDiscordProfileFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name, member, user, want, avatar string
		memberStatus, userStatus         int
	}{
		{name: "global display name", member: `{"user":{"id":"123","username":"account","global_name":"Display","avatar":"global-avatar"}}`, memberStatus: 200, want: "Display", avatar: "/avatars/123/global-avatar"},
		{name: "username and default avatar", member: `{"user":{"id":"123","username":"account","discriminator":"0"}}`, memberStatus: 200, want: "account", avatar: "/embed/avatars/"},
		{name: "member unavailable", memberStatus: 404, user: `{"id":"123","username":"account","global_name":"Display","avatar":"global-avatar"}`, userStatus: 200, want: "Display", avatar: "/avatars/123/global-avatar"},
		{name: "profile unavailable", memberStatus: 404, userStatus: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := dg.New("Bot test-token")
			if err != nil {
				t.Fatal(err)
			}
			var delivered string
			lookups := 0
			s.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				status, body := 200, `{"id":"message"}`
				switch {
				case strings.Contains(r.URL.Path, "/members/"):
					status, body = tc.memberStatus, tc.member
					lookups++
				case strings.HasSuffix(r.URL.Path, "/users/123"):
					status, body = tc.userStatus, tc.user
					lookups++
				case strings.HasSuffix(r.URL.Path, "/channels/channel/messages"):
					var m dg.MessageSend
					if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
						t.Fatal(err)
					}
					delivered = m.Content
				default:
					t.Fatalf("unexpected request: %s", r.URL.Path)
				}
				if status != 200 {
					body = `{"message":"Unknown User","code":10013}`
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			g := &gateway{s: s, bot: "bot", hooks: map[string]*dg.Webhook{}}
			p := g.profile(context.Background(), "guild", "123")
			if tc.want != "" {
				if p.err != nil || p.name != tc.want || !strings.Contains(p.avatar, tc.avatar) {
					t.Fatalf("profile: %+v", p)
				}
				return
			}
			if p.err == nil {
				t.Fatal("missing profile succeeded")
			}
			if _, err := g.SendUser(context.Background(), "channel", "guild", "123", "still delivered"); err != nil {
				t.Fatal(err)
			}
			if delivered != "**You (terminal)**\nstill delivered" || lookups != 2 {
				t.Fatalf("fallback: %q, %d lookups", delivered, lookups)
			}
		})
	}
}
