package tui

import (
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

// TestSpinnerWhileWaiting: an agent between turns that waits on another
// agent or a job shows the spinner indicator naming them, and the spinner
// keeps ticking; with nothing to wait on there is neither, and the channel
// chat keeps only its own loader.
func TestSpinnerWhileWaiting(t *testing.T) {
	m := channelModel()
	m.selected = 0
	m.refreshViewport()
	if v := stripANSI(m.vp.View()); strings.Contains(v, "Waiting…") || m.animating() {
		t.Fatalf("an idle agent shows no spinner (animating %v):\n%s", m.animating(), v)
	}
	m.agents[0].State = protocol.AgentWaiting
	m.agents[0].Awaiting = []string{"b"}
	m.agents[0].Jobs = []protocol.JobInfo{{ID: "j1", Label: "go test"}}
	m.refreshViewport()
	if v := stripANSI(m.vp.View()); !strings.Contains(v, "Waiting… · @scout · 1 job") || !m.animating() {
		t.Fatalf("a waiting agent keeps its spinner (animating %v):\n%s", m.animating(), v)
	}
	m.superChat = true
	m.refreshViewport()
	if v := stripANSI(m.vp.View()); strings.Contains(v, "Waiting…") {
		t.Fatalf("the channel chat has its own loader, not an agent's:\n%s", v)
	}
}
