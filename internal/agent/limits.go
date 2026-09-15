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
		if o := s.st.agents[id]; id != a.ID && !o.killed && o.awaiting[a.ID] > 0 {
			evs = append(evs, s.event(id, event.InputQueued, event.Input{ID: NewID("i"), Kind: event.InputResponse, Text: text, From: a.ID, FromName: st.name}))
		}
	}
	wake, _ := s.commitLocked(context.Background(), evs...)
	s.mu.Unlock()
	signal(wake)
}
