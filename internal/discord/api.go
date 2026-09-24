// Package discord bridges Stavlos channels to Discord. The daemon is reached
// only through its public socket protocol; Discord I/O lives behind API.
package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nicodes/stavlos/internal/httpx"
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
	SendQuestion(context.Context, string, string, string, []dg.MessageComponent) (string, string, error) // message ID, webhook ID
	SendUser(context.Context, string, string, string, string) (string, error)
	Edit(context.Context, string, string, string, []dg.MessageComponent) error
	EditWebhook(context.Context, string, string, string, []dg.MessageComponent) error
	Message(context.Context, string, string) (*dg.Message, error)
	OwnWebhook(context.Context, string, string) bool
	Typing(context.Context, string) error
	Respond(context.Context, *dg.Interaction, *dg.InteractionResponse) error
	ResponseEdit(context.Context, *dg.Interaction, string, []dg.MessageComponent) error
}

type gateway struct {
	s        *dg.Session
	mu       sync.Mutex // guards the caches only, never a Discord request
	hooks    map[string]*dg.Webhook
	filling  map[string]chan struct{} // mu; webhook lookups in flight, closed when stored
	bot      string
	app      string
	profiles map[string]cachedProfile // mu; cached per guild/user
}

// suppressEmbeds keeps Discord from unfurling links in agent and transcript
// text: a URL an agent writes must not be fetched without a permission prompt.
const suppressEmbeds = dg.MessageFlagsSuppressEmbeds

var errMissing = errors.New("discord message no longer exists")

