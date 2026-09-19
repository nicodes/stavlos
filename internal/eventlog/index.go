package eventlog

import (
	"context"
	"database/sql"
	"strings"

	"github.com/nicodes/stavlos/internal/event"
)

// index keeps the channels table in step with a channel's events, inside
// the transaction that appends them: a channel's row exists exactly when
// its channel.created does, and its name, archived flag and title can
// never disagree with the log. The table is a projection, rebuilt by
// nothing else.
func index(ctx context.Context, tx *sql.Tx, e event.Event) error {
	switch e.Type {
	case event.ChannelCreated:
		var p event.ChannelCreatedPayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO channels(id, name, dir, created) VALUES(?,?,?,?)`, e.Channel, p.Name, p.Dir, e.Time.UnixNano())
		return err
	case event.ChannelUpdated:
		var p event.ChannelUpdatedPayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		if p.Name != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE channels SET name = ? WHERE id = ?`, *p.Name, e.Channel); err != nil {
				return err
			}
		}
		if p.Dir != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE channels SET dir = ? WHERE id = ?`, *p.Dir, e.Channel); err != nil {
				return err
			}
		}
		return nil
	case event.ChannelArchived:
		_, err := tx.ExecContext(ctx, `UPDATE channels SET archived = 1 WHERE id = ?`, e.Channel)
		return err
	case event.AssistantMessage:
		return indexUsage(ctx, tx, e)
	case event.ChatPosted, event.InputQueued:
		if title := titleOf(e); title != "" {
			_, err := tx.ExecContext(ctx, `UPDATE channels SET title = ? WHERE id = ? AND title = ''`, title, e.Channel)
			return err
		}
	}
	return nil
}

// titleOf is the title an event can give its channel: the first line of the
// human's message, a channel post or one typed into an agent's chat.
func titleOf(e event.Event) string {
	var text string
	switch e.Type {
	case event.ChatPosted:
		var p event.ChatPayload
		if e.Decode(&p) != nil {
			return ""
		}
		text = p.Text
	case event.InputQueued:
		var in event.Input
		if e.Decode(&in) != nil || in.Kind != event.InputPrompt && in.Kind != event.InputSteer || in.Post != "" {
			return "" // an agent's input, or a post (titled by its chat.posted)
		}
		text = in.Text
	}
	t := strings.TrimSpace(text)
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}
