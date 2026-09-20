package daemon

import (
	"context"
	"time"
)

// service is something the daemon runs beside its channels: a front door for
// a kind of client, or a loop that watches a clock. Discord, the web
// listener, the plan poller, the recap loop and the watcher of Discord's
// status each used to be started by a line of their own in Main or Serve and
// stopped, if at all, by a line of their own in Close. They are a list now:
// started in order once the socket is listening, stopped in reverse.
type service struct {
	name  string
	start func(ctx context.Context) // returns at once; what it starts ends with ctx
	stop  func()                    // nil when ending ctx is all it needs
}

// services lists what this daemon runs. The front doors are the daemon's
// own; Main adds the loops a real daemon runs and a test's does not.
func (d *Daemon) services() []service {
	list := []service{
		{name: "discord", start: func(context.Context) {
			if d.Discord != nil {
				d.Discord.Start()
			}
		}, stop: d.closeDiscord},
		{name: "web", start: d.startWeb, stop: d.closeWeb},
	}
	return append(list, d.loops...)
}

// startServices runs once the socket is listening, since a front door's
// clients arrive through it.
func (d *Daemon) startServices(ctx context.Context) {
	for _, s := range d.services() {
		s.start(ctx)
	}
}

// stopServices is the first thing Close does: nothing new arrives while the
// channels stop.
func (d *Daemon) stopServices() {
	list := d.services()
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].stop != nil {
			list[i].stop()
		}
	}
}

// runLoops adds what a real daemon runs in the background (docs/plan-usage.md,
// the recap clock, the notice of Discord's status changing).
func (d *Daemon) runLoops() {
	d.loops = []service{
		{name: "plan usage", start: func(ctx context.Context) {
			// The subscriptions that report plan usage nowhere but a usage
			// endpoint are asked once now, then only as they are used.
			d.Registry.EnablePlanPolling()
			go d.Registry.PollAllPlanUsage(ctx, 0)
		}},
		{name: "recap", start: func(ctx context.Context) { go d.recapLoop(ctx) }},
		{name: "discord status", start: func(ctx context.Context) { d.watchDiscord(ctx, time.Second) }},
	}
}
