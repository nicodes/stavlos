package discord

import (
	"context"
	"errors"
	"time"

	dg "github.com/bwmarrin/discordgo"
)

type cachedProfile struct {
	name, avatar string
	expires      time.Time
	err          error
}

// SendUser mirrors a terminal post with the operator's Discord profile. Its
// separate webhook distinguishes human speech from agent reply targets.
func (g *gateway) SendUser(ctx context.Context, channel, guild, user, text string) (string, error) {
	p := g.profile(ctx, guild, user)
	if p.err != nil {
		return g.Send(ctx, channel, "", "**You (terminal)**\n"+text, nil)
	}
	return g.sendWebhook(ctx, channel, "stavlos-terminal", p.name, p.avatar, text, nil)
}

func (g *gateway) profile(ctx context.Context, guild, user string) cachedProfile {
	key := guild + ":" + user
	g.mu.Lock()
	p, ok := g.profiles[key]
	g.mu.Unlock()
	if ok && time.Now().Before(p.expires) {
		return p
	}
	p = g.fetchProfile(ctx, guild, user)
	ttl := 5 * time.Minute
	if p.err != nil {
		ttl = time.Minute
	}
	p.expires = time.Now().Add(ttl)
	g.mu.Lock()
	if g.profiles == nil {
		g.profiles = map[string]cachedProfile{}
	}
	g.profiles[key] = p
	g.mu.Unlock()
	return p
}

func (g *gateway) fetchProfile(ctx context.Context, guild, user string) cachedProfile {
	m, err := g.s.GuildMember(guild, user, dg.WithContext(ctx))
	if err == nil && m != nil && m.User != nil {
		m.GuildID = guild
		name := m.Nick
		if name == "" {
			name = displayName(m.User)
		}
		if name != "" {
			return cachedProfile{name: name, avatar: m.AvatarURL("256")}
		}
	}
	u, err := g.s.User(user, dg.WithContext(ctx))
	if err != nil {
		return cachedProfile{err: apiError(err)}
	}
	if u == nil || displayName(u) == "" {
		return cachedProfile{err: errors.New("discord user has no display name")}
	}
	return cachedProfile{name: displayName(u), avatar: u.AvatarURL("256")}
}

func displayName(u *dg.User) string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}
