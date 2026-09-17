package discord

import (
	"context"
	"errors"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func (m promptMessage) isQuestion() bool {
	return m.Permission == nil && (m.Webhook != "" || len(m.Questions) > 0)
}

func promptQuestions(p protocol.PromptInfo) []protocol.Question {
	if len(p.Questions) > 0 {
		return p.Questions
	}
	return []protocol.Question{{Question: p.Question}}
}

// Outstanding question and permission cards need their outcomes replayed. worker.seq
// still suppresses old chat messages, spawns and other notifications.
func (b *Bridge) questionReplayFrom(channel string, seq int64) int64 {
	from := seq + 1
	for _, m := range b.store.snapshot() {
		if m.Channel == channel && (m.isQuestion() || m.Permission != nil) {
			from = min(from, max(1, m.Seq+1))
		}
	}
	return from
}

func (w *worker) settleMissingPrompts(ctx context.Context, current map[string]protocol.PromptInfo, n *protocol.PromptNotification) error {
	for id, m := range w.b.store.snapshot() {
		if m.Channel != w.discord {
			continue
		}
		if _, ok := current[id]; ok {
			continue
		}
		if m.isQuestion() || m.Permission != nil {
			if err := w.awaitQuestionResult(ctx, id, m, n); err != nil {
				return err
			}
			continue
		}
		text := "Resolved or withdrawn at another client (or during restart)."
		if n != nil && n.Prompt.ID == id {
			switch n.Action {
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
	return nil
}

// A prompt can disappear from prompt.list before its ask.resolved is recorded.
// Retain its message mapping and question instead of discarding the answer's
// destination. Clear the controls while the durable result catches up.
func (w *worker) awaitQuestionResult(ctx context.Context, id string, m promptMessage, n *protocol.PromptNotification) error {
	if m.Permission != nil {
		return w.awaitPermissionResult(ctx, id, m, n)
	}
	p := protocol.PromptInfo{Questions: m.Questions}
	if n != nil && n.Prompt.ID == id {
		if len(n.Prompt.Questions) > 0 || n.Prompt.Question != "" {
			p.Questions = promptQuestions(n.Prompt)
		}
		if n.Action == protocol.ActionWithdrawn || n.Action == protocol.ActionDefaulted {
			outcome := event.AskWithdrawn
			if n.Action == protocol.ActionDefaulted {
				outcome = event.AskDefaulted
			}
			return w.finishPrompt(ctx, id, recordedQuestionResult(p, event.AskResolvedPayload{Outcome: outcome}))
		}
	}
	if w.shown[id] == "awaiting-result" {
		return nil
	}
	if err := w.b.editPrompt(ctx, m, questionResultNote(p, "Updating result…"), nil); err != nil {
		if errors.Is(err, errMissing) {
			delete(w.shown, id)
			return w.b.store.set(id, promptMessage{})
		}
		return err
	}
	w.shown[id] = "awaiting-result"
	return nil
}

func (w *worker) questionEvent(ctx context.Context, e event.Event) error {
	if e.Channel != "" && e.Channel != w.id {
		return nil
	}
	if e.Type == event.AskRequested {
		var p event.AskRequestedPayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		m := w.b.store.snapshot()[p.ID]
		if m.Message == "" || m.Channel != w.discord {
			return nil
		}
		if p.Kind == string(protocol.PromptPermission) {
			if m.Permission == nil {
				m.Permission = &protocol.PromptInfo{ID: p.ID, Kind: protocol.PromptPermission, Tool: p.Tool, Input: p.Input, Dir: p.Dir, Prefix: p.Prefix, Question: p.Question, From: p.From}
			}
			m.Seq = max(m.Seq, e.Seq)
			return w.b.store.set(p.ID, m)
		}
		if p.Kind != string(protocol.PromptQuestion) {
			return nil
		}
		if len(m.Questions) == 0 {
			m.Questions = promptQuestions(protocol.PromptInfo{Question: p.Question, Questions: p.Questions})
		}
		m.Seq = max(m.Seq, e.Seq)
		return w.b.store.set(p.ID, m)
	}
	var p event.AskResolvedPayload
	if err := e.Decode(&p); err != nil {
		return err
	}
	m := w.b.store.snapshot()[p.ID]
	if m.Message != "" && m.Channel == w.discord && m.Permission != nil {
		return w.finishPrompt(ctx, p.ID, recordedPermissionResult(*m.Permission, p))
	}
	if m.Message == "" || m.Channel != w.discord || !m.isQuestion() {
		return nil
	}
	return w.finishPrompt(ctx, p.ID, recordedQuestionResult(protocol.PromptInfo{Questions: m.Questions}, p))
}

func recordedPermissionResult(p protocol.PromptInfo, result event.AskResolvedPayload) string {
	p.ClaimedBy = ""
	text, _ := promptView(p, "")
	mark := "ℹ️"
	if result.Outcome == event.AskWithdrawn {
		mark = "⏹️"
	} else {
		switch result.Answer {
		case protocol.AnswerAllow, protocol.AnswerAllowAlways, protocol.AnswerAllowPrefix:
			mark = "✔️"
		case protocol.AnswerDeny:
			mark = "❌"
		}
	}
	decision := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(protocol.PermissionResult(p, result))
	decision = strings.TrimRight(clip(escapeMarkdown(decision), 1000), "\\")
	note := " " + mark + " **" + decision + "**"
	head, body, hasBody := strings.Cut(text, "\n")
	head = clip(head, 2000-units(note)) + note
	if hasBody && units(head) < 2000 {
		return head + "\n" + clipMarkdown(body, 1999-units(head))
	}
	return head
}

func (w *worker) awaitPermissionResult(ctx context.Context, id string, m promptMessage, n *protocol.PromptNotification) error {
	if n != nil && n.Prompt.ID == id && n.Action == protocol.ActionWithdrawn {
		return w.finishPrompt(ctx, id, recordedPermissionResult(*m.Permission, event.AskResolvedPayload{Outcome: event.AskWithdrawn}))
	}
	if w.shown[id] == "awaiting-result" {
		return nil
	}
	text, _ := promptView(*m.Permission, "")
	if err := w.b.editPrompt(ctx, m, clipMarkdown(text, 1950)+"\n\nUpdating result…", nil); err != nil {
		if errors.Is(err, errMissing) {
			delete(w.shown, id)
			return w.b.store.set(id, promptMessage{})
		}
		return err
	}
	w.shown[id] = "awaiting-result"
	return nil
}

func recordedQuestionResult(p protocol.PromptInfo, result event.AskResolvedPayload) string {
	switch result.Outcome {
	case event.AskWithdrawn:
		return questionResultNote(p, "Question withdrawn.")
	case event.AskDefaulted:
		return questionResultNote(p, "Defaulted by the daemon.")
	case event.AskAnswered:
		if len(p.Questions) > 0 && len(result.Details) == len(p.Questions) {
			d := newDraft(p)
			for i, a := range result.Details {
				for _, n := range a.Selected {
					d.selected[i][n] = true
				}
				d.text[i] = a.Custom
			}
			return answeredQuestions(p, d)
		}
		// Older clients send only flattened answers. Show those verbatim rather
		// than guessing which comma-separated words were checked options.
		answer := result.Answer
		if len(result.Answers) > 0 {
			answer = strings.Join(result.Answers, "\n")
		}
		return questionResultNote(p, "Answer: "+answer)
	default:
		return questionResultNote(p, "Question resolved.")
	}
}

func questionResultNote(p protocol.PromptInfo, note string) string {
	note = "\n\n" + clip(note, 1000)
	return clip(answeredQuestions(p, nil), 2000-units(note)) + note
}
