package daemon

import (
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/auth"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/registry"
)

// TestPlanUsageKeptAcrossRestarts: an observed reading is written to the
// data directory, restored by the next daemon, and reported only while
// its provider is signed in.
func TestPlanUsageKeptAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	store := auth.Open(dir + "/auth.json")
	d := &Daemon{Registry: registry.New(nil).WithStore(store), DataDir: dir}
	d.loadPlanUsage()
	observed := time.Unix(1_700_000_000, 0).UTC()
	d.Registry.SeedPlanUsage("openai", model.PlanUsage{Observed: observed, Windows: []model.UsageWindow{{UsedPercent: 42, Minutes: 300}}})
	d.savePlanUsage()
	if got := d.planUsage(); len(got.Plans) != 0 {
		t.Fatalf("not signed in: %+v", got)
	}

	next := &Daemon{Registry: registry.New(nil).WithStore(store), DataDir: dir}
	next.loadPlanUsage()
	if err := store.Set("openai", auth.Credential{Type: "oauth", Access: "a", Refresh: "r", Expires: time.Now().Add(time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	got := next.planUsage()
	if len(got.Plans) != 1 || got.Plans[0].Name != "ChatGPT" || !got.Plans[0].Observed.Equal(observed) || got.Plans[0].Windows[0].UsedPercent != 42 || got.Plans[0].Windows[0].Minutes != 300 {
		t.Fatalf("restored: %+v", got)
	}
}
