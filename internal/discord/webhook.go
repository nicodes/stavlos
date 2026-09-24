package discord

import (
	"context"
	"errors"

	dg "github.com/bwmarrin/discordgo"
)

// withComponents opts into component handling on webhook execution and edits.
// Interactive controls additionally require a webhook owned by this app.
func withComponents(cfg *dg.RequestConfig) {
	q := cfg.Request.URL.Query()
	q.Set("with_components", "true")
	cfg.Request.URL.RawQuery = q.Encode()
}

// cardFlags marks a prompt card: Components V2 layout, and no link unfurls
// for the agent text it carries.
const cardFlags = dg.MessageFlagsIsComponentsV2 | suppressEmbeds

func (g *gateway) applicationID() string {
	if g.app != "" {
		return g.app
	}
	return g.bot
}

// SendQuestion posts an interactive card using the asking agent's identity.
// Its webhook ID is persisted separately from its credential.
func (g *gateway) SendQuestion(ctx context.Context, channel, agent, text string, components []dg.MessageComponent) (string, string, error) {
	h, err := g.hook(ctx, channel, "stavlos")
	if err != nil {
		return "", "", err
	}
	if h.ApplicationID != g.applicationID() {
		return "", "", errors.New("question webhook is not owned by this Discord application")
	}
	id, err := g.executeWebhook(ctx, channel, "stavlos", &dg.WebhookParams{
		Username: clip(agent, 80), Flags: cardFlags, Components: cardComponents(text, components),
		AllowedMentions: &dg.MessageAllowedMentions{Parse: []dg.AllowedMentionType{}},
	})
	return id, h.ID, err
}

// ownedWebhook obtains the token using bot authentication after a restart.
// The stored webhook ID remains valid even if its display name was changed.
func (g *gateway) ownedWebhook(ctx context.Context, id string) (*dg.Webhook, error) {
	g.mu.Lock()
	for _, h := range g.hooks {
		if h.ID == id && h.ApplicationID == g.applicationID() {
			g.mu.Unlock()
			return h, nil
		}
	}
	g.mu.Unlock()
	h, err := g.s.Webhook(id, dg.WithContext(ctx))
	if err != nil {
		return nil, apiError(err)
	}
	if h.ApplicationID != g.applicationID() || h.Token == "" {
		return nil, errors.New("cannot edit a question webhook owned by another application")
	}
	g.mu.Lock()
	g.hooks["id:"+id] = h
	g.mu.Unlock()
	return h, nil
}

func (g *gateway) EditWebhook(ctx context.Context, webhook, id, text string, components []dg.MessageComponent) error {
	h, err := g.ownedWebhook(ctx, webhook)
	if err != nil {
		return err
	}
	err = g.editCard(ctx, dg.EndpointWebhookMessage(h.ID, h.Token, id), dg.EndpointWebhookToken("", ""), text, components, withComponents)
	if errors.Is(apiError(err), errMissing) {
		g.mu.Lock()
		for key, hook := range g.hooks {
			if hook.ID == webhook {
				delete(g.hooks, key)
			}
		}
		g.mu.Unlock()
	}
	return apiError(err)
}

// Components V2 permits more than five action rows: four options, a custom
// answer and Submit can each have their own row. Text lives in a TextDisplay,
// including after resolution; V2 cannot be removed once enabled on a message.
func cardComponents(text string, controls []dg.MessageComponent) []dg.MessageComponent {
	components := make([]dg.MessageComponent, 0, len(controls)+1)
	if text != "" {
		components = append(components, dg.TextDisplay{Content: text})
	}
	return append(components, controls...)
}

func (g *gateway) editCard(ctx context.Context, endpoint, bucket, text string, controls []dg.MessageComponent, opts ...dg.RequestOption) error {
	// discordgo's WebhookEdit lacks Flags. Use its normal authenticated,
	// rate-limited request path with an explicit payload. Clearing legacy
	// content/embeds also migrates existing cards without posting a new message.
	payload := struct {
		Flags           dg.MessageFlags            `json:"flags"`
		Content         *string                    `json:"content"`
		Embeds          []*dg.MessageEmbed         `json:"embeds"`
		Components      []dg.MessageComponent      `json:"components"`
		AllowedMentions *dg.MessageAllowedMentions `json:"allowed_mentions"`
	}{Flags: cardFlags, Components: cardComponents(text, controls), AllowedMentions: &dg.MessageAllowedMentions{Parse: []dg.AllowedMentionType{}}}
	opts = append(opts, dg.WithContext(ctx))
	_, err := g.s.RequestWithBucketID("PATCH", endpoint, payload, bucket, opts...)
	return apiError(err)
}

func (b *Bridge) editPrompt(ctx context.Context, m promptMessage, text string, components []dg.MessageComponent) error {
	if m.Webhook != "" {
		return b.api.EditWebhook(ctx, m.Webhook, m.Message, text, components)
	}
	return b.api.Edit(ctx, m.Channel, m.Message, text, components)
}
