package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestCancelStopsOwnedBackgroundCommandWithoutWakingAgent(t *testing.T) {
	for _, lostLog := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded", true: "log-failure"}[lostLog], func(t *testing.T) {
			fm := &fakeModel{steps: []step{reply(call("heartbeat", "shell", `{"command":"while :; do printf tick >> heartbeat; sleep 0.02; done","background":true}`)), reply(text("waiting"))}}
			c, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","policy":{"shell":"allow"},"sandbox":{"enabled":false}}`}, fm)
			if err := c.SetMode(context.Background(), protocol.ModeYolo); err != nil {
				t.Fatal(err)
			}
			runTurn(t, c, h, "start a job")
			root := c.Root()
			path := filepath.Join(c.Dir(), "heartbeat")
			waitUntil(t, h, func() bool { info, err := os.Stat(path); return err == nil && info.Size() > 0 })
			c.mu.Lock()
			var run *jobRun
			for _, job := range root.jobs {
				run = job
			}
			c.mu.Unlock()
			if run == nil {
				t.Fatal("job not adopted")
			}
			if lostLog {
				h.mu.Lock()
				h.failType = event.JobStopped
				h.mu.Unlock()
			}
			root.Cancel()
			select {
			case <-run.handle.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("cancel did not stop the command")
			}
			before, _ := os.ReadFile(path)
			time.Sleep(100 * time.Millisecond)
			after, _ := os.ReadFile(path)
			if string(before) != string(after) || len(root.Info().Jobs) != 0 {
				t.Fatal("cancelled command kept changing files")
			}
			if len(fm.requests()) != 2 || len(h.ofType(event.JobFinished, root.ID)) != 0 {
				t.Fatal("cancelled job woke the model")
			}
			if !lostLog && len(h.ofType(event.JobStopped, root.ID)) != 1 {
				t.Fatal("job cancellation absent from evidence")
			}
			if got := runTurn(t, c, h, "resume deliberately"); got.Turn != 2 {
				t.Fatalf("explicit resume failed: %+v", got)
			}
		})
	}
}

func TestCancelledTurnCannotAdoptALateJob(t *testing.T) {
	c, _ := newTestChannel(t, testConfig{}, &fakeModel{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Root().adoptJob(ctx, "late", nil, time.Second); err == nil {
		t.Fatal("cancelled turn adopted a background process")
	}
	if len(c.Root().Info().Jobs) != 0 {
		t.Fatal("late job changed runtime state")
	}
}
