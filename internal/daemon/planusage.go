package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/protocol"
)

// planUsageFile keeps each provider's latest observed plan usage and a
// rolling history of what was observed before it, so a restarted daemon
// shows the last reading (with its age) before its first model call, and
// can chart the plan over time (docs/plan-usage.md).
const planUsageFile = "plan-usage.json"

// planUsageHistory bounds the kept points, and planUsageGap how close
// together two of them may be: a point every model call would be mostly
// duplicates, and the chart reads a percentage per bucket.
const (
	planUsageHistory = 5_000
	planUsageGap     = time.Minute
)

// planUsageFileEntry is one provider in the file: the reading the registry
// restores, and the points behind it.
type planUsageFileEntry struct {
	model.PlanUsage
	History []protocol.PlanUsagePoint `json:"history,omitempty"`
}

// planUsagePercent is the most used window of a reading: what the nav's row
// shows, and what the chart plots.
func planUsagePercent(u model.PlanUsage) float64 {
	used := 0.0
	for _, w := range u.Windows {
		if !w.ResetsAt.IsZero() && !w.ResetsAt.After(u.Observed) {
			continue // that window had reset by the time it was read
		}
		used = max(used, min(max(w.UsedPercent, 0), 100))
	}
	return used
}

// loadPlanUsage restores the kept readings into the registry and starts
// keeping new ones.
func (d *Daemon) loadPlanUsage() {
	path := filepath.Join(d.DataDir, planUsageFile)
	if b, err := os.ReadFile(path); err == nil {
		var kept map[string]planUsageFileEntry
		if err := json.Unmarshal(b, &kept); err != nil {
			log.Printf("plan usage: %s: %v", path, err)
		}
		d.planUsageMu.Lock()
		d.planHistory = map[string][]protocol.PlanUsagePoint{}
		for provider, e := range kept {
			d.planHistory[provider] = e.History
		}
		d.planUsageMu.Unlock()
		for provider, e := range kept {
			d.Registry.SeedPlanUsage(provider, e.PlanUsage)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("plan usage: %v", err)
	}
	d.Registry.OnPlanUsage(func(provider string, u model.PlanUsage) {
		d.recordPlanUsage(provider, u)
		d.savePlanUsage()
	})
}

// recordPlanUsage adds a point for a new reading, unless the last one is
// younger than planUsageGap and says the same.
func (d *Daemon) recordPlanUsage(provider string, u model.PlanUsage) {
	if u.Observed.IsZero() || len(u.Windows) == 0 {
		return
	}
	pt := protocol.PlanUsagePoint{At: u.Observed.UTC(), UsedPercent: planUsagePercent(u)}
	d.planUsageMu.Lock()
	defer d.planUsageMu.Unlock()
	if d.planHistory == nil {
		d.planHistory = map[string][]protocol.PlanUsagePoint{}
	}
	pts := d.planHistory[provider]
	if n := len(pts); n > 0 {
		if !pt.At.After(pts[n-1].At) {
			return // an older reading, or the same one again
		}
		if pt.UsedPercent == pts[n-1].UsedPercent && pt.At.Sub(pts[n-1].At) < planUsageGap {
			pts[n-1].At = pt.At // unchanged and recent: move the last point along
			return
		}
	}
	pts = append(pts, pt)
	if len(pts) > planUsageHistory {
		pts = append([]protocol.PlanUsagePoint(nil), pts[len(pts)-planUsageHistory:]...)
	}
	d.planHistory[provider] = pts
}

// planUsageFirst is when a provider's kept history starts, or an hour
// before to when there is none: a chart of "all" spans at least that.
func (d *Daemon) planUsageFirst(provider string, to time.Time) time.Time {
	d.planUsageMu.Lock()
	defer d.planUsageMu.Unlock()
	first := to.Add(-time.Hour)
	if pts := d.planHistory[provider]; len(pts) > 0 && pts[0].At.Before(first) {
		first = pts[0].At
	}
	return first
}

// planUsageSeries is a provider's history bucketed into percentages: the
// highest reading in each bucket, with an empty bucket carrying the last
// known one forward, since usage holds until the next reading.
func (d *Daemon) planUsageSeries(provider string, from, to time.Time, buckets int) []float64 {
	d.planUsageMu.Lock()
	pts := append([]protocol.PlanUsagePoint(nil), d.planHistory[provider]...)
	d.planUsageMu.Unlock()
	out := make([]float64, buckets)
	span := to.Sub(from)
	if span <= 0 {
		return out
	}
	seen := make([]bool, buckets)
	before := -1.0 // the last reading before the span
	for _, p := range pts {
		switch {
		case p.At.Before(from):
			before = p.UsedPercent
		case !p.At.Before(to):
		default:
			i := min(int(p.At.Sub(from)*time.Duration(buckets)/span), buckets-1)
			out[i], seen[i] = max(out[i], p.UsedPercent), true
		}
	}
	last := before
	for i := range out {
		if !seen[i] {
			if last < 0 {
				continue // nothing known yet at this point
			}
			out[i] = last
			continue
		}
		last = out[i]
	}
	return out
}

// savePlanUsage writes every kept reading, replacing the file whole.
func (d *Daemon) savePlanUsage() {
	d.planUsageMu.Lock()
	defer d.planUsageMu.Unlock()
	kept := map[string]planUsageFileEntry{}
	for provider, u := range d.Registry.PlanUsage() {
		kept[provider] = planUsageFileEntry{PlanUsage: u, History: d.planHistory[provider]}
	}
	for provider, pts := range d.planHistory { // points whose reading the registry no longer holds
		if _, ok := kept[provider]; !ok {
			kept[provider] = planUsageFileEntry{History: pts}
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return
	}
	path := filepath.Join(d.DataDir, planUsageFile)
	f, err := os.CreateTemp(d.DataDir, ".plan-usage-*")
	if err != nil {
		log.Printf("plan usage: %v", err)
		return
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(f.Name())
		log.Printf("plan usage: %v", errors.Join(werr, cerr))
		return
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		log.Printf("plan usage: %v", err)
	}
}

// planUsage is the kept readings of the providers signed in now.
func (d *Daemon) planUsage() protocol.PlanUsageResult {
	var out protocol.PlanUsageResult
	for provider, u := range d.Registry.PlanUsage() {
		if st, ok := d.Registry.Status(provider); !ok || !st.Connected || len(u.Windows) == 0 {
			continue
		}
		info := protocol.PlanUsageInfo{Provider: provider, Name: registry.SubscriptionName(provider), Observed: u.Observed}
		for _, w := range u.Windows {
			info.Windows = append(info.Windows, protocol.UsageWindowInfo{UsedPercent: w.UsedPercent, Minutes: w.Minutes, ResetsAt: w.ResetsAt})
		}
		out.Plans = append(out.Plans, info)
	}
	sort.Slice(out.Plans, func(i, j int) bool { return out.Plans[i].Provider < out.Plans[j].Provider })
	return out
}

// recapTick is how often the daemon looks for a channel whose recap is due;
// the interval itself is per channel and at least a minute.
const recapTick = 20 * time.Second

// recapLoop asks each channel whose recap is due for a status report
// (agent.Channel.MaybeRecap decides), until ctx ends.
func (d *Daemon) recapLoop(ctx context.Context) {
	t := time.NewTicker(recapTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, c := range d.channelList() {
				if err := c.MaybeRecap(ctx, now); err != nil {
					log.Printf("recap %s: %v", c.ID, err)
				}
			}
		}
	}
}
