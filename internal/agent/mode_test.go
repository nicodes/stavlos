package agent

import (
	"context"
	"testing"
)

// TestChannelStartsInTheConfiguredMode: a new channel starts in the
// config's mode, and the log keeps it: recovery restores it whatever the
// config says by then.
func TestChannelStartsInTheConfiguredMode(t *testing.T) {
	s, h := newTestChannel(t, testConfig{json: `{"model":"fake/m1","mode":"auto"}`}, &fakeModel{})
	if s.Mode() != "auto" {
		t.Fatalf("a new channel starts in the configured mode: %s", s.Mode())
	}
	cfg := *s.Config()
	cfg.Mode = "ask"
	h.mu.Lock()
	evs := append(h.events[:0:0], h.events...)
	h.mu.Unlock()
	s2, err := Recover(context.Background(), h, s.ID, s.Dir, s.Created, &cfg, evs)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Mode() != "auto" {
		t.Fatalf("recovery keeps the mode the channel was created in: %s", s2.Mode())
	}
}
