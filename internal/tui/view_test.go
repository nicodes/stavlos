package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/dialog"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/render"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

func TestBuildLogo(t *testing.T) {
	rows := buildLogo("stav")
	if len(rows) != 5 {
		t.Fatalf("want 5 rows, got %d", len(rows))
	}
	w := ansi.StringWidth(rows[0])
	if w != 7+8+7+8+3 { // s t a v, one space between letters
		t.Fatalf("width: got %d, want %d", w, 7+8+7+8+3)
	}
	for i, r := range rows {
		if ansi.StringWidth(r) != w {
			t.Errorf("row %d width %d != %d: %q", i, ansi.StringWidth(r), w, r)
		}
		if strings.Trim(r, "█ ") != "" {
			t.Errorf("row %d has glyphs outside the block set: %q", i, r)
		}
	}
	// Unknown letters keep the grid aligned.
	for _, r := range buildLogo("s?s") {
		if ansi.StringWidth(r) != 7*3+2 {
			t.Errorf("unknown glyph broke alignment: %q", r)
		}
	}
}

func TestLogoLines(t *testing.T) {
	big := logoLines(80)
	if len(big) != 5 {
		t.Fatalf("want 5 rows, got %d", len(big))
	}
	w := ansi.StringWidth(big[0])
	for i, r := range big {
		if ansi.StringWidth(r) != w {
			t.Errorf("row %d width %d != %d", i, ansi.StringWidth(r), w)
		}
	}
	small := logoLines(logoMinWidth - 1)
	if len(small) != 1 || stripANSI(small[0]) != "stavlos" {
		t.Fatalf("narrow fallback: %q", small)
	}
}

func TestPlaceholderIndexCycles(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	i0 := placeholderIndex(t0)
	if i0 < 0 || i0 >= len(placeholders) {
		t.Fatalf("index out of range: %d", i0)
	}
	if i1 := placeholderIndex(t0.Add(placeholderPeriod)); i1 == i0 {
		t.Fatalf("index should change after one period: %d", i1)
	}
	if placeholderIndex(t0.Add(placeholderPeriod/2)) != i0 {
		t.Fatal("index should be stable within a period")
	}
	full := placeholderPeriod * time.Duration(len(placeholders))
	if placeholderIndex(t0.Add(full)) != i0 {
		t.Fatal("index should wrap around after a full cycle")
	}
}

func TestFooterRight(t *testing.T) {
	got := stripANSI(footerRight(footerInfo{home: true, connected: false, label: "coder"}))
	if got != "Get started /providers" {
		t.Fatalf("not connected: %q", got)
	}
	got = stripANSI(footerRight(footerInfo{home: true, connected: true, label: "coder", model: "anthropic/claude-opus-5"}))
	if got != "" {
		t.Fatalf("connected home: %q", got)
	}
	got = stripANSI(footerRight(footerInfo{connected: true, label: "coder", model: "anthropic/claude-opus-5"}))
	if got != "" {
		t.Fatalf("no context window: %q", got)
	}
	got = stripANSI(footerRight(footerInfo{connected: true, label: "coder", model: "anthropic/claude-opus-5", context: 22_000, window: 1_100_000}))
	if got != "2% · 22k/1.1m tokens" {
		t.Fatalf("context: %q", got)
	}
}

func TestFmtCost(t *testing.T) {
	cases := map[float64]string{0: "0.00", 0.0123: "0.0123", 0.01: "0.01", 1.5: "1.50", 2.3456: "2.3456", 0.00004: "0.00"}
	for in, want := range cases {
		if got := format.Cost(in); got != want {
			t.Errorf("format.Cost(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestMetaLine(t *testing.T) {
	if got := stripANSI(metaLine("main", "coder", "anthropic/claude-opus-5", "", 0, "", metaNone, metaNone)); got != "main (coder) ─ claude-opus-5 ─ default" {
		t.Fatalf("with model: %q", got)
	}
	if got := stripANSI(metaLine("main", "coder", "", "", 0, "", metaNone, metaNone)); got != "main (coder) ─ no model — /models" {
		t.Fatalf("no model: %q", got)
	}
	if got := stripANSI(metaLine("scout", "explorer", "ollama/llama3", "", 2, "", metaNone, metaNone)); got != "scout (explorer) ─ llama3 ─ default ─ 2 queued" {
		t.Fatalf("queued: %q", got)
	}
	if got := stripANSI(metaLine("main", "coder", "openai/gpt-5", "high", 0, "", metaNone, metaNone)); got != "main (coder) ─ gpt-5 ─ high" {
		t.Fatalf("variant: %q", got)
	}
	if got := stripANSI(metaLine("main", "coder", "openai/gpt-5", "", 0, "YOLO", metaNone, metaNone)); got != "YOLO ─ main (coder) ─ gpt-5 ─ default" {
		t.Fatalf("yolo: %q", got)
	}
}

func TestInputBoxAndPromptWidth(t *testing.T) {
	if got := promptBoxWidth(80); got != 75 {
		t.Fatalf("80 cols: got %d, want 75 (the floor; fits within width-4)", got)
	}
	if got := promptBoxWidth(78); got != 74 {
		t.Fatalf("78 cols: got %d, want 74 (capped at width-4)", got)
	}
	if got := promptBoxWidth(200); got != 140 {
		t.Fatalf("200 cols: got %d, want 140", got)
	}
	if got := promptBoxWidth(60); got != 56 {
		t.Fatalf("60 cols: got %d, want 56", got)
	}
}

func TestHomeAndChannelViews(t *testing.T) {
	m := newModel(context.Background(), nil, "sess-1234-5678")
	m.width, m.height = 100, 30
	m.reconciled = true
	m.loading = false
	m.channel.Dir = "/tmp/proj"
	m.layout()

	home := m.View()
	lines := strings.Split(home, "\n")
	if len(lines) != 30 {
		t.Fatalf("home view must fill the window: %d lines", len(lines))
	}
	plain := stripANSI(home)
	if !strings.Contains(plain, "███████ ████████") || strings.Contains(plain, "sign in with ChatGPT or Grok") || !strings.Contains(plain, "Get started /providers") {
		t.Fatalf("home view:\n%s", plain)
	}
	if strings.Contains(plain, "channel ") {
		t.Fatal("home view must not show the sidebar")
	}
	if !strings.Contains(plain, format.ShortHome(m.channel.Dir)) {
		t.Fatalf("home view should name the channel directory above the meta row:\n%s", plain)
	}
	if !strings.Contains(plain, "Giddy up!") {
		t.Fatalf("home view should carry the tagline:\n%s", plain)
	}
	if strings.Contains(plain, "permission (") || strings.Contains(plain, "agents (") {
		t.Fatalf("home view should not show the empty tab strip:\n%s", plain)
	}
	for _, f := range m.focusOrder() {
		if f == focusTabs {
			t.Fatal("the empty strip should not be a tab stop on the home screen")
		}
	}
	// a waiting prompt does not bring the strip to the home screen; it opens
	// its own dialog when it arrives
	m.applyPromptNotification(protocol.PromptNotification{Action: "requested", Prompt: protocol.PromptInfo{ID: "p", Kind: "trust", Agent: "a"}})
	if v := stripANSI(m.View()); strings.Contains(v, "trust 1/1") || m.focus != focusPermission || !strings.Contains(v, "Trust 1/1") {
		t.Fatalf("home with a prompt: focus=%v\n%s", m.focus, v)
	}
	for _, f := range m.focusOrder() {
		if f == focusTabs {
			t.Fatal("the strip is never a stop on the home screen")
		}
	}
	m.closeDialog()
	m.prompts = nil
	for _, l := range lines {
		if ansi.StringWidth(l) > 100 {
			t.Fatalf("line wider than the window: %q", l)
		}
	}

	// A user message moves to the channel state; ctrl+b shows the sidebar.
	m.agents = []protocol.AgentInfo{{ID: "a1", Name: "root", Role: "coder", Model: "anthropic/claude-x", State: "idle"}}
	m.channel.Model = "anthropic/claude-x"
	m.transcript("a1").Notice("hello")
	m.showTree = true
	m.layout()
	sess := stripANSI(m.View())
	if !strings.Contains(sess, "hello") || !strings.Contains(sess, "Stavlos") || strings.Contains(sess, "\nidle ") || !strings.Contains(sess, "\nChannels ") || !strings.Contains(sess, "$0.00") {
		t.Fatalf("channel view:\n%s", sess)
	}
	m.width = 90 // too narrow: sidebar auto-hides
	m.layout()
	if strings.Contains(stripANSI(m.View()), "channel  sess-123") {
		t.Fatal("sidebar should auto-hide below 100 columns")
	}
}

func TestAgentRows(t *testing.T) {
	now := time.Now()
	spawned := map[string]time.Time{"c1": now.Add(-75 * time.Second), "c2": now.Add(-3 * time.Second)}
	agents := []protocol.AgentInfo{
		{ID: "root", Name: "coder", Role: "coder", State: "waiting", Awaiting: []string{"c1", "c2", "c3"}},
		{ID: "c1", Parent: "root", Name: "scout", Role: "explorer", State: "running", Turn: 2, CostUSD: 0.0012},
		{ID: "c2", Parent: "root", Name: "tester", Role: "tester", State: "idle"},
		{ID: "c3", Parent: "root", Name: "done", Role: "explorer", State: "killed"},
		{ID: "g1", Parent: "c1", Name: "grandchild", Role: "explorer", State: "running", Awaiting: []string{"root"}},
	}
	// the tab lists what the agent waits on: c1 and c2 (c3 is dead), not
	// the grandchild it never asked; the grandchild waits on its grandparent
	if got := awaitedOf(agents, "g1"); len(got) != 1 || got[0].ID != "root" {
		t.Fatalf("awaited of g1: %+v", got)
	}
	rows := agentRows(awaitedOf(agents, "root"), spawned, map[string]string{"c1": "Running the tests now, hold on while I look through all of it"}, nil, now, 100)
	if len(rows) != 2 {
		t.Fatalf("rows %d: %q", len(rows), rows)
	}
	if !strings.Contains(rows[0], "scout (explorer)  Running the tests now, hold on while I l…  ") || !strings.Contains(rows[0], "working · ") || !strings.Contains(rows[0], "1m15s") || strings.Contains(rows[0], "⑂") {
		t.Fatalf("%q", rows[0])
	}
	if strings.Contains(rows[0], "wakes parent") {
		t.Fatalf("armed marker: %q", rows)
	}
	if !strings.Contains(rows[1], "tester") || !strings.Contains(rows[1], "3s") || strings.Contains(rows[1], "turn") {
		t.Fatalf("%q", rows[1])
	}
	if rows := agentRows(awaitedOf(agents, "c2"), spawned, nil, nil, now, 100); len(rows) != 0 {
		t.Fatalf("c2 waits on nobody: %q", rows)
	}
	if got := format.Elapsed(3725 * time.Second); got != "1h02m" {
		t.Fatalf("%s", got)
	}

	// The async tab counts what the selected agent waits on (agents and
	// jobs), and lists them once focused.
	m := channelModel()
	m.spawned = spawned
	m.agents = agents
	m.selected = 0
	view := stripANSI(tabsView(m, 100))
	if !strings.Contains(view, "async 2") || strings.Contains(view, "agents") || strings.Contains(view, "scout") || strings.Contains(view, "grandchild") {
		t.Fatalf("collapsed async tab should only count:\n%s", view)
	}
	m.focus = focusAsync
	if view = stripANSI(tabsView(m, 100)); strings.Contains(view, "scout") {
		t.Fatalf("the strip never lists agents:\n%s", view)
	}
	view = stripANSI(m.tabDialog(100))
	if !strings.Contains(view, "scout") || strings.Contains(view, "grandchild") || !strings.HasPrefix(view, "╭") {
		t.Fatalf("async dialog:\n%s", view)
	}
}

func TestJobRows(t *testing.T) {
	now := time.Now().Truncate(time.Second) // Started is RFC3339: whole seconds
	jobs := []protocol.JobInfo{
		{ID: "m1", Agent: "root", Label: "go test", Spec: "go test ./...", Started: now.Add(-75 * time.Second).Format(time.RFC3339), Progress: "42 lines"},
		{ID: "m2", Agent: "root", Label: "src changes", Spec: "./watch.sh", Started: now.Add(-3 * time.Second).Format(time.RFC3339)},
		{ID: "m3", Agent: "root", Label: "cooldown", Spec: "sleep 300", Started: now.Add(-2 * time.Hour).Format(time.RFC3339), Progress: "3m left"},
	}
	rows := jobRows(jobs, "coder", "coder", now, 100)
	if len(rows) != 3 {
		t.Fatalf("rows %d: %q", len(rows), rows)
	}
	plain := make([]string, len(rows))
	for i, r := range rows {
		plain[i] = stripANSI(r)
	}
	// command: owner in bold, the job, progress, elapsed — no glyph
	if !strings.HasPrefix(plain[0], "  coder (coder)  go test") {
		t.Fatalf("command row: %q", plain[0])
	}
	for _, want := range []string{"42 lines", "1m15s"} {
		if !strings.Contains(plain[0], want) {
			t.Fatalf("command row lacks %q: %q", want, plain[0])
		}
	}
	// a second running job: no wake tag, elapsed
	if !strings.HasPrefix(plain[1], "  coder (coder)  src changes") || !strings.HasSuffix(strings.TrimRight(plain[1], " "), "coder (coder)  src changes  3s") {
		t.Fatalf("second job row: %q", plain[1])
	}
	// progress and hours elapsed
	if !strings.HasPrefix(plain[2], "  coder (coder)  cooldown") || !strings.Contains(plain[2], "3m left · 2h00m") {
		t.Fatalf("third job row: %q", plain[2])
	}
	// a bad Started stamp just drops the elapsed field
	rows = jobRows([]protocol.JobInfo{{ID: "x", Label: "w", Started: "nope"}}, "", "", now, 100)
	if len(rows) != 1 || strings.Contains(rows[0], "command") || !strings.HasSuffix(strings.TrimRight(stripANSI(rows[0]), " "), "w") {
		t.Fatalf("bad stamp: %q", rows)
	}
	if jobRows(nil, "coder", "coder", now, 100) != nil {
		t.Fatal("no jobs should give no rows")
	}

	// The section reads the selected agent's Jobs; unfocused it is one
	// summary line, focused it lists one row per job.
	m := channelModel()
	m.agents = []protocol.AgentInfo{{ID: "root", Name: "coder", Role: "coder", State: "idle", Jobs: jobs[:3]}}
	m.selected = 0
	view := stripANSI(tabsView(m, 100))
	if !strings.Contains(view, "async 3") || strings.Count(view, "\n") != 1 {
		t.Fatalf("collapsed async tab:\n%s", view)
	}
	m.focus = focusAsync
	if view := stripANSI(tabsView(m, 100)); strings.Count(view, "\n") != 1 {
		t.Fatalf("the strip keeps its two rows with a tab open:\n%s", view)
	}
	// the dialog: title (with esc: close), a blank line, one row per job, inside the border
	if view := stripANSI(m.tabDialog(100)); !strings.Contains(view, "esc: close") || !inOrder(view, "Async 3", "go test", "cooldown") {
		t.Fatalf("async dialog:\n%s", view)
	}
	m.focus = focusInput
	// The strip is budgeted out of the transcript height; the dialog is not.
	m.width, m.height = 120, 40
	m.showTree = false
	m.layout()
	collapsed := m.vp.Height
	_, kbCollapsed := m.keyBarView()
	m.focus = focusAsync
	m.layout()
	_, kbOpen := m.keyBarView()
	// open, only the key bar legend may change height with the focus
	if want := collapsed - (kbOpen - kbCollapsed); m.vp.Height != want {
		t.Fatalf("layout: viewport %d collapsed, %d open with 3 jobs, want %d", collapsed, m.vp.Height, want)
	}
	// the strip stays (with "(0)") once the jobs are gone
	m.focus = focusInput
	m.agents[0].Jobs = nil
	m.layout()
	if m.vp.Height != collapsed || !strings.Contains(stripANSI(tabsView(m, 100)), "async 0") {
		t.Fatalf("layout: viewport %d without jobs, want %d:\n%s", m.vp.Height, collapsed, stripANSI(tabsView(m, 100)))
	}
}

func TestHistoryNavigation(t *testing.T) {
	m := newModel(context.Background(), nil, "s")
	m.pushHistory("first")
	m.pushHistory("second")
	m.pushHistory("second") // duplicate collapses
	if len(m.history) != 2 || m.histIdx != 2 {
		t.Fatalf("%v %d", m.history, m.histIdx)
	}
	m.input.SetValue("draft")
	m.historyMove(-1)
	if m.input.Value() != "second" {
		t.Fatalf("up: %q", m.input.Value())
	}
	m.historyMove(-1)
	m.historyMove(-1) // clamps at oldest
	if m.input.Value() != "first" {
		t.Fatalf("up twice: %q", m.input.Value())
	}
	m.historyMove(1)
	m.historyMove(1)
	if m.input.Value() != "draft" {
		t.Fatalf("back to draft: %q", m.input.Value())
	}
}

func TestSidebarFocusAndSelect(t *testing.T) {
	m := newModel(context.Background(), nil, "s")
	m.width, m.height = 120, 40
	m.agents = []protocol.AgentInfo{{ID: "a", Name: "coder", State: "running"}, {ID: "b", Name: "scout", Depth: 1, State: "running"}, {ID: "c", Name: "tester", Depth: 1, State: "running"}}
	m.toggleTree()
	if !m.showTree || m.focus != focusSidebar || m.input.Focused() {
		t.Fatalf("open should focus the sidebar: show=%v focus=%v inputFocused=%v", m.showTree, m.focus, m.input.Focused())
	}
	down := tea.KeyMsg{Type: tea.KeyDown}
	m.handleKey(down)
	m.handleKey(down)
	if m.sbCursor != 4 || m.selected != 0 { // row 0 is + channel, 1 this channel, agent i is row i+2
		t.Fatalf("cursor %d selected %d", m.sbCursor, m.selected)
	}
	// the cursor is a row background (a visible marker here), never an arrow
	markCursorForTest(t)
	body, _ := m.sidebarBody(30)
	rows := body[2:] // under the channels title and this channel's row
	if !strings.HasPrefix(rows[2], render.GutterMark) || strings.Contains(rows[0], render.GutterMark) || strings.Contains(strings.Join(rows, ""), "▶") || strings.Contains(strings.Join(rows, ""), "▸") {
		t.Fatalf("cursor row: %q", rows)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
	if m.selected != 2 || m.focus != focusInput || !m.input.Focused() {
		t.Fatalf("space: selected %d focus %v", m.selected, m.focus)
	}
	// ↑ in the input now walks history, not agents
	m.pushHistory("hello")
	m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "hello" || m.selected != 2 {
		t.Fatalf("history: %q selected %d", m.input.Value(), m.selected)
	}
	m.toggleTree()
	if m.showTree || m.focus != focusInput || !m.input.Focused() {
		t.Fatal("close should return focus to the input")
	}
	// ctrl+b from the sidebar closes it; esc just returns to the input.
	m.toggleTree()
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.showTree || m.focus != focusInput {
		t.Fatalf("esc: show=%v focus=%v", m.showTree, m.focus)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlB})
	if m.showTree || m.focus != focusInput {
		t.Fatalf("ctrl+b: show=%v focus=%v", m.showTree, m.focus)
	}
}

// channelModel is a model in the channel state (one transcript item) at a
// size where the sidebar fits.
func channelModel() Model {
	m := newModel(context.Background(), nil, "s")
	m.width, m.height = 120, 40
	m.reconciled, m.loading = true, false
	m.agents = []protocol.AgentInfo{{ID: "a", Name: "coder"}, {ID: "b", Name: "scout", Depth: 1}}
	m.transcript("a").Notice("hello")
	m.layout()
	return m
}

func press(m *Model, msgs ...tea.KeyMsg) tea.Cmd {
	var cmd tea.Cmd
	for _, k := range msgs {
		cmd = m.handleKey(k)
		m.ensureFocus()
		m.layout()
	}
	return cmd
}

func TestTabCyclesFocus(t *testing.T) {
	tab := tea.KeyMsg{Type: tea.KeyTab}
	stab := tea.KeyMsg{Type: tea.KeyShiftTab}
	right := tea.KeyMsg{Type: tea.KeyRight}
	left := tea.KeyMsg{Type: tea.KeyLeft}
	t.Run("chat, input, strip and meta row", func(t *testing.T) {
		m := channelModel()
		if m.focus != focusInput {
			t.Fatalf("default focus %v", m.focus)
		}
		// No prompt, sidebar hidden, top to bottom: chat → input → tabs → meta
		// row, wrapping. From the input, tab goes down to the strip and
		// shift+tab up to the chat. The strip is one stop, landing on permission,
		// and ←/→ walk the tabs; the meta row is a stop too: ←/→ pick role,
		// model, variant.
		press(&m, stab)
		if m.focus != focusChat || m.follow || m.input.Focused() {
			t.Fatalf("shift+tab: focus=%v follow=%v", m.focus, m.follow)
		}
		press(&m, tab, tab, tab) // chat → input → strip → meta row
		if m.focus != focusMeta || m.metaSel != metaRole || m.input.Focused() {
			t.Fatalf("tab x3: focus=%v sel=%v", m.focus, m.metaSel)
		}
		press(&m, left) // leftmost already (the role): stays
		press(&m, right)
		if m.metaSel != metaModel {
			t.Fatalf("→ should move to the model: %v", m.metaSel)
		}
		press(&m, right)
		press(&m, right) // rightmost: stays on the variant
		if m.metaSel != metaVariant {
			t.Fatalf("→→ should stop on the variant: %v", m.metaSel)
		}
		if cmd := press(&m, tea.KeyMsg{Type: tea.KeySpace}); cmd == nil {
			t.Fatal("space on a meta part should open its dialog")
		}
		press(&m, tab, tab, tab) // meta row → chat → input → strip
		// the strip: a highlight, no dialog yet
		if m.focus != focusTabs || m.tabSel != 0 || strings.Contains(m.View(), "╭") {
			t.Fatalf("back to the strip: focus=%v sel=%d", m.focus, m.tabSel)
		}
		press(&m, left) // already leftmost: stays
		if m.tabSel != 0 {
			t.Fatalf("left at the edge: sel=%d", m.tabSel)
		}
		press(&m, right, right, right, right, right)
		press(&m, right) // already rightmost (mcp): stays
		if m.focus != focusTabs || m.tabSel != 4 {
			t.Fatalf("right x6: focus=%v sel=%d", m.focus, m.tabSel)
		}
		press(&m, left, left)
		if m.tabSel != 2 {
			t.Fatalf("left x2: sel=%d", m.tabSel)
		}
		// enter opens the highlighted tab's own dialog; ←/→ do not switch inside it
		press(&m, tea.KeyMsg{Type: tea.KeySpace})
		if dv := stripANSI(m.tabDialog(100)); m.focus != focusAsync || !strings.Contains(dv, "Async 0") || !strings.Contains(dv, "not waiting on anything") || strings.Contains(dv, "permission") {
			t.Fatalf("enter: focus=%v\n%s", m.focus, dv)
		}
		press(&m, right)
		if m.focus != focusAsync {
			t.Fatalf("→ inside a dialog should do nothing: focus=%v", m.focus)
		}
		// esc returns to where the dialog was opened from: the strip, with the
		// closed tab still highlighted
		press(&m, tea.KeyMsg{Type: tea.KeyEsc})
		if m.focus != focusTabs || m.tabSel != 2 {
			t.Fatalf("esc: focus=%v sel=%d", m.focus, m.tabSel)
		}
		press(&m, left, left, left)               // past dirs and questions to permission
		press(&m, tea.KeyMsg{Type: tea.KeySpace}) // permission dialog, nothing waiting
		if dv := stripANSI(m.tabDialog(100)); m.focus != focusPermission || !strings.Contains(dv, "Permission 0") || !strings.Contains(dv, "no prompts waiting") {
			t.Fatalf("enter on permission: focus=%v\n%s", m.focus, dv)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}) // nothing to answer: ignored
		if m.focus != focusPermission {
			t.Fatalf("y on an empty permission dialog: focus=%v", m.focus)
		}
		press(&m, tab) // tab from a dialog moves on from the strip: to the meta row
		if m.focus != focusMeta || m.input.Focused() {
			t.Fatalf("tab from a dialog: focus=%v", m.focus)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyEsc}) // meta row → input
		if m.focus != focusInput || !m.follow || !m.input.Focused() {
			t.Fatalf("esc from the meta row: focus=%v follow=%v", m.focus, m.follow)
		}
	})
	t.Run("with the sidebar", func(t *testing.T) {
		m := channelModel()
		// Sidebar shown: input → tabs → meta row → sidebar → chat → input.
		m.showTree = true
		m.layout()
		var seen []focus
		for i := 0; i < 5; i++ {
			press(&m, tab)
			seen = append(seen, m.focus)
		}
		if want := []focus{focusTabs, focusMeta, focusSidebar, focusChat, focusInput}; !equalFocus(seen, want) {
			t.Fatalf("with sidebar: %v, want %v", seen, want)
		}
		// Hiding the sidebar while it has focus falls back to the input.
		press(&m, tab, tab, tab) // input → tabs → meta row → sidebar
		m.showTree = false
		m.ensureFocus()
		if m.focus != focusInput {
			t.Fatalf("sidebar hidden: focus=%v", m.focus)
		}
	})
	t.Run("with a pending prompt", func(t *testing.T) {
		m := channelModel()
		// Pending prompt: strip (highlighting permission) → meta → chat → input.
		m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Agent: "a", Tool: "shell"}}
		if m.focus != focusInput {
			t.Fatal("a new prompt must not steal focus")
		}
		var seen []focus
		for i := 0; i < 4; i++ {
			press(&m, tab)
			seen = append(seen, m.focus)
		}
		if want := []focus{focusTabs, focusMeta, focusChat, focusInput}; !equalFocus(seen, want) {
			t.Fatalf("with prompt: %v, want %v", seen, want)
		}
	})
	t.Run("the strip lands on its leftmost tab", func(t *testing.T) {
		m := channelModel()
		// Whatever the tabs hold, landing on the strip always highlights the
		// leftmost tab; ←/→ move from there.
		m.prompts = nil
		m.agents = append(m.agents, protocol.AgentInfo{ID: "c", Parent: "a", Name: "kid", State: "working"})
		press(&m, tab) // input → strip
		if m.focus != focusTabs || m.tabSel != 0 {
			t.Fatalf("the strip should land on permission even with a child: %v sel %d", m.focus, m.tabSel)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyEsc})
		m.agents = m.agents[:len(m.agents)-1]
		m.agents[0].Jobs = []protocol.JobInfo{{ID: "j", Label: "sleep"}}
		press(&m, tab)
		if m.focus != focusTabs || m.tabSel != 0 {
			t.Fatalf("the strip should land on permission even with a job: %v sel %d", m.focus, m.tabSel)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyEsc})
		m.agents[0].Jobs = nil
		m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Agent: "a", Tool: "shell"}}
		press(&m, tab, tea.KeyMsg{Type: tea.KeySpace}) // input → strip → the permission dialog
		if m.focus != focusInlinePermission {
			t.Fatalf("tab enter from input: %v", m.focus)
		}
		// Answering the prompt elsewhere closes the dialog back onto the strip
		// it was opened from.
		m.removePrompt("p")
		m.ensureFocus()
		if m.focus != focusInput {
			t.Fatalf("prompt gone: focus=%v sel=%d", m.focus, m.tabSel)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyEsc}) // strip → input
	})
	t.Run("tab never cycles agents", func(t *testing.T) {
		m := channelModel()
		// Tab never cycles agents any more; ctrl+n still does.
		press(&m, tab, tab)
		if m.selected != 0 {
			t.Fatalf("tab changed the selection to %d", m.selected)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyCtrlN})
		if m.selected != 1 {
			t.Fatalf("ctrl+n: selected %d", m.selected)
		}
	})
}

