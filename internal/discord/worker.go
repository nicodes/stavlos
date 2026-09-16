package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	dg "github.com/bwmarrin/discordgo"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

type work struct {
	link        *link
	event       *event.Event
	prompt      *protocol.PromptNotification
	message     *dg.Message
	interaction *dg.Interaction
	snapshot    *protocol.ReconcileResult
	done        chan error
}

type worker struct {
	b            *Bridge
	id, discord  string
	ctx          context.Context
	cancel       context.CancelFunc
	queue        chan work
	ready        *link // Bridge.mu; the following fields belong to run alone
	link         *link
	seq          int64
	info         protocol.ChannelInfo
	agents       []protocol.AgentInfo
	prompts      map[string]protocol.PromptInfo
	drafts       map[string]*draft
	shown        map[string]string
	seenMessages []string // bounded gateway duplicate suppression, retained across daemon reconnects
}

func (w *worker) enqueue(t work) bool {
	select {
	case <-w.ctx.Done():
		return false
	default:
	}
	select {
	case w.queue <- t:
		return true
	default:
		log.Printf("discord: channel %s overloaded; reconnecting", w.id)
		t.link.cancel()
		return false
	}
}

func (w *worker) run() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case t := <-w.queue:
			err := w.execute(t)
			if t.done != nil {
				t.done <- err
			}
			if err != nil && t.link.ctx.Err() == nil {
				log.Printf("discord: channel %s: %v", w.id, err)
				t.link.cancel()
			}
		case <-tick.C:
			if w.link == nil || w.link.ctx.Err() != nil {
				continue
			}
			if err := w.execute(work{link: w.link}); err != nil && w.link.ctx.Err() == nil {
				log.Printf("discord: channel %s refresh: %v", w.id, err)
				w.link.cancel()
			}
		}
	}
}

