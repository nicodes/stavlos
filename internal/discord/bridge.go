package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/pkg/client"
)

type rpc interface {
	Call(context.Context, string, any, any) error
}
type link struct {
	rpc    rpc
	id     string
	ctx    context.Context
	cancel context.CancelFunc
}

func call[P, R any](ctx context.Context, c rpc, m protocol.Method[P, R], p P) (R, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var r R
	err := c.Call(ctx, m.Name, p, &r)
	return r, err
}

// Bridge owns routing; each channel worker exclusively owns its UI state.
// mu protects routing only, never a Discord request or a daemon call.
type Bridge struct {
	cfg                                 config.Discord
	api                                 API
	bot                                 string
	store                               *promptStore
	mu                                  sync.RWMutex
	live                                *link
	workers                             map[string]*worker // Stavlos ID
	byDiscord                           map[string]*worker
	wg                                  sync.WaitGroup
	gatewayConnected                    bool
	botName, guildName, connectionError string
	gatewayLost                         chan struct{}
	gatewayOnce                         sync.Once
}

// BridgeStatus is the live connection view used by the daemon's controller.
type BridgeStatus struct {
	Gateway, Daemon       bool
	Bot, GuildName, Error string
	Channels              int
}

// Status snapshots metadata only; it never exposes configuration credentials.
func (b *Bridge) Status() BridgeStatus {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s := BridgeStatus{Gateway: b.gatewayConnected, Bot: b.botName, GuildName: b.guildName, Error: b.connectionError}
	s.Daemon = b.live != nil && b.live.ctx.Err() == nil
	if s.Daemon {
		for _, w := range b.workers {
			if w.ready == b.live {
				s.Channels++
			}
		}
	}
	return s
}

func (b *Bridge) gatewayState(connected bool) {
	b.mu.Lock()
	b.gatewayConnected = connected
	b.mu.Unlock()
	if !connected && b.gatewayLost != nil {
		b.gatewayOnce.Do(func() { close(b.gatewayLost) })
	}
}

// New prepares a bridge using already resolved configuration.
func New(cfg config.Discord, api API, bot, statePath string) (*Bridge, error) {
	s, err := openStore(statePath)
	if err != nil {
		return nil, err
	}
	return &Bridge{cfg: cfg, api: api, bot: bot, store: s, workers: map[string]*worker{}, byDiscord: map[string]*worker{}, gatewayLost: make(chan struct{})}, nil
}

// Run maintains the daemon connection until ctx ends. A connection loss starts
// a fresh snapshot subscription, never a replay of offline chat.
func (b *Bridge) Run(ctx context.Context, socket string) error {
	defer func() {
		b.mu.Lock()
		for _, w := range b.workers {
			w.cancel()
		}
		b.mu.Unlock()
		b.wg.Wait()
	}()
	delay := time.Second
	for ctx.Err() == nil {
		c, err := client.DialContext(ctx, socket)
		if err == nil {
			started := time.Now()
			err = b.connected(ctx, c)
			_ = c.Close()
			if time.Since(started) > 30*time.Second {
				delay = time.Second
			}
		}
		if ctx.Err() != nil {
			break
		}
		b.mu.Lock()
		b.connectionError = err.Error()
		b.mu.Unlock()
		log.Printf("discord: daemon connection interrupted: %v; retry in %s", err, delay)
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
		delay = min(30*time.Second, delay*2)
	}
	return nil
}

func (b *Bridge) connected(ctx context.Context, c *client.Client) error {
	a, err := call(ctx, c, protocol.Attach, protocol.AttachParams{Client: "discord", Tier: protocol.TierFallback})
	if err != nil {
		return err
	}
	lctx, cancel := context.WithCancel(ctx)
	l := &link{rpc: c, id: a.ClientID, ctx: lctx, cancel: cancel}
	b.mu.Lock()
	b.live = l
	b.connectionError = ""
	b.mu.Unlock()
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			select {
			case <-lctx.Done():
				return
			case <-c.Closed():
				cancel()
				return
			case n := <-c.Notifications:
				b.notification(l, n)
			}
		}
	}()
	defer func() {
		cancel()
		_ = c.Close()
		<-pumpDone
		b.mu.Lock()
		b.live = nil
		for _, w := range b.workers {
			w.ready = nil
		}
		b.mu.Unlock()
		b.disablePrompts(context.WithoutCancel(ctx))
	}()
	log.Printf("discord: attached to daemon as fallback")
	for {
		if err := b.discover(ctx, l); err != nil {
			return err
		}
		select {
		case <-lctx.Done():
			return errors.New("connection closed or worker overloaded")
		case <-time.After(5 * time.Second):
		}
	}
}

