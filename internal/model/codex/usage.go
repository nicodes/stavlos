package codex

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/model"
)

// usageFromHeaders reads the shared Codex allowance's windows from a
// Responses call's headers, as the Codex CLI does (codex-api
// rate_limits.rs): x-codex-{primary,secondary}-{used-percent,
// window-minutes,reset-at}. ok is false when no window has data
// (docs/plan-usage.md).
func usageFromHeaders(h http.Header, now time.Time) (model.PlanUsage, bool) {
	u := model.PlanUsage{Observed: now}
	for _, name := range []string{"primary", "secondary"} {
		prefix := "x-codex-" + name + "-"
		used, err := strconv.ParseFloat(strings.TrimSpace(h.Get(prefix+"used-percent")), 64)
		if err != nil || math.IsNaN(used) || math.IsInf(used, 0) {
			continue
		}
		w := model.UsageWindow{UsedPercent: min(max(used, 0), 100)}
		if m, err := strconv.Atoi(strings.TrimSpace(h.Get(prefix + "window-minutes"))); err == nil && m > 0 {
			w.Minutes = m
		}
		reset, err := strconv.ParseInt(strings.TrimSpace(h.Get(prefix+"reset-at")), 10, 64)
		if err == nil && reset > 0 {
			w.ResetsAt = time.Unix(reset, 0)
		}
		if used == 0 && w.Minutes == 0 && w.ResetsAt.IsZero() {
			continue // no data, as Codex reads it
		}
		u.Windows = append(u.Windows, w)
	}
	return u, len(u.Windows) > 0
}
