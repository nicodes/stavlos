// Package discord bridges Stavlos channels to Discord. The daemon is reached
// only through its public socket protocol; Discord I/O lives behind API.
package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	dg "github.com/bwmarrin/discordgo"
)

// API is the Discord boundary. Tests substitute it without a gateway or token.
type API interface {
	Channels(context.Context, string) ([]*dg.Channel, error)
	Create(context.Context, string, dg.GuildChannelCreateData) (*dg.Channel, error)
	Rename(context.Context, string, string) error
	Send(context.Context, string, string, string, []dg.MessageComponent) (string, error)
	Edit(context.Context, string, string, string, []dg.MessageComponent) error
	Message(context.Context, string, string) (*dg.Message, error)
	OwnWebhook(context.Context, string, string) bool
	Typing(context.Context, string) error
	Respond(context.Context, *dg.Interaction, *dg.InteractionResponse) error
	ResponseEdit(context.Context, *dg.Interaction, string, []dg.MessageComponent) error
}

type gateway struct {
	s     *dg.Session
	mu    sync.Mutex
	hooks map[string]*dg.Webhook
	bot   string
}

var errMissing = errors.New("discord message no longer exists")

// Open signs in, registers guild commands and connects the gateway. Handlers
// return promptly: Bridge routes work to bounded channel workers.
func Open(ctx context.Context, token, guild string, makeBridge func(API, string) (*Bridge, error)) (*Bridge, func(), error) {
	s, err := dg.New("Bot " + token)
	if err != nil {
		return nil, nil, errors.New("cannot initialize Discord session")
	}
	s.Client = &http.Client{Timeout: 20 * time.Second}
	s.MaxRestRetries = 0 // a failed send may already have reached Discord
	s.Identify.Intents = dg.IntentsGuilds | dg.IntentsGuildMessages | dg.IntentsMessageContent
	u, err := s.User("@me", dg.WithContext(ctx))
	if err != nil {
		return nil, nil, apiError(err)
	}
	g := &gateway{s: s, hooks: map[string]*dg.Webhook{}, bot: u.ID}
	b, err := makeBridge(g, u.ID)
	if err != nil {
		return nil, nil, err
	}
	s.AddHandler(func(_ *dg.Session, m *dg.MessageCreate) { b.Message(m) })
	s.AddHandler(func(_ *dg.Session, i *dg.InteractionCreate) { b.Interaction(i) })
	app, err := s.Application("@me")
	if err != nil {
		return nil, nil, apiError(err)
	}
	_, err = s.ApplicationCommandBulkOverwrite(app.ID, guild, []*dg.ApplicationCommand{
		{Name: "status", Description: "Show this channel's Stavlos agents"},
		{Name: "cancel", Description: "Cancel an agent's current turn", Options: []*dg.ApplicationCommandOption{
			{Type: dg.ApplicationCommandOptionString, Name: "agent", Description: "Agent name (default main)"},
		}},
	}, dg.WithContext(ctx))
	if err != nil {
		return nil, nil, apiError(err)
	}
	if err := s.Open(); err != nil {
		_ = s.Close()
		return nil, nil, apiError(err)
	}
	return b, func() { _ = s.Close() }, nil
}

// apiError intentionally excludes request URLs and bodies, which can include
// webhook tokens. A status code is sufficient for the operational log.
func apiError(err error) error {
	if err == nil {
		return nil
	}
	var re *dg.RESTError
	if errors.As(err, &re) && re.Response != nil {
		if re.Response.StatusCode == http.StatusNotFound {
			return errMissing
		}
		return fmt.Errorf("discord HTTP %d", re.Response.StatusCode)
	}
	return errors.New("discord request failed (check connection, token and gateway intents)")
}

