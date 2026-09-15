package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// Replies (docs/super-chat.md): every request an agent takes from another
// agent, and every post the human makes in the channel chat, is owed a reply
// sent with the message tool (the text a turn ends with reaches no one
// there). A message the human types in the agent's own chat is answered in
// that chat, so it owes nothing. Whenever a turn ends on its own
// with replies still owed, and the agent is not waiting on an agent or a
// job (whose result wakes it anyway), a reminder turn is queued naming
// everyone still owed. After maxNudges reminders in a row with no reply the
// agent is left alone; a reply or a new message resets the count. What is
// owed is exposed as AgentInfo.Due, never injected into the prompt.

// maxNudges bounds reminder turns in a row that get no reply, so a stuck
// model (one that keeps running out of output, say) cannot loop.
const maxNudges = 3

// senderID is the agent id of an envelope source "agent:<id>", "" for
// anything else.
func senderID(source string) string {
	if id, ok := strings.CutPrefix(source, "agent:"); ok {
		return id
	}
	return ""
}

// owedBy is the party a logged input is owed to: the sending agent, the
// human for a channel chat post (a prompt or steer with no sender that
// carries its post), and nobody for the human's message typed in the agent's
// own chat, answers, job results and reminders.
func owedBy(in event.UserMessagePayload) string {
	switch in.Kind {
	case event.MsgPrompt, event.MsgSteer:
		if in.FromID != "" {
			return in.FromID
		}
		if in.From == "" && in.Post != "" {
			return tools.User
		}
	case event.MsgAgentResponse, event.MsgMonitorFired, event.MsgReminder, event.MsgNote:
	}
	return ""
}

// took records the reply a logged input is owed and resets the nudge
// count. The human's input also sets the post a message to the user
// answers: its chat post, or none for a message typed in the agent's own
// chat.
func (a *Agent) took(in event.UserMessagePayload) {
	party := owedBy(in)
	human := in.From == "" && in.FromID == "" && (in.Kind == event.MsgPrompt || in.Kind == event.MsgSteer)
	if party == "" && !human {
		return
	}
	a.mu.Lock()
	if party != "" {
		a.owed[party] = true
		a.nudges = 0
	}
	if human {
		a.lastPost = in.Post // "" for a message typed in the agent's own chat
	}
	a.mu.Unlock()
}

// currentPost is the channel chat post a message to the user answers, ""
// for none.
func (a *Agent) currentPost() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastPost
}

// settle clears the reply owed to party (the agent messaged them, or they
// are gone) and resets the nudge count.
func (a *Agent) settle(party string) {
	a.mu.Lock()
	delete(a.owed, party)
	a.nudges = 0
	a.mu.Unlock()
}

// dueLocked lists the parties the agent owes a reply, sorted. Callers hold
// a.mu.
func (a *Agent) dueLocked() []string {
	out := make([]string, 0, len(a.owed))
	for p := range a.owed {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// endReplies runs when a turn ends on its own (not cancelled, not failed):
// if replies are still owed, the agent is not waiting on an agent or a job,
// and the nudges in a row are under maxNudges, a reminder naming everyone
// owed is queued and starts the next turn.
func (a *Agent) endReplies(ctx context.Context, reason event.TurnReason) {
	if reason != event.ReasonEndTurn && reason != event.ReasonMaxTokens || !a.s.Config().Reminders {
		return
	}
	a.mu.Lock()
	if len(a.owed) == 0 || a.nudges >= maxNudges || a.waitingOn() {
		a.mu.Unlock()
		return
	}
	parties := a.dueLocked()
	a.nudges++
	a.remind = parties
	a.mu.Unlock()
	_, _ = a.record(ctx, event.ReminderQueued, event.RepliesPayload{Parties: parties, Names: a.s.partyNames(parties)})
}

// partyNames is how parties read to a model or a human: "user", or the
// agent's name.
func (s *Channel) partyNames(parties []string) []string {
	names := make([]string, len(parties))
	for i, p := range parties {
		names[i] = p
		if p != tools.User {
			names[i] = s.senderLabel("agent:" + p)
		}
	}
	return names
}

// reminderText is the input a reminder turn starts with.
func (s *Channel) reminderText(parties []string) string {
	names := s.partyNames(parties)
	return fmt.Sprintf("[reminder from the harness] Your last turn ended without replying to %s. The text you end a turn with reaches no one: send each reply with message (to: %s, kind: response). If there is nothing more to say, a one-line message still tells them where things stand.",
		strings.Join(names, ", "), strings.Join(names, " or "))
}
