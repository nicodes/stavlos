package agent

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/tools"
)

// Open requests live in one table per channel, keyed by request ID: who
// asked, and which recipients have not answered yet. Both directions are
// views of that table — what an agent owes is the entries it holds
// unanswered, what it waits on is the entries it sent — so the two can
// never drift apart. The human is a party like any other, which is why a
// prompt owed a reply needs no separate bookkeeping from an agent's request.
//
// Only explicit responses settle an entry; info and final prose do not. A
// turn ending with entries owed and nothing else to wait on queues a
// reminder of each. Progress or a new request resets the bounded nudge count.

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

// request is one open request: the asking party, and the recipients who
// have not answered it yet. held is the subset that has taken the input,
// and so owes the answer now; the rest is still in an inbox.
type request struct {
	event.ReplyRequest
	seq    int             // arrival order, so what is owed reads oldest first
	legacy bool            // sent before request IDs: settled by party
	open   map[string]bool // recipient → has not answered
	held   map[string]bool // recipient → has taken it
}

// requestID is the ID an input's obligation is filed under: its own before
// request IDs existed.
func requestID(in event.Input) string {
	if in.RequestID != "" {
		return in.RequestID
	}
	return in.ID
}

func replyRequest(in event.Input, party string) event.ReplyRequest {
	name := in.FromName
	if party == tools.User {
		name = tools.User
	}
	text := []rune(strings.Join(strings.Fields(in.Text), " "))
	if len(text) > 200 {
		text = append(text[:199], '…')
	}
	return event.ReplyRequest{ID: requestID(in), From: party, FromName: name, To: slices.Clone(in.To), Text: string(text), Post: in.Post}
}

// asked opens the request an input carries, or widens an open one to
// another recipient. Recipients join as the input is queued, so the sender
// waits from the moment it asks; the obligation to answer starts when the
// recipient takes the input (took).
func (cs *channelState) asked(in event.Input, target string) *request {
	party := owedBy(in)
	if party == "" {
		return nil
	}
	r := replyRequest(in, party)
	req := cs.requests[r.ID]
	if req == nil {
		cs.reqSeq++
		req = &request{ReplyRequest: r, seq: cs.reqSeq, legacy: in.RequestID == "", open: map[string]bool{}, held: map[string]bool{}}
		cs.requests[r.ID] = req
	}
	req.open[target] = true
	return req
}

// settle clears one recipient's obligation and the sender's wait on it in
// the same stroke; the entry goes when nobody is left to answer.
func (cs *channelState) settle(req *request, responder string) {
	delete(req.open, responder)
	delete(req.held, responder)
	if len(req.open) == 0 {
		delete(cs.requests, req.ID)
	}
}

// openRequests lists the table in arrival order, so every walk of it is
// deterministic and safe to settle from.
func (cs *channelState) openRequests() []*request {
	out := make([]*request, 0, len(cs.requests))
	for _, req := range cs.requests {
		out = append(out, req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// took marks a taken input's request as held by the agent: it owes the
// answer from here. A new request resets the nudge count.
func (a *agentState) took(in event.Input) {
	if req := a.cs.asked(in, a.id); req != nil {
		req.held[a.id] = true
		a.nudges = 0
	}
	if in.From == "" && (in.Kind == event.InputPrompt || in.Kind == event.InputSteer) {
		a.lastPost = in.Post
	}
}

// settleReplies settles the requests recipient asked responder, by ID.
func (cs *channelState) settleReplies(responder *agentState, recipient string, ids []string) {
	for _, id := range ids {
		if req := cs.requests[id]; req != nil && req.From == recipient {
			cs.settle(req, responder.id)
			responder.nudges = 0
		}
	}
}

// settleLegacy settles by party, for log events written before responses
// referenced request IDs. It never clears a request that carried one, even
// when replay mixes old and new event formats.
func (cs *channelState) settleLegacy(responder *agentState, recipient string) {
	for _, req := range cs.openRequests() {
		if req.legacy && req.From == recipient && req.open[responder.id] {
			cs.settle(req, responder.id)
			responder.nudges = 0
		}
	}
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
	requests := st.pendingReplies()
	if !s.cfg.Reminders || len(requests) == 0 || st.nudges >= maxNudges || st.waiting() {
		s.mu.Unlock()
		return
	}
	var parties []string
	for _, request := range requests {
		parties = append(parties, request.From)
	}
	wake, _ := s.commitLocked(context.Background(), s.event(a.ID, event.InputQueued,
		event.Input{ID: NewID("i"), Kind: event.InputReminder, Parties: parties, Names: s.partyNamesLocked(parties), Requests: requests}))
	s.mu.Unlock()
	signal(wake)
}

// --- views of the table ---

// pendingReplies is what the agent owes: the requests it has taken and not
// answered, oldest first.
func (a *agentState) pendingReplies() []event.ReplyRequest {
	var out []event.ReplyRequest
	for _, req := range a.cs.openRequests() {
		if req.held[a.id] {
			r := req.ReplyRequest
			r.To = slices.Clone(r.To)
			out = append(out, r)
		}
	}
	return out
}

// owedRequest is one request the agent holds unanswered.
func (a *agentState) owedRequest(id string) (event.ReplyRequest, bool) {
	req := a.cs.requests[id]
	if req == nil || !req.held[a.id] {
		return event.ReplyRequest{}, false
	}
	return req.ReplyRequest, true
}

// due lists the parties the agent owes a reply, sorted.
func (a *agentState) due() []string {
	var out []string
	for _, request := range a.pendingReplies() {
		if !slices.Contains(out, request.From) {
			out = append(out, request.From)
		}
	}
	sort.Strings(out)
	return out
}

// awaitingReplies is what the agent waits for: a row per recipient of each
// request it sent that is still unanswered.
func (a *agentState) awaitingReplies() []event.ReplyRequest {
	var out []event.ReplyRequest
	for _, req := range a.cs.openRequests() {
		if req.From != a.id {
			continue
		}
		for _, target := range sortedKeys(req.open) {
			name := target
			if t := a.cs.agents[target]; t != nil {
				name = t.name
			}
			out = append(out, event.ReplyRequest{ID: req.ID, From: target, FromName: name, Text: req.Text})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].From < out[j].From
	})
	return out
}

// awaitingOn lists the requests the agent sent that target has not
// answered, sorted by ID.
func (a *agentState) awaitingOn(target string) []string {
	var out []string
	for _, req := range a.cs.openRequests() {
		if req.From == a.id && req.open[target] {
			out = append(out, req.ID)
		}
	}
	sort.Strings(out)
	return out
}

// awaitingIDs lists the agents this one waits on, sorted.
func (a *agentState) awaitingIDs() []string {
	var out []string
	for _, req := range a.cs.openRequests() {
		if req.From != a.id {
			continue
		}
		for target := range req.open {
			if !slices.Contains(out, target) {
				out = append(out, target)
			}
		}
	}
	sort.Strings(out)
	return out
}

// awaitingAny reports whether any request the agent sent is unanswered.
func (a *agentState) awaitingAny() bool {
	for _, req := range a.cs.requests {
		if req.From == a.id && len(req.open) > 0 {
			return true
		}
	}
	return false
}

// forgetParty drops an agent from the table: it can no longer answer what
// it holds, and nothing it asked can still be owed.
func (cs *channelState) forgetParty(id string) {
	for _, req := range cs.openRequests() {
		if req.From == id {
			delete(cs.requests, req.ID)
			continue
		}
		cs.settle(req, id)
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
