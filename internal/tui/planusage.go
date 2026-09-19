package tui

import (
	"context"
	"fmt"
	"math"
	"strconv"
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

// cacheUsageMsg is a usage.cache reply: how much of the last hour's model
// calls the providers served from their prompt caches.
type cacheUsageMsg struct {
	res protocol.CacheUsageResult
	err error
}

func cacheUsageCmd(ctx context.Context, c *client.Client) tea.Cmd {
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.CacheUsage, protocol.CacheUsageParams{})
		return cacheUsageMsg{res, err}
	})
}

// onCacheUsage keeps the reply; a failed call keeps the last one.
func (m *Model) onCacheUsage(msg cacheUsageMsg) {
	if msg.err == nil {
		m.cache = msg.res
	}
}

// cacheRow is the nav's cache monitor: what share of the last hour's model
// calls came from the providers' prompt caches ("cache 94%"), grey, orange
// under 70% and red under 40% — a low share means calls are re-sending
// conversations at full price (docs/prompt-caching.md). "" before any call.
//
// The figure is the total, but the colour and the name beside it are the
// worst provider's, once it has carried enough traffic to judge
// (cacheJudgeTokens): "cache 96% · openai 2%". A total is dominated by
// whichever provider does the most work, and the regression this row exists
// for was one provider's.
func (m Model) cacheRow(width int) string {
	total := m.cache.Fresh + m.cache.Cached
	if total <= 0 {
		return ""
	}
	pct := int(m.cache.Cached * 100 / total)
	worst, worstName := pct, ""
	for _, p := range m.cache.Providers {
		if t := p.Fresh + p.Cached; t >= cacheJudgeTokens {
			if share := int(p.Cached * 100 / t); share < worst {
				worst, worstName = share, p.Provider
			}
		}
	}
	st := theme.StyleDim
	switch {
	case worst < 40:
		st = theme.StyleError
	case worst < 70:
		st = theme.StyleWarn
	}
	label, figure := "cache", fmt.Sprintf("%d%%", pct)
	if worstName != "" && worst < 70 {
		figure += fmt.Sprintf(" · %s %d%%", worstName, worst)
	}
	gap := max(1, width-ansi.StringWidth(label)-ansi.StringWidth(figure))
	return theme.StyleDim.Render(label+strings.Repeat(" ", gap)) + st.Render(figure)
}

// cacheJudgeTokens is how much input a provider must have carried in the
// window before its cache share is judged: the first call of a conversation
// caches nothing, and a handful of short calls says little.
const cacheJudgeTokens = 200_000

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

// planRow is one row of the nav's plan usage block: a window of a plan.
type planRow struct {
	plan   int // index into m.plans
	window protocol.UsageWindowInfo
	first  bool // the plan's first row, which carries its name
}

// planRowList is a row for every window of every plan with a reading, the
// plans in order and each plan's windows shortest first: a plan limited by
// five hours and by the week shows both, since either can be the one that
// stops the work.
func (m Model) planRowList() []planRow {
	var rows []planRow
	for i, p := range m.plans {
		for j, w := range p.Windows {
			rows = append(rows, planRow{plan: i, window: w, first: j == 0})
		}
	}
	return rows
}

// planUsageRows are the nav's Subscriptions section, width wide:
//
//	Subscriptions
//	ChatGPT 5h ━━━━━━──────  38%
//	        wk ━━──────────  17%
//
// the title, a row per window with the plan's name on its first, then a blank
// row; nothing at all without a reading.
func (m Model) planUsageRows(width int, now time.Time) []string {
	list := m.planRowList()
	if len(list) == 0 {
		return nil
	}
	nameW := 0
	for _, p := range m.plans {
		if len(p.Windows) > 0 {
			nameW = max(nameW, ansi.StringWidth(p.Name))
		}
	}
	nameW = min(nameW, max(1, width/3))
	rows := make([]string, 0, len(list)+2)
	rows = append(rows, navSection("Subscriptions"))
	for _, r := range list {
		name := ""
		if r.first {
			name = ansi.Truncate(m.plans[r.plan].Name, nameW, "…")
		}
		label := name + strings.Repeat(" ", nameW-ansi.StringWidth(name))
		if span := windowSpan(r.window.Minutes); span != "" {
			label += " " + span
		}
		rows = append(rows, planUsageRow(label, windowUsed(r.window, now), width))
	}
	return append(rows, "")
}

