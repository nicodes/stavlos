package agent

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// Replies are tracked by request ID, including human prompts and steers.
// Only explicit responses settle them; info and final prose do not. A turn
// ending with requests owed and nothing else to wait on queues a reminder of
// each pending request. Progress or a new request resets the bounded nudge count.

// maxNudges bounds reminder turns in a row that get no reply, so a stuck
// model cannot loop.
const maxNudges = 3

// nudgeLimit is how many reminders in a row an agent gets before the
// harness leaves it alone; 0 when reminders are off.
func nudgeLimit(reminders bool) int {
	if !reminders {
		return 0
	}
	return maxNudges
}

// owedBy is the party an input is owed to once taken: the sending agent for
// a request, the human for a channel chat post, nobody otherwise.
func owedBy(in event.Input) string {
	switch in.Kind {
	case event.InputRequest:
		return in.From
	case event.InputSteer:
		if in.From == "" && (in.Post != "" || in.RequestID != "") {
			return tools.User
		}
	case event.InputPrompt:
		if in.From == "" && in.RequestID != "" {
			return tools.User
		}
	default:
	}
	return ""
}

// took registers each consumed request independently. lastPost is retained for
// legacy state snapshots; new responses associate posts through reply_to.
func (a *agentState) took(in event.Input) {
	if party := owedBy(in); party != "" {
		request := replyRequest(in, party)
		if _, exists := a.owed[request.ID]; !exists {
			a.owedOrder = append(a.owedOrder, request.ID)
		}
		a.owed[request.ID] = replyDebt{ReplyRequest: request, legacy: in.RequestID == ""}
		a.nudges = 0
	}
	if in.From == "" && (in.Kind == event.InputPrompt || in.Kind == event.InputSteer) {
		a.lastPost = in.Post
	}
}

// settleRequest clears one obligation and resets the nudge count.
func (a *agentState) settleRequest(id string) {
	if _, exists := a.owed[id]; !exists {
		return
	}
	delete(a.owed, id)
	a.owedOrder = slices.DeleteFunc(a.owedOrder, func(v string) bool { return v == id })
	a.nudges = 0
}

// endReplies runs when a turn ends on its own: with replies still owed,
// nothing to wait for and nudges to spare, a reminder naming everyone owed
// is queued, and it starts the next turn.
func (a *Agent) endReplies(reason event.TurnReason) {
	if reason != event.ReasonEndTurn && reason != event.ReasonMaxTokens {
		return
	}
	s := a.c
	s.mu.Lock()
	st := a.state()
	if !s.cfg.Reminders || len(st.owed) == 0 || st.nudges >= maxNudges || st.waiting() {
		s.mu.Unlock()
		return
	}
	requests := st.pendingReplies()
	var parties []string
	for _, request := range requests {
		parties = append(parties, request.From)
	}
	wake, _ := s.commitLocked(context.Background(), s.event(a.ID, event.InputQueued,
		event.Input{ID: NewID("i"), Kind: event.InputReminder, Parties: parties, Names: s.partyNamesLocked(parties), Requests: requests}))
	s.mu.Unlock()
	signal(wake)
}

type replyDebt struct {
	event.ReplyRequest
	legacy bool
}
type replyWait struct {
	request event.ReplyRequest
	targets map[string]bool
	legacy  bool
}

func replyRequest(in event.Input, party string) event.ReplyRequest {
	id := in.RequestID
	if id == "" {
		id = in.ID
	}
	name := in.FromName
	if party == tools.User {
		name = tools.User
	}
	text := []rune(strings.Join(strings.Fields(in.Text), " "))
	if len(text) > 200 {
		text = append(text[:199], '…')
	}
	return event.ReplyRequest{ID: id, From: party, FromName: name, To: slices.Clone(in.To), Text: string(text), Post: in.Post}
}

func (a *agentState) trackRequest(in event.Input, target string) {
	r := replyRequest(in, in.From)
	if a.waits == nil {
		a.waits = map[string]*replyWait{}
	}
	w := a.waits[r.ID]
	if w == nil {
		w = &replyWait{request: r, targets: map[string]bool{}, legacy: in.RequestID == ""}
		a.waits[r.ID] = w
	}
	w.targets[target] = true
}

func (a *agentState) pendingReplies() []event.ReplyRequest {
	var out []event.ReplyRequest
	for _, id := range a.owedOrder {
		if r, ok := a.owed[id]; ok {
			request := r.ReplyRequest
			request.To = slices.Clone(request.To)
			out = append(out, request)
		}
	}
	return out
}

func (cs *channelState) awaitingReplies(a *agentState) []event.ReplyRequest {
	var out []event.ReplyRequest
	for id, w := range a.waits {
		for target := range w.targets {
			name := target
			if t := cs.agents[target]; t != nil {
				name = t.name
			}
			out = append(out, event.ReplyRequest{ID: id, From: target, FromName: name, Text: w.request.Text})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].From < out[j].From
	})
	return out
}

func (cs *channelState) settleReplies(responder *agentState, recipient string, ids []string) {
	for _, id := range ids {
		if debt, ok := responder.owed[id]; ok {
			if debt.From != recipient {
				continue
			}
			responder.settleRequest(id)
		}
		if requester := cs.agents[recipient]; requester != nil {
			requester.finishWait(id, responder.id)
		}
	}
}

func (a *agentState) finishWait(id, target string) {
	w := a.waits[id]
	if w == nil || !w.targets[target] {
		return
	}
	delete(w.targets, target)
	if len(w.targets) == 0 {
		delete(a.waits, id)
	}
	a.awaiting[target]--
	if a.awaiting[target] <= 0 {
		delete(a.awaiting, target)
	}
}

// Legacy log events may settle legacy requests by party. They never clear a
// newly tracked request, even when replay mixes old and new event formats.
func (cs *channelState) settleLegacy(responder *agentState, recipient string) {
	for id, debt := range responder.owed {
		if debt.legacy && debt.From == recipient {
			responder.settleRequest(id)
		}
	}
	if requester := cs.agents[recipient]; requester != nil {
		for id, wait := range requester.waits {
			if wait.legacy {
				requester.finishWait(id, responder.id)
			}
		}
	}
}

// partyNamesLocked is how parties read: "user", or the agent's name.
func (c *Channel) partyNamesLocked(parties []string) []string {
	names := make([]string, len(parties))
	for i, p := range parties {
		names[i] = p
		if a := c.st.agents[p]; a != nil {
			names[i] = a.name
		}
	}
	return names
}
