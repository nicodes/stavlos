package tui

import (
	"strings"

	"github.com/nicodes/stavlos/internal/tui/theme"
)

// The nav's header is a list of rows, built once: what each row is, whether
// it shows, and what it draws. Where a row is on the screen is its place in
// the list, which is how a click or the pointer finds it.
//
// It used to be index arithmetic kept in step by hand in three places: the
// builder, the functions that said where each row was (navTopRows,
// sidebarWebRow, sidebarSystemRow, sidebarDiscordRow, sidebarTrustRow, planAt),
// and the click and hover code that compared against them. Moving a row meant
// editing all of them and the tests that named rows by number; it was done
// three times in one week.

// navRowID says what a header row is.
type navRowID int

const (
	navBlank navRowID = iota
	navTitle
	navSystem   // every channel's tokens and cost
	navSelected // the selected chat's
	navClientsTitle
	navWeb
	navDiscord
	navSubscriptionsTitle
	navPlan // one window of a plan; plan says which
	navTrust
	navCache
)

// navRow is one row of the header.
type navRow struct {
	id   navRowID
	plan int // navPlan: index into m.plans
	text string
}

// navHeader is the header's rows, in order:
//
//	Stavlos                       ⚙
//
//	System               2k · $0.25
//	@main                1k · $0.20
//
//	Clients
//	● Web UI 127.0.0.1:4999
//	○ Discord disconnected
//
//	Subscriptions                   (only with a plan reading)
//	ChatGPT 5h ━━━━━━──────  38%
//	        wk ━━──────────  17%
//
//	project trusted                 (each only while it has something to say,
//	cache 94%                        and the blank under them with them)
//
// The "Channels" title is the body's first row. The tree's first row follows,
// which is how a click on the sidebar finds its agent.
func (m Model) navHeader(width int) []navRow {
	blank := navRow{id: navBlank}
	rows := []navRow{
		{id: navTitle, text: theme.StyleAccent.Bold(true).Render("Stavlos") + strings.Repeat(" ", max(1, width-len("Stavlos")-2)) + theme.StyleDim.Render(channelGear+" ")},
		blank,
		{id: navSystem, text: m.navUsageRow(navSystem, width)},
		{id: navSelected, text: m.navUsageRow(navSelected, width)},
		blank,
		{id: navClientsTitle, text: navSection("Clients")},
		{id: navWeb, text: m.webIndicator(width)},
		{id: navDiscord, text: m.discordIndicator(width)},
		blank,
	}
	if plans := m.planRowList(); len(plans) > 0 {
		texts := m.planUsageRows(width, clock()) // the title, a row per window, a blank
		rows = append(rows, navRow{id: navSubscriptionsTitle, text: texts[0]})
		for i, p := range plans {
			rows = append(rows, navRow{id: navPlan, plan: p.plan, text: texts[i+1]})
		}
		rows = append(rows, blank)
	}
	monitors := len(rows)
	if trust := m.trustRow(width); trust != "" {
		rows = append(rows, navRow{id: navTrust, text: trust})
	}
	if cache := m.cacheRow(width); cache != "" {
		rows = append(rows, navRow{id: navCache, text: cache})
	}
	if len(rows) > monitors {
		rows = append(rows, blank)
	}
	return rows
}

// sidebarHeader is the header as text.
func (m Model) sidebarHeader(width int) []string {
	rows := m.navHeader(width)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.text
	}
	return out
}

// navRowAt is the header row drawn at screen row y.
func (m Model) navRowAt(y int) (navRow, bool) {
	rows := m.navHeader(sidebarWidth - 1)
	if y < 0 || y >= len(rows) {
		return navRow{}, false
	}
	return rows[y], true
}

// navRowIndex is the screen row of the first header row that is id, -1 when
// it is not showing.
func (m Model) navRowIndex(id navRowID) int {
	for i, r := range m.navHeader(sidebarWidth - 1) {
		if r.id == id {
			return i
		}
	}
	return -1
}