func (w *worker) execute(t work) error {
	if t.link.ctx.Err() != nil {
		w.abandoned(t)
		return t.link.ctx.Err()
	}
	ctx, cancel := context.WithTimeout(t.link.ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(w.ctx, cancel)
	defer stop()
	if t.snapshot != nil {
		w.link, w.seq, w.info, w.agents = t.link, t.snapshot.Seq, t.snapshot.Channel, t.snapshot.Agents
		w.shown, w.drafts = map[string]string{}, map[string]*draft{}
		return w.refreshPrompts(ctx)
	}
	if w.link != t.link {
		return nil
	}
	switch {
	case t.event != nil:
		return w.mirror(ctx, *t.event)
	case t.prompt != nil:
		return w.syncPrompts(ctx, t.prompt)
	case t.message != nil:
		return w.post(ctx, t.message)
	case t.interaction != nil:
		text, components, err := w.interact(ctx, t.interaction)
		if err != nil {
			text = "Could not complete action: " + err.Error()
			components = nil
		}
		feedback, stop := context.WithTimeout(w.ctx, 5*time.Second)
		defer stop()
		return w.b.api.ResponseEdit(feedback, t.interaction, clip(text, 2000), components)
	default:
		return w.refresh(ctx)
	}
}

func (w *worker) abandoned(t work) {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	if t.message != nil {
		_ = w.say(ctx, "", "Connection changed before this task was sent. Please resend when connected.")
	}
	if t.interaction != nil {
		_ = w.b.api.ResponseEdit(ctx, t.interaction, "Connection changed before this action was sent. Reopen the current prompt and try again.", nil)
	}
}

func (w *worker) refresh(ctx context.Context) error {
	r, err := call(ctx, w.link.rpc, protocol.AgentTree, protocol.AgentTreeParams{Channel: w.id})
	if err != nil {
		return err
	}
	w.agents = r.Agents
	if err := w.refreshPrompts(ctx); err != nil {
		return err
	}
	for _, a := range w.agents {
		if a.State == protocol.AgentRunning {
			return w.b.api.Typing(ctx, w.discord)
		}
	}
	return nil
}

func (w *worker) say(ctx context.Context, user, text string) error {
	for _, part := range splitText(text) {
		if _, err := w.b.api.Send(ctx, w.discord, user, part, nil); err != nil {
			return err
		}
	}
	return nil
}

func (w *worker) mirror(ctx context.Context, e event.Event) error {
	if e.Seq <= w.seq {
		return nil
	}
	w.seq = e.Seq
	switch e.Type {
	case event.ChannelUpdated:
		var p event.ChannelUpdatedPayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		if p.Name != nil {
			w.info.Name = *p.Name
		}
		return nil
	case event.ChatPosted, event.ChatMessage:
		var p event.ChatPayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		if e.Type == event.ChatPosted {
			if p.From == "human:discord" {
				return nil
			}
			return w.say(ctx, "", "**You (terminal)**\n"+p.Text)
		}
		return w.say(ctx, p.From, p.Text)
	case event.AgentSpawned:
		var p event.AgentSpawnedPayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		return w.say(ctx, "", p.Name+" joined ("+p.Role+").")
	case event.AgentUpdated:
		var p event.AgentUpdatedPayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		if p.Name == nil && p.Role == nil {
			return nil
		}
		text := w.agentName(e.Agent) + " updated"
		if p.Name != nil {
			text += " · name: " + *p.Name
		}
		if p.Role != nil {
			text += " · role: " + *p.Role
		}
		return w.say(ctx, "", text)
	case event.AgentKilled:
		return w.say(ctx, "", w.agentName(e.Agent)+" stopped.")
	default:
		return nil
	}
}

func (w *worker) agentName(id string) string {
	for _, a := range w.agents {
		if a.ID == id {
			return a.Name
		}
	}
	return id
}

func (w *worker) post(ctx context.Context, m *dg.Message) error {
	if m.ID != "" {
		for _, id := range w.seenMessages {
			if id == m.ID {
				return nil
			}
		}
		w.seenMessages = append(w.seenMessages, m.ID)
		if len(w.seenMessages) > 1024 {
			w.seenMessages = w.seenMessages[1:]
		}
	}
	text := stripBot(m.Content, w.b.bot)
	if text == "" {
		return w.say(ctx, "", "Send a text task; attachment-only messages are not supported.")
	}
	refs, _ := protocol.Addressees(text)
	if len(refs) == 0 && m.MessageReference != nil && m.MessageReference.MessageID != "" {
		r := m.MessageReference
		if r.ChannelID != "" && r.ChannelID != w.discord {
			return w.say(ctx, "", "Reply targets must be in this channel.")
		}
		target, err := w.b.api.Message(ctx, w.discord, r.MessageID)
		if err != nil {
			return w.say(ctx, "", "Could not read the reply target; address the agent with @name instead.")
		}
		if target.WebhookID != "" && target.Author != nil && w.b.api.OwnWebhook(ctx, w.discord, target.WebhookID) {
			text = "@" + target.Author.Username + " " + text
		}
	}
	_, err := call(ctx, w.link.rpc, protocol.ChannelPost, protocol.ChannelPostParams{Channel: w.id, Text: text})
	if err != nil {
		// No transport retry: the daemon might have committed the post before
		// the response was lost. Returning feedback must survive that loss.
		feedback, cancel := context.WithTimeout(w.ctx, 5*time.Second)
		defer cancel()
		var pe *protocol.Error
		if errors.As(err, &pe) {
			return w.say(feedback, "", "Task rejected: "+pe.Message)
		}
		return w.say(feedback, "", "Task delivery is uncertain after a connection error. Check the TUI before resending.")
	}
	return nil
}

func (w *worker) refreshPrompts(ctx context.Context) error {
	return w.syncPrompts(ctx, nil)
}

func (w *worker) syncPrompts(ctx context.Context, notification *protocol.PromptNotification) error {
	r, err := call(ctx, w.link.rpc, protocol.PromptList, protocol.PromptListParams{Channel: w.id})
	if err != nil {
		return err
	}
	current := map[string]protocol.PromptInfo{}
	for _, p := range r.Prompts {
		if p.Channel == w.id && p.Escalated {
			current[p.ID] = p
		}
	}
	w.prompts = current
	for key, d := range w.drafts {
		if _, ok := current[d.prompt]; !ok {
			delete(w.drafts, key)
		}
	}
	for id, m := range w.b.store.snapshot() {
		if m.Channel != w.discord {
			continue
		}
		if _, ok := current[id]; !ok {
			text := "Resolved or withdrawn at another client (or during restart)."
			if notification != nil && notification.Prompt.ID == id {
				switch notification.Action {
				case protocol.ActionDefaulted:
					text = "Defaulted by the daemon."
				case protocol.ActionWithdrawn:
					text = "Withdrawn."
				default:
				}
			}
			if err := w.finishPrompt(ctx, id, text); err != nil {
				return err
			}
		}
	}
	for _, p := range current {
		encoded, _ := json.Marshal(p)
		if w.shown[p.ID] == string(encoded) {
			continue
		}
		text, components := promptView(p, w.link.id)
		parts := splitText(text)
		m := w.b.store.snapshot()[p.ID]
		if m.Message == "" {
			for _, part := range parts[:len(parts)-1] {
				if err := w.say(ctx, "", part); err != nil {
					return err
				}
			}
			id, err := w.b.api.Send(ctx, w.discord, "", parts[len(parts)-1], components)
			if err != nil {
				return err
			}
			if err := w.b.store.set(p.ID, promptMessage{Channel: w.discord, Message: id}); err != nil {
				return err
			}
		} else {
			if err := w.b.api.Edit(ctx, w.discord, m.Message, parts[len(parts)-1], components); err != nil {
				if !errors.Is(err, errMissing) {
					return err
				}
				if err := w.b.store.set(p.ID, promptMessage{}); err != nil {
					return err
				}
				delete(w.shown, p.ID)
				continue // recreate on the next refresh
			}
		}
		w.shown[p.ID] = string(encoded)
	}
	return nil
}

func (w *worker) finishPrompt(ctx context.Context, id, text string) error {
	m := w.b.store.snapshot()[id]
	if m.Message != "" {
		if err := w.b.api.Edit(ctx, m.Channel, m.Message, clip(text, 2000), nil); err != nil && !errors.Is(err, errMissing) {
			return err
		}
		if err := w.b.store.set(id, promptMessage{}); err != nil {
			return err
		}
	}
	delete(w.shown, id)
	delete(w.prompts, id)
	return nil
}

func (w *worker) status(ctx context.Context) (string, error) {
	r, err := call(ctx, w.link.rpc, protocol.AgentTree, protocol.AgentTreeParams{Channel: w.id})
	if err != nil {
		return "", err
	}
	w.agents = r.Agents
	var text strings.Builder
	cost := 0.0
	for _, a := range r.Agents {
		cost += a.CostUSD
	}
	fmt.Fprintf(&text, "#%s · %d agents · $%.2f · %d prompts waiting here\n", w.info.Name, len(r.Agents), cost, len(w.prompts))
	for _, a := range r.Agents {
		fmt.Fprintf(&text, "%s@%s · %s · turn %d · $%.2f\n", strings.Repeat("  ", min(a.Depth, 10)), a.Name, a.State, a.Turn, a.CostUSD)
		for _, todo := range a.Todos {
			if todo.Status == "in_progress" {
				fmt.Fprintf(&text, "  ▸ %s\n", todo.Text)
			}
		}
	}
	return text.String(), nil
}