func equalFocus(a, b []focus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPromptHotkeysNeedPermissionFocus(t *testing.T) {
	y := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}
	space := tea.KeyMsg{Type: tea.KeySpace}
	m := channelModel()
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Agent: "a", Tool: "shell"}}

	// Input focus: y is typed, the prompt is untouched.
	if cmd := press(&m, y); m.promptBusy != "" || m.claimedByUs["p"] || m.input.Value() != "y" {
		t.Fatalf("input focus: busy=%q claimed=%v input=%q cmd=%v", m.promptBusy, m.claimedByUs["p"], m.input.Value(), cmd != nil)
	}
	m.input.Reset()

	// Permission focus: letters do nothing; space on the highlighted option
	// (Allow once at the top) answers (claim + reply as one tea.Cmd).
	press(&m, tea.KeyMsg{Type: tea.KeyTab}) // input → strip, highlighting permission
	if m.focus != focusTabs || m.tabSel != 0 {
		t.Fatalf("focus %v sel %d", m.focus, m.tabSel)
	}
	press(&m, space)
	if m.focus != focusInlinePermission {
		t.Fatalf("focus %v", m.focus)
	}
	if cmd := press(&m, y); cmd != nil || m.promptBusy != "" || m.input.Value() != "" {
		t.Fatalf("y is not a hotkey any more: busy=%q input=%q cmd=%v", m.promptBusy, m.input.Value(), cmd != nil)
	}
	if cmd := press(&m, space); cmd == nil || m.promptBusy != "p" || !m.claimedByUs["p"] {
		t.Fatalf("permission focus: busy=%q claimed=%v cmd=%v", m.promptBusy, m.claimedByUs["p"], cmd != nil)
	}
	m.promptBusy = ""
	m.focus = focusInput
	if pv := stripANSI(tabsView(m, 80)); !strings.Contains(pv, "! 1/1") || strings.Count(pv, "\n") != 1 {
		t.Fatalf("unfocused prompt should be one strip line: %q", pv)
	}
	m.focus = focusPermission
	hs := m.keyHints()
	if hs[0].Key != "↑/↓" || hs[1].Key != "space/enter" || hs[1].Desc != "choose" {
		t.Fatalf("permission hints: %+v", hs)
	}

	// Trust prompts: two options, trust or not now; the second denies.
	m.prompts = []protocol.PromptInfo{{ID: "t", Kind: "trust", Input: []byte(`{"dir":"/x","hash":"h","files":["stavlos.json"]}`)}}
	m.promptBusy = ""
	if body := stripANSI(strings.Join(m.tabBodyLines(80), "\n")); !strings.Contains(body, "◆ /x") || !strings.Contains(body, "  stavlos.json") || !strings.Contains(body, "▸ ● Trust this project's config  until these files change") || !strings.Contains(body, "  ○ Not now  run on the global config only") {
		t.Fatalf("trust body:\n%s", body)
	}
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}); cmd != nil || m.promptBusy != "" {
		t.Fatal("a must not answer a trust prompt")
	}
	// A repository's names show their controls instead of obeying them.
	evil := m
	evil.prompts = []protocol.PromptInfo{{ID: "t", Kind: "trust", Input: []byte(`{"dir":"/x\u001b]0;pwn\u0007","hash":"h","files":["evil\u001b[2J\u202e.json"]}`)}}
	if body := strings.Join(evil.tabBodyLines(80), "\n"); !strings.Contains(body, "/x^[") || !strings.Contains(body, "evil^[<U+202E>.json") || strings.Contains(body, "\x1b]") || strings.Contains(body, "\x1b[2J") {
		t.Fatalf("trust body obeyed a control:\n%q", body)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown})
	if cmd := press(&m, space); cmd == nil || m.promptBusy != "t" {
		t.Fatal("space on Not now should answer a trust prompt")
	}

	// A question is not a permission: the permission tab ignores it and the
	// questions dialog takes it (typing goes to its field, enter answers).
	m.prompts = []protocol.PromptInfo{{ID: "q", Kind: "question", Agent: "a", Questions: []protocol.Question{{Question: "which?", Options: []protocol.QuestionOption{{Label: "blue"}}}}}}
	m.promptBusy = ""
	m.ensureFocus()
	if m.currentPrompt() != nil || m.currentQuestion() == nil {
		t.Fatal("a question must not sit in the permission queue")
	}
	m.setFocus(focusQuestions)
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("red")})
	if m.promptInput.Value() != "red" || m.input.Value() != "" {
		t.Fatalf("typing: field=%q input=%q", m.promptInput.Value(), m.input.Value())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEnter}) // stage text before Submit
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.promptBusy != "q" || m.promptInput.Value() != "" {
		t.Fatalf("enter: busy=%q field=%q", m.promptBusy, m.promptInput.Value())
	}
	// Enter in the input focus sends a prompt, it never answers a question.
	press(&m, tea.KeyMsg{Type: tea.KeyEsc}) // the dialog closes back onto the strip it was opened from
	if m.focus != focusInput {
		t.Fatalf("esc should return to the input: %v", m.focus)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc}) // strip → input
	m.promptBusy = ""
	m.input.SetValue("hello agent")
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.promptBusy != "" || m.history[len(m.history)-1] != "hello agent" {
		t.Fatalf("input enter answered the question: busy=%q", m.promptBusy)
	}
}

// TestPermissionDialogOptions: the subject line reads "$ command  name
// (role)", and the options are the fixed set — with a prefix row only for
// a simple shell command whose prefix can be derived.
func TestPermissionDialogOptions(t *testing.T) {
	m := channelModel()
	m.agents[0].Role = "general"
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Tool: "shell", Agent: "a", Input: []byte(`{"command":"go test ./... -run TestRoles"}`), Prefix: "go test"}}
	m.setFocus(focusPermission)
	body := stripANSI(strings.Join(m.tabBodyLines(80), "\n"))
	want := []string{
		"$ go test ./... -run TestRoles  coder (general)",
		"▸ ● Allow once",
		"  ○ Allow for this channel  this exact command",
		"  ○ Allow go test for this channel  every command starting with it",
		"  ○ Deny  with an optional reason",
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Fatalf("missing %q:\n%s", w, body)
		}
	}
	if strings.Contains(body, "outside") || strings.Contains(body, "add") {
		t.Fatalf("a plain permission has no directory rows:\n%s", body)
	}
	// ↑ wraps to the bottom, ↓ from there to the top; space on the prefix
	// row sends the prefix allow
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	if m.permSel != 3 {
		t.Fatalf("sel %d", m.permSel)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown})
	if m.permSel != 2 || !strings.Contains(stripANSI(strings.Join(m.tabBodyLines(80), "\n")), "▸ ● Allow go test") {
		t.Fatalf("sel %d", m.permSel)
	}
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeySpace}); cmd == nil || m.promptBusy != "p" {
		t.Fatalf("space on the prefix row: cmd=%v busy=%q", cmd != nil, m.promptBusy)
	}
	// a prompt without a prefix (the daemon derives it: a compound command,
	// a non-shell tool) offers no prefix row; a new prompt starts at the top
	// again
	m.promptBusy = ""
	m.prompts = []protocol.PromptInfo{{ID: "p2", Kind: "permission", Tool: "shell", Agent: "a", Input: []byte(`{"command":"go test && rm -rf x"}`)}}
	if body := stripANSI(strings.Join(m.tabBodyLines(80), "\n")); strings.Contains(body, "for this channel  every") || !strings.Contains(body, "▸ ● Allow once") {
		t.Fatalf("compound command:\n%s", body)
	}
	m.prompts = []protocol.PromptInfo{{ID: "p3", Kind: "permission", Tool: "read", Agent: "a", Input: []byte(`{"path":"/repo/x"}`)}}
	if body := stripANSI(strings.Join(m.tabBodyLines(80), "\n")); strings.Contains(body, "every command") || !strings.Contains(body, "Allow for this channel  this exact call") {
		t.Fatalf("read:\n%s", body)
	}
	if len(permOptions(&m.prompts[0])) != 3 {
		t.Fatalf("options %+v", permOptions(&m.prompts[0]))
	}
	// web_fetch: the subject is the URL, the prefix row is the host
	m.prompts = []protocol.PromptInfo{{ID: "p4", Kind: "permission", Tool: "web_fetch", Agent: "a", Input: []byte(`{"url":"https://pkg.go.dev/net/http"}`), Prefix: "pkg.go.dev"}}
	body = stripANSI(strings.Join(m.tabBodyLines(80), "\n"))
	for _, w := range []string{"↓ https://pkg.go.dev/net/http  coder (general)", "○ Allow for this channel  this exact URL", "○ Allow pkg.go.dev for this channel  every page on this host"} {
		if !strings.Contains(body, w) {
			t.Fatalf("missing %q:\n%s", w, body)
		}
	}
}

func TestChatCursorMovesAndRenders(t *testing.T) {
	markCursorForTest(t)
	m := channelModel()
	tr := m.transcript("a")
	for i := 0; i < 8; i++ {
		feed(tr.Apply, userMsg(int64(i+1), "a", "msg "+string(rune('A'+i))))
	}
	feed(tr.Apply, toolCall(9, "a", "c1", "shell", `{"command":"ls"}`))
	tr.Apply(mk(10, "a", event.ToolFinished, event.ToolFinishedPayload{CallID: "c1", Name: "shell", Output: strings.TrimRight(strings.Repeat("out\n", 8), "\n")}))
	m.height = 20 // a viewport smaller than the transcript so the cursor has to scroll
	m.layout()
	m.refreshViewport()
	items := tr.Items() // notice + 8 user + tool = 10

	press(&m, tea.KeyMsg{Type: tea.KeyShiftTab}) // input → chat (the chat sits above the input)
	if m.focus != focusChat || m.chatCursor != items-1 || m.follow {
		t.Fatalf("enter chat: focus=%v cursor=%d follow=%v", m.focus, m.chatCursor, m.follow)
	}
	// marked is the first non-blank line carrying the cursor marker.
	marked := func() string {
		for _, l := range strings.Split(stripANSI(m.vp.View()), "\n") {
			if s := strings.TrimSpace(strings.TrimPrefix(l, render.GutterMark)); strings.HasPrefix(l, render.GutterMark) && s != "" {
				return s
			}
		}
		return ""
	}
	if got := marked(); !strings.HasPrefix(got, "$ Shell") {
		t.Fatalf("last item should be marked: %q", got)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	if m.chatCursor != items-2 {
		t.Fatalf("up: cursor %d", m.chatCursor)
	}
	if got := marked(); got != "› @user: msg H" {
		t.Fatalf("cursor item not marked: %q\n%s", got, stripANSI(m.vp.View()))
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")}, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.chatCursor != items-2-1-chatPage {
		t.Fatalf("k + pgup: cursor %d", m.chatCursor)
	}
	// The cursor item is scrolled into view.
	r := m.itemRows[m.chatCursor]
	if r.First < m.vp.YOffset || r.Last >= m.vp.YOffset+m.vp.Height {
		t.Fatalf("cursor rows %+v not visible at offset %d height %d", r, m.vp.YOffset, m.vp.Height)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyHome})
	if m.chatCursor != 0 || m.vp.YOffset != 0 {
		t.Fatalf("home: cursor %d offset %d", m.chatCursor, m.vp.YOffset)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEnd})
	if m.chatCursor != items-1 {
		t.Fatalf("end: cursor %d", m.chatCursor)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.chatCursor != items-1 {
		t.Fatalf("down clamps: cursor %d", m.chatCursor)
	}

	// Under the cursor the tool item previews a few lines; enter expands
	// this item fully; enter again returns to the preview.
	view := func() string { return stripANSI(m.vp.View()) }
	if n := strings.Count(view(), "out"); n != render.PreviewLines-1 {
		t.Fatalf("preview before enter (%d 'out' lines):\n%s", n, view())
	}
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if !m.expanded["a"][items-1] || strings.Count(view(), "out") != 8 {
		t.Fatalf("expanded after enter (%v):\n%s", m.expanded["a"], view())
	}
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if m.expanded["a"][items-1] || strings.Count(view(), "out") != render.PreviewLines-1 {
		t.Fatalf("preview after second enter:\n%s", view())
	}
	// Expansion is per visit: expand, move away, come back → preview again.
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if strings.Count(view(), "out") != 8 {
		t.Fatalf("expanded again:\n%s", view())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	if len(m.expanded["a"]) != 0 {
		t.Fatalf("moving away should drop the expansion: %v", m.expanded["a"])
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown})
	if n := strings.Count(view(), "out"); n != render.PreviewLines-1 {
		t.Fatalf("back on the item it should be the preview (%d):\n%s", n, view())
	}
	// Enter on a non-tool item is inert.
	press(&m, tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeySpace})
	if len(m.expanded["a"]) != 0 {
		t.Fatalf("enter on a user item changed overrides: %v", m.expanded["a"])
	}
	// Leaving the chat with an item expanded folds it for next time.
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeySpace})
	if strings.Count(view(), "out") != 8 {
		t.Fatalf("expanded before leaving:\n%s", view())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc}, tea.KeyMsg{Type: tea.KeyShiftTab}) // esc → input; shift+tab back up to the chat
	if m.focus != focusChat || len(m.expanded["a"]) != 0 || strings.Count(view(), "out") != render.PreviewLines-1 {
		t.Fatalf("re-entering the chat should show the preview: focus=%v %v\n%s", m.focus, m.expanded["a"], view())
	}

	// Leaving the chat resumes following and drops the marker.
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusInput || !m.follow || !m.vp.AtBottom() || strings.Contains(stripANSI(m.vp.View()), render.GutterMark) {
		t.Fatalf("leave chat: focus=%v follow=%v bottom=%v", m.focus, m.follow, m.vp.AtBottom())
	}
	if hs := m.keyHints(); hs[4].Key != "tab" || hs[4].Desc != "next section" {
		t.Fatalf("input hints: %+v", hs)
	}
}

func TestAgentOutcomeColours(t *testing.T) {
	cases := []struct {
		a    protocol.AgentInfo
		want string
	}{
		{protocol.AgentInfo{State: "running"}, "working"},
		{protocol.AgentInfo{State: "blocked"}, "working"},
		{protocol.AgentInfo{State: "idle"}, "idle"},
		{protocol.AgentInfo{State: "idle", LastError: "boom"}, "error"},
		{protocol.AgentInfo{State: "waiting"}, "waiting"},
		{protocol.AgentInfo{State: "killed"}, "complete"},
	}
	for _, c := range cases {
		if got := agentOutcome(c.a); got != c.want {
			t.Errorf("%+v: got %s want %s", c.a, got, c.want)
		}
	}
	if agentDot(protocol.AgentInfo{State: "idle"}) == agentDot(protocol.AgentInfo{State: "killed"}) {
		t.Error("idle and complete should use different glyphs")
	}
}

func TestAgentsAndPromptCollapseUnlessFocused(t *testing.T) {
	m := newModel(context.Background(), nil, "s")
	m.loading = false
	m.width, m.height = 120, 40
	m.reconciled = true
	m.transcript("root").Notice("hello") // a channel, not the home screen (which has no strip)
	m.agents = []protocol.AgentInfo{
		{ID: "root", Name: "coder", Role: "coder", State: "waiting", Awaiting: []string{"c1", "c2"}},
		{ID: "c1", Parent: "root", Name: "scout", Role: "explorer", State: "running"},
		{ID: "c2", Parent: "root", Name: "checks", Role: "tester", State: "idle"},
	}
	m.prompts = []protocol.PromptInfo{{ID: "p1", Kind: "permission", Tool: "shell", Agent: "root", Input: []byte(`{"command":"make test"}`)}}

	// unfocused: one strip line, counts only
	sv := stripANSI(tabsView(m, 100))
	if strings.Count(sv, "\n") != 1 || !strings.Contains(sv, "async 2") || strings.Contains(sv, "agents") || !strings.Contains(sv, "! 1/1") ||
		strings.Contains(sv, "scout") || strings.Contains(sv, "checks") || strings.Contains(sv, "make test") || strings.Contains(sv, "tab to") {
		t.Fatalf("collapsed strip should only count:\n%s", sv)
	}

	// tab order: the chat, the input, the strip, the meta row
	order := m.focusOrder()
	if len(order) != 4 || order[0] != focusChat || order[1] != focusInput || order[2] != focusTabs || order[3] != focusMeta {
		t.Fatalf("order %v", order)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyTab}) // input → strip (highlighting permission: a prompt waits)
	if m.focus != focusTabs || m.tabSel != 0 || strings.Contains(m.View(), "╭") {
		t.Fatalf("the strip should highlight permission without a dialog: focus=%v sel=%d", m.focus, m.tabSel)
	}
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if sv := stripANSI(tabsView(m, 100)); m.focus != focusInlinePermission || strings.Count(sv, "\n") != 1 || strings.Contains(sv, "make test") {
		t.Fatalf("the strip should stay one line with the permission open: focus=%v\n%s", m.focus, sv)
	}
	if full := stripANSI(m.View()); !strings.Contains(full, "! @user Permission: Shell") || !strings.Contains(full, "$ make test") || !strings.Contains(full, "Allow once") || strings.Contains(full, "╭") {
		t.Fatalf("permission should render inline:\n%s", full)
	}
	// esc returns to the strip (permission still highlighted); →→→ space opens async
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusInput {
		t.Fatalf("esc: focus=%v sel=%d", m.focus, m.tabSel)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyTab})
	press(&m, tea.KeyMsg{Type: tea.KeyRight}, tea.KeyMsg{Type: tea.KeyRight}, tea.KeyMsg{Type: tea.KeySpace}) // past dirs to async
	if m.focus != focusAsync {
		t.Fatalf("focus %v", m.focus)
	}
	if dv := stripANSI(m.tabDialog(100)); !strings.Contains(dv, "Async 2") || !strings.Contains(dv, "▸") || !strings.Contains(dv, "scout") || !strings.Contains(dv, "checks") {
		t.Fatalf("async dialog:\n%s", dv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc}, tea.KeyMsg{Type: tea.KeyRight}, tea.KeyMsg{Type: tea.KeySpace}) // strip (async) → todo
	if m.focus != focusTodo {
		t.Fatalf("todo: focus=%v", m.focus)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc}, tea.KeyMsg{Type: tea.KeyLeft}, tea.KeyMsg{Type: tea.KeySpace}) // strip (todo) → async for the selection test
	if m.focus != focusAsync {
		t.Fatalf("focus %v", m.focus)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown})
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	// c2 has no chat yet, so its view is the home screen, which has no strip
	// to return to: the dialog closes onto the input instead
	if m.selectedID() != "c2" || m.focus != focusInput {
		t.Fatalf("enter should select the child under the cursor: %s focus %v", m.selectedID(), m.focus)
	}
	// now the selected agent waits on nothing: the tab stays, reading 0
	if v := stripANSI(tabsView(m, 100)); !strings.Contains(v, "async 0") {
		t.Fatalf("empty async tab:\n%s", v)
	}
}

func TestSectionTabStrip(t *testing.T) {
	m := channelModel()
	m.agents = []protocol.AgentInfo{
		{ID: "root", Name: "coder", Role: "coder", State: "working", Awaiting: []string{"c1"}, Jobs: []protocol.JobInfo{{ID: "j1", Label: "go test"}}},
		{ID: "c1", Parent: "root", Name: "scout", Role: "explorer", State: "working"},
	}
	m.selected = 0
	m.prompts = []protocol.PromptInfo{{ID: "p1", Kind: "permission", Tool: "shell", Agent: "root", Input: []byte(`{"command":"make test"}`), Prefix: "make test"}}

	// unfocused: the channel's tabs over the agent's, counts only
	v := stripANSI(tabsView(m, 100))
	if strings.Count(v, "\n") != 1 || !strings.Contains(v, "! 1/1 · dirs 0\nasync 2 ─ todo") ||
		strings.Contains(v, "scout") || strings.Contains(v, "go test") || strings.Contains(v, "make test") {
		t.Fatalf("tab strip:\n%s", v)
	}
	// async focused: the strip is unchanged; the dialog has its own title,
	// a blank line, then the awaited agent's row over the job's row
	m.focus = focusAsync
	if sv := stripANSI(tabsView(m, 100)); strings.Count(sv, "\n") != 1 || strings.Contains(sv, "scout") {
		t.Fatalf("strip with async focused:\n%s", sv)
	}
	v = stripANSI(m.tabDialog(100))
	lines := strings.Split(v, "\n")
	// the title (with esc: close), a blank line, the awaited agent's row
	// under the cursor, then the job's row
	title, scout, job := findLine(lines, "Async 2"), findLine(lines, "scout"), findLine(lines, "coder (coder)  go test")
	if title < 0 || !strings.HasSuffix(strings.TrimRight(lines[title], " │"), "esc: close") || strings.Contains(lines[title], "permission") || strings.TrimSpace(strings.Trim(lines[title+1], "│")) != "" ||
		scout <= title+1 || !strings.Contains(lines[scout], "▸") || strings.Contains(lines[scout], "go test") || job <= scout {
		t.Fatalf("async dialog:\n%s", v)
	}
	// space on the job row selects nothing and keeps the dialog
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeySpace})
	if m.focus != focusAsync || m.selectedID() != "root" {
		t.Fatalf("space on a job row: focus=%v selected=%s", m.focus, m.selectedID())
	}
	// permission focused: the tool row over its command
	m.focus = focusPermission
	v = stripANSI(m.tabDialog(100))
	if !inOrder(v, "Permission 1/1", "│ $ make test  coder (coder)", "│ ▸ ● Allow once") || strings.Contains(v, "scout") {
		t.Fatalf("permission dialog:\n%s", v)
	}
	// no prompt: the tab stays with a zero count and the generic hint
	m.prompts = nil
	m.focus = focusInput
	if v := stripANSI(tabsView(m, 100)); !strings.Contains(v, "! 0") || strings.Contains(v, "tab to") {
		t.Fatalf("empty permission tab:\n%s", v)
	}
}