func (b *Bridge) notification(l *link, r protocol.Response) {
	var id string
	t := work{link: l}
	switch r.Method {
	case protocol.NEvent:
		var n protocol.EventNotification
		if json.Unmarshal(r.Params, &n) != nil {
			return
		}
		id, t.event = n.Event.Channel, &n.Event
	case protocol.NPrompt:
		var n protocol.PromptNotification
		if json.Unmarshal(r.Params, &n) != nil {
			return
		}
		id, t.prompt = n.Prompt.Channel, &n
	default:
		return // token deltas never enter the reliable queue
	}
	b.mu.RLock()
	w := b.workers[id]
	b.mu.RUnlock()
	if w != nil {
		w.enqueue(t)
	}
}

func (b *Bridge) allowed(dir string) bool {
	p, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	p, err = filepath.EvalSymlinks(p)
	return err == nil && slices.Contains(b.cfg.Dirs, p)
}

func (b *Bridge) discover(ctx context.Context, l *link) error {
	list, err := call(l.ctx, l.rpc, protocol.ChannelList, protocol.ChannelListParams{})
	if err != nil {
		return err
	}
	channels, err := b.api.Channels(l.ctx, b.cfg.Guild)
	if err != nil {
		return err
	}
	category, err := b.category(l.ctx, channels)
	if err != nil {
		return err
	}
	mapped, err := channelMarkers(channels, category)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, info := range list.Channels {
		if info.Archived || !b.allowed(info.Dir) {
			continue
		}
		keep[info.ID] = true
		if err := b.bind(ctx, l, info, category, mapped[info.ID]); err != nil {
			return err
		}
	}
	b.mu.Lock()
	var removed []*worker
	for id, w := range b.workers {
		if !keep[id] {
			delete(b.workers, id)
			delete(b.byDiscord, w.discord)
			w.cancel()
			removed = append(removed, w)
		}
	}
	b.mu.Unlock()
	for _, w := range removed {
		_, _ = call(l.ctx, l.rpc, protocol.Unsubscribe, protocol.SubscribeParams{Channel: w.id})
	}
	return b.retireUnmapped(l.ctx)
}

func (b *Bridge) category(ctx context.Context, channels []*dg.Channel) (string, error) {
	id := ""
	for _, ch := range channels {
		if ch.Type != dg.ChannelTypeGuildCategory || ch.Name != b.cfg.Category {
			continue
		}
		if id != "" {
			return "", errors.New("multiple Discord categories match configured category")
		}
		id = ch.ID
	}
	if id != "" {
		return id, nil
	}
	ch, err := b.api.Create(ctx, b.cfg.Guild, dg.GuildChannelCreateData{Name: b.cfg.Category, Type: dg.ChannelTypeGuildCategory})
	if err != nil {
		return "", err
	}
	return ch.ID, nil
}

func channelMarkers(channels []*dg.Channel, category string) (map[string]*dg.Channel, error) {
	out := map[string]*dg.Channel{}
	for _, ch := range channels {
		if ch.ParentID != category || ch.Type != dg.ChannelTypeGuildText {
			continue
		}
		marked := false
		for _, line := range strings.Split(ch.Topic, "\n") {
			if !strings.HasPrefix(line, "stavlos-channel:") {
				continue
			}
			id := strings.TrimSpace(strings.TrimPrefix(line, "stavlos-channel:"))
			if id == "" {
				continue
			}
			if marked {
				return nil, fmt.Errorf("discord channel %s has multiple Stavlos markers", ch.ID)
			}
			marked = true
			if out[id] != nil {
				return nil, fmt.Errorf("duplicate Discord topic marker for %s", id)
			}
			out[id] = ch
		}
	}
	return out, nil
}

