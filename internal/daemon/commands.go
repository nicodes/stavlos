package daemon

import (
	"context"
	"fmt"
	"sort"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/protocol"
)

func (d *Daemon) commandConfig(channel string) (*agent.Channel, *config.Effective, error) {
	s, err := d.channel(channel)
	if err != nil {
		return nil, nil, err
	}
	dir := s.Dir()
	cfg, err := config.Load(dir, d.trust)
	if err != nil {
		return nil, nil, err
	}
	if s.Dir() != dir {
		return nil, nil, fmt.Errorf("channel directory changed; retry the command")
	}
	s.SetConfig(cfg)
	if cfg.TrustPending {
		d.maybeTrustPrompt(s)
	}
	return s, cfg, nil
}

func (d *Daemon) listCommands(channel string) (protocol.CommandListResult, error) {
	_, cfg, err := d.commandConfig(channel)
	if err != nil {
		return protocol.CommandListResult{}, err
	}
	result := protocol.CommandListResult{}
	for _, c := range cfg.Commands {
		result.Commands = append(result.Commands, protocol.CommandInfo{Name: c.Name, Description: c.Description})
	}
	sort.Slice(result.Commands, func(i, j int) bool { return result.Commands[i].Name < result.Commands[j].Name })
	return result, nil
}

func (d *Daemon) runCommand(ctx context.Context, p protocol.CommandRunParams, source string) error {
	s, cfg, err := d.commandConfig(p.Channel)
	if err != nil {
		return err
	}
	c, ok := cfg.Commands[p.Name]
	if !ok {
		if cfg.TrustPending {
			return fmt.Errorf("project configuration needs trust before project commands are available")
		}
		return fmt.Errorf("unknown command /%s", p.Name)
	}
	return s.RunCommand(ctx, cfg.Dir, p.Agent, c.Body, source)
}
