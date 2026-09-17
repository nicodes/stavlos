package tui

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/pkg/client"
)

// The plan usage bars at the top of the nav: how much of each signed-in
// subscription's allowance is used, per window, as its latest model call's
// response reported it (docs/plan-usage.md). The daemon only listens; the
// TUI asks it on the catalog tick.

// planUsageMsg is a plan.usage reply.
type planUsageMsg struct {
	res protocol.PlanUsageResult
	err error
}

func planUsageCmd(ctx context.Context, c *client.Client) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.PlanUsage, protocol.None{})
		return planUsageMsg{res, err}
	})
}

// onPlanUsage keeps the reply; a failed call keeps the last one.
func (m *Model) onPlanUsage(msg planUsageMsg) {
	if msg.err == nil {
		m.plans = msg.res.Plans
	}
}

// planUsageRows are the nav's plan usage block, width wide: a row per plan,
// "ChatGPT ━━━━━━──────  38%", the bar and percent of its most used window
// (the limit that binds first), then a blank row; none without a reading.
func (m Model) planUsageRows(width int, now time.Time) []string {
	var rows []string
	for _, p := range m.plans {
		if len(p.Windows) == 0 {
			continue
		}
		used := 0.0
		for _, w := range p.Windows {
			used = max(used, windowUsed(w, now))
		}
		rows = append(rows, planUsageRow(p.Name, used, width))
	}
	if len(rows) > 0 {
		rows = append(rows, "")
	}
	return rows
}

// windowUsed is a window's percent used, clamped; a window whose reset has
// passed since the reading has started over, so it reads 0.
func windowUsed(w protocol.UsageWindowInfo, now time.Time) float64 {
	if !w.ResetsAt.IsZero() && !w.ResetsAt.After(now) {
		return 0
	}
	return min(max(w.UsedPercent, 0), 100)
}

// planUsageRow draws "name ━━━━━━──────  38%": the bar in accent, orange
// from 70% and red from 90%.
func planUsageRow(name string, used float64, width int) string {
	const pctW = 4
	name = ansi.Truncate(name, max(1, width/2), "…")
	barW := max(1, width-ansi.StringWidth(name)-1-1-pctW)
	filled := int(math.Round(used / 100 * float64(barW)))
	bar := theme.StyleAccent
	switch {
	case used >= 90:
		bar = theme.StyleError
	case used >= 70:
		bar = theme.StyleWarn
	}
	pct := fmt.Sprintf("%*s", pctW, fmt.Sprintf("%.0f%%", used))
	return theme.StyleDim.Render(name+" ") + bar.Render(strings.Repeat("━", filled)) + theme.StyleRule.Render(strings.Repeat("─", barW-filled)) + theme.StyleDim.Render(" "+pct)
}
