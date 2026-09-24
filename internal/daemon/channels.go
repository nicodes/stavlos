package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/protocol"
)

// The channels the daemon holds: finding one, creating, renaming, moving and
// archiving it, and the list clients are shown.

func (d *Daemon) channel(id string) (*agent.Channel, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.channels[id]
	if !ok {
		return nil, fmt.Errorf("channel %q %w", id, errNotFound)
	}
	return s, nil
}

// channelList snapshots the channels. d.mu is never held while a channel is
// called: a channel holds its own lock while it appends, the log's writer
// delivers the commit to clients under d.mu, and holding d.mu across a
// channel call would close that loop into a deadlock.
func (d *Daemon) channelList() []*agent.Channel {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]*agent.Channel, 0, len(d.channels))
	for _, s := range d.channels {
		out = append(out, s)
	}
	return out
}

// agentChannel finds the channel owning an agent.
func (d *Daemon) agentChannel(agentID string) (*agent.Channel, *agent.Agent, error) {
	for _, s := range d.channelList() {
		if a, ok := s.Agent(agentID); ok {
			return s, a, nil
		}
	}
	return nil, nil, fmt.Errorf("agent %q %w", agentID, errNotFound)
}

// CreateChannel creates and starts a channel in dir.
func (d *Daemon) CreateChannel(ctx context.Context, dir, modelID, root, want string) (*agent.Channel, error) {
	dir, err := config.WorkingDirectory("", dir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(dir, d.trust)
	if err != nil {
		return nil, err
	}
	id := agent.NewID("c")
	s := agent.New(d, id, dir, cfg, modelID, root)
	d.nameMu.Lock()
	defer d.nameMu.Unlock()
	taken, err := d.channelNames(ctx, "")
	if err != nil {
		return nil, err
	}
	name := agent.UniqueName(filepath.Base(dir), "channel", func(n string) bool { return taken[n] })
	if strings.TrimSpace(want) != "" {
		if name, err = checkName(want, taken); err != nil {
			return nil, err
		}
	}
	if err := s.Start(ctx, name); err != nil { // channel.created indexes it
		return nil, err
	}
	d.mu.Lock()
	d.channels[id] = s
	d.mu.Unlock()
	d.maybeTrustPrompt(s)
	return s, nil
}

// SetChannelDir changes an idle channel's default directory and rebinds its
// project configuration. Old trust prompts are dismissed, never transferred.
func (d *Daemon) SetChannelDir(ctx context.Context, id, dir string) (protocol.ChannelInfo, error) {
	s, err := d.channel(id)
	if err != nil {
		return protocol.ChannelInfo{}, err
	}
	old := s.Dir()
	if err := s.SetDir(ctx, dir, func(dir string) (*config.Effective, error) { return config.Load(dir, d.trust) }); err != nil {
		return protocol.ChannelInfo{}, err
	}
	if old != s.Dir() {
		d.withdrawTrustPrompts(id, "directory-change")
	}
	d.maybeTrustPrompt(s)
	return s.Info(), nil
}

// ArchiveChannel archives a channel. Its trust prompt, if one is open, is
// withdrawn: nobody will answer it for a channel that is gone from the list.
func (d *Daemon) ArchiveChannel(ctx context.Context, id string) error {
	s, err := d.channel(id)
	if err != nil {
		return err
	}
	if err := s.Archive(ctx); err != nil {
		return err
	}
	d.withdrawTrustPrompts(id, "archived")
	return nil
}

// RenameChannel gives a channel another name: normalised like an agent's
// ("#Docs Site" becomes docs-site), refused when it leaves nothing or another
// channel, archived or not, has it.
func (d *Daemon) RenameChannel(ctx context.Context, id, want string) error {
	s, err := d.channel(id)
	if err != nil {
		return err
	}
	d.nameMu.Lock()
	defer d.nameMu.Unlock()
	taken, err := d.channelNames(ctx, id)
	if err != nil {
		return err
	}
	name, err := checkName(want, taken)
	if err != nil {
		return err
	}
	return s.Rename(ctx, name)
}

// checkName normalises a name the human chose ("#Docs Site" becomes
// docs-site) and refuses one that leaves nothing or another channel has.
func checkName(want string, taken map[string]bool) (string, error) {
	name := agent.NormalizeName(strings.TrimPrefix(strings.TrimSpace(want), "#"))
	if name == "" {
		return "", fmt.Errorf("%q has no letters or digits to name a channel with", want)
	}
	if taken[name] {
		return "", fmt.Errorf("#%s is taken by another channel", name)
	}
	return name, nil
}

// channelNames is every channel's name but except's. Callers hold d.nameMu.
func (d *Daemon) channelNames(ctx context.Context, except string) (map[string]bool, error) {
	rows, err := d.Log.Channels(ctx)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, r := range rows {
		if r.ID != except {
			taken[r.Name] = true
		}
	}
	return taken, nil
}

// ChannelList returns infos.
func (d *Daemon) ChannelList(ctx context.Context, dir string, archived bool) ([]protocol.ChannelInfo, error) {
	rows, err := d.Log.Channels(ctx)
	if err != nil {
		return nil, err
	}
	var out []protocol.ChannelInfo
	for _, r := range rows {
		if dir != "" && r.Dir != dir {
			continue
		}
		if r.Archived && !archived {
			continue
		}
		d.mu.RLock()
		s, ok := d.channels[r.ID]
		d.mu.RUnlock()
		var info protocol.ChannelInfo
		if ok {
			info = s.Info()
		} else {
			info = protocol.ChannelInfo{ID: r.ID, Name: r.Name, Dir: r.Dir, Created: r.Created.Format(time.RFC3339), Archived: r.Archived}
		}
		info.Seq, info.Title = r.LastSeq, r.Title
		info.DirError = config.DirectoryError(info.Dir)
		for _, p := range d.esc.Pending(r.ID) { // what the channel waits on the human for
			if p.Kind == "question" {
				info.Questions++
			} else {
				info.Permissions++
			}
		}
		out = append(out, info)
	}
	return out, nil
}