// windowSpan names a window by its length: "5h", "wk", "mo", "3d"; "" when
// the provider did not say.
func windowSpan(minutes int) string {
	switch {
	case minutes <= 0:
		return ""
	case minutes == 7*24*60:
		return "wk"
	case minutes >= 28*24*60 && minutes <= 31*24*60:
		return "mo"
	case minutes%(24*60) == 0:
		return strconv.Itoa(minutes/(24*60)) + "d"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "h"
	}
	return strconv.Itoa(minutes) + "m"
}

// planCommand is /plan [provider]: the chart of that subscription's plan
// usage, or of the first plan with a reading.
func (m *Model) planCommand(rest string) tea.Cmd {
	if len(m.plans) == 0 {
		return m.setStatus("no plan usage yet: it comes with the next model call, or when the daemon next asks the provider", false)
	}
	p := m.plans[0]
	for _, q := range m.plans {
		if strings.EqualFold(q.Provider, rest) || strings.EqualFold(q.Name, rest) {
			p = q
		}
	}
	return m.openPlanUsage(p.Provider, p.Name)
}

// planAt is the plan one of whose rows the nav draws at header row y (the block
// starts at row 2), and whether y is one of those rows.
func (m Model) planAt(y int) (protocol.PlanUsageInfo, bool) {
	if r, ok := m.navRowAt(y); ok && r.id == navPlan {
		return m.plans[r.plan], true
	}
	return protocol.PlanUsageInfo{}, false
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
	name = ansi.Truncate(name, max(1, width*2/3), "…")
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

// recapCommand is /recap [minutes|off]: how long the channel may go without
// the human hearing from it before its main agent is asked for a status
// report. With no argument it reports the setting.
func (m *Model) recapCommand(rest string) tea.Cmd {
	rest = strings.TrimSpace(rest)
	switch {
	case rest == "":
		if m.channel.Recap == 0 {
			return m.setStatus("recap is off: /recap 10 asks for a status report after 10 quiet minutes", false)
		}
		return m.setStatus(fmt.Sprintf("recap every %d min of quiet; /recap off turns it off", m.channel.Recap), false)
	case strings.EqualFold(rest, "off"), rest == "0":
		return setRecapCmd(m.ctx, m.c, m.channelID, 0)
	}
	rest = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(rest, "minutes")), "min"), "m")
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil || n <= 0 {
		return m.setStatus("usage: /recap <minutes> or /recap off", true)
	}
	return setRecapCmd(m.ctx, m.c, m.channelID, n)
}

// trustRow is the nav's project-configuration monitor: whether the selected
// channel's project layer is trusted. Untrusted is the one that matters —
// its roles, skills and commands are not loaded, so the channel looks like
// it has one role and nothing else says why — but the row stays either way,
// because "trusted" is what makes its absence meaningful. "" for a
// directory with no project configuration at all, where there is nothing to
// trust.
func (m Model) trustRow(width int) string {
	if m.channel.TrustFiles == 0 {
		return ""
	}
	figure, st := "trusted", theme.StyleDim
	if m.channel.TrustPending {
		figure, st = "untrusted", theme.StyleWarn
	}
	const label = "project"
	gap := max(1, width-ansi.StringWidth(label)-ansi.StringWidth(figure))
	return theme.StyleDim.Render(label+strings.Repeat(" ", gap)) + st.Render(figure)
}

// trustClick is what the project row does: untrusted, it asks for the
// decision again, since that is the only thing worth doing about it;
// trusted, it opens the configuration the row is reporting on.
func (m *Model) trustClick() tea.Cmd {
	if !m.channel.TrustPending {
		return m.openConfigEditor(false)
	}
	return m.askTrust()
}

// askTrust asks the daemon to raise the channel's trust prompt again.
// Resuming a channel re-checks its project layer, which is what publishes
// the prompt, so the row is the way back to a decision that was skipped or
// invalidated by an edit.
func (m *Model) askTrust() tea.Cmd {
	if m.channelID == "" {
		return nil
	}
	channel := m.channelID
	return resultCmd(m.ctx, "", func(ctx context.Context) error {
		return call(ctx, m.c, protocol.ChannelResume, protocol.ChannelRef{Channel: channel})
	})
}
