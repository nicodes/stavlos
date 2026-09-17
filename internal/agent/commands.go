package agent

import (
	"context"
	"fmt"

	"github.com/nicodes/stavlos/internal/event"
)

// RunCommand delivers an already-resolved prompt using the current agent/model.
// The directory check and enqueue share the channel lock so a directory switch
// cannot apply an old project's command to a new working directory.
func (c *Channel) RunCommand(ctx context.Context, dir, agent, text, source string) error {
	c.mu.Lock()
	if c.reconfiguring || c.st.dir != dir {
		c.mu.Unlock()
		return fmt.Errorf("channel directory changed; retry the command")
	}
	channelPost := agent == ""
	if channelPost && len(c.st.order) > 0 {
		agent = c.st.order[0]
	}
	a := c.st.agents[agent]
	if a == nil || a.killed {
		c.mu.Unlock()
		return fmt.Errorf("no live agent selected in this channel")
	}
	in := event.Input{ID: NewID("i"), Kind: event.InputSteer, Text: text}
	in.RequestID = in.ID
	var evs []event.Event
	if channelPost {
		in.Post, in.To = NewID("post"), []string{a.name}
		in.RequestID = in.Post
		evs = append(evs, c.event("", event.ChatPosted, event.ChatPayload{ID: in.Post, RequestID: in.Post, Kind: "request", From: source, Text: text, To: in.To}))
	}
	evs = append(evs, c.event(agent, event.InputQueued, in))
	wake, err := c.commitLocked(ctx, evs...)
	c.mu.Unlock()
	signal(wake)
	return err
}
