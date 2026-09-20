package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nicodes/stavlos/internal/protocol"
)

// changed tells every attached client that what names is no longer what it
// was last told. Clients used to find out by asking: each TUI asked for
// Discord's status every 2 s and the web UI's every 5 s, for as long as it
// ran.
func (d *Daemon) changed(what string) {
	b, _ := json.Marshal(protocol.ChangedNotification{What: what})
	line := notification(protocol.NChanged, b)
	for _, c := range d.clientList() {
		c.send(line, true) // a hint: a client that misses one still asks now and then
	}
}

// watchDiscord notices Discord's status changing. The service changes it in
// a dozen places and from its own goroutines (connecting, connected, an
// error, a guild's name arriving), so the daemon compares what it reports:
// one comparison a second in this process, with no clients attached too,
// instead of a request every two seconds from each of them.
func (d *Daemon) watchDiscord(ctx context.Context, every time.Duration) {
	if d.Discord == nil {
		return
	}
	last := d.Discord.Status()
	go d.watchDiscordFrom(ctx, every, last)
}

func (d *Daemon) watchDiscordFrom(ctx context.Context, every time.Duration, last protocol.DiscordStatus) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if now := d.Discord.Status(); now != last {
				last = now
				d.changed(protocol.ChangedDiscord)
			}
		}
	}
}