// Open signs in, registers guild commands and connects the gateway. Handlers
// return promptly: Bridge routes work to bounded channel workers.
func Open(ctx context.Context, token, guild string, makeBridge func(API, string) (*Bridge, error)) (*Bridge, func(), error) {
	s, err := dg.New("Bot " + token)
	if err != nil {
		return nil, nil, errors.New("cannot initialize Discord session")
	}
	s.Client = httpx.New(httpx.Options{Timeout: 20 * time.Second})
	s.MaxRestRetries = 0 // a failed send may already have reached Discord
	s.Identify.Intents = dg.IntentsGuilds | dg.IntentsGuildMessages | dg.IntentsMessageContent
	u, err := s.User("@me", dg.WithContext(ctx))
	if err != nil {
		return nil, nil, apiError(err)
	}
	guildInfo, err := s.Guild(guild, dg.WithContext(ctx))
	if err != nil {
		if errors.Is(apiError(err), errMissing) {
			return nil, nil, errors.New("discord server not found; check the guild ID and invite the bot")
		}
		return nil, nil, apiError(err)
	}
	g := &gateway{s: s, hooks: map[string]*dg.Webhook{}, bot: u.ID}
	b, err := makeBridge(g, u.ID)
	if err != nil {
		return nil, nil, err
	}
	b.mu.Lock()
	b.botName, b.guildName = u.Username, guildInfo.Name
	b.mu.Unlock()
	s.AddHandler(func(_ *dg.Session, _ *dg.Connect) { b.gatewayState(true) })
	s.AddHandler(func(_ *dg.Session, _ *dg.Disconnect) { b.gatewayState(false) })
	s.AddHandler(func(_ *dg.Session, m *dg.MessageCreate) { b.Message(m) })
	s.AddHandler(func(_ *dg.Session, i *dg.InteractionCreate) { b.Interaction(i) })
	app, err := application(ctx, s)
	if err != nil {
		return nil, nil, apiError(err)
	}
	g.app = app.ID
	_, err = s.ApplicationCommandBulkOverwrite(app.ID, guild, applicationCommands(), dg.WithContext(ctx))
	if err != nil {
		return nil, nil, apiError(err)
	}
	closeGateway, err := openGateway(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	return b, closeGateway, nil
}

// application is Session.Application("@me") with a context: discordgo's own
// method takes no request options, so it would outlive a cancelled Open.
func application(ctx context.Context, s *dg.Session) (*dg.Application, error) {
	body, err := s.RequestWithBucketID(http.MethodGet, dg.EndpointOAuth2Application("@me"), nil, dg.EndpointOAuth2Application(""), dg.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	var app dg.Application
	if err := json.Unmarshal(body, &app); err != nil {
		return nil, err
	}
	return &app, nil
}

// apiError intentionally excludes request URLs and bodies, which can include
// webhook tokens. A status code is sufficient for the operational log.
func apiError(err error) error {
	if err == nil {
		return nil
	}
	var re *dg.RESTError
	if errors.As(err, &re) && re.Response != nil {
		if re.Response.StatusCode == http.StatusUnauthorized {
			return errors.New("discord rejected the bot token (HTTP 401)")
		}
		if re.Response.StatusCode == http.StatusForbidden {
			return errors.New("discord denied access; check server and bot permissions (HTTP 403)")
		}
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

// hook returns the bot's webhook for a channel, creating it once. Only the
// cache is read or written under the lock: a slow lookup for one channel must
// not stall posts to every other channel. A per-key guard makes concurrent
// misses share one lookup rather than create duplicate webhooks.
func (g *gateway) hook(ctx context.Context, channel, name string) (*dg.Webhook, error) {
	key := channel + ":" + name
	var wait chan struct{}
	for {
		g.mu.Lock()
		if h := g.hooks[key]; h != nil {
			g.mu.Unlock()
			return h, nil
		}
		inFlight := g.filling[key]
		if inFlight == nil {
			wait = make(chan struct{})
			if g.filling == nil {
				g.filling = map[string]chan struct{}{}
			}
			g.filling[key] = wait
			g.mu.Unlock()
			break
		}
		g.mu.Unlock()
		select {
		case <-inFlight:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	h, err := g.lookupHook(ctx, channel, name)
	g.mu.Lock()
	if err == nil {
		g.hooks[key] = h
	}
	delete(g.filling, key)
	close(wait)
	g.mu.Unlock()
	return h, err
}

func (g *gateway) lookupHook(ctx context.Context, channel, name string) (*dg.Webhook, error) {
	hooks, err := g.s.ChannelWebhooks(channel, dg.WithContext(ctx))
	if err != nil {
		return nil, apiError(err)
	}
	for _, h := range hooks {
		if h.Name == name && h.User != nil && h.User.ID == g.bot {
			return h, nil
		}
	}
	h, err := g.s.WebhookCreate(channel, name, "", dg.WithContext(ctx))
	if err != nil {
		return nil, apiError(err)
	}
	return h, nil
}
func (g *gateway) OwnWebhook(ctx context.Context, channel, id string) bool {
	// Only agent speech is addressable by replying. Human terminal posts use
	// a separate webhook, even if the operator's display name is an agent name.
	h, err := g.hook(ctx, channel, "stavlos")
	return err == nil && h.ID == id
}
func (g *gateway) Send(ctx context.Context, channel, user, text string, components []dg.MessageComponent) (string, error) {
	if user != "" {
		return g.sendWebhook(ctx, channel, "stavlos", user, "", text, components)
	}
	m, err := g.s.ChannelMessageSendComplex(channel, &dg.MessageSend{Content: text, Components: components, Flags: suppressEmbeds,
		AllowedMentions: &dg.MessageAllowedMentions{Parse: []dg.AllowedMentionType{}}}, dg.WithContext(ctx))
	if err != nil {
		return "", apiError(err)
	}
	return m.ID, nil
}

func (g *gateway) sendWebhook(ctx context.Context, channel, hook, name, avatar, text string, components []dg.MessageComponent) (string, error) {
	return g.executeWebhook(ctx, channel, hook, &dg.WebhookParams{
		Username: clip(name, 80), AvatarURL: avatar, Content: text, Components: components, Flags: suppressEmbeds,
		AllowedMentions: &dg.MessageAllowedMentions{Parse: []dg.AllowedMentionType{}},
	})
}

func (g *gateway) executeWebhook(ctx context.Context, channel, hook string, payload *dg.WebhookParams) (string, error) {
	h, err := g.hook(ctx, channel, hook)
	if err != nil {
		return "", err
	}
	opts := []dg.RequestOption{dg.WithContext(ctx)}
	if payload.Components != nil {
		opts = append(opts, withComponents)
	}
	m, err := g.s.WebhookExecute(h.ID, h.Token, true, payload, opts...)
	if err != nil {
		if errors.Is(apiError(err), errMissing) {
			g.mu.Lock()
			delete(g.hooks, channel+":"+hook)
			g.mu.Unlock()
		}
		return "", apiError(err)
	}
	return m.ID, nil
}
func (g *gateway) Edit(ctx context.Context, channel, id, text string, components []dg.MessageComponent) error {
	// Bot-owned prompt cards from older versions can also acquire the taller
	// layout. All subsequent edits (including resolution) stay in V2 format.
	return g.editCard(ctx, dg.EndpointChannelMessage(channel, id), dg.EndpointChannelMessage(channel, ""), text, components)
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
	if _, question := questionInteraction(i); question || i.Message != nil && i.Message.Flags&dg.MessageFlagsIsComponentsV2 != 0 {
		return g.editCard(ctx, dg.EndpointWebhookMessage(i.AppID, i.Token, "@original"), dg.EndpointWebhookToken("", ""), text, components)
	}
	if components == nil {
		components = []dg.MessageComponent{}
	}
	_, err := g.s.InteractionResponseEdit(i, &dg.WebhookEdit{Content: &text, Components: &components, AllowedMentions: &dg.MessageAllowedMentions{Parse: []dg.AllowedMentionType{}}}, dg.WithContext(ctx))
	return apiError(err)
}
