package tui

import (
	"context"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/dialog"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/pkg/client"
)

// The usage dialogs chart tokens or cost over time as a bar graph: the
// system's (every channel), a channel's (every agent's) or one agent's. The
// nav's usage rows open them, the tokens figure one and the cost figure the
// other; /tokens and /cost open them on the selected chat.

// usageKind is what a usage dialog charts.
type usageKind int

const (
	usageTokens usageKind = iota
	usageCost
)

// usageRanges are the spans a usage dialog charts, ←/→ between them; "all"
// runs from the first model call.
var usageRanges = []struct {
	name string
	d    time.Duration
}{
	{"all", 0},
	{"30d", 30 * 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"24h", 24 * time.Hour},
	{"1h", time.Hour},
}

// usageChartRows is how many rows tall the bars are.
const usageChartRows = 8

// usageAxisW is the width of the value labels left of the bars.
const usageAxisW = 8

// usageDialog is the open usage dialog: what it charts, whose, over which
// range, and the last series the daemon returned.
type usageDialog struct {
	kind           usageKind
	label          string // "System", "#proj" or "@main"
	channel, agent string // "" for every channel; agent "" for the whole channel
	rng            int    // index into usageRanges
	series         *protocol.UsageSeriesResult
	err            string
	epoch          uint64 // a reply for an older request is dropped
}

// usageMsg is a usage.series reply.
type usageMsg struct {
	epoch uint64
	res   protocol.UsageSeriesResult
	err   error
}

// openUsage opens the kind dialog on the system (system true), or else on
// the selected chat: the channel's in its chat, the agent's in its own.
func (m *Model) openUsage(kind usageKind, system bool) tea.Cmd {
	d := usageDialog{kind: kind, label: "System", rng: m.usage.rng}
	switch a := m.selectedAgent(); {
	case system:
	case a != nil && !m.superChat:
		d.label, d.channel, d.agent = "@"+a.Name, m.channelID, a.ID
	default:
		d.label, d.channel = channelLabel(m.channel), m.channelID
	}
	d.epoch = m.usage.epoch
	var closed tea.Cmd
	if m.ov != nil {
		closed = m.closeOverlay() // one dialog at a time
	}
	m.usage = d
	return tea.Batch(closed, m.setFocus(focusUsage), m.usageFetch())
}

// usageOnSelectedChat reports whether the open usage dialog charts the
// selected chat: the agent whose chat is open, or the channel in its chat.
func (m Model) usageOnSelectedChat() bool {
	if m.superChat {
		return m.usage.channel == m.channelID && m.usage.agent == ""
	}
	return m.usage.agent != "" && m.usage.agent == m.selectedID()
}

// usageFetch asks the daemon for the open dialog's series, one bucket per
// column of its chart.
func (m *Model) usageFetch() tea.Cmd {
	m.usage.epoch++
	epoch, d := m.usage.epoch, m.usage
	p := protocol.UsageSeriesParams{Channel: d.channel, Agent: d.agent, Buckets: usageChartWidth(m.width)}
	if r := usageRanges[d.rng]; r.d > 0 {
		now := time.Now()
		p.From, p.To = now.Add(-r.d), now
	}
	ctx, c := m.ctx, m.c
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		res, err := client.Do(ctx, c, protocol.UsageSeries, p)
		return usageMsg{epoch, res, err}
	})
}

// onUsage keeps a reply for the open dialog's latest request.
func (m *Model) onUsage(msg usageMsg) {
	if msg.epoch != m.usage.epoch {
		return
	}
	if msg.err != nil {
		m.usage.err = msg.err.Error()
		return
	}
	m.usage.err, m.usage.series = "", &msg.res
}

// usageKey handles keys while a usage dialog is open: ←/→ change the range,
// t and c switch between tokens and cost, esc closes.
func (m *Model) usageKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, keys.OvClose):
		return m.closeDialog()
	case key.Matches(msg, keys.TabLeft):
		if m.usage.rng > 0 {
			m.usage.rng--
			m.usage.series = nil
			return m.usageFetch()
		}
	case key.Matches(msg, keys.TabRight):
		if m.usage.rng < len(usageRanges)-1 {
			m.usage.rng++
			m.usage.series = nil
			return m.usageFetch()
		}
	case msg.String() == "t":
		m.usage.kind = usageTokens
	case msg.String() == "c":
		m.usage.kind = usageCost
	}
	return nil
}