func TestChannelViewFillsHeight(t *testing.T) {
	m := channelModel()
	m.showTree = false
	for _, f := range []focus{focusInput, focusAsync, focusPermission} {
		m.focus = f
		m.width, m.height = 100, 30
		m.layout()
		v := m.View()
		if got := strings.Count(v, "\n") + 1; got != m.height {
			t.Fatalf("focus %v: view is %d lines, want %d:\n%s", f, got, m.height, stripANSI(v))
		}
		lines := strings.Split(stripANSI(v), "\n")
		si := -1
		for i, l := range lines {
			if strings.HasPrefix(l, "! ") {
				si = i
			}
		}
		// the divider leads with the agent and carries its tabs; under it come
		// a blank line, the input, a blank line and the strip (! ? dirs), the
		// last row
		ri := -1
		for i := 0; i < si; i++ {
			if strings.HasPrefix(lines[i], "─") {
				ri = i
			}
		}
		if ri < 0 || si < 2 || si != len(lines)-1 || !strings.HasPrefix(lines[ri], "─ coder ─ ") || !strings.Contains(lines[ri], "async ") || strings.TrimSpace(lines[ri+1]) != "" || !strings.HasPrefix(lines[ri+2], " ASK › ") || strings.TrimSpace(lines[si-1]) != "" {
			t.Fatalf("focus %v: the divider carries the agent and its tabs, then come a blank line, the input, a blank line and the strip:\n%s", f, stripANSI(v))
		}
	}
}

func TestPermissionShowsWholeCommand(t *testing.T) {
	m := channelModel()
	long := "for f in $(ls /very/long/path/to/somewhere/deep/in/the/tree); do echo processing \"$f\" && sleep 1 && rm -f \"$f\".bak; done"
	m.prompts = []protocol.PromptInfo{{ID: "p1", Kind: "permission", Tool: "shell", Agent: "a", Input: []byte(`{"command":` + strconv.Quote(long+"\necho second line") + `}`)}}
	m.focus = focusPermission
	v := stripANSI(strings.Join(m.tabBodyLines(52), "\n"))
	// every line fits the width, nothing is elided, and the newline is kept
	for _, l := range strings.Split(v, "\n") {
		if ansi.StringWidth(l) > 52 || strings.Contains(l, "…") {
			t.Fatalf("line %q too wide or elided:\n%s", l, v)
		}
	}
	joined := strings.ReplaceAll(strings.ReplaceAll(v, "\n  ", ""), "\n", "")
	for _, want := range []string{"rm -f \"$f\".bak; done", "echo second line"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
	if !strings.Contains(v, "\n  echo second line") {
		t.Fatalf("newline in the command should start a new row:\n%s", v)
	}
}

func TestDividerAndStripRepo(t *testing.T) {
	m := channelModel()
	m.showTree = false
	m.hideKeys = false // the key bar is off by default; this test checks the rows above it
	m.channel.Dir = "/repo/project"
	m.channel.Model, m.agents[0].Model = "chatgpt/gpt-5", "chatgpt/gpt-5"
	m.agents[0].Tokens, m.agents[0].CostUSD = 1500, 0.02
	m.width, m.height = 100, 30
	m.layout()
	lines := strings.Split(stripANSI(m.View()), "\n")
	rule, strip := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "─ coder ─ "):
			rule = i
		case strings.HasPrefix(l, "! "):
			strip = i
		}
	}
	if rule < 0 || strip != rule+4 { // a blank line, the input, a blank line, the strip
		t.Fatalf("no divider or strip under it:\n%s", strings.Join(lines, "\n"))
	}
	// the agent on the left, its tabs then tokens and cost on the right; no
	// context figure while the window is unknown; tokens and cost are the nav's
	if row := lines[rule]; !strings.HasPrefix(row, "─ coder ─ gpt-5 ─ default ─") || strings.Contains(row, "·") || !strings.HasSuffix(row, "─ async 0 ─ todo 0 ─ mcp 0 ─") || strings.Contains(row, "tokens") || strings.Contains(row, "$") ||
		strings.Contains(row, "%") || strings.Contains(row, "/repo/project") || ansi.StringWidth(row) != 100 {
		t.Fatalf("divider %q", row)
	}
	// with a window: "used% · used/window tokens"
	m.agents[0].Context, m.agents[0].ContextWindow = 62_000, 200_000
	if right := stripANSI(m.footerRightView()); right != "31% · 62k/200k tokens" {
		t.Fatalf("usage: %q", right)
	}
	if got := stripANSI(contextBar(250_000, 200_000)); got != "100% · 250k/200k tokens" {
		t.Fatalf("context %q", got)
	}
	if contextBar(5, 0) != "" {
		t.Fatal("no context figure without a window")
	}
	// a running compaction is a chat item: a rule with a sweeping bar, which
	// the result replaces in place; the status line is not involved
	now := time.Now()
	m.status = ""
	m.applyEvent(event.Event{Seq: 50, Agent: "a", Type: event.CompactionStarted, Time: now, Payload: event.MustPayload(event.CompactionPayload{Before: 84_000})})
	tr := m.transcript("a")
	if !m.compactTick || !tr.Compacting() || m.status != "" {
		t.Fatalf("compaction should be tracked in the chat: tick=%v compacting=%v status=%q", m.compactTick, tr.Compacting(), m.status)
	}
	m.refreshViewport()
	v := stripANSI(m.vp.View())
	if !strings.Contains(v, "┄┄ compacting ") || strings.Count(v, "▰")+strings.Count(v, "▱") != 10 || strings.Contains(stripANSI(m.statusLine(100)), "compact") {
		t.Fatalf("chat while compacting:\n%s", v)
	}
	if a, b := stripANSI(render.CompactSweep(0)), stripANSI(render.CompactSweep(5)); a == b {
		t.Fatalf("the bar should move: %q %q", a, b)
	}
	before := len(tr.All())
	m.applyEvent(event.Event{Seq: 51, Agent: "a", Type: event.CompactionDone, Time: now, Payload: event.MustPayload(event.CompactionPayload{FromSeq: 1, ToSeq: 40, Summary: "S", Before: 84_000, After: 12_000})})
	m.refreshViewport()
	v = stripANSI(m.vp.View())
	if tr.Compacting() || m.status != "" || strings.Contains(v, "compacting") || !strings.Contains(v, "┄┄ compacted 84k → 12k tokens ┄┄") {
		t.Fatalf("the result should replace the bar in the chat: status=%q\n%s", m.status, v)
	}
	if len(tr.All()) != before+1 { // blank, rule, summary, blank replaced blank, rule, blank
		t.Fatalf("the result should take the bar's item: %d → %d lines", before, len(tr.All()))
	}
	// a failure replaces the bar with a note; an ended turn with no result
	// marks it interrupted
	m.applyEvent(event.Event{Seq: 52, Agent: "a", Type: event.CompactionStarted, Time: now, Payload: event.MustPayload(event.CompactionPayload{Before: 84_000})})
	m.applyEvent(event.Event{Seq: 53, Agent: "a", Type: event.CompactionFailed, Time: now, Payload: event.MustPayload(event.CompactionPayload{Before: 84_000, Error: "boom"})})
	m.applyEvent(event.Event{Seq: 54, Agent: "a", Type: event.CompactionStarted, Time: now, Payload: event.MustPayload(event.CompactionPayload{Before: 84_000})})
	m.applyEvent(event.Event{Seq: 55, Agent: "a", Type: event.TurnEnded, Time: now, Payload: event.MustPayload(event.TurnEndedPayload{Turn: 1, Reason: "error"})})
	m.refreshViewport()
	v = stripANSI(m.vp.View())
	if tr.Compacting() || !strings.Contains(v, "Compaction failed boom") || !strings.Contains(v, "Compaction interrupted") || strings.Contains(v, "compacting ") {
		t.Fatalf("failed and interrupted compactions:\n%s", v)
	}
	// the tick stops once nothing is compacting
	nm, _ := m.Update(compactTickMsg{})
	if nm.(Model).compactTick {
		t.Fatal("the tick should stop when no chat is compacting")
	}
	if strip < 4 || rule != strip-4 || strings.TrimSpace(lines[strip-1]) != "" || !strings.HasPrefix(lines[strip-2], " ASK › ") || strings.TrimSpace(lines[strip-3]) != "" {
		t.Fatalf("under the divider come a blank line, the input, a blank line and the strip:\n%s", strings.Join(lines, "\n"))
	}
	// the repo is not on the strip (the dirs tab shows it)
	if strings.Contains(strings.Join(lines, "\n"), "/repo/project") {
		t.Fatal("the repo path should not appear in the footer")
	}
}

func TestTurnIndicatorFollowsPrompts(t *testing.T) {
	m := channelModel()
	m.showTree = false
	m.transcript("a").Apply(event.Event{Seq: 1, Agent: "a", Type: event.TurnStarted, Time: time.Now(), Payload: event.MustPayload(event.TurnPayload{Turn: 1})})
	m.refreshViewport()
	if v := stripANSI(m.View()); !strings.Contains(v, transcript.TurnVerbs[0]+"…") || strings.Contains(v, "permission requested") {
		t.Fatalf("mid-turn:\n%s", v)
	}
	m.applyPromptNotification(protocol.PromptNotification{Action: "requested", Prompt: protocol.PromptInfo{ID: "p", Kind: "permission", Agent: "a", Tool: "shell"}})
	if v := stripANSI(m.View()); !strings.Contains(v, "! permission requested") || strings.Contains(v, transcript.TurnVerbs[0]+"…") {
		t.Fatalf("waiting on a permission:\n%s", v)
	}
	// a prompt for another agent does not change the selected agent's indicator
	m.applyPromptNotification(protocol.PromptNotification{Action: "answered", Prompt: protocol.PromptInfo{ID: "p", Agent: "a"}})
	m.applyPromptNotification(protocol.PromptNotification{Action: "requested", Prompt: protocol.PromptInfo{ID: "q", Kind: "permission", Agent: "b", Tool: "shell"}})
	if v := stripANSI(m.View()); !strings.Contains(v, transcript.TurnVerbs[0]+"…") || strings.Contains(v, "permission requested") {
		t.Fatalf("after the answer:\n%s", v)
	}
}

func TestPermissionArrivalDoesNotStealFocus(t *testing.T) {
	req := func(id string) protocol.PromptNotification {
		return protocol.PromptNotification{Action: "requested", Prompt: protocol.PromptInfo{ID: id, Kind: "permission", Agent: "a", Tool: "shell"}}
	}
	// idle input: the first prompt opens the permission tab
	m := channelModel()
	m.applyPromptNotification(req("p1"))
	if m.focus != focusInput {
		t.Fatalf("idle input should retain focus: focus=%v", m.focus)
	}
	// a second prompt behind a pending one changes nothing
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	m.applyPromptNotification(req("p2"))
	if m.focus != focusInput {
		t.Fatalf("second prompt should not steal focus: %v", m.focus)
	}
	// a draft in the input is never interrupted
	m = channelModel()
	m.input.SetValue("half a thou")
	m.applyPromptNotification(req("p3"))
	if m.focus != focusInput {
		t.Fatalf("typing should keep focus: %v", m.focus)
	}
	// nor is another section
	m = channelModel()
	m.setFocus(focusChat)
	m.applyPromptNotification(req("p4"))
	if m.focus != focusChat {
		t.Fatalf("chat focus should stay: %v", m.focus)
	}
}

func TestParentAgentCreateLineFollowsChildEvents(t *testing.T) {
	m := channelModel()
	m.agents = []protocol.AgentInfo{{ID: "a", Name: "main", Role: "coder"}}
	m.selected = 0
	ev := func(seq int64, agent string, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Channel: "s", Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	m.applyEvent(ev(1, "a", event.TurnStarted, event.TurnPayload{Turn: 1}))
	feed(func(e event.Event) { e.Channel = "s"; m.applyEvent(e) }, toolCall(2, "a", "c1", "agent_create", `{"archetype":"explorer","label":"scout","task":"look"}`))
	m.applyEvent(ev(3, "c1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "c1", Parent: "a", Role: "explorer", Name: "scout", Model: "fake/m1", Depth: 1}))
	m.applyEvent(ev(4, "a", event.ToolFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "agent_create", Output: "spawned scout (explorer) as c1"}))
	tone := func() transcript.Tone {
		for _, l := range m.transcript("a").All() {
			if l.Kind == transcript.LineTool && l.Tool == "agent_create" {
				return l.Tone
			}
		}
		t.Fatal("no agent_create line in the parent's chat")
		return transcript.ToneNone
	}
	if tone() != transcript.ToneWorking {
		t.Fatalf("child running: tone %v", tone())
	}
	m.applyEvent(ev(5, "c1", event.TurnStarted, event.TurnPayload{Turn: 1}))
	if tone() != transcript.ToneWorking {
		t.Fatalf("child mid-turn: tone %v", tone())
	}
	m.applyEvent(ev(6, "c1", event.TurnEnded, event.TurnEndedPayload{Turn: 1, Reason: "end_turn"}))
	if tone() != transcript.ToneNone {
		t.Fatalf("child idle after answering: tone %v", tone())
	}
	m.applyEvent(ev(7, "c1", event.AgentKilled, nil))
	if tone() != transcript.ToneError {
		t.Fatalf("child killed: tone %v", tone())
	}
}

func TestLastSnippet(t *testing.T) {
	tr := transcript.NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	if lastSnippet(nil) != "" || lastSnippet(tr) != "" {
		t.Fatal("empty")
	}
	feed(tr.Apply, userMsg(1, "a", "look around"))
	if got := lastSnippet(tr); got != "@user: look around" {
		t.Fatalf("prompt: %q", got)
	}
	feed(tr.Apply, toolCall(2, "a", "c1", "shell", `{"command":"ls -la"}`))
	if got := lastSnippet(tr); got != "Shell  ls -la" {
		t.Fatalf("tool: %q", got)
	}
	tr.Apply(mk(3, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "Found **three** files.\n"}}}))
	if got := lastSnippet(tr); got != "Found three files." { // plain: bold markers dropped
		t.Fatalf("text (trailing blank skipped): %q", got)
	}
	// a multi-line message reads from its first line, not its last
	tr.Apply(mk(4, event.AssistantMessage, event.AssistantMessagePayload{Turn: 2, Blocks: []model.Block{{Type: model.BlockText, Text: "First the plan.\nThen the details.\nFinally the caveat."}}}))
	if got := lastSnippet(tr); !strings.HasPrefix(got, "First the plan. Then the details.") {
		t.Fatalf("multi-line message should start at its beginning: %q", got)
	}
	// a tool call with output: the call line comes first
	feed(tr.Apply, toolCall(5, "a", "c2", "shell", `{"command":"go test"}`))
	tr.Apply(mk(6, event.ToolFinished, event.ToolFinishedPayload{CallID: "c2", Name: "shell", Output: "ok\nPASS"}))
	if got := lastSnippet(tr); !strings.HasPrefix(got, "Shell  go test") {
		t.Fatalf("tool item should start with the call: %q", got)
	}
}

func TestHelpTogglesKeyBar(t *testing.T) {
	m := channelModel()
	m.showTree = false
	m.width, m.height = 100, 30
	m.layout()
	before := m.vp.Height
	if _, kb := m.keyBarView(); kb != 0 || strings.Contains(stripANSI(m.View()), "history") {
		t.Fatal("the key bar should be hidden by default")
	}
	m.command("/help")
	v := stripANSI(m.View())
	if _, kb := m.keyBarView(); kb == 0 || !strings.Contains(v, "enter") || strings.Count(v, "\n")+1 != m.height {
		t.Fatalf("after /help the key bar should show and the view still fill the window (kb=%d):\n%s", kb, v)
	}
	if m.vp.Height >= before {
		t.Fatalf("the chat should shrink to make room: %d → %d", before, m.vp.Height)
	}
	m.command("/help")
	if _, kb := m.keyBarView(); kb != 0 {
		t.Fatal("/help again should hide the key bar")
	}
}

func TestEscTwiceCancelsTheTurn(t *testing.T) {
	m := channelModel()
	m.agents[0].State = "running"
	esc := tea.KeyMsg{Type: tea.KeyEsc}
	// text in the input: esc clears it and does not arm
	m.input.SetValue("draft")
	if cmd := press(&m, esc); cmd != nil || m.input.Value() != "" || !m.cancelArmed.IsZero() {
		t.Fatalf("esc with text should only clear: cmd=%v value=%q armed=%v", cmd != nil, m.input.Value(), m.cancelArmed)
	}
	// empty input, busy agent: first esc arms with a warning, second cancels
	if cmd := press(&m, esc); cmd == nil || m.cancelArmed.IsZero() || !strings.Contains(m.status, "esc again") {
		t.Fatalf("first esc should warn and arm: status=%q armed=%v", m.status, m.cancelArmed)
	}
	if cmd := press(&m, esc); cmd == nil || !m.cancelArmed.IsZero() {
		t.Fatalf("second esc should send the cancel and disarm: cmd=%v armed=%v", cmd != nil, m.cancelArmed)
	}
	// another key in between disarms
	press(&m, esc)
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !m.cancelArmed.IsZero() {
		t.Fatal("typing should disarm the cancel")
	}
	m.input.Reset()
	// an expired arm starts over
	press(&m, esc)
	m.cancelArmed = time.Now().Add(-2 * cancelWindow)
	press(&m, esc)
	if m.cancelArmed.IsZero() || time.Since(m.cancelArmed) > time.Second {
		t.Fatal("after the window a fresh esc should re-arm rather than cancel")
	}
	// an idle agent: esc does nothing
	m.agents[0].State = "idle"
	m.cancelArmed = time.Time{}
	if cmd := press(&m, esc); cmd != nil || !m.cancelArmed.IsZero() {
		t.Fatal("esc on an idle agent should be inert")
	}
}

func TestFocusAlwaysLandsLeftmost(t *testing.T) {
	m := channelModel()
	tab := tea.KeyMsg{Type: tea.KeyTab}
	right := tea.KeyMsg{Type: tea.KeyRight}
	press(&m, tab, right) // input → strip, highlight moved to agents
	if m.focus != focusTabs || m.tabSel != 1 {
		t.Fatalf("setup: focus=%v sel=%d", m.focus, m.tabSel)
	}
	press(&m, tab, right, right) // strip → meta row, then over to the variant
	if m.focus != focusMeta || m.metaSel != metaVariant {
		t.Fatalf("setup: focus=%v sel=%v", m.focus, m.metaSel)
	}
	// coming back, neither remembers where it was: leftmost again
	press(&m, tab, tab, tab) // meta row → chat → input → strip
	if m.focus != focusTabs || m.tabSel != 0 {
		t.Fatalf("strip should land on permission: focus=%v sel=%d", m.focus, m.tabSel)
	}
	press(&m, tab)
	if m.focus != focusMeta || m.metaSel != metaRole {
		t.Fatalf("meta row should land on its leftmost part, the role: focus=%v sel=%v", m.focus, m.metaSel)
	}
}

