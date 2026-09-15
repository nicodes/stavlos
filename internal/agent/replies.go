package agent

import (
	"context"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// Replies (docs/super-chat.md): every request an agent takes from another
// agent, and every post the human makes in the channel chat, is owed a reply
// sent with the message tool (the text a turn ends with reaches no one
// there). A message the human types in the agent's own chat is answered in
// that chat, so it owes nothing. Whenever a turn ends on its own with
// replies still owed, and the agent is not waiting on an agent or a job
// (whose result wakes it anyway), a reminder is queued naming everyone
// still owed. After maxNudges reminders in a row with no reply the agent is
// left alone; a reply or a new request resets the count.

// maxNudges bounds reminder turns in a row that get no reply, so a stuck
// model cannot loop.
const maxNudges = 3

// owedBy is the party an input is owed to once taken: the sending agent for
// a request, the human for a channel chat post, nobody otherwise.
func owedBy(in event.Input) string {
	switch in.Kind {
	case event.InputRequest:
		return in.From
	case event.InputSteer:
		if in.From == "" && in.Post != "" {
			return tools.User
		}
	default:
	}
	return ""
}

// took records what a taken input is owed, and the post the human's input
// delivered ("" for a message typed into the agent's own chat), which a
// message to the user then answers.
func (a *agentState) took(in event.Input) {
	if party := owedBy(in); party != "" {
		a.owed[party] = true
		a.nudges = 0
	}
	if in.From == "" && (in.Kind == event.InputPrompt || in.Kind == event.InputSteer) {
		a.lastPost = in.Post
	}
}

// settle clears the reply owed to party and resets the nudge count.
func (a *agentState) settle(party string) {
	delete(a.owed, party)
	a.nudges = 0
}

// endReplies runs when a turn ends on its own: with replies still owed,
// nothing to wait for and nudges to spare, a reminder naming everyone owed
// is queued, and it starts the next turn.
func (a *Agent) endReplies(reason event.TurnReason) {
	if reason != event.ReasonEndTurn && reason != event.ReasonMaxTokens {
		return
	}
	s := a.s
	s.mu.Lock()
	st := a.state()
	if !s.cfg.Reminders || len(st.owed) == 0 || st.nudges >= maxNudges || st.waiting() {
		s.mu.Unlock()
		return
	}
	parties := st.due()
	wake, _ := s.commitLocked(context.Background(), s.event(a.ID, event.InputQueued,
		event.Input{ID: NewID("i"), Kind: event.InputReminder, Parties: parties, Names: s.partyNamesLocked(parties)}))
	s.mu.Unlock()
	signal(wake)
}

// partyNamesLocked is how parties read: "user", or the agent's name.
func (s *Channel) partyNamesLocked(parties []string) []string {
	names := make([]string, len(parties))
	for i, p := range parties {
		names[i] = p
		if a := s.st.agents[p]; a != nil {
			names[i] = a.name
		}
	}
	return names
}
