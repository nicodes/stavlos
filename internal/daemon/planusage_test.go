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

// TestPlanUsageHistory: readings become points (an unchanged one inside the
// gap moves the last point instead of adding one), the series buckets the
// highest reading per bucket and carries the last known one forward, and
// the points survive a restart.
func TestPlanUsageHistory(t *testing.T) {
	dir := t.TempDir()
	d := &Daemon{Registry: registry.New(nil).WithStore(auth.Open(dir + "/auth.json")), DataDir: dir}
	d.loadPlanUsage()
	t0 := time.Now().Add(-time.Hour).UTC()
	at := func(offset time.Duration, pct float64) model.PlanUsage {
		return model.PlanUsage{Observed: t0.Add(offset), Windows: []model.UsageWindow{{UsedPercent: pct, Minutes: 10080, ResetsAt: t0.Add(24 * time.Hour)}}}
	}
	d.recordPlanUsage("openai", at(0, 10))
	d.recordPlanUsage("openai", at(10*time.Second, 10)) // unchanged and recent: no new point
	d.recordPlanUsage("openai", at(30*time.Minute, 60))
	d.recordPlanUsage("openai", at(20*time.Minute, 99)) // older than the last: dropped
	if n := len(d.planHistory["openai"]); n != 2 {
		t.Fatalf("points: %d (%+v)", n, d.planHistory["openai"])
	}
	series := d.planUsageSeries("openai", t0, t0.Add(time.Hour), 4)
	if len(series) != 4 || series[0] != 10 || series[1] != 10 || series[2] != 60 || series[3] != 60 {
		t.Fatalf("buckets carry the last reading forward: %v", series)
	}
	d.savePlanUsage()
	next := &Daemon{Registry: registry.New(nil).WithStore(auth.Open(dir + "/auth.json")), DataDir: dir}
	next.loadPlanUsage()
	if got := next.planUsageSeries("openai", t0, t0.Add(time.Hour), 2); len(got) != 2 || got[1] != 60 {
		t.Fatalf("restored: %v", got)
	}
	// a window that had already reset when it was read counts as 0
	if pct := planUsagePercent(model.PlanUsage{Observed: t0, Windows: []model.UsageWindow{{UsedPercent: 80, ResetsAt: t0.Add(-time.Minute)}}}); pct != 0 {
		t.Fatalf("reset window: %v", pct)
	}
}
