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
	case event.ChannelRenamed:
		var p event.NamePayload
		if err := e.Decode(&p); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE channels SET name = ? WHERE id = ?`, p.Name, e.Channel)
		return err
	case event.ChannelArchived:
		_, err := tx.ExecContext(ctx, `UPDATE channels SET archived = 1 WHERE id = ?`, e.Channel)
		return err
	case event.PromptQueued, event.SteerReceived:
		if title := titleOf(e); title != "" {
			_, err := tx.ExecContext(ctx, `UPDATE channels SET title = ? WHERE id = ? AND title = ''`, title, e.Channel)
			return err
		}
	}
	return nil
}

// titleOf is the title an event can give its channel: the first line of the
// human's message.
func titleOf(e event.Event) string {
	var p event.TextPayload
	if e.Decode(&p) != nil || !strings.HasPrefix(p.Source, "human:") {
		return ""
	}
	t := strings.TrimSpace(p.Text)
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}
