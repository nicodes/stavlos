package daemon

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/agent"
	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tools"
)

func (d *Daemon) BoardCreate(ctx context.Context, source, caller, name string, refs []string) (string, error) {
	home, err := d.channel(source)
	if err != nil {
		return "", err
	}
	members, err := home.BoardMembers(caller, refs)
	if err != nil {
		return "", err
	}
	d.nameMu.Lock()
	defer d.nameMu.Unlock()
	taken, err := d.channelNames(ctx, "")
	if err != nil {
		return "", err
	}
	name, err = checkName(name, taken)
	if err != nil {
		return "", err
	}
	board := agent.New(d, agent.NewID("c"), home.Dir(), home.Config(), "", "")
	if err := board.StartBoard(ctx, name, event.BoardInfo{Source: source, Owner: caller, Members: members}); err != nil {
		board.Stop()
		return "", err
	}
	d.mu.Lock()
	d.channels[board.ID] = board
	d.mu.Unlock()
	return fmt.Sprintf("created #%s; channel: %s; members: %s. The configured Discord bridge mirrors this channel automatically.", name, board.ID, strings.Join(d.boardMemberNames(home, members), ", ")), nil
}
func (d *Daemon) boardMemberNames(home *agent.Channel, ids []string) []string {
	var names []string
	for _, id := range ids {
		if a, ok := home.Agent(id); ok {
			names = append(names, a.Name())
		}
	}
	return names
}
func (d *Daemon) boardFor(source, caller, ref string) (*agent.Channel, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	for _, ch := range d.channelList() {
		if ch.ID != ref && ch.Name() != ref {
			continue
		}
		b := ch.Board()
		if b == nil || b.Source != source || !slices.Contains(b.Members, caller) {
			return nil, fmt.Errorf("you are not a member of channel %q", ref)
		}
		if ch.Archived() {
			return nil, fmt.Errorf("channel %q is archived", ref)
		}
		return ch, nil
	}
	return nil, fmt.Errorf("channel %q not found", ref)
}
func (d *Daemon) BoardList(_ context.Context, source, caller string) (string, error) {
	var lines []string
	for _, ch := range d.channelList() {
		b := ch.Board()
		if b != nil && b.Source == source && slices.Contains(b.Members, caller) && !ch.Archived() {
			lines = append(lines, fmt.Sprintf("#%s (%s)", ch.Name(), ch.ID))
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "no shared channels", nil
	}
	return strings.Join(lines, "\n"), nil
}
func (d *Daemon) BoardRead(ctx context.Context, source, caller, ref string, from int64, limit int) (string, error) {
	board, err := d.boardFor(source, caller, ref)
	if err != nil {
		return "", err
	}
	if from < 1 {
		from = 1
	}
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	evs, err := d.Log.Read(ctx, board.ID, from, limit)
	if err != nil {
		return "", err
	}
	lines := []string{"#" + board.Name() + " (" + board.ID + ")"}
	next := from
	for _, e := range evs {
		next = e.Seq + 1
		if e.Type != event.ChatPosted && e.Type != event.ChatMessage {
			continue
		}
		var p event.ChatPayload
		if err := e.Decode(&p); err != nil {
			return "", err
		}
		meta := ""
		if p.RequestID != "" {
			meta += " request_id=" + p.RequestID
		}
		if len(p.ReplyTo) > 0 {
			meta += " reply_to=" + strings.Join(p.ReplyTo, ",")
		}
		lines = append(lines, fmt.Sprintf("[%d] %s -> %s%s: %s", e.Seq, p.From, strings.Join(p.To, ", "), meta, p.Text))
	}
	lines = append(lines, fmt.Sprintf("next: %d", next))
	return strings.Join(lines, "\n"), nil
}
func (d *Daemon) BoardMessage(ctx context.Context, source, caller, ref string, to []string, text, kind string, reply []string) (string, error) {
	board, err := d.boardFor(source, caller, ref)
	if err != nil {
		return "", err
	}
	home, err := d.channel(source)
	if err != nil {
		return "", err
	}
	return home.DeliverBoard(ctx, board, caller, to, text, kind, reply, "")
}
func (d *Daemon) channelTree(ch *agent.Channel) []protocol.AgentInfo {
	if b := ch.Board(); b != nil {
		if home, err := d.channel(b.Source); err == nil {
			return home.BoardTree(b)
		}
		return nil
	}
	return ch.Tree()
}
func (d *Daemon) channelPost(ctx context.Context, ch *agent.Channel, text, source string) ([]string, error) {
	if b := ch.Board(); b != nil {
		home, err := d.channel(b.Source)
		if err != nil {
			return nil, err
		}
		return home.BoardHumanPost(ctx, ch, text, source)
	}
	return ch.Post(ctx, text, source)
}

// BoardReport lets the harness publish a terminal turn-limit answer, including
// requests queued before the limited agent could take them.
func (d *Daemon) BoardReport(ctx context.Context, source, caller, ref string, to []string, text string, reply []string) error {
	board, err := d.boardFor(source, caller, ref)
	if err != nil {
		return err
	}
	home, err := d.channel(source)
	if err != nil {
		return err
	}
	_, err = home.DeliverBoard(ctx, board, caller, to, text, tools.KindResponse, reply, "", true)
	return err
}