// usageTitle is the dialog's title: "Tokens · #proj".
func (m Model) usageTitle() string {
	if m.usage.kind == usageCost {
		return "Cost · " + m.usage.label
	}
	return "Tokens · " + m.usage.label
}

// usageChartWidth is how many bars (one column each) the chart of a dialog
// opened in a window width wide holds.
func usageChartWidth(width int) int {
	return max(dialog.Width(width)-4-usageAxisW-1, 1)
}

// usageBody is the dialog's body: the ranges (the charted one in accent)
// with the range's total at the right, then the bars with the peak and 0
// on their left, and the span's start and end under them.
func (m Model) usageBody(width int) []string {
	d := m.usage
	var names []string
	for i, r := range usageRanges {
		st := theme.StyleDim
		if i == d.rng {
			st = theme.StyleBoxTitleFocus
		}
		names = append(names, st.Render(r.name))
	}
	head := strings.Join(names, theme.StyleDim.Render(" · "))
	lines := []string{head, ""}
	switch {
	case d.err != "":
		return append(lines, theme.StyleError.Render(d.err))
	case d.series == nil:
		return append(lines, theme.StyleDim.Render("loading…"))
	}
	s := d.series
	values := make([]float64, len(s.Tokens))
	var total, peak float64
	for i := range values {
		values[i] = float64(s.Tokens[i])
		if d.kind == usageCost && i < len(s.Cost) {
			values[i] = s.Cost[i]
		}
		total += values[i]
		peak = max(peak, values[i])
	}
	if t := theme.StyleDim.Render("total " + usageValue(d.kind, total)); ansi.StringWidth(head)+2+ansi.StringWidth(t) <= width {
		lines[0] = head + strings.Repeat(" ", width-ansi.StringWidth(head)-ansi.StringWidth(t)) + t
	}
	if total == 0 {
		return append(lines, theme.StyleDim.Render("no model calls in this range"))
	}
	for _, row := range usageBars(values, peak, usageChartRows) {
		label := ""
		switch {
		case len(lines) == 2:
			label = usageValue(d.kind, peak)
		case len(lines) == 1+usageChartRows:
			label = "0"
		}
		label = ansi.Truncate(label, usageAxisW, "")
		lines = append(lines, theme.StyleDim.Render(strings.Repeat(" ", usageAxisW-ansi.StringWidth(label))+label)+" "+theme.StyleAccent.Render(row))
	}
	now := time.Now()
	start, end := usageWhen(s.From, now), usageWhen(s.To, now)
	gap := max(1, usageChartWidth(m.width)-ansi.StringWidth(start)-ansi.StringWidth(end))
	return append(lines, strings.Repeat(" ", usageAxisW+1)+theme.StyleDim.Render(start+strings.Repeat(" ", gap)+end))
}

// usageValue is a tokens or cost figure as the dialog prints it.
func usageValue(kind usageKind, v float64) string {
	if kind == usageCost {
		return "$" + format.Cost(v)
	}
	return format.Tokens(int(v))
}

// usageWhen labels an end of the chart's span: "now", "3h ago", "Sept 1".
func usageWhen(t, now time.Time) string {
	ago := format.Ago(t, now)
	switch {
	case ago == "now":
		return ago
	case ago != "" && ago[0] >= '0' && ago[0] <= '9':
		return ago + " ago"
	}
	return ago
}

// usageBars draws values as vertical bars rows tall, top row first, one
// column per value, scaled so peak fills the height in eighths of a row; a
// value above zero always shows at least the lowest eighth.
func usageBars(values []float64, peak float64, rows int) []string {
	const eighths = " ▁▂▃▄▅▆▇█"
	glyphs := []rune(eighths)
	heights := make([]int, len(values))
	for i, v := range values {
		if v > 0 && peak > 0 {
			heights[i] = max(1, int(v/peak*float64(rows*8)+0.5))
		}
	}
	out := make([]string, rows)
	for r := range rows {
		floor := (rows - 1 - r) * 8 // eighths below this row
		var b strings.Builder
		for _, h := range heights {
			b.WriteRune(glyphs[min(max(h-floor, 0), 8)])
		}
		out[r] = b.String()
	}
	return out
}

// usageDialogHints are the usage dialog's keys.
func usageDialogHints() []dialog.Hint {
	return []dialog.Hint{hint("←/→", "range"), hint("t", "tokens"), hint("c", "cost"), hint("ctrl+space", "input"), hint("esc", "close")}
}