func TestTodoTabAndDialog(t *testing.T) {
	m := channelModel()
	m.agents[0].Role = "general"
	// empty: the tab reads (0) and its dialog says so
	if sv := stripANSI(tabsView(m, 120)); !strings.Contains(sv, "async 0 ─ todo 0") {
		t.Fatalf("strip:\n%s", sv)
	}
	m.focus = focusTodo
	if dv := stripANSI(m.tabDialog(120)); !strings.Contains(dv, "Todo 0") || !strings.Contains(dv, "no todo items") {
		t.Fatalf("empty todo dialog:\n%s", dv)
	}
	m.focus = focusInput
	m.agents[0].Todos = []event.TodoItem{
		{ID: "t1", Text: "Read the code", Status: "done"},
		{ID: "t2", Text: "Fix the bug", Status: "in_progress"},
		{ID: "t3", Text: "Run the tests", Status: "pending"},
		{ID: "t4", Text: "Write docs", Status: "cancelled"},
	}
	// the count is done (finished + cancelled) over total
	if sv := stripANSI(tabsView(m, 120)); !strings.Contains(sv, "todo 2/4") {
		t.Fatalf("strip with items:\n%s", sv)
	}
	// tab → strip, → x3 lands on todo, enter opens its dialog
	tab := tea.KeyMsg{Type: tea.KeyTab}
	right := tea.KeyMsg{Type: tea.KeyRight}
	press(&m, tab, right, right, right, tea.KeyMsg{Type: tea.KeySpace})
	if m.focus != focusTodo {
		t.Fatalf("focus %v", m.focus)
	}
	dv := stripANSI(m.tabDialog(120))
	lines := strings.Split(dv, "\n")
	// the title (with esc: close), then the four items in order
	if title := findLine(lines, "Todo 2/4"); title < 0 || !strings.Contains(lines[title], "esc: close") ||
		!inOrder(dv, "Todo 2/4", "● Read the code", "◐ Fix the bug", "○ Run the tests", "× Write docs") {
		t.Fatalf("todo dialog:\n%s", dv)
	}
	if first := findLine(lines, "● Read the code"); !strings.Contains(lines[first], "▸") {
		t.Fatalf("the cursor should start on the first row:\n%s", dv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown})
	if m.agCursor != 1 {
		t.Fatalf("↓ should move the cursor: %d", m.agCursor)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusTabs || m.tabSel != 3 {
		t.Fatalf("esc should return to the strip on todo: focus=%v sel=%d", m.focus, m.tabSel)
	}
	// clicking the todo label on the strip opens the dialog
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	m.width, m.height = max(m.width, 120), max(m.height, 40)
	m.layout()
	lay := m.rows()
	row := stripANSI(m.ruleLine(m.width)) // the agent's tabs sit on the divider, before the usage
	x := ansi.StringWidth(row[:strings.Index(row, "todo")]) + 1
	nm, _ := m.Update(tea.MouseMsg{X: x, Y: lay.rule, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	nm, _ = nm.(Model).Update(tea.MouseMsg{X: x, Y: lay.rule, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	m = nm.(Model)
	if m.focus != focusTodo {
		t.Fatalf("clicking the todo label should open its dialog: %v", m.focus)
	}
	// the turn indicator names the in-progress item
	m.setFocus(focusInput)
	tr := m.transcript("a")
	tr.Apply(event.Event{Agent: "a", Type: event.TurnStarted, Payload: event.MustPayload(event.TurnPayload{Turn: 1})})
	m.refreshViewport()
	if v := stripANSI(m.vp.View()); !strings.Contains(v, "… · Fix the bug") {
		t.Fatalf("indicator should carry the in-progress item:\n%s", v)
	}
	// chat lines for the tools
	if got := transcript.ToolArg("todo", []byte(`{"update":[{"id":"t2","status":"done"}],"add":[{"text":"Run the tests"}]}`)); got != "t2 → done · Run the tests" {
		t.Fatalf("todo update arg %q", got)
	}
	if got := transcript.ToolArg("todo", []byte(`{"add":[{"text":"Run the tests"}]}`)); got != "Run the tests" {
		t.Fatalf("todo add arg %q", got)
	}
	if g, _ := transcript.ToolGlyph("todo"); g != transcript.GlyphToolTodo {
		t.Fatalf("todo glyph %q", g)
	}
}

func TestOverlayClosesBackToItsOrigin(t *testing.T) {
	m := channelModel()
	// opened from the meta row: closing leaves the meta row focused, the input blurred
	m.setFocus(focusMeta)
	m.openOverlay(newOverlay(ovRoles, overlayList, "Roles"))
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.ov != nil || m.focus != focusMeta || m.input.Focused() {
		t.Fatalf("meta origin: ov=%v focus=%v input=%v", m.ov != nil, m.focus, m.input.Focused())
	}
	// opened from the input: closing refocuses the input
	m.setFocus(focusInput)
	m.openOverlay(newOverlay(ovRoles, overlayList, "Roles"))
	if m.input.Focused() {
		t.Fatal("an open overlay should blur the input")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.ov != nil || m.focus != focusInput || !m.input.Focused() {
		t.Fatalf("input origin: ov=%v focus=%v input=%v", m.ov != nil, m.focus, m.input.Focused())
	}
}

func TestCtrlCTwiceQuits(t *testing.T) {
	m := channelModel()
	m.input.SetValue("draft")
	m.setFocus(focusChat)
	c := tea.KeyMsg{Type: tea.KeyCtrlC}
	cmd := press(&m, c)
	if cmd == nil || m.input.Value() != "" || m.focus != focusInput || m.quitArmed.IsZero() || !strings.Contains(m.status, "ctrl+c again") {
		t.Fatalf("first ctrl+c should clear, focus the input and warn: value=%q focus=%v status=%q", m.input.Value(), m.focus, m.status)
	}
	if msg := cmd(); msg != nil {
		if _, quit := msg.(tea.QuitMsg); quit {
			t.Fatal("first ctrl+c must not quit")
		}
	}
	// typing in between disarms
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if !m.quitArmed.IsZero() {
		t.Fatal("typing should disarm the quit")
	}
	press(&m, c)
	cmd = press(&m, c)
	if cmd == nil {
		t.Fatal("second ctrl+c should quit")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Fatalf("second ctrl+c should quit, got %T", cmd())
	}
	// an expired arm starts over
	m.quitArmed = time.Now().Add(-2 * cancelWindow)
	cmd = press(&m, c)
	if _, quit := cmd().(tea.QuitMsg); quit {
		t.Fatal("after the window a fresh ctrl+c should re-arm rather than quit")
	}
}

func TestChannelItemAndBind(t *testing.T) {
	created := time.Now().Add(-3 * time.Hour).Format(time.RFC3339)
	it := channelItem(protocol.ChannelInfo{ID: "s1", Name: "proj", Title: "fix the login bug", Created: created, Model: "openai/gpt-5", CostUSD: 0.12, Live: 2}, false)
	if it.label != "#proj" || !strings.HasPrefix(it.hint, "fix the login bug · 3h00m ago · openai/gpt-5 · $0.12 · 2 live") || it.good {
		t.Fatalf("item: %+v", it)
	}
	it = channelItem(protocol.ChannelInfo{ID: "s2", Name: "proj-2", Created: created}, true)
	if it.label != "#proj-2" || !strings.HasSuffix(it.hint, "current") || !it.good {
		t.Fatalf("current empty item: %+v", it)
	}

	// binding another channel starts every per-channel piece of state over
	m := channelModel()
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Agent: "a"}}
	m.chatCursor = 3
	m.seq = 42
	m.reconciled = true
	m.bindChannel(protocol.ChannelInfo{ID: "other", Dir: "/x"})
	if m.channelID != "other" || m.channel.Dir != "/x" || len(m.agents) != 0 || len(m.transcripts) != 0 || m.seq != 0 || len(m.prompts) != 1 || m.chatCursor != 0 || m.reconciled || m.focus != focusInput {
		t.Fatalf("state after bind: id=%s agents=%d transcripts=%d seq=%d prompts=%d cursor=%d reconciled=%v focus=%v", m.channelID, len(m.agents), len(m.transcripts), m.seq, len(m.prompts), m.chatCursor, m.reconciled, m.focus)
	}
	if !m.isHome() {
		t.Fatal("a freshly bound channel shows the home screen until its history replays")
	}
}

func TestChannelsPickerListsEveryChannel(t *testing.T) {
	m := channelModel()
	m.channelID = "cur"
	m.onChannels(channelsMsg{channels: []protocol.ChannelInfo{
		{ID: "cur", Name: "beta", Created: time.Now().Format(time.RFC3339)},
		{ID: "empty", Name: "gamma", Created: time.Now().Format(time.RFC3339)},
		{ID: "old", Name: "alpha", Title: "fix the login bug", Created: time.Now().Format(time.RFC3339)},
	}})
	if m.ov == nil || m.ov.kind != ovChannels {
		t.Fatal("picker should open")
	}
	var ids []string
	for _, it := range m.ov.items {
		ids = append(ids, it.id)
	}
	if strings.Join(ids, " ") != "old cur empty" {
		t.Fatalf("picker rows %v: every channel is listed, alphabetically, a named one even before its first prompt", ids)
	}
}

func TestMouseHoverMovesChatCursor(t *testing.T) {
	m := channelModel()
	m.showTree = false
	tr := m.transcript("a")
	for i := 0; i < 6; i++ {
		feed(tr.Apply, userMsg(int64(i+2), "a", fmt.Sprintf("prompt %d", i)))
	}
	m.width, m.height = 100, 40
	m.layout()
	m.refreshViewport()
	items := tr.Items()
	// find the screen row of the second item
	r, ok := m.itemRows[1]
	if !ok {
		t.Fatal("no rows for item 1")
	}
	move := func(x, y int) {
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
		m = nm.(Model)
	}
	move(5, r.First-m.vp.YOffset)
	if m.focus != focusChat || m.chatCursor != 1 || !m.hoverFocus {
		t.Fatalf("hover should focus the chat on item 1: focus=%v cursor=%d hover=%v", m.focus, m.chatCursor, m.hoverFocus)
	}
	// the row is highlighted like an arrow-key visit
	markCursorForTest(t)
	m.refreshViewport()
	if !strings.Contains(stripANSI(m.vp.View()), render.GutterMark+"› @user: prompt 0") {
		t.Fatalf("hovered item should carry the cursor:\n%s", stripANSI(m.vp.View()))
	}
	// hovering another item moves the cursor
	r2 := m.itemRows[items-1]
	move(5, r2.First-m.vp.YOffset)
	if m.chatCursor != items-1 {
		t.Fatalf("cursor should follow the mouse: %d", m.chatCursor)
	}
	// leaving the chat area hands focus back to the input
	move(5, m.vp.Height+2)
	if m.focus != focusInput || m.hoverFocus || !m.input.Focused() {
		t.Fatalf("leaving should restore the input: focus=%v hover=%v", m.focus, m.hoverFocus)
	}
	// keyboard focus is not dropped by the mouse leaving
	press(&m, tea.KeyMsg{Type: tea.KeyShiftTab}) // input → chat
	move(5, m.vp.Height+2)
	if m.focus != focusChat {
		t.Fatalf("keyboard chat focus should survive mouse movement: %v", m.focus)
	}
	// a tab dialog owns hover: the chat behind it is left alone
	m.setFocus(focusInput)
	m.setFocus(focusPermission)
	move(5, r.First-m.vp.YOffset)
	if m.focus != focusPermission || m.hoverFocus {
		t.Fatalf("hover must not take focus from an open tab dialog: %v", m.focus)
	}
}

func TestMouseClickTogglesItem(t *testing.T) {
	m := channelModel()
	m.showTree = false
	tr := m.transcript("a")
	feed(tr.Apply, userMsg(1, "a", "list the files"), toolCall(2, "a", "c1", "shell", `{"command":"ls"}`))
	tr.Apply(mk(3, "a", event.ToolFinished, event.ToolFinishedPayload{CallID: "c1", Name: "shell", Output: strings.TrimRight(strings.Repeat("out\n", 8), "\n")}))
	m.width, m.height = 100, 40
	m.layout()
	m.refreshViewport()
	item := tr.Items() - 1
	r := m.itemRows[item]
	click := func(y int) {
		nm, _ := m.Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		nm, _ = nm.(Model).Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
		m = nm.(Model)
	}
	view := func() string { return stripANSI(m.vp.View()) }
	click(r.First - m.vp.YOffset)
	if m.focus != focusChat || m.chatCursor != item || !m.expanded["a"][item] || strings.Count(view(), "out") != 8 {
		t.Fatalf("click should select and expand: focus=%v cursor=%d expanded=%v\n%s", m.focus, m.chatCursor, m.expanded["a"], view())
	}
	click(r.First - m.vp.YOffset)
	if m.expanded["a"][item] || strings.Count(view(), "out") != render.PreviewLines-1 {
		t.Fatalf("second click should collapse to the preview:\n%s", view())
	}
	// a click on the human's own input is inert beyond selecting it
	click(m.itemRows[1].First - m.vp.YOffset)
	if m.chatCursor != 1 || len(m.expanded["a"]) != 0 {
		t.Fatalf("click on the human's input: cursor=%d expanded=%v", m.chatCursor, m.expanded["a"])
	}
}

func TestMouseClicksFocusTabsAndInput(t *testing.T) {
	m := channelModel()
	m.showTree = false
	m.agents = []protocol.AgentInfo{
		{ID: "a", Name: "main", Role: "coder", State: "working", Awaiting: []string{"c1", "c2"}},
		{ID: "c1", Parent: "a", Name: "scout", Role: "explorer", State: "working"},
		{ID: "c2", Parent: "a", Name: "checks", Role: "tester", State: "idle"},
	}
	m.selected = 0
	m.width, m.height = 100, 40
	m.layout()
	m.refreshViewport()
	click := func(x, y int) {
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		nm, _ = nm.(Model).Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
		m = nm.(Model)
	}
	move := func(x, y int) {
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
		m = nm.(Model)
	}
	lay := m.rows()
	// the agent's tabs sit on the divider, before the usage
	metaCol := func(sub string) int {
		row := stripANSI(m.ruleLine(m.width))
		return ansi.StringWidth(row[:strings.Index(row, sub)]) + 1
	}
	click(metaCol("async"), lay.rule)
	if m.focus != focusAsync {
		t.Fatalf("clicking the async label should open the async tab: %v", m.focus)
	}
	// the dialog's rows: hovering one moves the cursor, clicking selects
	// that agent like enter and closes the dialog
	rowAt := func(text string) (int, int) {
		for y, l := range strings.Split(stripANSI(m.View()), "\n") {
			if i := strings.Index(l, text); i >= 0 {
				return i, y
			}
		}
		t.Fatalf("%q not on screen:\n%s", text, stripANSI(m.View()))
		return 0, 0
	}
	x, y := rowAt("checks")
	move(x, y)
	if m.agCursor != 1 || m.focus != focusAsync {
		t.Fatalf("hovering the second agent row should move the cursor: %d focus=%v", m.agCursor, m.focus)
	}
	click(x, y)
	if m.selectedID() != "c2" || m.focus != focusInput {
		t.Fatalf("clicking an agent row should select it: %s focus=%v", m.selectedID(), m.focus)
	}
	m.selected = 0
	// the strip labels still open dialogs directly while one is up
	click(metaCol("async"), lay.rule)
	click(metaCol("todo"), lay.rule)
	if m.focus != focusTodo {
		t.Fatalf("clicking a strip label should open that tab's dialog: %v", m.focus)
	}
	// clicking the input focuses it (and closes the dialog)
	lay = m.rows()
	click(5, lay.input)
	if m.focus != focusInput || !m.input.Focused() {
		t.Fatalf("clicking the input should focus it: %v", m.focus)
	}
	// hover over the chat while a tab dialog is open leaves the focus alone
	click(2, lay.strip) // permission tab
	if m.focus != focusPermission {
		t.Fatalf("permission tab: %v", m.focus)
	}
	r := m.itemRows[0]
	move(3, r.First-m.vp.YOffset)
	if m.focus != focusPermission || m.hoverFocus {
		t.Fatalf("hover must not take focus from the dialog: focus=%v hover=%v", m.focus, m.hoverFocus)
	}
}

func TestMetaRowHits(t *testing.T) {
	m := channelModel()
	m.agents = []protocol.AgentInfo{{ID: "a", Name: "main", Role: "coder", Model: "openai/gpt-5", Variant: "high"}}
	m.selected = 0
	m.channel.Mode = protocol.ModeYolo
	// "─ main (coder) ─ gpt-5 ─ high ───" leads the divider (the mode tag leads the input)
	row := stripANSI(m.ruleLine(m.width))
	at := func(sub string) int { return ansi.StringWidth(row[:strings.Index(row, sub)]) + 1 } // a column, not a byte offset
	for _, c := range []struct {
		x    int
		want metaPart
	}{
		{0, metaNone}, {at("main"), metaRole}, {at("(coder)"), metaRole},
		{at("gpt-5"), metaModel}, {at("high"), metaVariant}, {m.width - 1, metaNone},
	} {
		if got := m.metaHit(c.x); got != c.want {
			t.Fatalf("x=%d: got %v want %v\n%q", c.x, got, c.want, row)
		}
	}
	// whatever the mode, the divider starts with the role; a missing variant reads "default"
	m.channel.Mode = protocol.ModeAsk
	m.agents[0].Variant = ""
	row = stripANSI(m.ruleLine(m.width))
	if !strings.HasPrefix(row, "─ main (coder)") || m.metaHit(2) != metaRole || m.metaHit(at("default")+1) != metaVariant {
		t.Fatalf("ask divider: %q", row)
	}
}

func TestDialogRowsTakeTheMouse(t *testing.T) {
	m := channelModel()
	m.width, m.height = 100, 40
	m.layout()
	o := newOverlay(ovVariants, overlayList, "Variant")
	o.setItems([]overlayItem{{id: "", label: "default"}, {id: "low", label: "low"}, {id: "high", label: "high"}})
	m.openOverlay(o)
	// find row 2 ("high") by scanning the composited screen for its text
	view := stripANSI(m.View())
	lines := strings.Split(view, "\n")
	y := -1
	for i, l := range lines {
		if strings.Contains(l, "high") {
			y = i
		}
	}
	if y < 0 {
		t.Fatalf("no row for high:\n%s", view)
	}
	x := strings.Index(lines[y], "high")
	if idx, ok := m.ov.itemAt(x, y, m.width, m.bodyHeight(), ""); !ok || idx != 2 {
		t.Fatalf("itemAt(%d,%d) = %d %v", x, y, idx, ok)
	}
	// hover moves the dialog cursor; click picks the row (the dialog closes)
	nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
	m = nm.(Model)
	if m.ov == nil || m.ov.cursor != 2 {
		t.Fatalf("hover should move the dialog cursor: %+v", m.ov)
	}
	nm, _ = m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	nm, _ = nm.(Model).Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	m = nm.(Model)
	if m.ov != nil {
		t.Fatal("clicking a row should pick it and close the dialog")
	}
}

func TestModeChangeShowsInEveryChat(t *testing.T) {
	m := channelModel()
	m.agents = []protocol.AgentInfo{{ID: "a", Name: "main"}, {ID: "b", Parent: "a", Name: "scout"}}
	m.applyEvent(event.Event{Seq: 9, Channel: "s", Type: event.ChannelUpdated, Time: time.Now(), Payload: event.MustPayload(event.ChannelUpdatedPayload{Mode: event.Str("auto")})})
	for _, id := range []string{"a", "b"} {
		found := false
		for _, l := range m.transcript(id).All() {
			if strings.Contains(l.Text, "**Mode** → auto") {
				found = true
			}
		}
		if !found {
			t.Fatalf("agent %s chat lacks the mode line", id)
		}
	}
	if m.channel.Mode != "auto" || m.modeTag() != "AUTO" {
		t.Fatalf("channel mode should follow the event: %q", m.channel.Mode)
	}
	// /mode lists the three modes with the current one marked
	m.openMode()
	if m.ov == nil || len(m.ov.items) != 3 || m.ov.items[2].id != "yolo" || !strings.Contains(m.ov.items[1].hint, "current") {
		t.Fatalf("mode dialog: %+v", m.ov)
	}
}

func TestDragSelectsAndCopies(t *testing.T) {
	m := channelModel()
	m.showTree = false
	tr := m.transcript("a")
	feed(tr.Apply, userMsg(2, "a", "first line here"), userMsg(3, "a", "second line"))
	m.width, m.height = 60, 30
	m.layout()
	m.refreshViewport()
	frame := strings.Split(stripANSI(m.View()), "\n")
	y0, y1 := -1, -1
	for i, l := range frame {
		if strings.Contains(l, "first line here") {
			y0 = i
		}
		if strings.Contains(l, "second line") {
			y1 = i
		}
	}
	if y0 < 0 || y1 <= y0 {
		t.Fatalf("rows %d %d:\n%s", y0, y1, strings.Join(frame, "\n"))
	}
	col := func(line, sub string) int { return ansi.StringWidth(line[:strings.Index(line, sub)]) } // columns, not bytes
	x0 := col(frame[y0], "first")
	x1 := col(frame[y1], "second") + len("second") - 1
	ev := func(msg tea.MouseMsg) {
		nm, _ := m.Update(msg)
		m = nm.(Model)
	}
	ev(tea.MouseMsg{X: x0, Y: y0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.sel.active {
		t.Fatal("a press alone is not a selection")
	}
	ev(tea.MouseMsg{X: x1, Y: y1, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	if !m.sel.active {
		t.Fatal("dragging should start a selection")
	}
	got := m.selectedText()
	if !strings.HasPrefix(got, "first line here") || !strings.HasSuffix(got, "second") || strings.Count(got, "\n") != y1-y0 {
		t.Fatalf("selected text: %q", got)
	}
	// the highlighted frame still has the same plain text
	if stripANSI(m.View()) != strings.Join(frame, "\n") {
		t.Fatal("the highlight must not change the frame's text")
	}
	// releasing copies (a command) and keeps the highlight; the status says so
	nm, cmd := m.Update(tea.MouseMsg{X: x1, Y: y1, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	m = nm.(Model)
	if cmd == nil || !m.sel.active || !strings.HasPrefix(m.status, "copied ") {
		t.Fatalf("release: cmd=%v active=%v status=%q", cmd != nil, m.sel.active, m.status)
	}
	// a drag never counts as a click on the item under it
	if m.focus == focusChat && len(m.expanded["a"]) != 0 {
		t.Fatal("drag should not toggle items")
	}
	// the next press clears it
	ev(tea.MouseMsg{X: 1, Y: 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.sel.active {
		t.Fatal("a new press should drop the old selection")
	}
}

func TestInputGrowsWithTheMessage(t *testing.T) {
	m := channelModel()
	m.showTree = false
	m.width, m.height = 60, 30
	m.layout()
	one := m.vp.Height
	if m.inputRows() != 1 {
		t.Fatalf("empty input should be one line: %d", m.inputRows())
	}
	// an explicit line break grows it
	m.input.SetValue("first line\nsecond line")
	m.layout()
	if m.inputRows() != 2 || m.vp.Height != one-1 {
		t.Fatalf("two lines: height %d, viewport %d (was %d)", m.inputRows(), m.vp.Height, one)
	}
	if got := strings.Count(m.View(), "\n") + 1; got != m.height {
		t.Fatalf("view should still fill the window: %d lines", got)
	}
	// a long line wraps and grows it too
	m.input.SetValue(strings.Repeat("word ", 40))
	m.layout()
	if m.inputRows() < 3 {
		t.Fatalf("200 columns of text in a 60-column box should wrap to several lines: %d", m.inputRows())
	}
	// never past the cap
	m.input.SetValue(strings.Repeat("line\n", 60))
	m.layout()
	if m.inputRows() != m.inputCap() || m.inputCap() != m.height/2 { // half the window
		t.Fatalf("cap: rows=%d cap=%d height=%d", m.inputRows(), m.inputCap(), m.height)
	}
	saved := m.height
	m.height = 12
	m.layout()
	if m.inputCap() != inputMaxLines || m.inputRows() != inputMaxLines {
		t.Fatalf("a short window keeps the floor: rows=%d cap=%d", m.inputRows(), m.inputCap())
	}
	m.height = saved
	// back to one line when cleared
	m.input.Reset()
	m.layout()
	if m.inputRows() != 1 || m.vp.Height != one {
		t.Fatalf("cleared: height %d viewport %d", m.inputRows(), m.vp.Height)
	}
}

func TestInputNewlineAndHistoryKeys(t *testing.T) {
	m := channelModel()
	m.history = []string{"older prompt"}
	m.histIdx = len(m.history)
	type_ := func(s string) {
		for _, r := range s {
			press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
	type_("one")
	press(&m, tea.KeyMsg{Type: tea.KeyCtrlJ}) // ctrl+j breaks the line
	type_("two")
	if m.input.Value() != "one\ntwo" || m.inputRows() != 2 {
		t.Fatalf("ctrl+j should insert a newline: %q height %d", m.input.Value(), m.inputRows())
	}
	// on the second line ↑ moves within the draft, not into history
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "one\ntwo" || m.input.Line() != 0 {
		t.Fatalf("↑ inside a draft should move up a line: %q line %d", m.input.Value(), m.input.Line())
	}
	// on the first line ↑ walks history
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "older prompt" {
		t.Fatalf("↑ on the first line should recall history: %q", m.input.Value())
	}
}

func TestInputShowsOneChevron(t *testing.T) {
	m := channelModel()
	m.width, m.height = 60, 30
	m.input.SetValue("first line\nsecond line\nthird")
	m.layout()
	v := stripANSI(m.inputView())
	if strings.Count(v, "›") != 1 || !strings.HasPrefix(v, " ASK › first line") {
		t.Fatalf("one chevron on the first line only:\n%s", v)
	}
	for i, l := range strings.Split(v, "\n")[1:] {
		if !strings.HasPrefix(l, "  ") {
			t.Fatalf("continuation line %d should be indented under the chevron: %q", i+1, l)
		}
	}
}

func TestPastedMessageKeepsItsFirstLineInView(t *testing.T) {
	m := channelModel()
	m.width, m.height = 80, 30
	m.layout()
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Use subagents to summarize the repo.\nThen compare all responses and\ngive me the highlights."), Paste: true})
	if m.inputRows() != 3 {
		t.Fatalf("three pasted lines should give a three-line input: %d", m.inputRows())
	}
	v := stripANSI(m.inputView())
	lines := strings.Split(v, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], " ASK › Use subagents") || !strings.HasPrefix(lines[2], "       give me the highlights.") {
		t.Fatalf("the whole message should be visible from its first line:\n%s", v)
	}
}

func TestPasteWithCarriageReturns(t *testing.T) {
	m := channelModel()
	m.width, m.height = 80, 30
	m.layout()
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("first line\r\nsecond line\rthird line"), Paste: true})
	if m.input.Value() != "first line\nsecond line\nthird line" || m.inputRows() != 3 {
		t.Fatalf("CR/CRLF should become line breaks: %q height %d", m.input.Value(), m.inputRows())
	}
	if strings.ContainsRune(m.inputView(), '\r') {
		t.Fatal("no carriage return may reach the screen")
	}
}

// TestInputNeverHidesRows: for messages of many shapes the input is tall
// enough that every wrapped row is drawn (the textarea never scrolls).
func TestInputNeverHidesRows(t *testing.T) {
	m := channelModel()
	m.width, m.height = 80, 40
	m.layout()
	w := m.input.Width()
	cases := []string{
		strings.Repeat("a", w),      // exactly fills the row: the textarea spills to a second
		strings.Repeat("a", w-1),    // one short
		strings.Repeat("word ", 60), // long, word-wrapped
		strings.Repeat("x", 3*w+5),  // one unbroken token
		"Use subagents that in turn call other subagents to summarize the repo. Then compare all responses and give me the highlights.",
		"short\n" + strings.Repeat("longer second line ", 12) + "\nend",
	}
	for _, text := range cases {
		m.input.SetValue(text)
		m.layout()
		view := stripANSI(m.inputView())
		rows := strings.Split(view, "\n")
		first := strings.Split(text, "\n")[0]
		head := first
		if len([]rune(head)) > 10 {
			head = string([]rune(head)[:10])
		}
		if !strings.HasPrefix(rows[0], " ASK › "+head) {
			t.Fatalf("first row hidden for %q (height %d):\n%s", head, m.inputRows(), view)
		}
		if m.inputRows() > m.inputCap() {
			t.Fatalf("over the cap: %d", m.inputRows())
		}
		// the end of the message is on screen too (unless capped)
		last := []rune(strings.TrimRight(text, " "))
		tail := string(last[max(0, len(last)-5):])
		if m.inputRows() < m.inputCap() && !strings.Contains(view, strings.TrimSpace(tail)) {
			t.Fatalf("last row hidden for tail %q (height %d):\n%s", tail, m.inputRows(), view)
		}
	}
}

func TestCatalogDoesNotPopulateInputHistory(t *testing.T) {
	m := newModel(context.Background(), nil, "cur")
	m.width, m.height = 100, 40
	m.reconciled = true
	m.layout()
	now := time.Now()
	nm, _ := m.Update(channelsMsg{purpose: channelsNav, channels: []protocol.ChannelInfo{
		{ID: "cur", Title: "the one we are in", Created: now.Format(time.RFC3339)},
		{ID: "empty", Created: now.Format(time.RFC3339)},
		{ID: "s1", Title: "fix the login bug", Created: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		{ID: "s2", Title: "add a README section", Created: now.Add(-26 * time.Hour).Format(time.RFC3339)},
		{ID: "s3", Title: "fix the login bug", Created: now.Add(-50 * time.Hour).Format(time.RFC3339)}, // duplicate title
	}})
	m = nm.(Model)
	if m.ov != nil || strings.Contains(stripANSI(m.View()), "recent") {
		t.Fatal("no picker and no list: the history lives in ↑/↓")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "" {
		t.Fatalf("another channel's prompt leaked into history: %q", m.input.Value())
	}
	// a channel that already has its own history is left alone
	m.history = []string{"typed here"}
	m.histIdx = 1
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "typed here" {
		t.Fatal("this channel's own history was not recalled")
	}
}

func TestSidebarOnTheLeftAndMouseOffsets(t *testing.T) {
	m := channelModel()
	m.showTree = true
	m.width, m.height = 120, 40
	m.layout()
	m.refreshViewport()
	lines := strings.Split(stripANSI(m.View()), "\n")
	// the sidebar's header (the app name over the channel directory) is at
	// the left edge, the chat to its right
	found := false
	for _, l := range lines {
		if strings.HasPrefix(l, "Stavlos") {
			found = true
		}
	}
	if !found {
		t.Fatalf("sidebar should be on the left:\n%s", strings.Join(lines, "\n"))
	}
	// the divider and everything below span the whole window; the sidebar
	// stops above the divider
	rule := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "─") {
			rule = i
			break
		}
	}
	if rule < 0 || ansi.StringWidth(lines[rule]) != m.width || strings.TrimSpace(lines[rule+1]) != "" || !strings.HasPrefix(lines[rule+2], " ASK › ") {
		t.Fatalf("the rule should start at the left edge and span the window:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(lines[rule], "│") || strings.Contains(lines[rule+1], "│") {
		t.Fatal("the sidebar separator must not run past the divider")
	}
	ev := func(msg tea.MouseMsg) tea.Cmd {
		nm, cmd := m.Update(msg)
		m = nm.(Model)
		return cmd
	}
	// a click in the sidebar focuses it
	ev(tea.MouseMsg{X: 2, Y: 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	ev(tea.MouseMsg{X: 2, Y: 1, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	if m.focus != focusSidebar {
		t.Fatalf("click in the sidebar: focus=%v", m.focus)
	}
	// hovering the chat, right of the sidebar, still selects an item
	m.setFocus(focusInput)
	r := m.itemRows[0]
	ev(tea.MouseMsg{X: sidebarWidth + 1 + 3, Y: r.First - m.vp.YOffset, Action: tea.MouseActionMotion})
	if m.focus != focusChat || m.chatCursor != 0 {
		t.Fatalf("hover over the chat with the sidebar open: focus=%v cursor=%d", m.focus, m.chatCursor)
	}
	// hovering a sidebar row takes the chat's hover and marks that row
	// (the rows start below the header; the chat's own offsets land in it)
	ev(tea.MouseMsg{X: 2, Y: len(m.sidebarHeader(sidebarWidth - 1)), Action: tea.MouseActionMotion})
	if m.focus != focusSidebar || !m.hoverFocus {
		t.Fatalf("hover over a sidebar row: focus=%v hover=%v", m.focus, m.hoverFocus)
	}
	// the header is not a row: hover releases back to the input
	ev(tea.MouseMsg{X: 2, Y: 1, Action: tea.MouseActionMotion})
	if m.focus != focusInput || m.hoverFocus {
		t.Fatalf("hover over the sidebar header should release: focus=%v hover=%v", m.focus, m.hoverFocus)
	}
}

func TestSidebarRowsLeaveOneColumn(t *testing.T) {
	m := channelModel()
	m.showTree = true
	m.width, m.height = 120, 40
	m.agents = []protocol.AgentInfo{{ID: "a", Name: "a-very-long-agent-label-that-will-not-fit-here", Role: "general", State: "idle"}}
	m.selected = 0
	m.layout()
	for _, row := range m.treeRows(sidebarWidth - 1) {
		if w := ansi.StringWidth(stripANSI(row)); w != sidebarWidth-1 {
			t.Fatalf("a truncated row should be %d wide, got %d: %q", sidebarWidth-1, w, stripANSI(row))
		}
	}
}

// TestSidebarNav: the sidebar reads as the swarm nav — directory, usage,
// the ! ? tabs, and a tree whose rows carry a needs-you badge and
// the cost at the right edge; n jumps to the next agent waiting on you and
// a click on a row selects that agent.
func TestSidebarNav(t *testing.T) {
	t.Run("header and tree rows", func(t *testing.T) {
		m := sidebarNavModel()
		sb := strings.Split(stripANSI(m.sidebarView(20)), "\n")
		header := len(m.sidebarHeader(sidebarWidth - 1))
		// the title, a blank, the usage rows, a blank, the Clients section (its
		// title, Web UI, Discord), a blank; no Subscriptions section without a reading
		if header != 9 || !strings.HasPrefix(sb[0], "Stavlos") || strings.TrimSpace(sb[1]) != "" ||
			m.sidebarSystemRow() != 2 || strings.Join(strings.Fields(sb[2]), " ") != "System 2k · $0.25" || strings.Join(strings.Fields(sb[3]), " ") != "@main 1k · $0.20" ||
			strings.TrimSpace(sb[4]) != "" || strings.TrimSpace(sb[5]) != "Clients" ||
			m.navRowIndex(navWeb) != 6 || strings.TrimSpace(sb[m.navRowIndex(navWeb)]) != "○ Web UI" || m.sidebarDiscordRow() != 7 || !strings.Contains(sb[m.sidebarDiscordRow()], "Discord checking") ||
			strings.Contains(strings.Join(sb[:9], "\n"), "Subscriptions") ||
			strings.Contains(strings.Join(sb[:9], "\n"), "! 1/1") || strings.TrimSpace(sb[8]) != "" || !strings.HasPrefix(sb[9], "Channels ") || !strings.Contains(sb[9], " "+newChannelMark+" ") || strings.Contains(sb[9], "↑/↓") ||
			strings.Contains(strings.Join(sb, "\n"), "waiting") || strings.Contains(strings.Join(sb, "\n"), "need you") {
			t.Fatalf("header (%d rows):\n%s", header, strings.Join(sb[:10], "\n"))
		}
		// dirs is the open channel's: out of the tabs the strip walks, behind
		// the gear at the right edge of the channel's row; → on the row, or a
		// click on the gear, opens the dirs dialog
		if body, _ := m.sidebarBody(sidebarWidth - 1); !strings.HasSuffix(stripANSI(body[1]), " "+channelGear+" ") || !strings.Contains(stripANSI(body[1]), "#channel · …/x/Work/proj") || !strings.Contains(stripANSI(body[2]), "@main") || ansi.StringWidth(stripANSI(body[1])) != sidebarWidth-1 {
			t.Fatalf("channel row:\n%s", stripANSI(strings.Join(body, "\n")))
		}
		if slices.Contains(m.tabOrder(), focusDirs) {
			t.Fatalf("with the sidebar the strip's tabs skip dirs: %v", m.tabOrder())
		}
		m.setFocus(focusSidebar)
		m.sbCursor = hereRow(m)
		press(&m, tea.KeyMsg{Type: tea.KeyRight})
		if m.cfgEditor == nil || m.cfgEditor.scope.Scope != "project" {
			t.Fatal("→ on the channel row should open project configuration")
		}
		m.closeConfigEditor()
		gear := func(y int) {
			nm, _ := m.Update(tea.MouseMsg{X: sidebarWidth - 3, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
			nm, _ = nm.(Model).Update(tea.MouseMsg{X: sidebarWidth - 3, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
			m = nm.(Model)
		}
		gear(header + hereRow(m))
		if m.cfgEditor == nil || m.cfgEditor.scope.Channel != m.channelID {
			t.Fatal("a click on the gear should open this channel's configuration")
		}
		m.closeConfigEditor()
		// with the sidebar showing, the footer strip keeps only the agent's row
		if sv := stripANSI(m.sectionsView(120)); sv != "" || !strings.Contains(stripANSI(m.ruleLine(120)), "async") { // no footer strip: the agent's tabs sit on the divider
			t.Fatalf("strip with the sidebar:\n%s", sv)
		}
		rows := m.treeRows(sidebarWidth - 1)
		plain := make([]string, len(rows))
		for i, r := range rows {
			plain[i] = stripANSI(r)
			if w := ansi.StringWidth(plain[i]); w != sidebarWidth-1 {
				t.Fatalf("row %d should be %d wide, got %d: %q", i, sidebarWidth-1, w, plain[i])
			}
		}
		if !strings.HasPrefix(plain[0], "  ◐ @main (general)") || !strings.HasSuffix(plain[0], " $0.20") || strings.Contains(plain[0], "waiting") {
			t.Fatalf("root row: %q", plain[0])
		}
		if !strings.HasPrefix(plain[1], "    ! @world-politic") || !strings.HasSuffix(plain[1], " $0.05") || strings.Count(plain[1], "!") != 1 {
			t.Fatalf("an agent waiting on a permission shows ! in place of its dot, and its cost: %q", plain[1])
		}
		if !strings.HasSuffix(strings.TrimRight(plain[2], " "), " @business (general)") {
			t.Fatalf("a row with nothing on the right ends with the label: %q", plain[2])
		}
		if !strings.HasPrefix(plain[3], "    ? @asker") || !strings.HasSuffix(strings.TrimRight(plain[3], " "), "@asker (general)") {
			t.Fatalf("an agent waiting on a question shows ? in place of its dot: %q", plain[3])
		}
		// the channel's own row: a permission waits in it, so ! takes its dot
		if body, _ := m.sidebarBody(sidebarWidth - 1); strings.Fields(stripANSI(body[1]))[0] != "!" {
			t.Fatalf("channel row: %q", stripANSI(body[1]))
		}
	})
	t.Run("n selects the next agent that needs you", func(t *testing.T) {
		m := sidebarNavModel()
		// n jumps to the next agent needing you and selects it; again wraps
		m.setFocus(focusSidebar)
		press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		if m.sbCursor != 3 || m.selectedID() != "b" || m.focus != focusSidebar {
			t.Fatalf("n: cursor=%d selected=%s focus=%v", m.sbCursor, m.selectedID(), m.focus)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		if m.selectedID() != "d" {
			t.Fatalf("second n: %s", m.selectedID())
		}
		press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		if m.selectedID() != "b" {
			t.Fatalf("n should wrap: %s", m.selectedID())
		}
		m.prompts = nil
		press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		if m.selectedID() != "b" || !strings.Contains(m.status, "no agent is waiting") {
			t.Fatalf("n with nothing pending: %s %q", m.selectedID(), m.status)
		}
		if hs := m.keyHints(); hs[2].Key != "n" {
			t.Fatalf("hints %+v", hs)
		}
	})
	t.Run("a click on a tree row selects it", func(t *testing.T) {
		m := sidebarNavModel()
		m.prompts = nil
		// a click on a tree row selects that agent (rows start after the header)
		m.setFocus(focusInput)
		y := sidebarY(m, 4)
		nm, _ := m.Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		nm, _ = nm.(Model).Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
		m = nm.(Model)
		if m.selectedID() != "c" || m.focus != focusSidebar || m.sbCursor != 4 {
			t.Fatalf("click on a row: selected=%s focus=%v cursor=%d", m.selectedID(), m.focus, m.sbCursor)
		}
	})
	t.Run("the directory's channels, alphabetically", func(t *testing.T) {
		m := sidebarNavModel()
		m.prompts = nil
		// every channel of the directory in alphabetical order, whichever is
		// open, each led by its state dot: this one keeps its place with its
		// agents under it; space on another opens it, and so does a click
		m.channel.Name = "proj"
		m.navChannels = resumable([]protocol.ChannelInfo{
			{ID: "s-old", Name: "proj-2", Title: "fix the login bug", State: "working", Created: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
			{ID: "s-older", Name: "docs", Title: "docs sweep", Created: time.Now().Add(-26 * time.Hour).UTC().Format(time.RFC3339)},
		}, m.channelID)
		m.prompts = []protocol.PromptInfo{{ID: "p1", Channel: "s-old", Kind: "permission"}, {ID: "q1", Channel: "s-older", Kind: "question"}} // waiting in the other channels
		m.setFocus(focusSidebar)
		body, items := m.sidebarBody(sidebarWidth - 1)
		plain := make([]string, len(body))
		for i, r := range body {
			plain[i] = stripANSI(r)
		}
		na := len(m.agents)
		if f := strings.Fields(plain[2]); len(body) != na+5 || plain[na+4] != "" || items[na+4] != -1 || (!strings.HasPrefix(plain[0], "Channels ") || !strings.HasSuffix(plain[0], " "+newChannelMark+" ")) || strings.Join(strings.Fields(plain[1]), " ") != "? #docs "+channelGear ||
			strings.Join(f, " ") != "● #proj · /home/x/Work/proj "+channelGear || strings.Join(strings.Fields(plain[na+3]), " ") != "! #proj-2 "+channelGear || strings.Contains(strings.Join(plain, "\n"), "h00m") ||
			items[0] != 0 || items[1] != 1 || items[2] != 2 || items[3] != 3 || items[na+3] != na+3 || hereRow(m) != 2 {
			t.Fatalf("sidebar:\n%s\n%v", strings.Join(plain, "\n"), items)
		}
		for _, i := range []int{1, 2, na + 3} {
			if w := ansi.StringWidth(plain[i]); w != sidebarWidth-1 {
				t.Fatalf("channel rows fill the width: %d %q", w, plain[i])
			}
		}
		for m.sbCursor != 1 {
			press(&m, tea.KeyMsg{Type: tea.KeyDown})
		}
		if cmd := press(&m, tea.KeyMsg{Type: tea.KeySpace}); cmd == nil || !strings.Contains(m.status, "opening #docs") {
			t.Fatalf("space on a channel should open it: cmd=%v status=%q", cmd != nil, m.status)
		}
		for m.sbCursor != na+3 {
			press(&m, tea.KeyMsg{Type: tea.KeyDown})
		}
		press(&m, tea.KeyMsg{Type: tea.KeyDown}) // wraps to the + channel row
		if m.sbCursor != 0 {
			t.Fatalf("wrap: %d", m.sbCursor)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}) // from + channel, n still finds an agent
		m.switching = false                                           // the previous asynchronous selection completed
		y := sidebarY(m, na+3)
		nm, _ := m.Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		nm, _ = nm.(Model).Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
		m = nm.(Model)
		if !strings.Contains(m.status, "opening #proj-2") {
			t.Fatalf("a click on a channel should open it: %q", m.status)
		}
		// → on another channel's row: that channel opens on its dirs dialog
		m.switching = false
		m.setFocus(focusSidebar)
		m.sbCursor = 1
		if cmd := press(&m, tea.KeyMsg{Type: tea.KeyRight}); cmd == nil || !m.dirsNext || !strings.Contains(m.status, "opening #docs") {
			t.Fatalf("→ on #docs: cmd=%v next=%v status=%q", cmd != nil, m.dirsNext, m.status)
		}
		nm, _ = m.Update(switchedMsg{info: protocol.ChannelInfo{ID: "s-older", Name: "docs", Dir: "/x"}})
		m = nm.(Model)
		if m.channelID != "s-older" || m.cfgEditor == nil || m.cfgEditor.scope.Channel != "s-older" || m.dirsNext {
			t.Fatalf("the switch lands on the dirs dialog: id=%s focus=%v next=%v", m.channelID, m.focus, m.dirsNext)
		}
	})
	t.Run("+ channel names a new one", func(t *testing.T) {
		m := sidebarNavModel()
		m.prompts = nil
		// + channel sits above this channel's row, where the sidebar lands;
		// space on it opens a popup naming the new channel: typing (spaces
		// included) fills the field, enter creates it, esc cancels
		m.superChat = true // on the channel chat, the sidebar lands on this channel's row
		m.setFocus(focusSidebar)
		if m.sbCursor != 1 {
			t.Fatalf("the sidebar lands on this channel's row: %d", m.sbCursor)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeySpace})
		if m.ov == nil || m.ov.kind != ovNewChannel || m.ov.mode != overlayInput {
			t.Fatalf("space on the channels title should open the naming popup: %+v", m.ov)
		}
		m.closeOverlay()
		if press(&m, tea.KeyMsg{Type: tea.KeyRight}); m.ov == nil || m.ov.kind != ovNewChannel {
			t.Fatalf("→ on the channels title opens it too: %+v", m.ov)
		}
		m.closeOverlay()
		header := len(m.sidebarHeader(sidebarWidth - 1))
		for _, x := range []int{3, sidebarWidth - 3} { // the title's text does nothing; its + opens the popup
			nm, _ := m.Update(tea.MouseMsg{X: x, Y: header, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
			nm, _ = nm.(Model).Update(tea.MouseMsg{X: x, Y: header, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
			m = nm.(Model)
			if (m.ov != nil) != (x == sidebarWidth-3) {
				t.Fatalf("click at %d on the title: popup=%v", x, m.ov != nil)
			}
		}
		m.setFocus(focusSidebar)
		m.sbCursor = 0
		if dv := stripANSI(m.ov.view(100, "")); !strings.Contains(dv, "New channel") || strings.Contains(dv, "search") || strings.Contains(dv, "nothing to list") {
			t.Fatalf("popup:\n%s", dv)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("site")}, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ops")})
		if m.ov == nil || m.ov.input.Value() != "site ops" {
			t.Fatalf("space types in the popup: %+v", m.ov)
		}
		if hs := m.keyHints(); len(hs) != 2 || hs[0].Key != "enter" || hs[1].Key != "esc" {
			t.Fatalf("hints %+v", hs)
		}
		press(&m, tea.KeyMsg{Type: tea.KeyEnter})
		if m.ov == nil || m.ov.kind != ovNewChannelDir || m.ov.input.Value() != m.channel.Dir || m.ov.newName != "site ops" {
			t.Fatalf("directory step: %+v", m.ov)
		}
		if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.ov != nil || !strings.Contains(m.status, "creating a channel") {
			t.Fatalf("enter should create: cmd=%v ov=%v status=%q", cmd != nil, m.ov != nil, m.status)
		}
		press(&m, tea.KeyMsg{Type: tea.KeySpace})
		if press(&m, tea.KeyMsg{Type: tea.KeyEsc}); m.ov != nil {
			t.Fatal("esc should cancel the popup")
		}
		// the new channel opens on its chat, empty as it is, never the splash
		nm, _ := m.Update(switchedMsg{info: protocol.ChannelInfo{ID: "new", Name: "site-ops", Dir: "/x"}})
		m = nm.(Model)
		if m.channelID != "new" || m.isHome() {
			t.Fatalf("switched channel: id=%s home=%v", m.channelID, m.isHome())
		}
	})
	t.Run("a selected agent's prompts come first", func(t *testing.T) {
		m := sidebarNavModel()
		// the dialogs show the selected agent's prompt first, then the oldest
		m.prompts = []protocol.PromptInfo{
			{ID: "pb", Kind: "permission", Agent: "b", Tool: "shell"},
			{ID: "qd", Kind: "question", Agent: "d", Questions: []protocol.Question{{Question: "which?", Options: []protocol.QuestionOption{{Label: "x"}}}}},
			{ID: "pd", Kind: "permission", Agent: "d", Tool: "read"},
		}
		m.selected = 0 // main has none: the oldest of each kind
		if m.currentPrompt().ID != "pb" || m.currentQuestion() != nil {
			t.Fatalf("questions are scoped to the viewed chat: %s %+v", m.currentPrompt().ID, m.currentQuestion())
		}
		m.selected = 3 // asker: its own permission jumps ahead of b's
		if m.currentPrompt().ID != "pd" || m.currentQuestion().ID != "qd" {
			t.Fatalf("own prompt first: %s %s", m.currentPrompt().ID, m.currentQuestion().ID)
		}
		if perms, qs := m.promptCounts(); perms != 2 || qs != 0 {
			t.Fatalf("the strip still counts everything: %d %d", perms, qs)
		}
	})
}

// sidebarNavModel is a channel with the sidebar open: a root waiting on
// its children, one blocked on a permission and one on a question.
// hereRow is the sidebar cursor index of this channel's row.
func hereRow(m Model) int { return m.sidebarIndex(sidebarRow{kind: sbHere}) }

func sidebarNavModel() Model {
	m := channelModel()
	m.showTree = true
	m.width, m.height = 120, 40
	m.channel.Dir = "/home/x/Work/proj"
	m.channel.Created = time.Now().Add(-12 * time.Minute).UTC().Format(time.RFC3339)
	m.agents = []protocol.AgentInfo{
		{ID: "a", Name: "main", Role: "general", State: "waiting", Awaiting: []string{"b", "c"}, CostUSD: 0.20, Tokens: 1200},
		{ID: "b", Parent: "a", Depth: 1, Name: "world-politics", Role: "general", State: "blocked", CostUSD: 0.05, Tokens: 300},
		{ID: "c", Parent: "a", Depth: 1, Name: "business", Role: "general", State: "running"},
		{ID: "d", Parent: "a", Depth: 1, Name: "asker", Role: "general", State: "blocked"},
	}
	m.prompts = []protocol.PromptInfo{
		{ID: "p", Channel: m.channelID, Kind: "permission", Agent: "b", Tool: "shell"},
		{ID: "q", Channel: m.channelID, Kind: "question", Agent: "d", Questions: []protocol.Question{{Question: "which?", Options: []protocol.QuestionOption{{Label: "x"}}}}},
	}
	m.selected = 0
	m.layout()
	return m
}

func TestRoleAwareDialogs(t *testing.T) {
	m := channelModel()
	m.agents = []protocol.AgentInfo{
		{ID: "root", Name: "main", Role: "lead", Model: "openai/gpt-5", Awaiting: []string{"c1"}},
		{ID: "c1", Parent: "root", Name: "scout", Role: "reviewer", Model: "openai/gpt-5", Variant: "high"},
	}
	m.presets = []protocol.PresetInfo{
		{Name: "general", Description: "does it all", Type: "all", Spawn: []string{"general"}},
		{Name: "lead", Description: "runs the show", Type: "primary", Color: "blue"},
		{Name: "reviewer", Description: "reviews", Type: "subagent", Color: "cyan", Models: []protocol.ModelSpec{{ID: "openai/gpt-5", Variants: []string{"medium", "high"}}, {ID: "xai/*"}}},
	}
	names := func(o *overlay) []string {
		var out []string
		for _, it := range o.items {
			out = append(out, it.id)
		}
		return out
	}
	// /roles for the main agent: primary and all roles, not subagent ones
	m.selected = 0
	m.onRoles(rolesMsg{roles: m.presets})
	if got := strings.Join(names(m.ov), ","); got != "general,lead" {
		t.Fatalf("roles for main: %s", got)
	}
	m.closeOverlay()
	// …and for a subagent: subagent and all roles, not primary ones
	m.selected = 1
	m.onRoles(rolesMsg{roles: m.presets})
	if got := strings.Join(names(m.ov), ","); got != "general,reviewer" {
		t.Fatalf("roles for a child: %s", got)
	}
	if !strings.Contains(m.ov.items[1].hint, "subagent") || !strings.Contains(m.ov.items[1].hint, "gpt-5 +1") {
		t.Fatalf("role hint: %q", m.ov.items[1].hint)
	}
	m.closeOverlay()
	// /models under a whitelisted role offers only the allowed models (globs count)
	all := []protocol.ModelInfo{{ID: "openai/gpt-5"}, {ID: "openai/gpt-4"}, {ID: "xai/grok-4"}}
	m.onModels(modelsMsg{models: all})
	if got := strings.Join(names(m.ov), ","); got != "openai/gpt-5,xai/grok-4" || !strings.Contains(m.ov.title, "reviewer") {
		t.Fatalf("models for reviewer: %s (%s)", got, m.ov.title)
	}
	m.closeOverlay()
	// /variants under that role: only the listed ones, and no provider default
	m.onVariants(variantsMsg{model: "openai/gpt-5", current: "high", variants: []string{"low", "medium", "high", "xhigh"}})
	if got := strings.Join(names(m.ov), ","); got != "medium,high" {
		t.Fatalf("variants for reviewer: %s", got)
	}
	m.closeOverlay()
	// a role without a whitelist keeps everything, default included
	m.selected = 0
	m.onModels(modelsMsg{models: all})
	if len(m.ov.items) != 3 {
		t.Fatalf("models for lead: %d", len(m.ov.items))
	}
	m.closeOverlay()
	m.onVariants(variantsMsg{model: "openai/gpt-5", variants: []string{"low", "high"}})
	if got := strings.Join(names(m.ov), ","); got != ",low,high" {
		t.Fatalf("variants for lead: %q", got)
	}
	m.closeOverlay()
	// colours: a tinted role renders, an unknown colour is plain
	if roleStyle("cyan").GetForeground() == roleStyle("").GetForeground() {
		t.Fatal("cyan should tint")
	}
	rows := agentRows(awaitedOf(m.agents, "root"), nil, nil, m.roleTints(), time.Now(), 100)
	if len(rows) != 1 || !strings.Contains(stripANSI(rows[0]), "scout (reviewer)") {
		t.Fatalf("rows %q", rows)
	}
}

func TestMCPTabAndDialog(t *testing.T) {
	m := channelModel()
	if sv := stripANSI(tabsView(m, 120)); !strings.Contains(sv, "todo 0 ─ mcp 0") || !strings.Contains(sv, "! 0 · dirs 0") {
		t.Fatalf("strip:\n%s", sv)
	}
	started := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	m.agents[0].MCP = []protocol.MCPInfo{
		{Name: "github", State: "connected", Tools: []string{"mcp__github__get_issue", "mcp__github__create_issue"}, Started: started},
		{Name: "docs", State: "failed", Error: "spawn npx: not found"},
		{Name: "linear", State: "pending"},
	}
	if sv := stripANSI(tabsView(m, 120)); !strings.Contains(sv, "mcp 1/3") {
		t.Fatalf("strip with servers:\n%s", sv)
	}
	// tab → strip, → x5 lands on mcp, enter opens its dialog
	tab := tea.KeyMsg{Type: tea.KeyTab}
	right := tea.KeyMsg{Type: tea.KeyRight}
	press(&m, tab, right, right, right, right, right, tea.KeyMsg{Type: tea.KeySpace})
	if m.focus != focusMCP {
		t.Fatalf("focus %v", m.focus)
	}
	dv := stripANSI(m.tabDialog(120))
	// the title, then one row per server in order
	if !inOrder(dv, "MCP 1/3", "● github  2 tools · 2h00m", "× docs  spawn npx: not found", "○ linear  starts at the next turn") {
		t.Fatalf("mcp dialog:\n%s", dv)
	}
	// enter on a server lists its tools under it (short names), enter again folds them
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	dv = stripANSI(m.tabDialog(120))
	if !inOrder(dv, "● github", "get_issue", "create_issue", "× docs") || strings.Contains(dv, "mcp__") {
		t.Fatalf("expanded server:\n%s", dv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeySpace}) // a tool row: space does nothing
	if !m.mcpOpen["github"] {
		t.Fatal("enter on a tool row should not toggle anything")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeySpace})
	if m.mcpOpen["github"] || strings.Contains(stripANSI(m.tabDialog(120)), "get_issue") {
		t.Fatalf("enter should fold the server again:\n%s", stripANSI(m.tabDialog(120)))
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusTabs || m.tabSel != 4 {
		t.Fatalf("esc should return to the strip on mcp: focus=%v sel=%d", m.focus, m.tabSel)
	}
	// chat: tool names and server events
	if got := transcript.ToolTitle("mcp__github__create_issue"); got != "github · create_issue" {
		t.Fatalf("title %q", got)
	}
	if g, _ := transcript.ToolGlyph("mcp__github__create_issue"); g != transcript.GlyphToolMCP {
		t.Fatalf("glyph %q", g)
	}
	tr := m.transcript("a")
	tr.Apply(event.Event{Agent: "a", Type: event.MCPStarted, Payload: event.MustPayload(event.MCPStartedPayload{Server: "github", Tools: []string{"x", "y"}})})
	tr.Apply(event.Event{Agent: "a", Type: event.MCPFailed, Payload: event.MustPayload(event.MCPFailedPayload{Server: "docs", Error: "boom"})})
	tr.Apply(event.Event{Agent: "a", Type: event.MCPStopped, Payload: event.MustPayload(event.MCPRefPayload{Server: "github"})})
	m.refreshViewport()
	v := stripANSI(m.vp.View())
	for _, want := range []string{"≡ MCP github connected · 2 tools", "≡ MCP docs failed: boom", "≡ MCP github stopped"} {
		if !strings.Contains(v, want) {
			t.Fatalf("chat lacks %q:\n%s", want, v)
		}
	}
}

func TestDirsTabAndBoundaryPrompt(t *testing.T) {
	m := channelModel()
	m.channel.Dirs = []protocol.DirInfo{{Path: "/repo", Source: "channel"}, {Path: "/srv/shared", Source: "human"}, {Path: "/tmp/build", Source: "human"}}
	if sv := stripANSI(tabsView(m, 120)); !strings.Contains(sv, "dirs 3") {
		t.Fatalf("strip:\n%s", sv)
	}
	tab := tea.KeyMsg{Type: tea.KeyTab}
	right := tea.KeyMsg{Type: tea.KeyRight}
	press(&m, tab, right, tea.KeyMsg{Type: tea.KeySpace})
	if m.focus != focusDirs {
		t.Fatalf("focus %v", m.focus)
	}
	dv := stripANSI(m.tabDialog(120))
	lines := strings.Split(dv, "\n")
	if !strings.Contains(dv, "a add directory · space/enter edit · ctrl+d remove") || strings.Contains(dv, "esc close") {
		t.Fatalf("dirs dialog should carry its own hints (without esc):\n%s", dv)
	}
	if repo := findLine(lines, "▸ /repo  default"); repo < 0 || strings.Contains(lines[repo], "◆") || !inOrder(dv, "Dirs 3", "▸ /repo  default", "/srv/shared  human", "/tmp/build  human") {
		t.Fatalf("dirs dialog:\n%s", dv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown})
	if m.agCursor != 1 {
		t.Fatalf("cursor %d", m.agCursor)
	}
	// editing: enter on a row opens the path field prefilled, esc cancels it
	// without closing the dialog; a opens it empty; ctrl+d removes; the
	// channel row refuses both
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if m.dirEdit != "/srv/shared" || m.dirInput.Value() != "/srv/shared" || !m.dirInput.Focused() {
		t.Fatalf("edit: %q %q", m.dirEdit, m.dirInput.Value())
	}
	if dv := stripANSI(m.tabDialog(120)); !strings.Contains(dv, "replace /srv/shared") || !strings.Contains(dv, "› /srv/shared") {
		t.Fatalf("edit field:\n%s", dv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.dirEdit != "" || m.focus != focusDirs {
		t.Fatalf("esc should cancel the edit only: %q %v", m.dirEdit, m.focus)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m.dirEdit != "add" || m.dirInput.Value() != "" {
		t.Fatalf("add: %q %q", m.dirEdit, m.dirInput.Value())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("~/x")})
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.dirEdit != "" {
		t.Fatalf("enter should submit the add: cmd=%v edit=%q", cmd != nil, m.dirEdit)
	}
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyCtrlD}); cmd == nil {
		t.Fatal("ctrl+d on an added row should send the removal")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if m.dirEdit != "default" || m.dirInput.Value() != "/repo" {
		t.Fatalf("the default directory should be editable: %q %q", m.dirEdit, m.dirInput.Value())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusTabs || m.tabSel != 1 {
		t.Fatalf("esc: focus=%v sel=%d", m.focus, m.tabSel)
	}
	// a boundary prompt says so and offers the directory
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Tool: "read", Agent: "a", Input: []byte(`{"path":"/etc/hosts"}`), Dir: "/etc"}}
	m.setFocus(focusPermission)
	body := stripANSI(strings.Join(m.tabBodyLines(80), "\n"))
	for _, w := range []string{"▤ /etc/hosts  coder", "outside the channel's directories · /etc", "▸ ● Allow once", "  ○ Allow and add /etc  every agent in the channel can use it", "  ○ Allow and add another directory…  type the path", "  ○ Deny"} {
		if !strings.Contains(body, w) {
			t.Fatalf("boundary prompt body missing %q:\n%s", w, body)
		}
	}
	// "another directory" opens the path row prefilled with the offered
	// one; esc cancels the row only; enter answers with what was typed
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeySpace})
	if m.permEdit != "dir" || m.dirInput.Value() != "/etc" || !m.dirInput.Focused() {
		t.Fatalf("edit: %q %q", m.permEdit, m.dirInput.Value())
	}
	if body := stripANSI(strings.Join(m.tabBodyLines(80), "\n")); !strings.Contains(body, "directory to add") || !strings.Contains(body, "› /etc") || strings.Contains(body, "▸") {
		t.Fatalf("edit field:\n%s", body)
	}
	if hs := m.keyHints(); hs[0].Key != "enter" || hs[0].Desc != "allow + add this directory" {
		t.Fatalf("hints %+v", hs)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.permEdit != "" || m.focus != focusPermission || m.permSel != 2 {
		t.Fatalf("esc should cancel the edit only: %q %v %d", m.permEdit, m.focus, m.permSel)
	}
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	press(&m, tea.KeyMsg{Type: tea.KeyBackspace}, tea.KeyMsg{Type: tea.KeyBackspace}, tea.KeyMsg{Type: tea.KeyBackspace})
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.permEdit != "" || m.promptBusy != "p" || !m.claimedByUs["p"] {
		t.Fatalf("enter should answer with the edited directory: cmd=%v busy=%q", cmd != nil, m.promptBusy)
	}
	// "Allow and add" answers straight away
	m.promptBusy = ""
	m.prompts = []protocol.PromptInfo{{ID: "p2", Kind: "permission", Tool: "read", Agent: "a", Input: []byte(`{"path":"/etc/hosts"}`), Dir: "/etc"}}
	press(&m, tea.KeyMsg{Type: tea.KeyDown})
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeySpace}); cmd == nil || m.promptBusy != "p2" {
		t.Fatalf("Allow and add: cmd=%v busy=%q", cmd != nil, m.promptBusy)
	}
	m.promptBusy = ""
	// the chat notes an added directory
	tr := m.transcript("a")
	tr.Apply(event.Event{Agent: "a", Type: event.ChannelDirAdded, Payload: event.MustPayload(event.DirPayload{Dir: "/etc", Source: "human"})})
	m.setFocus(focusInput)
	m.refreshViewport()
	if v := stripANSI(m.vp.View()); !strings.Contains(v, "◆ Dirs + /etc (human)") {
		t.Fatalf("chat:\n%s", v)
	}
}

func TestDialogHintsWrap(t *testing.T) {
	hints := []dialog.Hint{hint("↑/↓", "option"), hint("space", "choose"), hint("a", "add directory"), hint("ctrl+d", "remove"), hint("esc", "close"), hint("tab", "next section"), hint("ctrl+c", "quit")}
	lines := dialog.HintLines(hints, 30)
	joined := stripANSI(strings.Join(lines, "\n"))
	for _, want := range []string{"↑/↓ option", "space choose", "a add directory", "ctrl+d remove"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("hint %q lost:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "esc") || strings.Contains(joined, "tab") || strings.Contains(joined, "…") {
		t.Fatalf("esc/tab/ctrl+c are not the dialog's own, and nothing should be cut:\n%s", joined)
	}
	for _, l := range lines {
		if ansi.StringWidth(l) > 30 {
			t.Fatalf("line wider than the dialog: %q", stripANSI(l))
		}
	}
	if len(lines) < 2 {
		t.Fatalf("hints should wrap at 30 columns: %d line(s)", len(lines))
	}
	if got := dialog.HintLines(hints, 200); len(got) != 1 {
		t.Fatalf("wide dialog: one line, got %d", len(got))
	}
}

func TestEnterSelectsAndCtrlSpaceReturnsToInput(t *testing.T) {
	m := channelModel()
	m.agents[0].Todos = []event.TodoItem{{ID: "t1", Text: "x", Status: "pending"}}
	tab := tea.KeyMsg{Type: tea.KeyTab}
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	space := tea.KeyMsg{Type: tea.KeySpace}
	ctrlSpace := tea.KeyMsg{Type: tea.KeyCtrlAt} // ctrl+space reaches a program as ctrl+@
	// strip: enter opens the highlighted tab, exactly as space does
	press(&m, tab)
	if m.focus != focusTabs {
		t.Fatalf("focus %v", m.focus)
	}
	press(&m, enter)
	if m.focus != focusPermission {
		t.Fatalf("enter on the strip should open the tab: %v", m.focus)
	}
	// a tab dialog: ctrl+space returns to the input (not the strip)
	press(&m, ctrlSpace)
	if m.focus != focusInput || !m.input.Focused() {
		t.Fatalf("ctrl+space in a dialog should return to the input: %v", m.focus)
	}
	press(&m, tab, space)
	if m.focus != focusPermission {
		t.Fatalf("space on the strip should open the tab: %v", m.focus)
	}
	press(&m, ctrlSpace)
	// chat: enter acts on the item and stays, ctrl+space goes back to typing
	press(&m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.focus != focusChat {
		t.Fatalf("focus %v", m.focus)
	}
	press(&m, enter)
	if m.focus != focusChat {
		t.Fatalf("enter in the chat should stay in the chat: %v", m.focus)
	}
	press(&m, ctrlSpace)
	if m.focus != focusInput {
		t.Fatalf("ctrl+space in the chat should return to the input: %v", m.focus)
	}
	// meta row: enter opens the part's dialog, as space does
	press(&m, tab, tab) // strip → meta row
	if m.focus != focusMeta {
		t.Fatalf("focus %v", m.focus)
	}
	if cmd := press(&m, enter); cmd == nil {
		t.Fatal("enter on the meta row should open the part's dialog")
	}
	press(&m, ctrlSpace)
	if m.focus != focusInput {
		t.Fatalf("ctrl+space on the meta row should return to the input: %v", m.focus)
	}
	// an overlay: enter picks the row, ctrl+space closes it with nothing picked
	m.setFocus(focusMeta)
	m.openOverlay(newOverlay(ovRoles, overlayList, "Roles"))
	m.ov.setItems([]overlayItem{{id: "general", label: "general"}})
	if cmd := press(&m, enter); cmd == nil || m.ov != nil {
		t.Fatalf("enter in an overlay should pick the row: cmd=%v ov=%v", cmd != nil, m.ov != nil)
	}
	m.openOverlay(newOverlay(ovRoles, overlayList, "Roles"))
	m.ov.setItems([]overlayItem{{id: "general", label: "general"}})
	press(&m, ctrlSpace)
	if m.ov != nil || m.focus != focusInput || !m.input.Focused() {
		t.Fatalf("ctrl+space in an overlay should close it onto the input: ov=%v focus=%v", m.ov != nil, m.focus)
	}
	// text fields keep enter: a question's typed answer
	m.prompts = []protocol.PromptInfo{{ID: "q", Kind: "question", Agent: "a", Questions: []protocol.Question{{Question: "which?", Options: []protocol.QuestionOption{{Label: "x"}}}}}}
	m.setFocus(focusQuestions)
	typedSpace := tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}} // as a terminal sends it
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}, typedSpace, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if m.promptInput.Value() != "a b" {
		t.Fatalf("space should type into the answer field: %q", m.promptInput.Value())
	}
	press(&m, enter)
	if m.q.typing || m.q.custom != "a b" || m.promptBusy != "" {
		t.Fatal("enter should stage custom text for Submit")
	}
	// and ctrl+space gets out of that text field
	m.setFocus(focusQuestions)
	m.q.typing = true
	press(&m, ctrlSpace)
	if m.focus != focusInput {
		t.Fatalf("ctrl+space should leave a dialog's text field: %v", m.focus)
	}
}

func TestArrowsMoveWithinTheDraftBeforeHistory(t *testing.T) {
	m := channelModel()
	m.pushHistory("older message")
	up := tea.KeyMsg{Type: tea.KeyUp}
	down := tea.KeyMsg{Type: tea.KeyDown}
	// three logical lines, cursor on the last: two ↑ stay inside, the third
	// walks history
	m.input.SetValue("one\ntwo\nthree")
	m.input.CursorEnd()
	press(&m, up)
	if m.input.Value() != "one\ntwo\nthree" || m.input.Line() != 1 {
		t.Fatalf("first ↑ should move up a line: line=%d value=%q", m.input.Line(), m.input.Value())
	}
	press(&m, up)
	if m.input.Line() != 0 {
		t.Fatalf("second ↑ should reach the top line: %d", m.input.Line())
	}
	press(&m, up)
	if m.input.Value() != "older message" {
		t.Fatalf("↑ on the top line should walk history: %q", m.input.Value())
	}
	press(&m, down)
	if m.input.Value() != "one\ntwo\nthree" {
		t.Fatalf("↓ past the end of history should restore the draft: %q", m.input.Value())
	}
	// a single long line wrapped over several rows behaves the same way
	m.input.SetValue(strings.Repeat("word ", 60)) // wraps well past one row in a 120-column box
	m.input.CursorEnd()
	m.layout()
	if m.input.LineInfo().RowOffset == 0 {
		t.Fatal("setup: the cursor should sit on a wrapped row below the first")
	}
	press(&m, up)
	if m.input.Value() == "older message" {
		t.Fatal("↑ on a wrapped row should move up within the line, not walk history")
	}
	for i := 0; i < 5 && m.input.LineInfo().RowOffset > 0; i++ {
		press(&m, up)
	}
	press(&m, up)
	if m.input.Value() != "older message" {
		t.Fatalf("↑ on the first wrapped row should walk history: %q", m.input.Value())
	}
}

func TestDenyTakesAnOptionalReason(t *testing.T) {
	m := channelModel()
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Tool: "shell", Agent: "a", Input: []byte(`{"command":"rm x"}`)}}
	m.setFocus(focusPermission)
	up, space := tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeySpace}
	press(&m, up, space) // Deny is the last row
	if m.permEdit != "deny" || m.promptBusy != "" || !m.dirInput.Focused() {
		t.Fatalf("Deny should open the reason row, not deny yet: edit=%q busy=%q", m.permEdit, m.promptBusy)
	}
	if body := stripANSI(strings.Join(m.tabBodyLines(80), "\n")); !strings.Contains(body, "deny · a reason") || !strings.Contains(body, "  ● Deny") {
		t.Fatalf("body:\n%s", body)
	}
	if hs := m.keyHints(); hs[0].Key != "enter" || hs[0].Desc != "deny" {
		t.Fatalf("hints %+v", hs)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.permEdit != "" || m.focus != focusPermission {
		t.Fatalf("esc should cancel the row only: %q %v", m.permEdit, m.focus)
	}
	// with a reason
	press(&m, space)
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("use")}, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("git")})
	if m.dirInput.Value() != "use git" {
		t.Fatalf("typed reason %q", m.dirInput.Value())
	}
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.permEdit != "" || m.promptBusy != "p" || !m.claimedByUs["p"] {
		t.Fatalf("enter should deny: cmd=%v busy=%q", cmd != nil, m.promptBusy)
	}
	// and without one
	m.promptBusy = ""
	m.prompts = []protocol.PromptInfo{{ID: "q", Kind: "permission", Tool: "shell", Agent: "a"}}
	press(&m, up, space)
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.promptBusy != "q" {
		t.Fatalf("enter on an empty reason should still deny: cmd=%v busy=%q", cmd != nil, m.promptBusy)
	}
}

func TestQuestionsInlineLegacyBatch(t *testing.T) {
	m := channelModel()
	m.agents[0].Role = "general"
	if sv := stripANSI(tabsView(m, 120)); !strings.Contains(sv, "! 0 · dirs 0\nasync") {
		t.Fatalf("strip:\n%s", sv)
	}
	batch := protocol.PromptInfo{ID: "q1", Kind: "question", Agent: "a", From: "coder", Role: "general", Tool: "ask_user", Questions: []protocol.Question{
		{Question: "Which backend?", Options: []protocol.QuestionOption{{Label: "Postgres", Description: "what the repo uses"}, {Label: "SQLite"}}},
		{Question: "Which extras?", Options: []protocol.QuestionOption{{Label: "Cache"}, {Label: "Queue"}, {Label: "Search"}}},
		{Question: "What should the service be called?", Options: []protocol.QuestionOption{{Label: "stavlos-api"}}},
	}}
	// a new question opens its dialog when the input is idle
	m.applyPromptNotification(protocol.PromptNotification{Action: "requested", Prompt: batch})
	if m.focus != focusInput || m.currentPrompt() != nil {
		t.Fatalf("a question should arrive without taking focus: focus=%v", m.focus)
	}
	if sv := stripANSI(tabsView(m, 120)); strings.Contains(sv, "? 1/3") || !strings.Contains(sv, "! 0") {
		t.Fatalf("strip with a question:\n%s", sv)
	}
	m.openQuestion(m.currentQuestion())
	cardView := func() string { rows, _, _ := m.questionCard(&batch); return stripANSI(strings.Join(rows, "\n")) }
	dv := cardView()
	for _, want := range []string{"? @user Which backend?", "  ▸ □ Postgres  what the repo uses", "    □ SQLite", "    □ Reply with a custom answer…", "[Submit answer]"} {
		if !strings.Contains(dv, want) {
			t.Fatalf("dialog lacks %q:\n%s", want, dv)
		}
	}
	// The asking agent belongs to the header, leaving the question text alone.
	lines := strings.Split(dv, "\n")
	if lines[0] != "? @user Which backend?" || strings.Contains(lines[1], "Which backend?") || strings.Contains(dv, " asks") {
		t.Fatalf("question/agent placement:\n%s", dv)
	}
	// a checklist: enter with nothing picked does nothing; ↓ space toggles
	// SQLite; enter confirms and moves on
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.q.idx != 0 {
		t.Fatalf("enter with nothing picked should stay: idx=%d", m.q.idx)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeySpace}, tea.KeyMsg{Type: tea.KeyEnter})
	if m.q.idx != 1 || m.q.answers[0] != "SQLite" {
		t.Fatalf("after the first question: idx=%d answers=%v", m.q.idx, m.q.answers)
	}
	// several options plus something typed, joined in order
	press(&m, tea.KeyMsg{Type: tea.KeySpace}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeySpace})
	if dv := cardView(); !strings.Contains(dv, "■ Cache") || !strings.Contains(dv, "□ Queue") || !strings.Contains(dv, "■ Search") {
		t.Fatalf("marks:\n%s", dv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown})  // onto "something else"
	press(&m, tea.KeyMsg{Type: tea.KeySpace}) // opens the field
	if !m.q.typing {
		t.Fatal("space on the last row should open the text field")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("metrics")})
	press(&m, tea.KeyMsg{Type: tea.KeyEsc}) // leave the field, keep the text
	if m.q.typing || m.q.custom != "metrics" {
		t.Fatalf("esc should keep the typed answer: typing=%v custom=%q", m.q.typing, m.q.custom)
	}
	if dv := cardView(); !strings.Contains(dv, "■ metrics") || strings.Contains(dv, "Custom answer:") {
		t.Fatalf("the typed answer should show as picked:\n%s", dv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.q.idx != 2 || m.q.answers[1] != "Cache, Search, metrics" {
		t.Fatalf("after the second question: idx=%d answers=%v", m.q.idx, m.q.answers)
	}
	// ← goes back to review, → returns
	press(&m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.q.idx != 1 {
		t.Fatalf("← should go back: %d", m.q.idx)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRight})
	// typing anywhere starts "something else"; enter in the field confirms
	// and, on the last question, submits the batch
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("stavlos")})
	if !m.q.typing || m.promptInput.Value() != "stavlos" {
		t.Fatalf("typing: %v %q", m.q.typing, m.promptInput.Value())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEnter}) // stage the custom answer
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.promptBusy != "q1" || !m.claimedByUs["q1"] {
		t.Fatalf("the last answer should submit the batch: cmd=%v busy=%q", cmd != nil, m.promptBusy)
	}
	if strings.Join(m.q.answers, "|") != "SQLite|Cache, Search, metrics|stavlos" {
		t.Fatalf("answers %v", m.q.answers)
	}
	// the answered batch leaves; the dialog closes onto where it came from
	m.applyPromptNotification(protocol.PromptNotification{Action: "answered", Prompt: batch})
	if m.focus != focusInput || m.currentQuestion() != nil {
		t.Fatalf("after the answer: focus=%v", m.focus)
	}
	// chat: the tool call line names the headers
	if got := transcript.ToolArg("ask_user", []byte(`{"questions":[{"question":"Which backend?"},{"question":"Call it?"}]}`)); got != "Which backend? · Call it?" {
		t.Fatalf("ask_user arg %q", got)
	}
}

// TestControlsNeverReachTheTerminal: escape sequences in a model's text,
// an agent's label or a channel title are removed on the way in, and a
// permission subject shows them as carets so nothing can hide.
func TestControlsNeverReachTheTerminal(t *testing.T) {
	m := channelModel()
	m.agents[0].Role = "general"
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Tool: "shell", Agent: "a", Input: []byte(`{"command":"rm -rf ~ \u001b[2K\u001b[1Gls -la"}`)}}
	m.setFocus(focusPermission)
	body := stripANSI(strings.Join(m.tabBodyLines(80), "\n"))
	if !strings.Contains(body, "$ rm -rf ~ ^[^[ls -la  coder (general)") {
		t.Fatalf("hidden bytes should show as carets:\n%s", body)
	}
	tr := m.transcript("a")
	feed(tr.Apply, userMsg(1, "a", "hi \x1b]0;evil\x07there"))
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Text: "str\x1b[31meam"})
	for _, l := range tr.All() {
		if strings.ContainsRune(l.Text, 0x1b) || strings.ContainsRune(l.Text, 0x07) {
			t.Fatalf("control reached the transcript: %q", l.Text)
		}
	}
	m.setAgents([]protocol.AgentInfo{{ID: "a", Name: "ma\x1b[2Kin", Role: "general"}})
	if m.agents[0].Name != "main" {
		t.Fatalf("label %q", m.agents[0].Name)
	}
	m.upsertPrompt(protocol.PromptInfo{ID: "q", Kind: "question", Question: "pick\x9b2K one"})
	if p := m.prompts[m.findPrompt("q")]; p.Question != "pick one" {
		t.Fatalf("question %q", p.Question)
	}
}

// TestTabRowsMoveVertically: ↑/↓ on the strip move the highlight between the
// channel's row and the agent's, keeping the column where the row allows.
func TestTabRowsMoveVertically(t *testing.T) {
	m := channelModel() // the sidebar hidden: ! ? dirs over the agent's row
	for _, c := range []struct {
		sel  int
		down bool
		want int
	}{
		{0, true, 2},  // ! → async
		{1, true, 3},  // dirs → todo
		{2, false, 0}, // async → !
		{4, false, 1}, // mcp → dirs
		{0, false, 0}, // top row stays
		{4, true, 4},  // bottom row stays
	} {
		if got := m.otherRowTab(c.sel, c.down); got != c.want {
			t.Errorf("otherRowTab(%d, %v) = %d, want %d", c.sel, c.down, got, c.want)
		}
	}
}

// TestPromptsAcrossChannels: every channel's prompts are kept, through a
// switch too; the tabs count and show them all, while opening a channel or an
// agent that waits on the human opens its dialog on its own prompts only.
func TestPromptsAcrossChannels(t *testing.T) {
	m := sidebarNavModel()
	here := m.channelID
	which := []protocol.Question{{Question: "which?", Options: []protocol.QuestionOption{{Label: "x"}}}}
	m.prompts = []protocol.PromptInfo{
		{ID: "p-here", Channel: here, Kind: "permission", Agent: "b", Tool: "shell"},
		{ID: "p-away", Channel: "elsewhere", ChannelName: "docs", Agent: "x9", From: "writer", Kind: "permission", Tool: "shell"},
		{ID: "q-here", Channel: here, Kind: "question", Agent: "d", Questions: which},
	}
	m.applyPromptNotification(protocol.PromptNotification{Action: protocol.ActionRequested, Prompt: protocol.PromptInfo{ID: "q-away", Channel: "elsewhere", Kind: "question", Agent: "x9", Questions: which}})
	if perms, questions := m.promptCounts(); len(m.prompts) != 4 || perms != 2 || questions != 1 || m.focus != focusInput {
		t.Fatalf("another channel's prompt is kept and does not pop a dialog: %d prompts, %d %d, focus %v", len(m.prompts), perms, questions, m.focus)
	}
	if who := m.promptWho(&m.prompts[1]); who != "@writer · #docs" {
		t.Fatalf("another channel's prompt names its agent and channel: %q", who)
	}
	// opening @world-politics (b) from the sidebar opens the permission dialog on its own prompt
	m.setFocus(focusSidebar)
	m.sidebarSelect(hereRow(m) + 2)
	if m.focus != focusInlinePermission || m.currentPrompt().ID != "p-here" || m.permissionVisible(m.prompts[1]) {
		t.Fatalf("agent open: focus=%v prompt=%s", m.focus, m.currentPrompt().ID)
	}
	if strings.Contains(m.View(), "╭") {
		t.Fatal("permission navigation should focus the inline card")
	}
	// closing drops the scope; the tab then opens on every channel's
	m.closeDialog()
	if m.scope != (promptScope{}) {
		t.Fatalf("scope after close: %+v", m.scope)
	}
	m.openTab(focusPermission)
	if perms, _ := m.promptCountsIn(m.scope); perms != 2 {
		t.Fatalf("a tab shows every channel's: %d", perms)
	}
	m.closeDialog()
	// @asker (d) waits on a question only: its open goes to the questions dialog
	m.setFocus(focusSidebar)
	m.sidebarSelect(hereRow(m) + 4)
	if m.focus != focusQuestions || m.currentQuestion().ID != "q-here" {
		t.Fatalf("agent with a question: focus=%v", m.focus)
	}
	m.closeDialog()
	// opening this channel goes to its permission, not the other channel's
	m.setFocus(focusSidebar)
	m.sidebarSelect(hereRow(m))
	if m.focus != focusInlinePermission || m.currentPrompt().ID != "p-here" {
		t.Fatalf("channel open: focus=%v", m.focus)
	}
	// a switch keeps every channel's prompts
	m.bindChannel(protocol.ChannelInfo{ID: "elsewhere", Dir: "/x"})
	if len(m.prompts) != 4 {
		t.Fatalf("prompts after a switch: %d", len(m.prompts))
	}
}

// TestChannelChatFooterIsTheChannels: the channel chat's footer carries only
// what is the channel's: the mode tag, the channel's tokens and cost, and no
// async · todo · mcp tabs; an agent's chat has them all back. The view
// fills the window either way.
func TestChannelChatFooterIsTheChannels(t *testing.T) {
	m := channelModel()
	m.reconciled = false // connected: the right side shows usage, not the sign-in nudge
	m.width, m.height = 100, 30
	m.agents[0].Tokens, m.agents[0].CostUSD = 1500, 0.02
	m.agents[0].Context, m.agents[0].ContextWindow = 62_000, 200_000
	m.transcript(m.agents[0].ID).Notice("hello") // not the home screen
	for _, tree := range []bool{false, true} {
		m.showTree = tree
		m.openChat()
		m.layout()
		left, spans := m.metaLeft()
		if !m.superChat || left != "" || len(spans) != 0 || len(m.metaParts()) != 0 {
			t.Fatalf("channel chat meta: %q %v %v", stripANSI(left), spans, m.metaParts())
		}
		if right := stripANSI(m.footerRightView()); right != "" { // no one agent's context; tokens and cost are the nav's
			t.Fatalf("channel chat right side: %q", right)
		}
		if sv := stripANSI(m.sectionsView(100)); strings.Contains(sv, "async") || slices.Contains(m.tabOrder(), focusAsync) || (tree == (sv != "")) {
			t.Fatalf("channel chat strip (tree %v): %q %v", tree, sv, m.tabOrder())
		}
		if got := strings.Count(m.View(), "\n") + 1; got != m.height {
			t.Fatalf("channel chat view is %d lines, want %d (tree %v)", got, m.height, tree)
		}
		m.openAgent(0)
		if sv := stripANSI(m.ruleLine(100)); !strings.Contains(sv, "async") || len(m.metaParts()) != 3 || !strings.Contains(stripANSI(m.footerRightView()), "31%") {
			t.Fatalf("agent chat footer: %q %v %q", sv, m.metaParts(), stripANSI(m.footerRightView()))
		}
		if got := strings.Count(m.View(), "\n") + 1; got != m.height {
			t.Fatalf("agent chat view is %d lines, want %d (tree %v)", got, m.height, tree)
		}
	}
}

// TestModeTagLeadsTheInput: the channel's mode tag sits before the input's ›,
// right-aligned in its own columns whatever the mode, and not on the meta
// row; a click on it acts like the old meta row tag (from yolo: back to ask).
func TestModeTagLeadsTheInput(t *testing.T) {
	m := channelModel()
	m.width, m.height = 100, 30
	m.transcript(m.agents[0].ID).Notice("hello")
	m.layout()
	for mode, lead := range map[string]string{protocol.ModeAsk: " ASK › ", protocol.ModeAuto: "AUTO › ", protocol.ModeYolo: "YOLO › "} {
		m.channel.Mode = mode
		if in := stripANSI(m.inputView()); !strings.HasPrefix(in, lead) {
			t.Fatalf("%s: input %q", mode, in)
		}
		if left, _ := m.metaLeft(); strings.Contains(stripANSI(left), strings.TrimSpace(lead[:4])) {
			t.Fatalf("%s: the meta row still carries the tag: %q", mode, stripANSI(left))
		}
	}
	lay := m.rows()
	nm, _ := m.Update(tea.MouseMsg{X: 1, Y: lay.input, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	nm, cmd := nm.(Model).Update(tea.MouseMsg{X: 1, Y: lay.input, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	if cmd == nil || nm.(Model).focus != focusInput {
		t.Fatalf("a click on the YOLO tag should set the mode back to ask: cmd=%v focus=%v", cmd != nil, nm.(Model).focus)
	}
}

// TestStatusSitsOverTheDivider: the transient status sits at the right end of
// the chat's last row, not on the divider, which keeps the agent at its left
// and the usage at its right; a long status is cut to the row.
func TestStatusSitsOverTheDivider(t *testing.T) {
	m := channelModel()
	m.reconciled = false // connected: the usage shows
	m.width, m.height = 100, 30
	m.agents[0].Tokens, m.agents[0].CostUSD = 1500, 0.02
	m.transcript(m.agents[0].ID).Notice("hello")
	m.openChat()
	m.layout()
	m.setStatus("copied 3 lines", false)
	lines := strings.Split(stripANSI(m.View()), "\n")
	rule := m.rows().rule
	if !strings.HasPrefix(lines[rule], "───") || strings.Contains(lines[rule], "copied") || strings.Contains(lines[rule], "tokens") || ansi.StringWidth(lines[rule]) != m.width {
		t.Fatalf("divider: %q", lines[rule])
	}
	if above := lines[rule-1]; !strings.HasSuffix(above, "copied 3 lines") || ansi.StringWidth(above) != m.width || len(lines) != m.height {
		t.Fatalf("the status sits at the right of the row above the divider:\n%s", strings.Join(lines, "\n"))
	}
	if r := m.rows(); r.input != rule+2 { // a blank line between the divider and the input
		t.Fatalf("rows: input at %d, divider at %d", r.input, rule)
	}
	m.setStatus(strings.Repeat("a long status ", 10), true)
	lines = strings.Split(stripANSI(m.View()), "\n")
	if above := lines[rule-1]; !strings.Contains(above, "…") || ansi.StringWidth(above) != m.width {
		t.Fatalf("a long status is cut to the row: %q", above)
	}
}

// tabsView is every tab label as laid out, a line per row, wherever it is
// drawn: the footer strip, the sidebar, the meta row's right end.
func tabsView(m Model, _ int) string {
	labels, _ := m.tabLabels(m.currentPrompt())
	return labels
}

// metaLine is the meta row's text without its click spans.
func metaLine(label, role, model, variant string, queued int, modeTag string, sel, hover metaPart) string {
	line, _ := metaLineSpans(label, role, model, variant, queued, modeTag, sel, hover)
	return line
}

// tabBodyLines is the focused tab's body, laid out for width columns.
func (m Model) tabBodyLines(width int) []string {
	lines, _ := m.tabBodyRows(width)
	return lines
}

// TestOtherChannelTreesStayOpen: the channel you leave keeps its tree in
// the sidebar, so opening another folds nothing; ← folds one by hand.
func TestOtherChannelTreesStayOpen(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	old := m.channelID
	m.stash() // leaving a channel keeps its agents
	if len(m.trees[old]) != 4 || m.treeClosed[old] {
		t.Fatalf("leaving a channel should keep its tree: %d rows closed=%v", len(m.trees[old]), m.treeClosed[old])
	}
	// bound to another channel of the directory now, the old one beside it
	dir := m.channel.Dir
	m.channelState = newChannelState("s2", protocol.ChannelInfo{ID: "s2", Name: "other", Dir: dir})
	m.reconciled = true
	m.agents = []protocol.AgentInfo{{ID: "z", Name: "solo", Role: "general", State: "idle"}}
	m.navChannels = []protocol.ChannelInfo{{ID: old, Name: "proj", Dir: dir}}
	m.layout()
	kept := func() int {
		n := 0
		for _, r := range m.sidebarRows() {
			if r.kind == sbOtherAgent {
				n++
			}
		}
		return n
	}
	if kept() != 4 {
		t.Fatalf("the channel left behind should keep its tree open: %d rows", kept())
	}
	body, items := m.sidebarBody(sidebarWidth - 1)
	if len(body) != len(items) {
		t.Fatalf("every drawn row needs its cursor index: %d rows, %d indices", len(body), len(items))
	}
	if drawn := stripANSI(strings.Join(body, "\n")); !strings.Contains(drawn, "@world-politics") || !strings.Contains(drawn, "@solo") {
		t.Fatalf("both trees should be drawn:\n%s", drawn)
	}
	// ← folds that channel's tree, and unfolds it again
	m.setFocus(focusSidebar)
	m.sbCursor = m.sidebarIndex(sidebarRow{kind: sbOther, k: 0})
	press(&m, tea.KeyMsg{Type: tea.KeyLeft})
	if kept() != 0 || !m.treeClosed[old] {
		t.Fatalf("← should fold the tree: %d rows closed=%v", kept(), m.treeClosed[old])
	}
	press(&m, tea.KeyMsg{Type: tea.KeyLeft})
	if kept() != 4 {
		t.Fatalf("← again should unfold it: %d rows", kept())
	}
}

// TestDividerTabsOpenWithTheSidebar: with the sidebar showing, the divider
// still spans the whole window, so a click on each agent tab there opens
// that tab's own dialog.
func TestDividerTabsOpenWithTheSidebar(t *testing.T) {
	for _, c := range []struct {
		label string
		want  focus
	}{{"async", focusAsync}, {"todo", focusTodo}, {"mcp", focusMCP}} {
		m := sidebarNavModel()
		m.prompts = nil
		m.superChat = false
		m.layout()
		if !m.sidebarVisible() {
			t.Fatal("the sidebar should show")
		}
		lay := m.rows()
		row := stripANSI(m.ruleLine(m.width))
		i := strings.Index(row, c.label+" ")
		if i < 0 {
			t.Fatalf("no %s tab on the divider: %q", c.label, row)
		}
		x := ansi.StringWidth(row[:i]) + 1
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: lay.rule, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		nm, _ = nm.(Model).Update(tea.MouseMsg{X: x, Y: lay.rule, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
		if got := nm.(Model).focus; got != c.want {
			t.Errorf("clicking %s on the divider opened %v, want %v", c.label, got, c.want)
		}
	}
}

// TestSidebarHidesQuietAgents: a channel's tree draws its main agent, and
// otherwise only agents that are busy, waiting, failed or waiting on the
// human (and the selected one);
// the row under it shows the idle ones too, and hides them again. A click
// on the channel whose chat is already open folds its tree.
func TestSidebarHidesQuietAgents(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.agents = []protocol.AgentInfo{
		{ID: "a", Name: "main", Role: "general", State: "idle"}, // the root: drawn even idle
		{ID: "b", Parent: "a", Depth: 1, Name: "napper", Role: "general", State: "idle"},
		{ID: "c", Parent: "a", Depth: 1, Name: "sleeper", Role: "general", State: "idle"},
		{ID: "d", Parent: "a", Depth: 1, Name: "waiter", Role: "general", State: "waiting"},
	}
	m.superChat = true
	m.layout()
	nav := func() string {
		body, _ := m.sidebarBody(sidebarWidth - 1)
		return stripANSI(strings.Join(body, "\n"))
	}
	if v := nav(); !strings.Contains(v, "@main") || !strings.Contains(v, "@waiter") || strings.Contains(v, "@napper") || strings.Contains(v, "@sleeper") || !strings.Contains(v, "▸ show all · 2 idle") {
		t.Fatalf("quiet agents should hide behind show all:\n%s", v)
	}
	m.setFocus(focusSidebar)
	m.sbCursor = m.sidebarIndex(sidebarRow{kind: sbHereAll})
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if v := nav(); !strings.Contains(v, "@napper") || !strings.Contains(v, "@sleeper") || !strings.Contains(v, "▾ hide idle") {
		t.Fatalf("show all should draw every agent:\n%s", v)
	}
	if r, _ := m.sidebarAt(m.sbCursor); r.kind != sbHereAll {
		t.Fatalf("the cursor should stay on the toggle row: %+v", r)
	}
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if v := nav(); strings.Contains(v, "@napper") {
		t.Fatalf("hide idle should hide them again:\n%s", v)
	}
	// the channel's chat is open: a click on its row folds the tree, another unfolds it
	click := func() {
		y := sidebarY(m, hereRow(m))
		nm, _ := m.Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		nm, _ = nm.(Model).Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
		m = nm.(Model)
	}
	click()
	if v := nav(); strings.Contains(v, "@main") || strings.Contains(v, "show all") || !m.superChat {
		t.Fatalf("a click on the open channel should fold its tree:\n%s", v)
	}
	click()
	if v := nav(); !strings.Contains(v, "@main") || !strings.Contains(v, "show all") {
		t.Fatalf("a second click should unfold it:\n%s", v)
	}
	// from an agent's chat, the click opens the channel's chat and folds nothing
	m.superChat = false
	click()
	if v := nav(); !m.superChat || !strings.Contains(v, "@main") {
		t.Fatalf("from an agent's chat the click opens the channel chat: super=%v\n%s", m.superChat, v)
	}
	// an idle agent stays in the tree while its own chat is open, not in the channel chat
	m.agents[1].State, m.selected = "idle", 1
	if v := nav(); strings.Contains(v, "@napper") {
		t.Fatalf("in the channel chat an idle agent hides:\n%s", v)
	}
	m.superChat = false
	if v := nav(); !strings.Contains(v, "@napper") || !strings.Contains(v, "show all · 1 idle") {
		t.Fatalf("the open agent stays shown:\n%s", v)
	}
}

// TestChannelTreeOpenCloseRules: a click on the selected channel toggles
// its tree; a click on an unselected channel opens its tree if closed and
// leaves an open one alone; switching channels never reopens or closes a
// tree, and no channel's toggle touches another's.
func TestChannelTreeOpenCloseRules(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.superChat = true
	old, dir := m.channelID, m.channel.Dir
	click := func(index int) tea.Cmd {
		y := sidebarY(m, index)
		if y < 0 {
			t.Fatalf("row %d is not drawn", index)
		}
		nm, _ := m.Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		nm, cmd := nm.(Model).Update(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
		m = nm.(Model)
		return cmd
	}
	// the selected channel: a click closes its tree
	click(hereRow(m))
	if !m.treeClosed[old] {
		t.Fatal("a click on the selected channel should close its tree")
	}
	// switch to another channel: the old one's tree stays closed
	m.stash()
	m.channelState = newChannelState("s2", protocol.ChannelInfo{ID: "s2", Name: "other", Dir: dir})
	m.reconciled, m.superChat = true, true
	m.agents = []protocol.AgentInfo{{ID: "z", Name: "solo", Role: "general", State: "running"}}
	m.navChannels = []protocol.ChannelInfo{{ID: old, Name: "proj", Dir: dir}}
	m.layout()
	otherAgents := func() int {
		n := 0
		for _, r := range m.sidebarRows() {
			if r.kind == sbOtherAgent {
				n++
			}
		}
		return n
	}
	if otherAgents() != 0 || !m.treeClosed[old] {
		t.Fatalf("switching away must not reopen a closed tree: %d rows", otherAgents())
	}
	// closing the selected channel's tree leaves the other alone
	click(hereRow(m))
	if !m.treeClosed["s2"] || !m.treeClosed[old] {
		t.Fatalf("toggles are per channel: s2=%v old=%v", m.treeClosed["s2"], m.treeClosed[old])
	}
	click(hereRow(m))
	if m.treeClosed["s2"] {
		t.Fatal("a second click on the selected channel should open its tree")
	}
	// an unselected, closed channel: a click opens its tree (and opens the channel)
	if cmd := click(m.sidebarIndex(sidebarRow{kind: sbOther, k: 0})); cmd == nil || m.treeClosed[old] || otherAgents() == 0 || m.treeClosed["s2"] {
		t.Fatalf("a click on an unselected closed channel should open its tree: closed=%v rows=%d", m.treeClosed[old], otherAgents())
	}
	// an unselected, open channel: a click leaves its tree open
	m.switching = false
	click(m.sidebarIndex(sidebarRow{kind: sbOther, k: 0}))
	if m.treeClosed[old] || otherAgents() == 0 {
		t.Fatal("a click on an unselected open channel should leave its tree open")
	}
}

// TestDividerDialogsGreyUntilOpen: the divider's buttons (role, model,
// variant and the agent's tabs) are grey, and the one whose dialog is open
// is in accent; one dialog is open at a time, and a click on another button
// swaps the open dialog for its own.
func TestDividerDialogsGreyUntilOpen(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	m := channelModel()
	m.superChat = false
	m.agents[0].Role, m.agents[0].Model = "coder", "openai/gpt-5"
	m.layout()
	parts := []string{"coder (coder)", "gpt-5", "default", "async 0", "todo 0", "mcp 0"}
	check := func(name, open string) {
		t.Helper()
		line := m.ruleLine(m.width)
		for _, p := range parts {
			want := theme.StyleDim.Render(p)
			if p == open {
				want = theme.StyleBoxTitleFocus.Render(p)
			}
			if !strings.Contains(line, want) {
				t.Fatalf("%s: %q should be drawn %s:\n%q", name, p, map[bool]string{true: "in accent", false: "grey"}[p == open], line)
			}
		}
	}
	click := func(label string) {
		t.Helper()
		row := stripANSI(m.ruleLine(m.width))
		i := strings.Index(row, label)
		if i < 0 {
			t.Fatalf("no %q on the divider: %q", label, row)
		}
		x, y := ansi.StringWidth(row[:i])+1, m.rows().rule
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		nm, _ = nm.(Model).Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
		m = nm.(Model)
	}
	check("nothing open", "")
	click("todo 0")
	if m.focus != focusTodo {
		t.Fatalf("todo should open: %v", m.focus)
	}
	check("todo open", "todo 0")
	click("mcp 0") // a tab dialog swaps for another
	if m.focus != focusMCP {
		t.Fatalf("mcp should replace todo: %v", m.focus)
	}
	check("mcp open", "mcp 0")
	click("gpt-5") // the tab dialog gives way to the model picker
	if isTab(m.focus) {
		t.Fatalf("the mcp dialog should close for the model picker: %v", m.focus)
	}
	m.openOverlay(newOverlay(ovModels, overlayList, "Select a model")) // what the picker's reply opens
	check("models open", "gpt-5")
	click("async 0") // the overlay gives way to the async dialog
	if m.ov != nil || m.focus != focusAsync {
		t.Fatalf("async should replace the model picker: overlay=%v focus=%v", m.ov != nil, m.focus)
	}
	check("async open", "async 0")
}

// TestSidebarUsageRows: the nav's top shows the system's tokens and cost
// (every channel) and the selected chat's: the channel's in its chat, the
// agent's in the agent's own chat.
func TestSidebarUsageRows(t *testing.T) {
	m := sidebarNavModel()
	m.navChannels = []protocol.ChannelInfo{{ID: "s-2", Name: "other", Tokens: 10_000, CostUSD: 1.5}}
	w := sidebarWidth - 1
	rows := func() []string {
		h := m.sidebarHeader(w)
		return []string{stripANSI(h[m.sidebarSystemRow()]), stripANSI(h[m.sidebarSelectedRow()])}
	}
	row := func(label, figures string) string { // the label left, the figures flush right
		return label + strings.Repeat(" ", w-len([]rune(label))-len([]rune(figures))) + figures
	}
	if r := rows(); r[0] != row("System", "12k · $1.75") || r[1] != row("@main", "1k · $0.20") {
		t.Fatalf("agent chat: %q", r)
	}
	m.superChat = true
	if r := rows(); r[0] != row("System", "12k · $1.75") || r[1] != row("#channel", "2k · $0.25") {
		t.Fatalf("channel chat: %q", r)
	}
	m.channel.Name = strings.Repeat("long", 10)
	if r := rows(); !strings.HasSuffix(r[1], "… 2k · $0.25") || ansi.StringWidth(r[1]) != w {
		t.Fatalf("a long label is cut, never the figures: %q", r[1])
	}
	// with the sidebar, the channel chat has no tabs: tab skips the strip
	if slices.Contains(m.focusOrder(), focusTabs) {
		t.Fatalf("no tab stop without tabs: %v", m.focusOrder())
	}
}

// TestUsageDialogs: the nav's usage rows open the tokens dialog from the
// tokens figure and the cost dialog from the cost, on the system or the
// selected chat (the agent's in its chat, the channel's in the channel
// chat); ←/→ change the range and refetch, t and c switch the chart, and
// the reply draws bars under the peak with the span below.
func TestUsageDialogs(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	clickRow := func(y int, onCost bool) tea.Cmd {
		row := stripANSI(m.sidebarHeader(sidebarWidth - 1)[y])
		x := ansi.StringWidth(row[:strings.LastIndex(row, "$")]) // on the cost
		if !onCost {
			x = ansi.StringWidth(row[:strings.LastIndex(row, " · $")]) - 1 // on the tokens figure
		}
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		nm, cmd := nm.(Model).Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
		m = nm.(Model)
		return cmd
	}
	if clickRow(m.sidebarSelectedRow(), false) == nil || m.focus != focusUsage || m.usage.kind != usageTokens || m.usage.agent != "a" || m.usageTitle() != "Tokens · @main" {
		t.Fatalf("the selected row's tokens: focus=%v %+v", m.focus, m.usage)
	}
	m.closeDialog()
	nm, _ := m.Update(tea.MouseMsg{X: 1, Y: m.sidebarSelectedRow(), Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}) // the label is no button
	nm, _ = nm.(Model).Update(tea.MouseMsg{X: 1, Y: m.sidebarSelectedRow(), Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	if m = nm.(Model); m.focus == focusUsage {
		t.Fatal("a click on the row's label should open nothing")
	}
	clickRow(m.sidebarSystemRow(), true)
	if m.focus != focusUsage || m.usage.kind != usageCost || m.usage.channel != "" || m.usageTitle() != "Cost · System" {
		t.Fatalf("the system row's cost: %+v", m.usage)
	}
	m.closeDialog()
	m.superChat = true
	clickRow(m.sidebarSelectedRow(), true)
	if m.usage.channel != m.channelID || m.usage.agent != "" || m.usageTitle() != "Cost · #channel" {
		t.Fatalf("the channel chat's cost: %+v", m.usage)
	}
	// a reply for an older request is dropped; the current one draws
	epoch := m.usage.epoch
	m.onUsage(usageMsg{epoch: epoch - 1, res: protocol.UsageSeriesResult{Tokens: []int{1}}})
	if m.usage.series != nil {
		t.Fatal("a stale reply should be dropped")
	}
	n := usageChartWidth(m.width)
	tokens, cost := make([]int, n), make([]float64, n)
	tokens[0], tokens[n-1], cost[n-1] = 500, 2000, 1.25
	now := time.Now()
	m.onUsage(usageMsg{epoch: epoch, res: protocol.UsageSeriesResult{From: now.Add(-3 * time.Hour), To: now, Tokens: tokens, Cost: cost}})
	body := stripANSI(strings.Join(m.usageBody(dialog.Width(m.width)-4), "\n"))
	if !strings.Contains(body, "total $1.25") || !strings.Contains(body, "$1.25 ") || !strings.Contains(body, "3h ago") || !strings.Contains(body, "now") || !strings.Contains(body, "█") {
		t.Fatalf("cost chart:\n%s", body)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	body = stripANSI(strings.Join(m.usageBody(dialog.Width(m.width)-4), "\n"))
	lines := strings.Split(body, "\n")
	if !strings.Contains(lines[0], "total 3k") || !strings.HasSuffix(lines[2], "█") || !strings.Contains(lines[2], "2k") || !strings.Contains(lines[1+usageChartRows], "0 █") || []rune(lines[usageChartRows-1])[usageAxisW+1] != ' ' || []rune(lines[usageChartRows])[usageAxisW+1] != '█' {
		t.Fatalf("tokens chart:\n%s", body)
	}
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyRight}); cmd == nil || m.usage.rng != 1 || m.usage.series != nil || m.usage.epoch == epoch {
		t.Fatalf("→ should pick the next range and refetch: rng=%d", m.usage.rng)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus == focusUsage {
		t.Fatal("esc should close the dialog")
	}
}

func TestUsageBars(t *testing.T) {
	got := usageBars([]float64{0, 0.1, 1, 4, 8}, 8, 2)
	if got[0] != "    █" || got[1] != " ▁▂██" {
		t.Fatalf("bars: %q", got)
	}
}

// TestDividerHoverLightens: the divider button under the pointer (a meta
// part, an agent tab or a usage figure) draws in the lighter text colour;
// moving off the buttons puts it back to grey, and an open dialog's button
// stays in accent.
func TestDividerHoverLightens(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	m := channelModel()
	m.superChat = false
	m.agents[0].Role, m.agents[0].Model, m.agents[1].Model, m.channel.Model = "coder", "openai/gpt-5", "openai/gpt-5", "openai/gpt-5"
	m.agents[0].CostUSD = 0.02
	m.layout()
	move := func(label string) {
		t.Helper()
		row := stripANSI(m.ruleLine(m.width))
		i := strings.LastIndex(row, label)
		if i < 0 {
			t.Fatalf("no %q on the divider: %q", label, row)
		}
		nm, _ := m.Update(tea.MouseMsg{X: ansi.StringWidth(row[:i]) + 1, Y: m.rows().rule, Action: tea.MouseActionMotion})
		m = nm.(Model)
	}
	parts := []string{"coder (coder)", "gpt-5", "default", "async 0", "todo 0", "mcp 0"}
	check := func(lit, accent string) {
		t.Helper()
		line := m.ruleLine(m.width)
		for _, p := range parts {
			want := theme.StyleDim.Render(p)
			switch p {
			case accent:
				want = theme.StyleBoxTitleFocus.Render(p)
			case lit:
				want = theme.StyleLit.Render(p)
			}
			if !strings.Contains(line, want) {
				t.Fatalf("hover %q, open %q: %q is not drawn as expected:\n%q", lit, accent, p, line)
			}
		}
	}
	for _, p := range parts {
		move(p)
		check(p, "")
	}
	// off the buttons: grey again
	nm, _ := m.Update(tea.MouseMsg{X: 1, Y: 1, Action: tea.MouseActionMotion})
	m = nm.(Model)
	check("", "")
	// an open dialog's button stays in accent under the pointer
	m.openTab(focusTodo)
	move("todo 0")
	check("", "todo 0")
	move("mcp 0")
	check("mcp 0", "todo 0")
}

// TestNavKeepsHoverAcrossChannelSwitch: a channel picked in the nav while
// the pointer is on it keeps the nav's focus once the switch lands, with
// the cursor (and its background) on the opened channel's row, still the
// row under the pointer; moving off the nav gives the input its focus back.
func TestNavKeepsHoverAcrossChannelSwitch(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.channel.Name = "proj"
	m.navChannels = []protocol.ChannelInfo{{ID: "s-docs", Name: "docs", Dir: "/home/x/Work/docs"}, {ID: "s-web", Name: "web", Dir: "/home/x/Work/web"}}
	old := m.channelID
	m.setFocus(focusInput)
	up := func(msg tea.Msg) {
		nm, _ := m.Update(msg)
		m = nm.(Model)
	}
	idx := m.sidebarIndex(sidebarRow{kind: sbOther, k: 1}) // #web, below this channel
	y := sidebarY(m, idx)
	up(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionMotion})
	up(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	up(tea.MouseMsg{X: 3, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	if !m.switching {
		t.Fatal("the click should switch to #web")
	}
	up(switchedMsg{info: protocol.ChannelInfo{ID: "s-web", Name: "web", Dir: "/home/x/Work/web"}})
	if len(m.navChannels) != 2 || m.navChannels[1].ID != old {
		t.Fatalf("the nav should list the channel left behind at once: %+v", m.navChannels)
	}
	_, items := m.sidebarLines(m.vp.Height)
	if y >= len(items) {
		t.Fatalf("row %d is gone after the switch: %d rows", y, len(items))
	}
	if m.focus != focusSidebar || m.sbCursor != hereRow(m) || items[y] != m.sbCursor {
		t.Fatalf("after the switch the nav should keep its cursor under the pointer: focus=%v cursor=%d here=%d under pointer=%d", m.focus, m.sbCursor, hereRow(m), items[y])
	}
	up(tea.MouseMsg{X: 60, Y: m.rows().input, Action: tea.MouseActionMotion}) // onto the input, off the nav
	if m.focus != focusInput {
		t.Fatalf("moving off the nav should give the input its focus back: %v", m.focus)
	}
}

// TestNavUsageFiguresHighlight: the nav's usage figures are buttons like the
// divider's: grey, lighter under the pointer, and in accent while their
// chart is open, the system's on the System row and the selected chat's on
// its row.
func TestNavUsageFiguresHighlight(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	m := sidebarNavModel()
	m.prompts = nil
	w := sidebarWidth - 1
	check := func(name string, y int, tokens, cost lipgloss.Style) {
		t.Helper()
		row := m.sidebarHeader(w)[y]
		fields := strings.Fields(stripANSI(row))
		tk, c := fields[len(fields)-3], fields[len(fields)-1]
		if !strings.Contains(row, tokens.Render(tk)) || !strings.Contains(row, cost.Render(c)) {
			t.Fatalf("%s: %q", name, row)
		}
	}
	move := func(y int, figure string) {
		row := stripANSI(m.sidebarHeader(w)[y])
		x := ansi.StringWidth(row[:strings.LastIndex(row, figure)])
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionMotion})
		m = nm.(Model)
	}
	check("grey", m.sidebarSystemRow(), theme.StyleDim, theme.StyleDim)
	move(m.sidebarSystemRow(), "$")
	check("hover cost", m.sidebarSystemRow(), theme.StyleDim, theme.StyleLit)
	check("the other row stays grey", m.sidebarSelectedRow(), theme.StyleDim, theme.StyleDim)
	move(m.sidebarSelectedRow(), "1k")
	check("hover tokens", m.sidebarSelectedRow(), theme.StyleLit, theme.StyleDim)
	check("hover moved off", m.sidebarSystemRow(), theme.StyleDim, theme.StyleDim)
	m.openUsage(usageTokens, false)
	check("selected tokens open, hovered", m.sidebarSelectedRow(), theme.StyleBoxTitleFocus, theme.StyleDim)
	check("system closed", m.sidebarSystemRow(), theme.StyleDim, theme.StyleDim)
	m.openUsage(usageCost, true)
	check("system cost open", m.sidebarSystemRow(), theme.StyleDim, theme.StyleBoxTitleFocus)
	check("selected closed", m.sidebarSelectedRow(), theme.StyleLit, theme.StyleDim)
}

// TestPlanUsageBars: each signed-in plan is one row at the top of the nav,
// its name, bar and percent, for its most used window (one whose reset has
// passed reads 0%); the usage rows under the block keep their clicks.
func TestPlanUsageBars(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	now := time.Now()
	w := sidebarWidth - 1
	if rows := m.planUsageRows(w, now); len(rows) != 0 || m.sidebarSystemRow() != 2 {
		t.Fatalf("no reading, no block: %q", rows)
	}
	// a row per window, shortest first, the plan's name on the first only
	m.plans = []protocol.PlanUsageInfo{
		{Provider: "openai", Name: "ChatGPT", Observed: now.Add(-5 * time.Minute), Windows: []protocol.UsageWindowInfo{
			{UsedPercent: 50, Minutes: 300, ResetsAt: now.Add(2 * time.Hour)},
			{UsedPercent: 100, Minutes: 10080, ResetsAt: now.Add(-time.Minute)}, // reset since the reading: 0%
		}},
		{Provider: "xai", Name: "Grok", Observed: now, Windows: []protocol.UsageWindowInfo{{UsedPercent: 100, Minutes: 10080, ResetsAt: now.Add(24 * time.Hour)}}},
		{Provider: "zai", Name: "Z.ai Coding Plan", Observed: now}, // signed in, no reading: no row
	}
	rows := m.planUsageRows(w, now)
	bar := w - len("ChatGPT 5h") - 1 - 1 - 4
	want := []string{
		"Subscriptions",
		"ChatGPT 5h " + strings.Repeat("━", (bar+1)/2) + strings.Repeat("─", bar-(bar+1)/2) + "  50%", // half, rounded up
		"        wk " + strings.Repeat("─", bar) + "   0%",
		"Grok    wk " + strings.Repeat("━", bar) + " 100%",
		"",
	}
	if len(rows) != len(want) {
		t.Fatalf("plan rows: %q", rows)
	}
	for i := range want {
		if got := stripANSI(rows[i]); got != want[i] || (i > 0 && want[i] != "" && ansi.StringWidth(got) != w) { // the title is not padded
			t.Fatalf("row %d: %q, want %q", i, got, want[i])
		}
	}
	// every row of a plan opens that plan's chart
	if _, ok := m.planAt(navAfterClients(m)); ok {
		t.Fatal("the Subscriptions title is no plan")
	}
	for y, provider := range map[int]string{navAfterClients(m) + 1: "openai", navAfterClients(m) + 2: "openai", navAfterClients(m) + 3: "xai"} {
		if p, ok := m.planAt(y); !ok || p.Provider != provider {
			t.Fatalf("row %d belongs to %q, got %q %v", y, provider, p.Provider, ok)
		}
	}
	if _, ok := m.planAt(navAfterClients(m) + 4); ok {
		t.Fatal("the blank row under the block is no plan")
	}
	for minutes, span := range map[int]string{300: "5h", 10080: "wk", 43200: "mo", 1440: "1d", 90: "90m", 0: ""} {
		if got := windowSpan(minutes); got != span {
			t.Fatalf("windowSpan(%d) = %q, want %q", minutes, got, span)
		}
	}
	header := m.sidebarHeader(w)
	// the block is the last of the sections: nothing above it moves
	if m.sidebarSystemRow() != 2 || !strings.HasPrefix(stripANSI(header[m.sidebarSystemRow()]), "System") || !strings.Contains(stripANSI(header[m.sidebarDiscordRow()]), "Discord") ||
		stripANSI(header[navAfterClients(m)]) != "Subscriptions" || len(header) != navAfterClients(m)+5 {
		t.Fatalf("the Subscriptions section follows Clients and moves nothing above it:\n%s", stripANSI(strings.Join(header, "\n")))
	}
	row := stripANSI(header[m.sidebarSystemRow()])
	x := ansi.StringWidth(row[:strings.LastIndex(row, "$")])
	nm, _ := m.Update(tea.MouseMsg{X: x, Y: m.sidebarSystemRow(), Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	nm, _ = nm.(Model).Update(tea.MouseMsg{X: x, Y: m.sidebarSystemRow(), Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	if m = nm.(Model); m.focus != focusUsage || m.usage.kind != usageCost || m.usage.channel != "" {
		t.Fatalf("a click on the moved System cost opens its chart: focus=%v %+v", m.focus, m.usage)
	}
}

// TestNavCacheMonitor: the nav shows what share of the last hour's calls
// came from the providers' prompt caches, above the Discord row, orange
// under 70% and red under 40%, and nothing before any call.
func TestNavCacheMonitor(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	m := sidebarNavModel()
	m.prompts = nil
	w := sidebarWidth - 1
	if row := m.cacheRow(w); row != "" {
		t.Fatalf("no calls, no row: %q", row)
	}
	discord := m.sidebarDiscordRow()
	for _, c := range []struct {
		fresh, cached int64
		want          string
		style         lipgloss.Style
	}{
		{100, 1900, "95%", theme.StyleDim},
		{100, 150, "60%", theme.StyleWarn},
		{900, 100, "10%", theme.StyleError},
	} {
		m.cache = protocol.CacheUsageResult{Fresh: c.fresh, Cached: c.cached}
		row := m.cacheRow(w)
		if !strings.Contains(row, c.style.Render(c.want)) || !strings.HasPrefix(stripANSI(row), "cache ") || ansi.StringWidth(stripANSI(row)) != w {
			t.Fatalf("%d/%d: %q", c.cached, c.fresh, row)
		}
		header := m.sidebarHeader(w)
		at := navAfterClients(m) // no plan reading here: the monitors come right after the Clients section
		if m.sidebarDiscordRow() != discord || stripANSI(header[at]) != stripANSI(row) || header[len(header)-1] != "" || len(header) != at+2 {
			t.Fatalf("the monitor sits under the sections, a blank after it, and moves nothing above:\n%s", stripANSI(strings.Join(header, "\n")))
		}
	}
}

// TestNavCacheMonitorNamesTheProviderThatIsMissing: a total is dominated by
// whichever provider does the most work. One provider whose cache stopped
// working, as ChatGPT's did on 2026-09-16, is named and colours the row,
// once it has carried enough traffic to judge.
func TestNavCacheMonitorNamesTheProviderThatIsMissing(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(prev)
	m := sidebarNavModel()
	w := sidebarWidth - 1
	m.cache = protocol.CacheUsageResult{Fresh: 30_000_000 + 9_800_000, Cached: 970_000_000 + 200_000, Providers: []protocol.CacheProviderUsage{
		{Provider: "xai", Fresh: 30_000_000, Cached: 970_000_000},
		{Provider: "openai", Fresh: 9_800_000, Cached: 200_000}, // 2%
		{Provider: "kimi", Fresh: 4_000, Cached: 0},             // one first call: too little to judge
	}}
	row := m.cacheRow(w)
	if text := stripANSI(row); !strings.HasSuffix(text, "96% · openai 2%") || ansi.StringWidth(text) != w {
		t.Fatalf("row %q", text)
	}
	if !strings.Contains(row, theme.StyleError.Render("96% · openai 2%")) {
		t.Fatalf("the row is not red for a provider at 2%%: %q", row)
	}
	m.cache.Providers[1] = protocol.CacheProviderUsage{Provider: "openai", Fresh: 1_000_000, Cached: 9_000_000}
	if text := stripANSI(m.cacheRow(w)); strings.Contains(text, "openai") {
		t.Fatalf("every provider is healthy, none is named: %q", text)
	}
}

// TestAsyncTabOwedReplies: the async tab counts the replies the selected
// agent owes alongside what it waits on, lists them with what the harness
// will do about them, and space on one opens that party's chat.
func TestAsyncTabOwedReplies(t *testing.T) {
	m := channelModel()
	m.agents[0].Role = "general"
	if sv := stripANSI(tabsView(m, 120)); !strings.Contains(sv, "async 0") || strings.Contains(sv, "nudges") {
		t.Fatalf("strip:\n%s", sv)
	}
	m.focus = focusAsync
	if dv := stripANSI(m.tabDialog(120)); !strings.Contains(dv, "Async 0") || !strings.Contains(dv, "no replies due") {
		t.Fatalf("empty dialog:\n%s", dv)
	}
	m.agents[0].PendingReplies = []event.ReplyRequest{
		{ID: "r1", From: "user", FromName: "user", Text: "fix the failing test"},
		{ID: "r2", From: "b", FromName: "scout", Text: "what did you find?"},
	}
	m.agents[0].Nudges, m.agents[0].NudgeLimit = 1, 3
	dv := stripANSI(m.tabDialog(120))
	if !strings.Contains(dv, "Async 2") || !strings.Contains(dv, "@user: fix the failing test") || !strings.Contains(dv, "@scout: what did you find?") ||
		!strings.Contains(dv, "a reminder follows a turn that ends owing these (1 of 3 used)") {
		t.Fatalf("dialog:\n%s", dv)
	}
	m.agents[0].Nudges = 3
	if dv := stripANSI(m.tabDialog(120)); !strings.Contains(dv, "3 empty reminder turns") {
		t.Fatalf("spent:\n%s", dv)
	}
	m.agents[0].NudgeLimit = 0
	if dv := stripANSI(m.tabDialog(120)); !strings.Contains(dv, "reminders are off") {
		t.Fatalf("off:\n%s", dv)
	}
	m.agents[0].Awaiting = []string{"b"}
	m.agents[0].Nudges, m.agents[0].NudgeLimit = 1, 3
	if dv := stripANSI(m.tabDialog(120)); strings.Contains(dv, "no reminder until that lands") || !strings.Contains(dv, "a reminder follows a turn that ends owing these") {
		t.Fatalf("awaiting an agent must not suppress the reminder copy:\n%s", dv)
	}
	m.agents[0].Jobs = []protocol.JobInfo{{ID: "j1", Spec: "sleep 30"}}
	if dv := stripANSI(m.tabDialog(120)); !strings.Contains(dv, "waiting on a job: no reminder until that lands") {
		t.Fatalf("job:\n%s", dv)
	}
	m.agents[0].Jobs = nil
	// space on the second row opens that agent's chat
	m.superChat = true
	m.agCursor = 1
	press(&m, tea.KeyMsg{Type: tea.KeySpace})
	if m.superChat || m.selectedID() != "b" || m.focus != focusInput {
		t.Fatalf("space should open @scout's chat: super=%v selected=%s focus=%v", m.superChat, m.selectedID(), m.focus)
	}
}

// TestSidebarKeepsIdleAncestors: an idle agent whose descendant is drawn
// stays in the tree, so the drawn one still hangs under its parent; an idle
// branch with nothing busy under it still hides.
func TestSidebarKeepsIdleAncestors(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	m.superChat = true
	m.agents = []protocol.AgentInfo{
		{ID: "a", Name: "main", Role: "general", State: "idle"},
		{ID: "b", Parent: "a", Depth: 1, Name: "middle", Role: "general", State: "idle"},
		{ID: "c", Parent: "b", Depth: 2, Name: "worker", Role: "general", State: "running"},
		{ID: "d", Parent: "a", Depth: 1, Name: "napper", Role: "general", State: "idle"},
		{ID: "e", Parent: "d", Depth: 2, Name: "sleeper", Role: "general", State: "idle"},
	}
	m.layout()
	shown, quiet := m.shownAgents(m.channelID, m.agents, "")
	names := func(idx []int) []string {
		var out []string
		for _, i := range idx {
			out = append(out, m.agents[i].Name)
		}
		return out
	}
	if got := names(shown); len(got) != 3 || got[0] != "main" || got[1] != "middle" || got[2] != "worker" {
		t.Fatalf("an idle parent of a running agent stays: %v", got)
	}
	if quiet != 2 { // napper and sleeper, with nothing busy under them
		t.Fatalf("quiet: %d", quiet)
	}
	nav := func() string {
		body, _ := m.sidebarBody(sidebarWidth - 1)
		return stripANSI(strings.Join(body, "\n"))
	}
	if v := nav(); !strings.Contains(v, "@middle") || !strings.Contains(v, "@worker") || strings.Contains(v, "@napper") || !strings.Contains(v, "▸ show all · 2 idle") {
		t.Fatalf("tree:\n%s", v)
	}
	// with the branch's worker idle too, the whole branch hides
	m.agents[2].State = "idle"
	if _, quiet := m.shownAgents(m.channelID, m.agents, ""); quiet != 4 {
		t.Fatalf("nothing busy anywhere: quiet %d", quiet)
	}
}

// TestPlanUsageChart: a click on a plan's row in the nav opens its chart,
// which draws every reading against the whole allowance and reads "now
// N%"; /plan opens the same.
func TestPlanUsageChart(t *testing.T) {
	m := sidebarNavModel()
	m.prompts = nil
	now := time.Now()
	m.plans = []protocol.PlanUsageInfo{{Provider: "openai", Name: "ChatGPT", Observed: now.Add(-time.Minute),
		Windows: []protocol.UsageWindowInfo{{UsedPercent: 40, Minutes: 10080, ResetsAt: now.Add(24 * time.Hour)}}}}
	m.layout()
	nm, _ := m.Update(tea.MouseMsg{X: 3, Y: navAfterClients(m) + 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	nm, _ = nm.(Model).Update(tea.MouseMsg{X: 3, Y: navAfterClients(m) + 1, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	m = nm.(Model)
	if m.focus != focusUsage || m.usage.kind != usagePlan || m.usage.provider != "openai" || m.usageTitle() != "Plan · ChatGPT" {
		t.Fatalf("a click on the plan row opens its chart: focus=%v %+v", m.focus, m.usage)
	}
	n := usageChartWidth(m.width)
	percent := make([]float64, n)
	for i := range percent {
		percent[i] = 20
	}
	percent[n-1] = 50
	m.onUsage(usageMsg{epoch: m.usage.epoch, res: protocol.UsageSeriesResult{From: now.Add(-3 * time.Hour), To: now}, percent: percent})
	body := stripANSI(strings.Join(m.usageBody(dialog.Width(m.width)-4), "\n"))
	lines := strings.Split(body, "\n")
	if !strings.Contains(lines[0], "now 50%") || !strings.Contains(lines[2], "100%") {
		t.Fatalf("a plan is drawn against the whole allowance:\n%s", body)
	}
	bottom, middle := lines[1+usageChartRows], lines[2+usageChartRows/2]
	if strings.Count(bottom, "█") != n || strings.Count(middle, "█") != 1 {
		t.Fatalf("every reading fills the bottom row, only the 50%% one reaches the middle:\n%s", body)
	}
	m.closeDialog()
	if cmd := m.command("/plan"); cmd == nil || m.usage.kind != usagePlan {
		t.Fatalf("/plan: %+v", m.usage)
	}
}

// TestRecapCommand: /recap reports the setting, takes minutes (with or
// without a unit), turns off, and refuses nonsense.
func TestRecapCommand(t *testing.T) {
	m := channelModel()
	m.command("/recap")
	if !strings.Contains(m.status, "recap is off") || m.statusErr {
		t.Fatalf("unset: %q", m.status)
	}
	m.channel.Recap = 10
	m.command("/recap")
	if !strings.Contains(m.status, "every 10 min of quiet") {
		t.Fatalf("set: %q", m.status)
	}
	for _, arg := range []string{"15", "15m", "15 min", "15 minutes"} {
		if cmd := m.command("/recap " + arg); cmd == nil {
			t.Fatalf("%q should set the timer", arg)
		}
	}
	for _, arg := range []string{"off", "0"} {
		if cmd := m.command("/recap " + arg); cmd == nil {
			t.Fatalf("%q should turn it off", arg)
		}
	}
	m.command("/recap soon")
	if !m.statusErr || !strings.Contains(m.status, "usage: /recap") {
		t.Fatalf("nonsense: %q", m.status)
	}
}

// TestProjectTrustRowInNav: the nav reports whether the selected channel's
// project configuration is trusted, above the Discord row — untrusted is
// the one that matters, since the channel silently loses its roles, skills
// and commands, but the row stays either way so its absence means something.
// A directory with no project configuration has no row at all.
func TestProjectTrustRowInNav(t *testing.T) {
	m := channelModel()
	m.channelID = "c1"
	w := sidebarWidth - 1
	if row := m.trustRow(w); row != "" || m.sidebarTrustRow() != -1 {
		t.Fatalf("no project configuration, no row: %q", stripANSI(row))
	}
	discord := m.sidebarDiscordRow()

	for _, c := range []struct {
		pending bool
		want    string
		style   lipgloss.Style
	}{
		{false, "trusted", theme.StyleDim},
		{true, "untrusted", theme.StyleWarn},
	} {
		m.channel.TrustFiles, m.channel.TrustPending = 3, c.pending
		row := m.trustRow(w)
		if !strings.HasPrefix(stripANSI(row), "project ") || !strings.Contains(row, c.style.Render(c.want)) ||
			ansi.StringWidth(stripANSI(row)) != w {
			t.Fatalf("pending=%v row: %q", c.pending, stripANSI(row))
		}
		header := m.sidebarHeader(w)
		at := m.sidebarTrustRow()
		if at < 0 || at >= len(header) || stripANSI(header[at]) != stripANSI(row) {
			t.Fatalf("the row is not where it says:\n%s", stripANSI(strings.Join(header, "\n")))
		}
		if m.sidebarDiscordRow() != discord || at != navAfterClients(m) {
			t.Fatalf("the project row sits under the sections and moves nothing above: project %d discord %d", at, m.sidebarDiscordRow())
		}
		if cmd := m.sidebarClick(0, at); cmd == nil {
			t.Fatalf("pending=%v: a click should do something", c.pending)
		}
	}
}
