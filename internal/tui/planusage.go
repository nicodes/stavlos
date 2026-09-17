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
	"github.com/nicodes/stavlos/internal/tui/format"
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

// planUsageRows are the nav's plan usage block, width wide: for each plan
// "ChatGPT" with the reading's age at the right, then a row per window
// ("5h ━━━━━━──────  38%  2h": its length, the bar, the percent used and
// the time to its reset), then a blank row; none without a reading.
func (m Model) planUsageRows(width int, now time.Time) []string {
	var rows []string
	for _, p := range m.plans {
		if len(p.Windows) == 0 {
			continue
		}
		age := format.Ago(p.Observed, now)
		gap := max(1, width-ansi.StringWidth(p.Name)-ansi.StringWidth(age))
		rows = append(rows, theme.StyleDim.Render(ansi.Truncate(p.Name+strings.Repeat(" ", gap)+age, width, "…")))
		for _, w := range p.Windows {
			rows = append(rows, usageWindowRow(w, width, now))
		}
	}
	if len(rows) > 0 {
		rows = append(rows, "")
	}
	return rows
}

// usageWindowRow draws one window. A window whose reset has passed since
// the reading has started over: it reads 0% with its reset unknown.
func usageWindowRow(w protocol.UsageWindowInfo, width int, now time.Time) string {
	used, reset := w.UsedPercent, ""
	switch {
	case !w.ResetsAt.IsZero() && !w.ResetsAt.After(now):
		used = 0
	case !w.ResetsAt.IsZero():
		reset = untilText(w.ResetsAt.Sub(now))
	}
	used = min(max(used, 0), 100)
	const labelW, pctW, resetW = 3, 4, 3
	barW := max(1, width-labelW-1-1-pctW-1-resetW)
	filled := int(math.Round(used / 100 * float64(barW)))
	bar := theme.StyleAccent
	switch {
	case used >= 90:
		bar = theme.StyleError
	case used >= 70:
		bar = theme.StyleWarn
	}
	label := fmt.Sprintf("%-*s", labelW, windowLabel(w.Minutes))
	pct := fmt.Sprintf("%*s", pctW, fmt.Sprintf("%.0f%%", used))
	return theme.StyleDim.Render(label+" ") + bar.Render(strings.Repeat("━", filled)) + theme.StyleRule.Render(strings.Repeat("─", barW-filled)) +
		theme.StyleDim.Render(" "+pct+" "+fmt.Sprintf("%*s", resetW, reset))
}

// windowLabel names a window by its length: "5h", "7d", "30m"; "" when
// unknown.
func windowLabel(minutes int) string {
	switch {
	case minutes <= 0:
		return ""
	case minutes%1440 == 0:
		return fmt.Sprintf("%dd", minutes/1440)
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	}
	return fmt.Sprintf("%dm", minutes)
}

// untilText is how long until a reset: "now", "45m", "3h", "6d".
func untilText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
