package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

// An embedded bridge can attach while recovered agents publish prompts.
func TestAttachIdentityConcurrentWithPromptDelivery(t *testing.T) {
	cl := &client{id: "client", name: "anonymous", tier: protocol.TierInteractive, send: func([]byte, bool) {}}
	d := &Daemon{clients: map[string]*client{cl.id: cl}}
	c := &conn{d: d, cl: cl}
	p, _ := json.Marshal(protocol.AttachParams{Client: "discord", Tier: protocol.TierFallback})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			if _, err := handlers[protocol.MAttach].h(context.Background(), c, p); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			d.notifyPrompt(protocol.PromptNotification{Action: protocol.ActionRequested}, []protocol.Tier{protocol.TierInteractive, protocol.TierFallback})
			_, _ = cl.identity()
		}
	}()
	wg.Wait()
	if name, tier := cl.identity(); name != "discord" || tier != protocol.TierFallback {
		t.Fatalf("identity: %s %s", name, tier)
	}
}
