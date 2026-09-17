package daemon

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/model/registry"
	"github.com/nicodes/stavlos/internal/protocol"
)

// planUsageFile keeps the latest observed plan usage per provider, so a
// restarted daemon shows the last reading (with its age) before its first
// model call (docs/plan-usage.md).
const planUsageFile = "plan-usage.json"

// loadPlanUsage restores the kept readings into the registry and starts
// keeping new ones.
func (d *Daemon) loadPlanUsage() {
	path := filepath.Join(d.DataDir, planUsageFile)
	if b, err := os.ReadFile(path); err == nil {
		var kept map[string]model.PlanUsage
		if err := json.Unmarshal(b, &kept); err != nil {
			log.Printf("plan usage: %s: %v", path, err)
		}
		for provider, u := range kept {
			d.Registry.SeedPlanUsage(provider, u)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("plan usage: %v", err)
	}
	d.Registry.OnPlanUsage(func(string, model.PlanUsage) { d.savePlanUsage() })
}

// savePlanUsage writes every kept reading, replacing the file whole.
func (d *Daemon) savePlanUsage() {
	d.planUsageMu.Lock()
	defer d.planUsageMu.Unlock()
	b, err := json.Marshal(d.Registry.PlanUsage())
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
