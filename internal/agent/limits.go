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
	var evs []event.Event
	for _, id := range s.st.order {
		o := s.st.agents[id]
		if o == nil || id == a.ID || o.killed {
			continue
		}
		if refs := o.awaitingOn(a.ID); len(refs) > 0 {
			evs = append(evs, s.event(id, event.InputQueued, event.Input{ID: NewID("i"), Kind: event.InputResponse, Text: text, From: a.ID, FromName: st.name, ReplyTo: refs}))
		}
	}
	var human []string
	var posts []string
	for _, request := range st.pendingReplies() {
		if request.From == "user" {
			human = append(human, request.ID)
			if request.Post != "" {
				posts = append(posts, request.Post)
			}
		}
	}
	if len(human) > 0 {
		evs = append(evs, s.event(a.ID, event.ChatMessage, event.ChatPayload{From: st.name, Text: text, Kind: "response", ReplyTo: human, Posts: posts}))
	}
	_ = s.commitLocked(context.Background(), evs...)
	s.mu.Unlock()
}