func (b *Bridge) bind(ctx context.Context, l *link, info protocol.ChannelInfo, category string, ch *dg.Channel) error {
	var err error
	if ch == nil {
		ch, err = b.api.Create(l.ctx, b.cfg.Guild, dg.GuildChannelCreateData{Name: clip(info.Name, 100), Type: dg.ChannelTypeGuildText,
			ParentID: category, Topic: "stavlos-channel:" + info.ID})
		if err != nil {
			return err
		}
	} else if ch.Name != info.Name {
		if err := b.api.Rename(l.ctx, ch.ID, clip(info.Name, 100)); err != nil {
			return err
		}
	}
	b.mu.Lock()
	w := b.workers[info.ID]
	if w != nil && w.discord != ch.ID {
		w.cancel()
		delete(b.byDiscord, w.discord)
		w = nil
	}
	if w == nil {
		wctx, cancel := context.WithCancel(ctx)
		w = &worker{b: b, id: info.ID, discord: ch.ID, ctx: wctx, cancel: cancel, queue: make(chan work, 256), prompts: map[string]protocol.PromptInfo{}, drafts: map[string]*draft{}, shown: map[string]string{}}
		b.workers[info.ID], b.byDiscord[ch.ID] = w, w
		b.wg.Add(1)
		go func() { defer b.wg.Done(); w.run() }()
		log.Printf("discord: mapped channel %s to %s", info.ID, ch.ID)
	}
	ready := w.ready == l
	b.mu.Unlock()
	if ready {
		return nil
	}
	if _, err := call(l.ctx, l.rpc, protocol.ChannelResume, protocol.ChannelRef{Channel: info.ID}); err != nil {
		return err
	}
	r, err := call(l.ctx, l.rpc, protocol.Reconcile, protocol.ChannelRef{Channel: info.ID})
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	if !w.enqueue(work{link: l, snapshot: &r, done: done}) {
		return errors.New("worker overloaded")
	}
	select {
	case err = <-done:
	case <-l.ctx.Done():
		return l.ctx.Err()
	}
	if err != nil {
		return err
	}
	if _, err := call(l.ctx, l.rpc, protocol.Subscribe, protocol.SubscribeParams{Channel: info.ID, From: b.questionReplayFrom(ch.ID, r.Seq)}); err != nil {
		return err
	}
	b.mu.Lock()
	w.ready = l
	b.mu.Unlock()
	return nil
}

func (b *Bridge) disablePrompts(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, p := range b.store.snapshot() {
		_ = b.editPrompt(ctx, p, "Stavlos disconnected — waiting to reconcile this prompt.", nil)
	}
}
func (b *Bridge) retireUnmapped(ctx context.Context) error {
	for id, p := range b.store.snapshot() {
		b.mu.RLock()
		w := b.byDiscord[p.Channel]
		b.mu.RUnlock()
		if w == nil {
			if err := b.editPrompt(ctx, p, "This channel is no longer bridged.", nil); err != nil && !errors.Is(err, errMissing) {
				return err
			}
			if err := b.store.set(id, promptMessage{}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Bridge) route(guild, channel, user string) (*worker, *link) {
	if guild != b.cfg.Guild || !slices.Contains(b.cfg.Approvers, user) {
		return nil, nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	w := b.byDiscord[channel]
	if w == nil {
		return nil, nil
	}
	l := b.live
	if l == nil || l.ctx.Err() != nil || w.ready != l {
		return w, nil
	}
	return w, l
}

// Message accepts only human posts in a mapped channel from an operator.
func (b *Bridge) Message(m *dg.MessageCreate) {
	if m == nil || m.Message == nil || m.Author == nil || m.Author.Bot || m.WebhookID != "" {
		return
	}
	w, l := b.route(m.GuildID, m.ChannelID, m.Author.ID)
	if w == nil {
		return
	}
	if l != nil && w.enqueue(work{link: l, message: m.Message}) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = b.api.Send(ctx, m.ChannelID, "", "Stavlos is offline or busy; this task was not queued. Please try again when connected.", nil)
}

// Interaction acknowledges within Discord's deadline, before queuing RPC or
// REST work. A modal must itself be the initial response, never a follow-up.
func (b *Bridge) Interaction(ic *dg.InteractionCreate) {
	if ic == nil || ic.Interaction == nil {
		return
	}
	i := ic.Interaction
	user := interactionUser(i)
	w, l := b.route(i.GuildID, i.ChannelID, user)
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	if w == nil || l == nil {
		_ = b.api.Respond(ctx, i, ephemeral("Not authorized here, or Stavlos is offline."))
		return
	}
	if r := modalResponse(i); r != nil {
		_ = b.api.Respond(ctx, i, r)
		return
	}
	response := &dg.InteractionResponse{Type: dg.InteractionResponseDeferredChannelMessageWithSource, Data: &dg.InteractionResponseData{Flags: dg.MessageFlagsEphemeral}}
	if _, question := questionInteraction(i); question {
		response = &dg.InteractionResponse{Type: dg.InteractionResponseDeferredMessageUpdate}
	}
	if _, _, ok := statusPage(i); ok {
		response = &dg.InteractionResponse{Type: dg.InteractionResponseDeferredMessageUpdate}
	}
	if err := b.api.Respond(ctx, i, response); err != nil {
		return
	}
	if !w.enqueue(work{link: l, interaction: i}) {
		_ = b.api.ResponseEdit(ctx, i, "Bridge overloaded; no action was sent. Try again after reconnect.", nil)
	}
}

func interactionUser(i *dg.Interaction) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}
func ephemeral(text string) *dg.InteractionResponse {
	return &dg.InteractionResponse{Type: dg.InteractionResponseChannelMessageWithSource, Data: &dg.InteractionResponseData{Content: text, Flags: dg.MessageFlagsEphemeral, AllowedMentions: &dg.MessageAllowedMentions{Parse: []dg.AllowedMentionType{}}}}
}
