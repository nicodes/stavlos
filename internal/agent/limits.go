package agent

import (
	"context"
	"fmt"

	"github.com/nicodes/stavlos/internal/event"
)

// reportTurnLimit answers every agent still waiting on a, so a subagent
// that ran out of turns does not leave its askers waiting forever.
func (a *Agent) reportTurnLimit(limit int) {
	s := a.c
	s.mu.Lock()
	st := a.state()
	text := fmt.Sprintf("%s reached its turn limit of %d without answering; message it again only if you raise the limit in its role, or delegate elsewhere.", st.name, limit)
	public := s.publicLimitRepliesLocked(a.ID)
	var evs []event.Event
	for _, id := range s.st.order {
		o := s.st.agents[id]
		if o == nil || id == a.ID || o.killed {
			continue
		}
		if refs := s.privateReplyIDsLocked(o.awaitingOn(a.ID)); len(refs) > 0 {
			evs = append(evs, s.event(id, event.InputQueued, event.Input{ID: NewID("i"), Kind: event.InputResponse, Text: text, From: a.ID, FromName: st.name, ReplyTo: refs}))
		}
	}
	var human []string
	var posts []string
	for _, request := range st.pendingReplies() {
		if request.From == "user" && request.Channel == "" {
			human = append(human, request.ID)
			if request.Post != "" {
				posts = append(posts, request.Post)
			}
		}
	}
	if len(human) > 0 {
		evs = append(evs, s.event(a.ID, event.ChatMessage, event.ChatPayload{From: st.name, Text: text, Kind: "response", ReplyTo: human, Posts: posts}))
	}
	// The limit is reached whether or not the log takes the news: the
	// askers are told in memory all the same, or they wait for ever.
	_ = s.commitFactLocked(context.Background(), evs...)
	s.mu.Unlock()
	if host, ok := s.host.(BoardHost); ok {
		for _, reply := range public {
			_ = host.BoardReport(context.Background(), s.ID, a.ID, reply.channel, reply.to, text, reply.ids)
		}
	}
}

type limitReply struct {
	channel string
	to, ids []string
}

func (c *Channel) publicLimitRepliesLocked(responder string) []limitReply {
	var out []limitReply
	for _, req := range c.st.openRequests() {
		if req.Channel != "" && req.open[responder] {
			out = append(out, limitReply{channel: req.Channel, to: []string{req.From}, ids: []string{req.ID}})
		}
	}
	return out
}
func (c *Channel) privateReplyIDsLocked(ids []string) []string {
	var out []string
	for _, id := range ids {
		if r := c.st.requests[id]; r == nil || r.Channel == "" {
			out = append(out, id)
		}
	}
	return out
}
