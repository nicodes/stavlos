package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/event"
)

// Dir is the channel's default working directory, independent of its clients.
func (c *Channel) Dir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.dir
}

// SetDir changes an idle channel's environment. load runs with turns gated,
// outside the channel lock. The durable event, new config and cache reset are
// published together. Additional absolute directory grants remain channel-owned.
func (c *Channel) SetDir(ctx context.Context, dir string, load func(string) (*config.Effective, error)) error {
	c.mu.Lock()
	if err := c.canChangeDirLocked(); err != nil {
		c.mu.Unlock()
		return err
	}
	old := c.st.dir
	c.reconfiguring = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.reconfiguring = false
		agents := c.agentsLocked()
		c.mu.Unlock()
		signal(agents) // inputs arriving during the transition now have a stable environment
	}()
	dir, err := config.WorkingDirectory(old, dir)
	if err != nil {
		return err
	}
	if dir == old {
		return nil
	}
	cfg, err := load(dir)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, a := range c.Agents() {
		a.disarmMCPIdle()
		a.stopMCP("", false)
	}
	stamp := instructionsStamp(cfg.InstructionFiles)
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := []event.Event{c.event("", event.ChannelUpdated, event.ChannelUpdatedPayload{Dir: event.Str(dir)})}
	for _, id := range c.st.order {
		if !c.st.agents[id].killed {
			evs = append(evs, c.event(id, event.InputQueued, event.Input{ID: NewID("i"), Kind: event.InputInfo,
				Text: fmt.Sprintf("[harness] The human changed this channel's default directory from %s to %s. Earlier relative paths refer to the old directory. Project configuration and instructions have been reloaded; remembered permissions were cleared and mode is now ask.", old, dir)}))
		}
	}
	if _, err := c.commitLocked(ctx, evs...); err != nil {
		return err
	}
	c.cfg, c.stamp = cfg, stamp
	for _, a := range c.agents {
		a.prefix = promptPrefix{}
		a.instructed = nil
	}
	return nil
}

func (c *Channel) canChangeDirLocked() error {
	if c.stopped || c.st.archived {
		return errors.New("cannot change the directory of a stopped or archived channel")
	}
	if c.reconfiguring {
		return errors.New("a directory change is already in progress")
	}
	for id, a := range c.st.agents {
		if a.killed {
			continue
		}
		if a.busy() || a.compacting || len(a.jobs) > 0 || len(a.asks) > 0 || c.agents[id].cancelTurn != nil || c.agents[id].maintenance > 0 {
			return errors.New("channel must be idle before changing its default directory: finish or cancel active turns, jobs and prompts")
		}
	}
	return nil
}
