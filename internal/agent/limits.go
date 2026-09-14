package agent

import "fmt"

// reportTurnLimit answers every agent still waiting on a, so a subagent
// that ran out of turns does not leave its askers waiting forever.
func (a *Agent) reportTurnLimit(limit int) {
	text := fmt.Sprintf("%s (%s) reached its turn limit of %d without answering; message it again only if you raise the limit in its role, or kill it and delegate elsewhere.", a.LabelNow(), a.ID, limit)
	o := orchestrator{s: a.s}
	for _, other := range a.s.agentsSnapshot() {
		if other.ID == a.ID {
			continue
		}
		other.mu.Lock()
		waiting := other.awaiting[a.ID] > 0
		other.mu.Unlock()
		if waiting {
			_ = o.Respond(a.ID, other.ID, text)
		}
	}
}