func (g *gateway) Channels(ctx context.Context, guild string) ([]*dg.Channel, error) {
	r, err := g.s.GuildChannels(guild, dg.WithContext(ctx))
	return r, apiError(err)
}
func (g *gateway) Create(ctx context.Context, guild string, data dg.GuildChannelCreateData) (*dg.Channel, error) {
	r, err := g.s.GuildChannelCreateComplex(guild, data, dg.WithContext(ctx))
	return r, apiError(err)
}
func (g *gateway) Rename(ctx context.Context, id, name string) error {
	_, err := g.s.ChannelEdit(id, &dg.ChannelEdit{Name: name}, dg.WithContext(ctx))
	return apiError(err)
}
func (g *gateway) hook(ctx context.Context, channel string) (*dg.Webhook, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if h := g.hooks[channel]; h != nil {
		return h, nil
	}
	hooks, err := g.s.ChannelWebhooks(channel, dg.WithContext(ctx))
	if err != nil {
		return nil, apiError(err)
	}
	for _, h := range hooks {
		if h.Name == "stavlos" && h.User != nil && h.User.ID == g.bot {
			g.hooks[channel] = h
			return h, nil
		}
	}
	h, err := g.s.WebhookCreate(channel, "stavlos", "", dg.WithContext(ctx))
	if err == nil {
		g.hooks[channel] = h
	}
	return h, apiError(err)
}
func (g *gateway) OwnWebhook(ctx context.Context, channel, id string) bool {
	h, err := g.hook(ctx, channel)
	return err == nil && h.ID == id
}
func (g *gateway) Send(ctx context.Context, channel, user, text string, components []dg.MessageComponent) (string, error) {
	var m *dg.Message
	var err error
	mentions := &dg.MessageAllowedMentions{Parse: []dg.AllowedMentionType{}}
	if user != "" {
		h, e := g.hook(ctx, channel)
		if e != nil {
			return "", e
		}
		m, err = g.s.WebhookExecute(h.ID, h.Token, true, &dg.WebhookParams{
			Username: clip(user, 80), Content: text, AllowedMentions: mentions,
		}, dg.WithContext(ctx))
	} else {
		m, err = g.s.ChannelMessageSendComplex(channel, &dg.MessageSend{Content: text, Components: components, AllowedMentions: mentions}, dg.WithContext(ctx))
	}
	if err != nil {
		if user != "" && errors.Is(apiError(err), errMissing) {
			g.mu.Lock()
			delete(g.hooks, channel)
			g.mu.Unlock()
		}
		return "", apiError(err)
	}
	return m.ID, nil
}
func (g *gateway) Edit(ctx context.Context, channel, id, text string, components []dg.MessageComponent) error {
	if components == nil {
		components = []dg.MessageComponent{}
	}
	_, err := g.s.ChannelMessageEditComplex(&dg.MessageEdit{ID: id, Channel: channel, Content: &text, Components: &components,
		AllowedMentions: &dg.MessageAllowedMentions{Parse: []dg.AllowedMentionType{}}}, dg.WithContext(ctx))
	return apiError(err)
}
func (g *gateway) Message(ctx context.Context, channel, id string) (*dg.Message, error) {
	m, err := g.s.ChannelMessage(channel, id, dg.WithContext(ctx))
	return m, apiError(err)
}
func (g *gateway) Typing(ctx context.Context, channel string) error {
	return apiError(g.s.ChannelTyping(channel, dg.WithContext(ctx)))
}
func (g *gateway) Respond(ctx context.Context, i *dg.Interaction, r *dg.InteractionResponse) error {
	return apiError(g.s.InteractionRespond(i, r, dg.WithContext(ctx)))
}
func (g *gateway) ResponseEdit(ctx context.Context, i *dg.Interaction, text string, components []dg.MessageComponent) error {
	if components == nil {
		components = []dg.MessageComponent{}
	}
	_, err := g.s.InteractionResponseEdit(i, &dg.WebhookEdit{Content: &text, Components: &components, AllowedMentions: &dg.MessageAllowedMentions{Parse: []dg.AllowedMentionType{}}}, dg.WithContext(ctx))
	return apiError(err)
}
