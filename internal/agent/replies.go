package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// Replies (docs/super-chat.md): every message an agent takes in, from the
// human or from another agent, is owed a reply sent with the message tool;
// the text a turn ends with reaches no one. A turn that ends owing replies
// gets one reminder, and a turn that ends still owing a party it was
// reminded of records the reply as missing. Nothing retries.

// senderID is the agent id of an envelope source "agent:<id>", "" for
// anything else.
func senderID(source string) string {
	if id, ok := strings.CutPrefix(source, "agent:"); ok {
		return id
	}
	return ""
}

// owedBy is the party a logged input is owed to: the sending agent, the
// human for a prompt or steer with no sender, and nobody for answers, job
// results and reminders (or a sender logged before ids were).
func owedBy(in event.UserMessagePayload) string {
	switch in.Kind {
	case event.MsgPrompt, event.MsgSteer:
		if in.FromID != "" {
			return in.FromID
		}
		if in.From == "" {
			return tools.User
		}
	case event.MsgAgentResponse, event.MsgMonitorFired, event.MsgReminder:
	}
	return ""
}

// took records the reply a logged input is owed. A new message from a
// party already reminded starts its reminder over.
func (a *Agent) took(in event.UserMessagePayload) {
	party := owedBy(in)
	if party == "" {
		return
	}
	a.mu.Lock()
	a.owed[party] = true
	delete(a.reminded, party)
	a.mu.Unlock()
}

// settle clears the reply owed to party: the agent messaged them, or they
// are gone.
func (a *Agent) settle(party string) {
	a.mu.Lock()
	delete(a.owed, party)
	delete(a.reminded, party)
	a.mu.Unlock()
}

// endReplies runs when a turn ends on its own (not cancelled, not failed):
// parties still owed a reply get a reminder queued, which starts the next
// turn, and parties reminded already are recorded as missing and dropped.
func (a *Agent) endReplies(ctx context.Context, reason event.TurnReason) {
	if reason != event.ReasonEndTurn && reason != event.ReasonMaxTokens || !a.s.Config().Reminders {
		return
	}
	var remind, missing []string
	a.mu.Lock()
	for p := range a.owed {
		if a.reminded[p] {
			missing = append(missing, p)
			delete(a.owed, p)
			delete(a.reminded, p)
		} else {
			remind = append(remind, p)
			a.reminded[p] = true
		}
	}
	sort.Strings(remind)
	a.remind = append(a.remind, remind...)
	a.mu.Unlock()
	sort.Strings(missing)
	if len(missing) > 0 {
		_, _ = a.record(ctx, event.ReplyMissing, event.RepliesPayload{Parties: missing, Names: a.s.partyNames(missing)})
	}
	if len(remind) > 0 {
		_, _ = a.record(ctx, event.ReminderQueued, event.RepliesPayload{Parties: remind, Names: a.s.partyNames(remind)})
	}
}

// partyNames is how parties read to a model or a human: "user", or the
// agent's name.
func (s *Session) partyNames(parties []string) []string {
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
func (s *Session) reminderText(parties []string) string {
	names := s.partyNames(parties)
	return fmt.Sprintf("[reminder from the harness] Your last turn ended without replying to %s. The text you end a turn with reaches no one: send each reply with message (to: %s). If there is nothing more to say, a one-line message still tells them where things stand. There is no second reminder.",
		strings.Join(names, ", "), strings.Join(names, " or "))
}
