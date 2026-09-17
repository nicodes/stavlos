package discord

import (
	"context"
	"encoding/json"
	"errors"
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
		if id, question := questionInteraction(t.interaction); question {
			return w.updateQuestionCard(id, err)
		}
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

// updateQuestionCard keeps edits and validation errors on the original card.
// A resolved prompt was already collapsed by answer/syncPrompts; a late click
// must not overwrite its resolution or recreate its controls.
func (w *worker) updateQuestionCard(id string, problem error) error {
	p, ok := w.prompts[id]
	if !ok {
		return nil
	}
	m := w.b.store.snapshot()[id]
	if m.Message == "" {
		return nil
	}
	text, components := w.promptView(p)
	if problem == nil && p.Kind != protocol.PromptQuestion {
		// Large permission subjects were posted as continuation messages with
		// controls on the last part. Keep edits on that same, bounded part.
		parts := splitText(text)
		if len(parts) > 0 {
			text = parts[len(parts)-1]
		}
	}
	if problem != nil {
		note := "\n\n" + clip(problem.Error(), 400)
		text = clipMarkdown(text, 2000-units(note)) + note
	}
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	// Use the canonical bot message, including for clicks on an old private
	// form created by an earlier bridge version.
	if err := w.b.editPrompt(ctx, m, text, components); err != nil {
		if !errors.Is(err, errMissing) {
			return err
		}
		delete(w.shown, id)
		return w.b.store.set(id, promptMessage{})
	}
	return nil
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

func (w *worker) terminalPost(ctx context.Context, text string) error {
	// The protocol identifies the terminal client, not its human operator.
	// With multiple operators there is no unambiguous Discord identity to use.
	if len(w.b.cfg.Approvers) != 1 {
		return w.say(ctx, "", "**You (terminal)**\n"+text)
	}
	for _, part := range splitText(text) {
		if _, err := w.b.api.SendUser(ctx, w.discord, w.b.cfg.Guild, w.b.cfg.Approvers[0], part); err != nil {
			return err
		}
	}
	return nil
}

func (w *worker) mirror(ctx context.Context, e event.Event) error {
	if e.Channel != "" && e.Channel != w.id {
		return nil
	}
	// Reconnect replays outcomes for stored question cards, even before the
	// ordinary chat cutoff. Other historical events remain suppressed.
	if e.Type == event.AskRequested || e.Type == event.AskResolved {
		w.seq = max(w.seq, e.Seq)
		return w.questionEvent(ctx, e)
	}
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
		if p.Dir != nil {
			w.info.Dir = *p.Dir
			return w.say(ctx, "", "Default directory changed to "+*p.Dir+". Mode is ask; remembered approvals cleared.")
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
			// The default main recipient is implicit in Discord. Other targets
			// and multi-recipient messages retain their addressing context.
			if len(p.To) > 0 && (len(p.To) != 1 || p.To[0] != "main") {
				p.Text = "@" + strings.Join(p.To, " @") + " " + p.Text
			}
			return w.terminalPost(ctx, p.Text)
		}
		return w.say(ctx, p.From, channelMessageText(p.To, p.Text))
	case event.AgentSpawned:
		var p event.AgentSpawnedPayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		w.rememberAgent(p.ID, p.Name, p.Role, p.Parent)
		return w.say(ctx, w.agentName(p.Parent), p.Name+" joined ("+p.Role+").")
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
		for n := range w.agents {
			if w.agents[n].ID != e.Agent {
				continue
			}
			if p.Name != nil {
				w.agents[n].Name = *p.Name
			}
			if p.Role != nil {
				w.agents[n].Role = *p.Role
			}
		}
		return w.say(ctx, "", text)
	case event.AgentKilled:
		return w.say(ctx, "", w.agentName(e.Agent)+" stopped.")
	default:
		return nil
	}
}

// Keep identities current between tree polls: a newly created agent can spawn
// another child immediately, and renamed parents should author as their new name.
func (w *worker) rememberAgent(id, name, role, parent string) {
	for i := range w.agents {
		if w.agents[i].ID == id {
			w.agents[i].Name, w.agents[i].Role, w.agents[i].Parent = name, role, parent
			return
		}
	}
	w.agents = append(w.agents, protocol.AgentInfo{ID: id, Channel: w.id, Name: name, Role: role, Parent: parent})
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
	if r := m.MessageReference; r != nil && (r.ChannelID != "" && r.ChannelID != w.discord || r.GuildID != "" && r.GuildID != w.b.cfg.Guild) {
		return w.say(ctx, "", "Reply targets must be in this channel.")
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
	if err := w.settleMissingPrompts(ctx, current, notification); err != nil {
		return err
	}
	for _, p := range r.Prompts {
		if _, ok := current[p.ID]; !ok {
			continue
		}
		encoded, _ := json.Marshal(p)
		if w.shown[p.ID] == string(encoded) {
			continue
		}
		text, components := w.promptView(p)
		parts := []string{text} // questions stay one form, even near the message size limit
		if p.Kind != protocol.PromptQuestion {
			parts = splitText(text)
		}
		m := w.b.store.snapshot()[p.ID]
		if m.Message == "" {
			for _, part := range parts[:len(parts)-1] {
				if err := w.say(ctx, "", part); err != nil {
					return err
				}
			}
			var id, webhook string
			var err error
			if p.Kind == protocol.PromptQuestion || p.Kind == protocol.PromptPermission {
				name := p.From
				if name == "" {
					name = w.agentName(p.Agent)
				}
				if name == "" {
					name = "agent"
				}
				id, webhook, err = w.b.api.SendQuestion(ctx, w.discord, name, parts[len(parts)-1], components)
			} else {
				id, err = w.b.api.Send(ctx, w.discord, "", parts[len(parts)-1], components)
			}
			if err != nil {
				return err
			}
			m = promptMessage{Channel: w.discord, Message: id, Webhook: webhook}
			m = m.withPrompt(p, w.seq)
			if err := w.b.store.set(p.ID, m); err != nil {
				return err
			}
		} else {
			if m.missingPrompt(p) {
				m = m.withPrompt(p, m.Seq)
				if err := w.b.store.set(p.ID, m); err != nil {
					return err
				}
			}
			if err := w.b.editPrompt(ctx, m, parts[len(parts)-1], components); err != nil {
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
		if err := w.b.editPrompt(ctx, m, clip(text, 2000), nil); err != nil && !errors.Is(err, errMissing) {
			return err
		}
		if err := w.b.store.set(id, promptMessage{}); err != nil {
			return err
		}
	}
	delete(w.shown, id)
	delete(w.prompts, id)
	delete(w.drafts, id)
	return nil
}
