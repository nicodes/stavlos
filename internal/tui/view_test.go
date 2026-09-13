package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

func TestBuildLogo(t *testing.T) {
	rows := buildLogo("stav")
	if len(rows) != 4 {
		t.Fatalf("want 4 rows, got %d", len(rows))
	}
	w := ansi.StringWidth(rows[0])
	if w != 4*4+3 {
		t.Fatalf("width: got %d, want %d", w, 4*4+3)
	}
	for i, r := range rows {
		if ansi.StringWidth(r) != w {
			t.Errorf("row %d width %d != %d: %q", i, ansi.StringWidth(r), w, r)
		}
		if strings.Trim(r, "▀▄█ ") != "" {
			t.Errorf("row %d has non-block glyphs: %q", i, r)
		}
	}
	// Unknown letters keep the grid aligned.
	for _, r := range buildLogo("s?s") {
		if ansi.StringWidth(r) != 4*3+2 {
			t.Errorf("unknown glyph broke alignment: %q", r)
		}
	}
}

func TestLogoLines(t *testing.T) {
	big := logoLines(80)
	if len(big) != 4 {
		t.Fatalf("want 4 rows, got %d", len(big))
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
	got = stripANSI(footerRight(footerInfo{connected: true, label: "coder", model: "anthropic/claude-opus-5", tokens: 12_345, cost: 0.0123}))
	if got != "12k tokens · $0.0123" {
		t.Fatalf("session: %q", got)
	}
}

func TestFmtCost(t *testing.T) {
	cases := map[float64]string{0: "0.00", 0.0123: "0.0123", 0.01: "0.01", 1.5: "1.50", 2.3456: "2.3456", 0.00004: "0.00"}
	for in, want := range cases {
		if got := fmtCost(in); got != want {
			t.Errorf("fmtCost(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestMetaLine(t *testing.T) {
	if got := stripANSI(metaLine("main", "coder", "anthropic/claude-opus-5", "", 0, false, metaNone)); got != "main (coder) · claude-opus-5 · default" {
		t.Fatalf("with model: %q", got)
	}
	if got := stripANSI(metaLine("main", "coder", "", "", 0, false, metaNone)); got != "main (coder) · no model — /models" {
		t.Fatalf("no model: %q", got)
	}
	if got := stripANSI(metaLine("scout", "explorer", "ollama/llama3", "", 2, false, metaNone)); got != "scout (explorer) · llama3 · default · 2 queued" {
		t.Fatalf("queued: %q", got)
	}
	if got := stripANSI(metaLine("main", "coder", "openai/gpt-5", "high", 0, false, metaNone)); got != "main (coder) · gpt-5 · high" {
		t.Fatalf("variant: %q", got)
	}
	if got := stripANSI(metaLine("main", "coder", "openai/gpt-5", "", 0, true, metaNone)); got != "YOLO · main (coder) · gpt-5 · default" {
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

func TestHomeAndSessionViews(t *testing.T) {
	m := newModel(nil, nil, "sess-1234-5678")
	m.width, m.height = 100, 30
	m.reconciled = true
	m.loading = false
	m.session.Dir = "/tmp/proj"
	m.layout()

	home := m.View()
	lines := strings.Split(home, "\n")
	if len(lines) != 30 {
		t.Fatalf("home view must fill the window: %d lines", len(lines))
	}
	plain := stripANSI(home)
	if !strings.Contains(plain, "▀") || strings.Contains(plain, "sign in with ChatGPT or Grok") || !strings.Contains(plain, "Get started /providers") {
		t.Fatalf("home view:\n%s", plain)
	}
	if strings.Contains(plain, "session ") {
		t.Fatal("home view must not show the sidebar")
	}
	if !strings.Contains(plain, "Saddle up.") {
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
	// once a prompt is waiting the strip (and its tab stop) appears
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "trust", Agent: "a"}}
	if v := stripANSI(m.View()); !strings.Contains(v, "trust (1)") {
		t.Fatalf("a waiting prompt should bring the strip to the home screen:\n%s", v)
	}
	m.prompts = nil
	for _, l := range lines {
		if ansi.StringWidth(l) > 100 {
			t.Fatalf("line wider than the window: %q", l)
		}
	}

	// A user message moves to the session state; ctrl+b shows the sidebar.
	m.agents = []protocol.AgentInfo{{ID: "a1", Label: "root", Archetype: "coder", Model: "anthropic/claude-x", State: "idle"}}
	m.session.Model = "anthropic/claude-x"
	m.transcript("a1").Notice("hello")
	m.showTree = true
	m.layout()
	sess := stripANSI(m.View())
	if !strings.Contains(sess, "hello") || !strings.Contains(sess, "session  sess-123") {
		t.Fatalf("session view:\n%s", sess)
	}
	m.width = 90 // too narrow: sidebar auto-hides
	m.layout()
	if strings.Contains(stripANSI(m.View()), "session  sess-123") {
		t.Fatal("sidebar should auto-hide below 100 columns")
	}
}

func TestAgentRows(t *testing.T) {
	now := time.Now()
	spawned := map[string]time.Time{"c1": now.Add(-75 * time.Second), "c2": now.Add(-3 * time.Second)}
	agents := []protocol.AgentInfo{
		{ID: "root", Label: "coder", Archetype: "coder", State: "idle"},
		{ID: "c1", Parent: "root", Label: "scout", Archetype: "explorer", State: "running", Turn: 2, CostUSD: 0.0012},
		{ID: "c2", Parent: "root", Label: "tester", Archetype: "tester", State: "idle"},
		{ID: "c3", Parent: "root", Label: "done", Archetype: "explorer", State: "killed"},
		{ID: "g1", Parent: "c1", Label: "grandchild", Archetype: "explorer", State: "running"},
	}
	rows := agentRows(agents, "root", spawned, map[string]string{"c1": "Running the tests now, hold on while I look through all of it"}, now, 100)
	if len(rows) != 2 {
		t.Fatalf("rows %d: %q", len(rows), rows)
	}
	if !strings.Contains(rows[0], "scout (explorer)  Running the tests now, hold on while I l…  ") || !strings.Contains(rows[0], "1m15s") || !strings.Contains(rows[0], "⑂") {
		t.Fatalf("%q", rows[0])
	}
	if strings.Contains(rows[0], "wakes parent") {
		t.Fatalf("armed marker: %q", rows)
	}
	if !strings.Contains(rows[1], "tester") || !strings.Contains(rows[1], "3s") || strings.Contains(rows[1], "turn") {
		t.Fatalf("%q", rows[1])
	}
	if rows := agentRows(agents, "c2", spawned, nil, now, 100); len(rows) != 0 {
		t.Fatalf("no children expected: %q", rows)
	}
	if got := fmtElapsed(3725 * time.Second); got != "1h02m" {
		t.Fatalf("%s", got)
	}

	// The agents tab counts only the selected agent's children, and lists
	// them once focused.
	m := sessionModel()
	m.spawned = spawned
	m.agents = agents
	m.selected = 0
	view := stripANSI(m.sectionsView(100))
	if !strings.Contains(view, "agents (2)") || strings.Contains(view, "scout") || strings.Contains(view, "grandchild") {
		t.Fatalf("collapsed agents tab should only count:\n%s", view)
	}
	m.focus = focusAgents
	if view = stripANSI(m.sectionsView(100)); strings.Contains(view, "scout") {
		t.Fatalf("the strip never lists agents:\n%s", view)
	}
	view = stripANSI(m.tabDialog(100))
	if !strings.Contains(view, "scout") || strings.Contains(view, "grandchild") || !strings.HasPrefix(view, "╭") {
		t.Fatalf("agents dialog:\n%s", view)
	}
}

func TestMonitorRows(t *testing.T) {
	now := time.Now().Truncate(time.Second) // Started is RFC3339: whole seconds
	monitors := []protocol.MonitorInfo{
		{ID: "m1", Agent: "root", Kind: "command", Label: "go test", Spec: "go test ./...", State: "running", Started: now.Add(-75 * time.Second).Format(time.RFC3339), Progress: "42 lines"},
		{ID: "m2", Agent: "root", Kind: "command", Label: "src changes", Spec: "./watch.sh", State: "running", Started: now.Add(-3 * time.Second).Format(time.RFC3339)},
		{ID: "m3", Agent: "root", Kind: "command", Label: "cooldown", Spec: "sleep 300", State: "running", Started: now.Add(-2 * time.Hour).Format(time.RFC3339), Progress: "3m left"},
		{ID: "m4", Agent: "root", Kind: "command", Label: "old", State: "fired", Started: now.Format(time.RFC3339)},
	}
	rows := monitorRows(monitors, "coder", "coder", now, 100)
	if len(rows) != 3 {
		t.Fatalf("rows %d: %q", len(rows), rows)
	}
	plain := make([]string, len(rows))
	for i, r := range rows {
		plain[i] = stripANSI(r)
	}
	// command: glyph, bold label, kind, progress, elapsed
	if !strings.HasPrefix(plain[0], "  $") || !strings.Contains(plain[0], "coder (coder)  go test") {
		t.Fatalf("command row: %q", plain[0])
	}
	for _, want := range []string{"42 lines", "1m15s"} {
		if !strings.Contains(plain[0], want) {
			t.Fatalf("command row lacks %q: %q", want, plain[0])
		}
	}
	// a second running job: no wake tag, elapsed
	if !strings.HasPrefix(plain[1], "  $ coder (coder)  src changes") || !strings.HasSuffix(strings.TrimRight(plain[1], " "), "coder (coder)  src changes  3s") {
		t.Fatalf("second job row: %q", plain[1])
	}
	// progress and hours elapsed
	if !strings.HasPrefix(plain[2], "  $ coder (coder)  cooldown") || !strings.Contains(plain[2], "3m left · 2h00m") {
		t.Fatalf("third job row: %q", plain[2])
	}
	// a bad Started stamp just drops the elapsed field
	rows = monitorRows([]protocol.MonitorInfo{{ID: "x", Kind: "command", Label: "w", State: "running", Started: "nope"}}, "", "", now, 100)
	if len(rows) != 1 || strings.Contains(rows[0], "command") || !strings.HasSuffix(strings.TrimRight(stripANSI(rows[0]), " "), "w") {
		t.Fatalf("bad stamp: %q", rows)
	}
	if monitorRows(nil, "coder", "coder", now, 100) != nil {
		t.Fatal("no monitors should give no rows")
	}

	// The section reads the selected agent's Monitors; unfocused it is one
	// summary line, focused it lists one row per job.
	m := sessionModel()
	m.agents = []protocol.AgentInfo{{ID: "root", Label: "coder", Archetype: "coder", State: "idle", Monitors: monitors[:3]}}
	m.selected = 0
	view := stripANSI(m.sectionsView(100))
	if !strings.Contains(view, "async (3)") || strings.Count(view, "\n") != 0 {
		t.Fatalf("collapsed async tab:\n%s", view)
	}
	m.focus = focusAsync
	if view := stripANSI(m.sectionsView(100)); strings.Count(view, "\n") != 0 {
		t.Fatalf("the strip stays one line with a tab open:\n%s", view)
	}
	// the dialog: title, rule, one row per job, inside the border
	if view := stripANSI(m.tabDialog(100)); strings.Count(view, "\n") != 6 || !strings.Contains(view, "go test") || !strings.Contains(view, "cooldown") {
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
	m.agents[0].Monitors = nil
	m.layout()
	if m.vp.Height != collapsed || !strings.Contains(stripANSI(m.sectionsView(100)), "async (0)") {
		t.Fatalf("layout: viewport %d without jobs, want %d:\n%s", m.vp.Height, collapsed, stripANSI(m.sectionsView(100)))
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
	m.agents = []protocol.AgentInfo{{ID: "a", Label: "coder"}, {ID: "b", Label: "scout", Depth: 1}, {ID: "c", Label: "tester", Depth: 1}}
	m.toggleTree()
	if !m.showTree || m.focus != focusSidebar || m.input.Focused() {
		t.Fatalf("open should focus the sidebar: show=%v focus=%v inputFocused=%v", m.showTree, m.focus, m.input.Focused())
	}
	down := tea.KeyMsg{Type: tea.KeyDown}
	m.handleKey(down)
	m.handleKey(down)
	if m.sbCursor != 2 || m.selected != 0 {
		t.Fatalf("cursor %d selected %d", m.sbCursor, m.selected)
	}
	rows := m.treeRows(30)
	if !strings.Contains(rows[2], "▶") || !strings.Contains(rows[0], "▸") {
		t.Fatalf("markers: %q", rows)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.selected != 2 || m.focus != focusInput || !m.input.Focused() {
		t.Fatalf("enter: selected %d focus %v", m.selected, m.focus)
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

// sessionModel is a model in the session state (one transcript item) at a
// size where the sidebar fits.
func sessionModel() Model {
	m := newModel(context.Background(), nil, "s")
	m.width, m.height = 120, 40
	m.reconciled, m.loading = true, false
	m.agents = []protocol.AgentInfo{{ID: "a", Label: "coder"}, {ID: "b", Label: "scout", Depth: 1}}
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
	m := sessionModel()
	if m.focus != focusInput {
		t.Fatalf("default focus %v", m.focus)
	}
	// No prompt, sidebar hidden: input → chat → meta row → tabs → input. The
	// strip is one stop; all empty, it opens on permission, and ←/→ walk the
	// tabs. The meta row is a stop too: ←/→ pick role, model, variant.
	right := tea.KeyMsg{Type: tea.KeyRight}
	left := tea.KeyMsg{Type: tea.KeyLeft}
	press(&m, tab)
	if m.focus != focusChat || m.follow || m.input.Focused() {
		t.Fatalf("tab: focus=%v follow=%v", m.focus, m.follow)
	}
	press(&m, tab)
	if m.focus != focusMeta || m.metaSel != metaRole || m.input.Focused() {
		t.Fatalf("tab tab: focus=%v sel=%v", m.focus, m.metaSel)
	}
	press(&m, left) // leftmost already (no YOLO): stays
	press(&m, right)
	if m.metaSel != metaModel {
		t.Fatalf("→ should move to the model: %v", m.metaSel)
	}
	press(&m, right)
	press(&m, right) // rightmost: stays on the variant
	if m.metaSel != metaVariant {
		t.Fatalf("→→ should stop on the variant: %v", m.metaSel)
	}
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
		t.Fatal("enter on a meta part should open its dialog")
	}
	press(&m, tab)
	if m.focus != focusPermission || !strings.Contains(stripANSI(m.tabDialog(100)), "no prompts waiting") {
		t.Fatalf("tab x3: focus=%v\n%s", m.focus, stripANSI(m.tabDialog(100)))
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}) // nothing to answer: ignored
	if m.focus != focusPermission {
		t.Fatalf("y on an empty permission tab: focus=%v", m.focus)
	}
	press(&m, left) // already leftmost: stays
	if m.focus != focusPermission {
		t.Fatalf("left at the edge: focus=%v", m.focus)
	}
	press(&m, right)
	if m.focus != focusAgents || !strings.Contains(stripANSI(m.tabDialog(100)), "no subagents running") {
		t.Fatalf("right: focus=%v\n%s", m.focus, stripANSI(m.tabDialog(100)))
	}
	press(&m, right)
	if m.focus != focusAsync || !strings.Contains(stripANSI(m.tabDialog(100)), "no async jobs running") {
		t.Fatalf("right right: focus=%v\n%s", m.focus, stripANSI(m.tabDialog(100)))
	}
	press(&m, right) // already rightmost: stays
	if m.focus != focusAsync {
		t.Fatalf("right at the edge: focus=%v", m.focus)
	}
	press(&m, left)
	if m.focus != focusAgents {
		t.Fatalf("left: focus=%v", m.focus)
	}
	press(&m, tab) // any tab → input
	if m.focus != focusInput || !m.follow || !m.input.Focused() {
		t.Fatalf("tab from a tab: focus=%v follow=%v", m.focus, m.follow)
	}
	press(&m, stab)
	if m.focus != focusPermission {
		t.Fatalf("shift+tab: focus=%v", m.focus)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusInput {
		t.Fatalf("esc: focus=%v", m.focus)
	}

	// Sidebar shown: input → sidebar → chat → meta row → tabs → input.
	m.showTree = true
	m.layout()
	var seen []focus
	for i := 0; i < 5; i++ {
		press(&m, tab)
		seen = append(seen, m.focus)
	}
	if want := []focus{focusSidebar, focusChat, focusMeta, focusPermission, focusInput}; !equalFocus(seen, want) {
		t.Fatalf("with sidebar: %v, want %v", seen, want)
	}
	// Hiding the sidebar while it has focus falls back to the input.
	press(&m, tab) // input → sidebar
	m.showTree = false
	m.ensureFocus()
	if m.focus != focusInput {
		t.Fatalf("sidebar hidden: focus=%v", m.focus)
	}

	// Pending prompt: chat → permission (the first non-empty tab) → input.
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Agent: "a", Tool: "bash"}}
	if m.focus != focusInput {
		t.Fatal("a new prompt must not steal focus")
	}
	seen = nil
	for i := 0; i < 4; i++ {
		press(&m, tab)
		seen = append(seen, m.focus)
	}
	if want := []focus{focusChat, focusMeta, focusPermission, focusInput}; !equalFocus(seen, want) {
		t.Fatalf("with prompt: %v, want %v", seen, want)
	}
	// With a child but no prompt, the strip opens on agents; with only a
	// job, on async.
	m.prompts = nil
	m.agents = append(m.agents, protocol.AgentInfo{ID: "c", Parent: "a", Label: "kid", State: "working"})
	press(&m, stab)
	if m.focus != focusAgents {
		t.Fatalf("first non-empty tab should be agents: %v", m.focus)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	m.agents = m.agents[:len(m.agents)-1]
	m.agents[0].Monitors = []protocol.MonitorInfo{{ID: "j", Kind: "command", Label: "sleep", State: "running"}}
	press(&m, stab)
	if m.focus != focusAsync {
		t.Fatalf("first non-empty tab should be async: %v", m.focus)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	m.agents[0].Monitors = nil
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Agent: "a", Tool: "bash"}}
	press(&m, stab) // input → permission
	if m.focus != focusPermission {
		t.Fatalf("shift+tab from input: %v", m.focus)
	}
	// Answering the prompt elsewhere returns focus to the input.
	m.removePrompt("p")
	m.ensureFocus()
	if m.focus != focusInput {
		t.Fatalf("prompt gone: focus=%v", m.focus)
	}
	// Tab never cycles agents any more; ctrl+n still does.
	press(&m, tab, tab)
	if m.selected != 0 {
		t.Fatalf("tab changed the selection to %d", m.selected)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyCtrlN})
	if m.selected != 1 {
		t.Fatalf("ctrl+n: selected %d", m.selected)
	}
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
	m := sessionModel()
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Agent: "a", Tool: "bash"}}

	// Input focus: y is typed, the prompt is untouched.
	if cmd := press(&m, y); m.promptBusy != "" || m.claimedByUs["p"] || m.input.Value() != "y" {
		t.Fatalf("input focus: busy=%q claimed=%v input=%q cmd=%v", m.promptBusy, m.claimedByUs["p"], m.input.Value(), cmd != nil)
	}
	m.input.Reset()

	// Permission focus: y answers (claim + reply as one tea.Cmd).
	press(&m, tea.KeyMsg{Type: tea.KeyShiftTab}) // the strip opens on permission: a prompt waits
	if m.focus != focusPermission {
		t.Fatalf("focus %v", m.focus)
	}
	if cmd := press(&m, y); cmd == nil || m.promptBusy != "p" || !m.claimedByUs["p"] {
		t.Fatalf("permission focus: busy=%q claimed=%v cmd=%v", m.promptBusy, m.claimedByUs["p"], cmd != nil)
	}
	if m.input.Value() != "" {
		t.Fatalf("y leaked into the input: %q", m.input.Value())
	}
	// The box's hint line tells an unfocused user to tab in first.
	m.promptBusy = ""
	if strings.Contains(stripANSI(m.sectionsView(80)), "tab to focus") {
		t.Fatal("focused box should show the hotkeys directly")
	}
	m.focus = focusInput
	if pv := stripANSI(m.sectionsView(80)); !strings.Contains(pv, "permission (1)") || strings.Count(pv, "\n") != 0 {
		t.Fatalf("unfocused prompt should be one strip line: %q", pv)
	}
	m.focus = focusPermission
	hs := m.keyHints()
	if hs[0].key != "y" || hs[1].key != "a" || hs[2].key != "n" {
		t.Fatalf("permission hints: %+v", hs)
	}

	// Trust prompts: "a" does nothing.
	m.prompts = []protocol.PromptInfo{{ID: "t", Kind: "trust", Input: []byte(`{"dir":"/x","hash":"h"}`)}}
	m.promptBusy = ""
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}); cmd != nil || m.promptBusy != "" {
		t.Fatal("a must not answer a trust prompt")
	}
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}); cmd == nil || m.promptBusy != "t" {
		t.Fatal("n should answer a trust prompt")
	}

	// Questions: typing goes to the box's own field, enter answers.
	m.prompts = []protocol.PromptInfo{{ID: "q", Kind: "question", Question: "which?", Options: []string{"red", "blue"}}}
	m.promptBusy = ""
	m.ensureFocus()
	if !m.promptInput.Focused() {
		t.Fatal("question field should take focus")
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if m.promptInput.Value() != "2" || m.input.Value() != "" {
		t.Fatalf("typing: field=%q input=%q", m.promptInput.Value(), m.input.Value())
	}
	if cmd := press(&m, tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil || m.promptBusy != "q" || m.promptInput.Value() != "" {
		t.Fatalf("enter: busy=%q field=%q", m.promptBusy, m.promptInput.Value())
	}
	// Enter in the input focus sends a prompt, it never answers a question.
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	m.promptBusy = ""
	m.input.SetValue("hello agent")
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.promptBusy != "" || m.history[len(m.history)-1] != "hello agent" {
		t.Fatalf("input enter answered the question: busy=%q", m.promptBusy)
	}
}

func TestChatCursorMovesAndRenders(t *testing.T) {
	markCursorForTest(t)
	m := sessionModel()
	tr := m.transcript("a")
	for i := 0; i < 8; i++ {
		tr.Apply(mk(int64(i+1), "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "msg " + string(rune('A'+i))}))
	}
	tr.Apply(mk(9, "a", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}))
	tr.Apply(mk(10, "a", event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c1", Name: "bash", Output: strings.TrimRight(strings.Repeat("out\n", 8), "\n")}))
	m.height = 20 // a viewport smaller than the transcript so the cursor has to scroll
	m.layout()
	m.refreshViewport()
	items := tr.Items() // notice + 8 user + tool = 10

	press(&m, tea.KeyMsg{Type: tea.KeyTab}) // input → chat (the cycle wraps: chat is first)
	if m.focus != focusChat || m.chatCursor != items-1 || m.follow {
		t.Fatalf("enter chat: focus=%v cursor=%d follow=%v", m.focus, m.chatCursor, m.follow)
	}
	// marked is the first non-blank line carrying the cursor marker.
	marked := func() string {
		for _, l := range strings.Split(stripANSI(m.vp.View()), "\n") {
			if s := strings.TrimSpace(strings.TrimPrefix(l, gutterMark)); strings.HasPrefix(l, gutterMark) && s != "" {
				return s
			}
		}
		return ""
	}
	if got := marked(); !strings.HasPrefix(got, "$ Bash") {
		t.Fatalf("last item should be marked: %q", got)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	if m.chatCursor != items-2 {
		t.Fatalf("up: cursor %d", m.chatCursor)
	}
	if got := marked(); got != "› msg H" {
		t.Fatalf("cursor item not marked: %q\n%s", got, stripANSI(m.vp.View()))
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")}, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.chatCursor != items-2-1-chatPage {
		t.Fatalf("k + pgup: cursor %d", m.chatCursor)
	}
	// The cursor item is scrolled into view.
	r := m.itemRows[m.chatCursor]
	if r.first < m.vp.YOffset || r.last >= m.vp.YOffset+m.vp.Height {
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
	if n := strings.Count(view(), "out"); n != previewLines-1 {
		t.Fatalf("preview before enter (%d 'out' lines):\n%s", n, view())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.expanded["a"][items-1] || strings.Count(view(), "out") != 8 {
		t.Fatalf("expanded after enter (%v):\n%s", m.expanded["a"], view())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.expanded["a"][items-1] || strings.Count(view(), "out") != previewLines-1 {
		t.Fatalf("preview after second enter:\n%s", view())
	}
	// Expansion is per visit: expand, move away, come back → preview again.
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if strings.Count(view(), "out") != 8 {
		t.Fatalf("expanded again:\n%s", view())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	if len(m.expanded["a"]) != 0 {
		t.Fatalf("moving away should drop the expansion: %v", m.expanded["a"])
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown})
	if n := strings.Count(view(), "out"); n != previewLines-1 {
		t.Fatalf("back on the item it should be the preview (%d):\n%s", n, view())
	}
	// Enter on a non-tool item is inert.
	press(&m, tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.expanded["a"]) != 0 {
		t.Fatalf("enter on a user item changed overrides: %v", m.expanded["a"])
	}
	// Leaving the chat with an item expanded folds it for next time.
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyEnter})
	if strings.Count(view(), "out") != 8 {
		t.Fatalf("expanded before leaving:\n%s", view())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc}, tea.KeyMsg{Type: tea.KeyTab}) // esc → input; tab wraps to the chat
	if m.focus != focusChat || len(m.expanded["a"]) != 0 || strings.Count(view(), "out") != previewLines-1 {
		t.Fatalf("re-entering the chat should show the preview: focus=%v %v\n%s", m.focus, m.expanded["a"], view())
	}

	// Leaving the chat resumes following and drops the marker.
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusInput || !m.follow || !m.vp.AtBottom() || strings.Contains(stripANSI(m.vp.View()), gutterMark) {
		t.Fatalf("leave chat: focus=%v follow=%v bottom=%v", m.focus, m.follow, m.vp.AtBottom())
	}
	if hs := m.keyHints(); hs[4].key != "tab" || hs[4].desc != "next section" {
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
	m.width, m.height = 120, 40
	m.reconciled = true
	m.agents = []protocol.AgentInfo{
		{ID: "root", Label: "coder", Archetype: "coder", State: "idle"},
		{ID: "c1", Parent: "root", Label: "scout", Archetype: "explorer", State: "running"},
		{ID: "c2", Parent: "root", Label: "checks", Archetype: "tester", State: "idle"},
	}
	m.prompts = []protocol.PromptInfo{{ID: "p1", Kind: "permission", Tool: "bash", Agent: "root", Input: []byte(`{"command":"make test"}`)}}

	// unfocused: one strip line, counts only
	sv := stripANSI(m.sectionsView(100))
	if strings.Count(sv, "\n") != 0 || !strings.Contains(sv, "agents (2)") || !strings.Contains(sv, "async (0)") || !strings.Contains(sv, "permission (1)") ||
		strings.Contains(sv, "scout") || strings.Contains(sv, "checks") || strings.Contains(sv, "make test") || strings.Contains(sv, "tab to") {
		t.Fatalf("collapsed strip should only count:\n%s", sv)
	}

	// tab order: chat is skipped on the home view; the strip, input, meta row
	order := m.focusOrder()
	if len(order) != 3 || order[0] != focusMeta || order[1] != focusTabs || order[2] != focusInput {
		t.Fatalf("order %v", order)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyTab}, tea.KeyMsg{Type: tea.KeyTab}) // input → meta row → permission (a prompt waits)
	if sv := stripANSI(m.sectionsView(100)); m.focus != focusPermission || strings.Count(sv, "\n") != 0 || strings.Contains(sv, "make test") {
		t.Fatalf("the strip should stay one line with the permission open: focus=%v\n%s", m.focus, sv)
	}
	// the dialog: the tab labels as its title, then the tool row over its command
	if body := strings.Join(m.tabBodyLines(60), "\n"); !strings.Contains(stripANSI(body), "$ Bash · coder\n       make test") || strings.Contains(body, "{") {
		t.Fatalf("permission should open as a tool row over its command:\n%s", stripANSI(body))
	}
	dv := stripANSI(m.tabDialog(100))
	if !strings.HasPrefix(dv, "╭") || !strings.Contains(dv, "permission (1) · agents (2) · async (0)") || !strings.Contains(dv, "make test") {
		t.Fatalf("permission dialog:\n%s", dv)
	}
	// the dialog is composited into the full view (here over the home screen)
	if full := stripANSI(m.View()); !strings.Contains(full, "make test") || !strings.Contains(full, "╭") {
		t.Fatalf("the dialog should render in the view:\n%s", full)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRight}) // permission → agents
	if m.focus != focusAgents {
		t.Fatalf("focus %v", m.focus)
	}
	if dv := stripANSI(m.tabDialog(100)); !strings.Contains(dv, "▸") || !strings.Contains(dv, "scout") || !strings.Contains(dv, "checks") {
		t.Fatalf("agents dialog:\n%s", dv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRight}) // agents → async
	if m.focus != focusAsync || !strings.Contains(stripANSI(m.tabDialog(100)), "no async jobs running") {
		t.Fatalf("async: focus=%v\n%s", m.focus, stripANSI(m.tabDialog(100)))
	}
	press(&m, tea.KeyMsg{Type: tea.KeyLeft}) // async → agents for the selection test
	if m.focus != focusAgents {
		t.Fatalf("focus %v", m.focus)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown})
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.selectedID() != "c2" || m.focus != focusInput {
		t.Fatalf("enter should select the child under the cursor: %s focus %v", m.selectedID(), m.focus)
	}
	// now the selected agent has no children: the tab stays, reading (0)
	if v := stripANSI(m.sectionsView(100)); !strings.Contains(v, "agents (0)") {
		t.Fatalf("empty agents tab:\n%s", v)
	}
}

func TestSectionTabStrip(t *testing.T) {
	m := sessionModel()
	m.agents = []protocol.AgentInfo{
		{ID: "root", Label: "coder", Archetype: "coder", State: "working", Monitors: []protocol.MonitorInfo{{ID: "j1", Kind: "command", Label: "go test", State: "running"}}},
		{ID: "c1", Parent: "root", Label: "scout", Archetype: "explorer", State: "working"},
	}
	m.selected = 0
	m.prompts = []protocol.PromptInfo{{ID: "p1", Kind: "permission", Tool: "bash", Agent: "root", Input: []byte(`{"command":"make test"}`)}}

	// unfocused: all three titles on one line, counts only
	v := stripANSI(m.sectionsView(100))
	if strings.Count(v, "\n") != 0 || !strings.Contains(v, "permission (1) · agents (1) · async (1)") ||
		strings.Contains(v, "scout") || strings.Contains(v, "go test") || strings.Contains(v, "make test") {
		t.Fatalf("tab strip:\n%s", v)
	}
	// agents focused: the strip is unchanged; the dialog titles itself with
	// the same labels, then its rows
	m.focus = focusAgents
	if sv := stripANSI(m.sectionsView(100)); strings.Count(sv, "\n") != 0 || strings.Contains(sv, "scout") {
		t.Fatalf("strip with agents focused:\n%s", sv)
	}
	v = stripANSI(m.tabDialog(100))
	lines := strings.Split(v, "\n")
	// border, title, rule, one row, border
	if len(lines) != 5 || !strings.Contains(lines[1], "permission (1) · agents (1) · async (1)") || !strings.Contains(lines[3], "▸") || !strings.Contains(lines[3], "scout") {
		t.Fatalf("agents dialog:\n%s", v)
	}
	// async focused: the job row
	m.focus = focusAsync
	v = stripANSI(m.tabDialog(100))
	lines = strings.Split(v, "\n")
	if len(lines) != 5 || !strings.Contains(lines[1], "async (1)") || !strings.Contains(lines[3], "▸") || !strings.Contains(lines[3], "go test") || strings.Contains(v, "scout") {
		t.Fatalf("async dialog:\n%s", v)
	}
	// permission focused: the tool row over its command
	m.focus = focusPermission
	v = stripANSI(m.tabDialog(100))
	lines = strings.Split(v, "\n")
	if len(lines) != 6 || !strings.Contains(lines[1], "permission (1)") || !strings.Contains(lines[3], "Bash") || !strings.Contains(lines[4], "       make test") || strings.Contains(v, "scout") {
		t.Fatalf("permission dialog:\n%s", v)
	}
	// no prompt: the tab stays with a zero count and the generic hint
	m.prompts = nil
	m.focus = focusInput
	if v := stripANSI(m.sectionsView(100)); !strings.Contains(v, "permission (0)") || strings.Contains(v, "tab to") {
		t.Fatalf("empty permission tab:\n%s", v)
	}
}

func TestSessionViewFillsHeight(t *testing.T) {
	m := sessionModel()
	m.showTree = false
	for _, f := range []focus{focusInput, focusAgents, focusAsync, focusPermission} {
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
			if strings.HasPrefix(l, "permission (") {
				si = i
			}
		}
		if si < 2 || !strings.HasPrefix(lines[si-2], "─") || !strings.HasPrefix(lines[si-1], "coder ·") {
			t.Fatalf("focus %v: under the rule come the meta row, then the strip:\n%s", f, stripANSI(v))
		}
	}
}

func TestPermissionShowsWholeCommand(t *testing.T) {
	m := sessionModel()
	long := "for f in $(ls /very/long/path/to/somewhere/deep/in/the/tree); do echo processing \"$f\" && sleep 1 && rm -f \"$f\".bak; done"
	m.prompts = []protocol.PromptInfo{{ID: "p1", Kind: "permission", Tool: "bash", Agent: "a", Input: []byte(`{"command":` + strconv.Quote(long+"\necho second line") + `}`)}}
	m.focus = focusPermission
	v := stripANSI(strings.Join(m.tabBodyLines(52), "\n"))
	// every line fits the width, nothing is elided, and the newline is kept
	for _, l := range strings.Split(v, "\n") {
		if ansi.StringWidth(l) > 52 || strings.Contains(l, "…") {
			t.Fatalf("line %q too wide or elided:\n%s", l, v)
		}
	}
	joined := strings.ReplaceAll(strings.ReplaceAll(v, "\n       ", ""), "\n", "")
	for _, want := range []string{"rm -f \"$f\".bak; done", "echo second line"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
	if !strings.Contains(v, "\n       echo second line") {
		t.Fatalf("newline in the command should start a new row:\n%s", v)
	}
}

func TestMetaRowAndStripRepo(t *testing.T) {
	m := sessionModel()
	m.showTree = false
	m.hideKeys = false // the key bar is off by default; this test checks the row above it
	m.session.Dir = "/repo/project"
	m.session.Model, m.agents[0].Model = "chatgpt/gpt-5", "chatgpt/gpt-5"
	m.agents[0].Tokens, m.agents[0].CostUSD = 1500, 0.02
	m.width, m.height = 100, 30
	m.layout()
	lines := strings.Split(stripANSI(m.View()), "\n")
	meta, strip := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "coder · "):
			meta = i
		case strings.HasPrefix(l, "permission ("):
			strip = i
		}
	}
	if meta < 0 || strip < 0 {
		t.Fatalf("no meta row or strip:\n%s", strings.Join(lines, "\n"))
	}
	// role and model on the left, tokens and cost on the right, no dots
	if row := lines[meta]; !strings.HasSuffix(row, "2k tokens · $0.02") || strings.Contains(row, "/repo/project") || ansi.StringWidth(row) > 100 {
		t.Fatalf("meta row: %q", row)
	}
	if meta < 1 || !strings.HasPrefix(lines[meta-1], "─") || strip != meta+1 {
		t.Fatalf("the meta row should sit under the rule with the strip below it:\n%s", strings.Join(lines, "\n"))
	}
	// the repo sits at the right edge of the tab strip
	if row := lines[strip]; !strings.HasSuffix(row, "/repo/project") || ansi.StringWidth(row) != 100 {
		t.Fatalf("strip: %q", row)
	}
	if strings.Count(strings.Join(lines, "\n"), "/repo/project") != 1 {
		t.Fatal("the repo should appear once")
	}
}

func TestTurnIndicatorFollowsPrompts(t *testing.T) {
	m := sessionModel()
	m.showTree = false
	m.transcript("a").Apply(event.Event{Seq: 1, Agent: "a", Type: event.TurnStarted, Time: time.Now(), Payload: event.MustPayload(event.TurnPayload{Turn: 1})})
	m.refreshViewport()
	if v := stripANSI(m.View()); !strings.Contains(v, turnVerbs[0]+"…") || strings.Contains(v, "permission requested") {
		t.Fatalf("mid-turn:\n%s", v)
	}
	m.applyPromptNotification(protocol.PromptNotification{Action: "requested", Prompt: protocol.PromptInfo{ID: "p", Kind: "permission", Agent: "a", Tool: "bash"}})
	if v := stripANSI(m.View()); !strings.Contains(v, "! permission requested") || strings.Contains(v, turnVerbs[0]+"…") {
		t.Fatalf("waiting on a permission:\n%s", v)
	}
	// a prompt for another agent does not change the selected agent's indicator
	m.applyPromptNotification(protocol.PromptNotification{Action: "answered", Prompt: protocol.PromptInfo{ID: "p", Agent: "a"}})
	m.applyPromptNotification(protocol.PromptNotification{Action: "requested", Prompt: protocol.PromptInfo{ID: "q", Kind: "permission", Agent: "b", Tool: "bash"}})
	if v := stripANSI(m.View()); !strings.Contains(v, turnVerbs[0]+"…") || strings.Contains(v, "permission requested") {
		t.Fatalf("after the answer:\n%s", v)
	}
}

func TestFirstPermissionOpensItsTabWhenIdle(t *testing.T) {
	req := func(id string) protocol.PromptNotification {
		return protocol.PromptNotification{Action: "requested", Prompt: protocol.PromptInfo{ID: id, Kind: "permission", Agent: "a", Tool: "bash"}}
	}
	// idle input: the first prompt opens the permission tab
	m := sessionModel()
	m.applyPromptNotification(req("p1"))
	if m.focus != focusPermission {
		t.Fatalf("idle input should jump to the permission tab: focus=%v", m.focus)
	}
	// a second prompt behind a pending one changes nothing
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	m.applyPromptNotification(req("p2"))
	if m.focus != focusInput {
		t.Fatalf("second prompt should not steal focus: %v", m.focus)
	}
	// a draft in the input is never interrupted
	m = sessionModel()
	m.input.SetValue("half a thou")
	m.applyPromptNotification(req("p3"))
	if m.focus != focusInput {
		t.Fatalf("typing should keep focus: %v", m.focus)
	}
	// nor is another section
	m = sessionModel()
	m.setFocus(focusChat)
	m.applyPromptNotification(req("p4"))
	if m.focus != focusChat {
		t.Fatalf("chat focus should stay: %v", m.focus)
	}
}

func TestParentAgentCreateLineFollowsChildEvents(t *testing.T) {
	m := sessionModel()
	m.agents = []protocol.AgentInfo{{ID: "a", Label: "main", Archetype: "coder"}}
	m.selected = 0
	ev := func(seq int64, agent string, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Session: "s", Agent: agent, Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	m.applyEvent(ev(1, "a", event.TurnStarted, event.TurnPayload{Turn: 1}))
	m.applyEvent(ev(2, "a", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "agent_create", Input: json.RawMessage(`{"archetype":"explorer","label":"scout","task":"look"}`)}))
	m.applyEvent(ev(3, "c1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "c1", Parent: "a", Archetype: "explorer", Label: "scout", Model: "fake/m1", Depth: 1}))
	m.applyEvent(ev(4, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "agent_create", Output: "spawned scout (explorer) as c1"}))
	tone := func() Tone {
		for _, l := range m.transcript("a").All() {
			if l.Kind == LineTool && l.tool == "agent_create" {
				return l.Tone
			}
		}
		t.Fatal("no agent_create line in the parent's chat")
		return ToneNone
	}
	if tone() != ToneWorking {
		t.Fatalf("child running: tone %v", tone())
	}
	m.applyEvent(ev(5, "c1", event.TurnStarted, event.TurnPayload{Turn: 1}))
	if tone() != ToneWorking {
		t.Fatalf("child mid-turn: tone %v", tone())
	}
	m.applyEvent(ev(6, "c1", event.TurnEnded, event.TurnEndedPayload{Turn: 1, Reason: "end_turn"}))
	if tone() != ToneNone {
		t.Fatalf("child idle after answering: tone %v", tone())
	}
	m.applyEvent(ev(7, "c1", event.AgentKilled, event.AgentRefPayload{ID: "c1"}))
	if tone() != ToneError {
		t.Fatalf("child killed: tone %v", tone())
	}
}

func TestLastSnippet(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	if lastSnippet(nil) != "" || lastSnippet(tr) != "" {
		t.Fatal("empty")
	}
	tr.Apply(mk(1, event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "look around"}))
	if got := lastSnippet(tr); got != "look around" {
		t.Fatalf("prompt: %q", got)
	}
	tr.Apply(mk(2, event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls -la"}`)}))
	if got := lastSnippet(tr); got != "Bash  ls -la" {
		t.Fatalf("tool: %q", got)
	}
	tr.Apply(mk(3, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "Found **three** files.\n"}}}))
	if got := lastSnippet(tr); got != "Found **three** files." {
		t.Fatalf("text (trailing blank skipped): %q", got)
	}
	// a multi-line message reads from its first line, not its last
	tr.Apply(mk(4, event.AssistantMessage, event.AssistantMessagePayload{Turn: 2, Blocks: []model.Block{{Type: model.BlockText, Text: "First the plan.\nThen the details.\nFinally the caveat."}}}))
	if got := lastSnippet(tr); !strings.HasPrefix(got, "First the plan. Then the details.") {
		t.Fatalf("multi-line message should start at its beginning: %q", got)
	}
	// a tool call with output: the call line comes first
	tr.Apply(mk(5, event.ToolCallStarted, event.ToolStartedPayload{CallID: "c2", Name: "bash", Input: json.RawMessage(`{"command":"go test"}`)}))
	tr.Apply(mk(6, event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c2", Name: "bash", Output: "ok\nPASS"}))
	if got := lastSnippet(tr); !strings.HasPrefix(got, "Bash  go test") {
		t.Fatalf("tool item should start with the call: %q", got)
	}
}

func TestHelpTogglesKeyBar(t *testing.T) {
	m := sessionModel()
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
	m := sessionModel()
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

func TestCtrlCTwiceQuits(t *testing.T) {
	m := sessionModel()
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

func TestSessionItemAndBind(t *testing.T) {
	created := time.Now().Add(-3 * time.Hour).Format(time.RFC3339)
	it := sessionItem(protocol.SessionInfo{ID: "s1", Title: "fix the login bug", Created: created, Model: "openai/gpt-5", CostUSD: 0.12, Live: 2}, false)
	if it.label != "fix the login bug" || !strings.HasPrefix(it.hint, "3h00m ago · openai/gpt-5 · $0.12 · 2 live") || it.good {
		t.Fatalf("item: %+v", it)
	}
	it = sessionItem(protocol.SessionInfo{ID: "s2", Created: created}, true)
	if it.label != "(empty session)" || !strings.HasSuffix(it.hint, "current") || !it.good {
		t.Fatalf("current empty item: %+v", it)
	}

	// binding another session starts every per-session piece of state over
	m := sessionModel()
	m.prompts = []protocol.PromptInfo{{ID: "p", Kind: "permission", Agent: "a"}}
	m.chatCursor = 3
	m.seq = 42
	m.reconciled = true
	m.bindSession(protocol.SessionInfo{ID: "other", Dir: "/x"})
	if m.sessionID != "other" || m.session.Dir != "/x" || len(m.agents) != 0 || len(m.transcripts) != 0 || m.seq != 0 || len(m.prompts) != 0 || m.chatCursor != 0 || m.reconciled || m.focus != focusInput {
		t.Fatalf("state after bind: id=%s agents=%d transcripts=%d seq=%d prompts=%d cursor=%d reconciled=%v focus=%v", m.sessionID, len(m.agents), len(m.transcripts), m.seq, len(m.prompts), m.chatCursor, m.reconciled, m.focus)
	}
	if !m.isHome() {
		t.Fatal("a freshly bound session shows the home screen until its history replays")
	}
}

func TestSessionsPickerSkipsEmptySessions(t *testing.T) {
	m := sessionModel()
	m.sessionID = "cur"
	m.onSessions(sessionsMsg{sessions: []protocol.SessionInfo{
		{ID: "cur", Created: time.Now().Format(time.RFC3339)},
		{ID: "empty", Created: time.Now().Format(time.RFC3339)},
		{ID: "old", Title: "fix the login bug", Created: time.Now().Format(time.RFC3339)},
	}})
	if m.ov == nil || m.ov.kind != ovSessions {
		t.Fatal("picker should open")
	}
	var ids []string
	for _, it := range m.ov.items {
		ids = append(ids, it.id)
	}
	if strings.Join(ids, " ") != "cur old" {
		t.Fatalf("picker rows %v: the untouched session should be left out, the current one kept", ids)
	}
}

func TestMouseHoverMovesChatCursor(t *testing.T) {
	m := sessionModel()
	m.showTree = false
	tr := m.transcript("a")
	for i := 0; i < 6; i++ {
		tr.Apply(mk(int64(i+2), "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: fmt.Sprintf("prompt %d", i)}))
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
	move(5, r.first-m.vp.YOffset)
	if m.focus != focusChat || m.chatCursor != 1 || !m.hoverFocus {
		t.Fatalf("hover should focus the chat on item 1: focus=%v cursor=%d hover=%v", m.focus, m.chatCursor, m.hoverFocus)
	}
	// the row is highlighted like an arrow-key visit
	markCursorForTest(t)
	m.refreshViewport()
	if !strings.Contains(stripANSI(m.vp.View()), gutterMark+"› prompt 0") {
		t.Fatalf("hovered item should carry the cursor:\n%s", stripANSI(m.vp.View()))
	}
	// hovering another item moves the cursor
	r2 := m.itemRows[items-1]
	move(5, r2.first-m.vp.YOffset)
	if m.chatCursor != items-1 {
		t.Fatalf("cursor should follow the mouse: %d", m.chatCursor)
	}
	// leaving the chat area hands focus back to the input
	move(5, m.vp.Height+2)
	if m.focus != focusInput || m.hoverFocus || !m.input.Focused() {
		t.Fatalf("leaving should restore the input: focus=%v hover=%v", m.focus, m.hoverFocus)
	}
	// keyboard focus is not dropped by the mouse leaving
	press(&m, tea.KeyMsg{Type: tea.KeyTab}) // input → chat
	move(5, m.vp.Height+2)
	if m.focus != focusChat {
		t.Fatalf("keyboard chat focus should survive mouse movement: %v", m.focus)
	}
	// a tab dialog owns hover: the chat behind it is left alone
	m.setFocus(focusInput)
	m.setFocus(focusPermission)
	move(5, r.first-m.vp.YOffset)
	if m.focus != focusPermission || m.hoverFocus {
		t.Fatalf("hover must not take focus from an open tab dialog: %v", m.focus)
	}
}

func TestMouseClickTogglesItem(t *testing.T) {
	m := sessionModel()
	m.showTree = false
	tr := m.transcript("a")
	tr.Apply(mk(2, "a", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}))
	tr.Apply(mk(3, "a", event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c1", Name: "bash", Output: strings.TrimRight(strings.Repeat("out\n", 8), "\n")}))
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
	click(r.first - m.vp.YOffset)
	if m.focus != focusChat || m.chatCursor != item || !m.expanded["a"][item] || strings.Count(view(), "out") != 8 {
		t.Fatalf("click should select and expand: focus=%v cursor=%d expanded=%v\n%s", m.focus, m.chatCursor, m.expanded["a"], view())
	}
	click(r.first - m.vp.YOffset)
	if m.expanded["a"][item] || strings.Count(view(), "out") != previewLines-1 {
		t.Fatalf("second click should collapse to the preview:\n%s", view())
	}
	// a click on a user item is inert beyond selecting it
	click(m.itemRows[0].first - m.vp.YOffset)
	if m.chatCursor != 0 || len(m.expanded["a"]) != 0 {
		t.Fatalf("click on a non-tool item: cursor=%d expanded=%v", m.chatCursor, m.expanded["a"])
	}
}

func TestMouseClicksFocusTabsAndInput(t *testing.T) {
	m := sessionModel()
	m.showTree = false
	m.agents = []protocol.AgentInfo{
		{ID: "a", Label: "main", Archetype: "coder", State: "working"},
		{ID: "c1", Parent: "a", Label: "scout", Archetype: "explorer", State: "working"},
		{ID: "c2", Parent: "a", Label: "checks", Archetype: "tester", State: "idle"},
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
	// the strip sits right under the rule; "agents (2)" starts after "permission (0)" + " · "
	agentsX := len("permission (0)") + 3 + 1
	click(agentsX, lay.strip)
	if m.focus != focusAgents {
		t.Fatalf("clicking the agents label should open the agents tab: %v", m.focus)
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
	if m.agCursor != 1 || m.focus != focusAgents {
		t.Fatalf("hovering the second agent row should move the cursor: %d focus=%v", m.agCursor, m.focus)
	}
	click(x, y)
	if m.selectedID() != "c2" || m.focus != focusInput {
		t.Fatalf("clicking an agent row should select it: %s focus=%v", m.selectedID(), m.focus)
	}
	m.selected = 0
	// a tab label on the dialog's title switches tabs
	click(agentsX, lay.strip)
	x, y = rowAt("async (0)")
	click(x, y)
	if m.focus != focusAsync {
		t.Fatalf("clicking a label on the dialog title should switch tabs: %v", m.focus)
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
	move(3, r.first-m.vp.YOffset)
	if m.focus != focusPermission || m.hoverFocus {
		t.Fatalf("hover must not take focus from the dialog: focus=%v hover=%v", m.focus, m.hoverFocus)
	}
}

func TestMetaRowHits(t *testing.T) {
	m := sessionModel()
	m.agents = []protocol.AgentInfo{{ID: "a", Label: "main", Archetype: "coder", Model: "openai/gpt-5", Variant: "high"}}
	m.selected = 0
	m.session.Yolo = true
	// "YOLO · main (coder) · openai/gpt-5 · high"
	row := stripANSI(metaLine("main", "coder", "openai/gpt-5", "high", 0, true, metaNone))
	at := func(sub string) int { return ansi.StringWidth(row[:strings.Index(row, sub)]) + 1 } // a column, not a byte offset
	for _, c := range []struct {
		x    int
		want metaPart
	}{
		{at("YOLO"), metaYolo}, {at("main"), metaRole}, {at("(coder)"), metaRole},
		{at("gpt-5"), metaModel}, {at("high"), metaVariant}, {len(row) + 5, metaNone},
	} {
		if got := m.metaHit(c.x); got != c.want {
			t.Fatalf("x=%d: got %v want %v", c.x, got, c.want)
		}
	}
	// without yolo the row starts at the name; a missing variant reads "default"
	m.session.Yolo = false
	m.agents[0].Variant = ""
	row = stripANSI(metaLine("main", "coder", "openai/gpt-5", "", 0, false, metaNone))
	if m.metaHit(0) != metaRole || m.metaHit(ansi.StringWidth(row[:strings.Index(row, "default")])+2) != metaVariant {
		t.Fatalf("no-yolo row: %q", row)
	}
}

func TestDialogRowsTakeTheMouse(t *testing.T) {
	m := sessionModel()
	m.width, m.height = 100, 40
	m.layout()
	o := newOverlay(ovVariants, overlayList, "Variant", "hint")
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

func TestYoloToggleShowsInEveryChat(t *testing.T) {
	m := sessionModel()
	m.agents = []protocol.AgentInfo{{ID: "a", Label: "main"}, {ID: "b", Parent: "a", Label: "scout"}}
	m.applyEvent(event.Event{Seq: 9, Session: "s", Type: event.SessionYoloChanged, Time: time.Now(), Payload: event.MustPayload(event.YoloPayload{On: true})})
	for _, id := range []string{"a", "b"} {
		found := false
		for _, l := range m.transcript(id).All() {
			if strings.Contains(l.Text, "yolo → on") {
				found = true
			}
		}
		if !found {
			t.Fatalf("agent %s chat lacks the yolo line", id)
		}
	}
	if !m.session.Yolo {
		t.Fatal("session flag should follow the event")
	}
}

func TestDragSelectsAndCopies(t *testing.T) {
	m := sessionModel()
	m.showTree = false
	tr := m.transcript("a")
	tr.Apply(mk(2, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "first line here"}))
	tr.Apply(mk(3, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "second line"}))
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
	m := sessionModel()
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
	m.input.SetValue(strings.Repeat("line\n", 30))
	m.layout()
	if m.inputRows() != inputMaxLines {
		t.Fatalf("cap: %d", m.inputRows())
	}
	// back to one line when cleared
	m.input.Reset()
	m.layout()
	if m.inputRows() != 1 || m.vp.Height != one {
		t.Fatalf("cleared: height %d viewport %d", m.inputRows(), m.vp.Height)
	}
}

func TestInputNewlineAndHistoryKeys(t *testing.T) {
	m := sessionModel()
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
	m := sessionModel()
	m.width, m.height = 60, 30
	m.input.SetValue("first line\nsecond line\nthird")
	m.layout()
	v := stripANSI(m.inputView())
	if strings.Count(v, "›") != 1 || !strings.HasPrefix(v, "› first line") {
		t.Fatalf("one chevron on the first line only:\n%s", v)
	}
	for i, l := range strings.Split(v, "\n")[1:] {
		if !strings.HasPrefix(l, "  ") {
			t.Fatalf("continuation line %d should be indented under the chevron: %q", i+1, l)
		}
	}
}

func TestPastedMessageKeepsItsFirstLineInView(t *testing.T) {
	m := sessionModel()
	m.width, m.height = 80, 30
	m.layout()
	press(&m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Use subagents to summarize the repo.\nThen compare all responses and\ngive me the highlights."), Paste: true})
	if m.inputRows() != 3 {
		t.Fatalf("three pasted lines should give a three-line input: %d", m.inputRows())
	}
	v := stripANSI(m.inputView())
	lines := strings.Split(v, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "› Use subagents") || !strings.HasPrefix(lines[2], "  give me the highlights.") {
		t.Fatalf("the whole message should be visible from its first line:\n%s", v)
	}
}

func TestPasteWithCarriageReturns(t *testing.T) {
	m := sessionModel()
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
	m := sessionModel()
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
		if !strings.HasPrefix(rows[0], "› "+head) {
			t.Fatalf("first row hidden for %q (height %d):\n%s", head, m.inputRows(), view)
		}
		if m.inputRows() > inputMaxLines {
			t.Fatalf("over the cap: %d", m.inputRows())
		}
		// the end of the message is on screen too (unless capped)
		last := []rune(strings.TrimRight(text, " "))
		tail := string(last[max(0, len(last)-5):])
		if m.inputRows() < inputMaxLines && !strings.Contains(view, strings.TrimSpace(tail)) {
			t.Fatalf("last row hidden for tail %q (height %d):\n%s", tail, m.inputRows(), view)
		}
	}
}

func TestStartScreenHistoryComesFromEarlierSessions(t *testing.T) {
	m := newModel(context.Background(), nil, "cur")
	m.width, m.height = 100, 40
	m.reconciled = true
	m.layout()
	now := time.Now()
	nm, _ := m.Update(sessionsMsg{quiet: true, sessions: []protocol.SessionInfo{
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
	if m.input.Value() != "fix the login bug" {
		t.Fatalf("↑ should recall the most recent earlier session's first prompt: %q", m.input.Value())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "add a README section" {
		t.Fatalf("↑↑ should recall the one before: %q", m.input.Value())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyUp}) // oldest was a duplicate title: nothing older
	if m.input.Value() != "add a README section" {
		t.Fatalf("duplicates are collapsed: %q", m.input.Value())
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown})
	if m.input.Value() != "" {
		t.Fatalf("↓ past the newest restores the empty draft: %q", m.input.Value())
	}
	// a session that already has its own history is left alone
	m.history = []string{"typed here"}
	m.histIdx = 1
	m.seedHistory([]protocol.SessionInfo{{ID: "x", Title: "elsewhere"}})
	if len(m.history) != 1 {
		t.Fatal("seeding must not touch an existing history")
	}
}

func TestStatusShowsAboveTheDivider(t *testing.T) {
	m := sessionModel()
	m.showTree = false
	m.width, m.height = 80, 30
	m.setStatus("copied 12 characters", false)
	m.layout()
	lines := strings.Split(stripANSI(m.View()), "\n")
	rule := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "─") {
			rule = i
			break
		}
	}
	if rule < 1 || !strings.HasPrefix(lines[rule-1], "copied 12 characters") {
		t.Fatalf("the status should sit left-aligned right above the rule:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(lines[rule+1], "copied") {
		t.Fatal("the meta row no longer carries the status")
	}
	// and the line is blank without a status
	m.status = ""
	m.layout()
	lines = strings.Split(stripANSI(m.View()), "\n")
	if strings.TrimSpace(lines[rule-1]) != "" {
		t.Fatalf("no status → blank line: %q", lines[rule-1])
	}
}

func TestSidebarOnTheLeftAndMouseOffsets(t *testing.T) {
	m := sessionModel()
	m.showTree = true
	m.width, m.height = 120, 40
	m.layout()
	m.refreshViewport()
	lines := strings.Split(stripANSI(m.View()), "\n")
	// the sidebar's session header is at the left edge, the chat to its right
	found := false
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimLeft(l, " "), "session") && strings.Index(l, "session") < sidebarWidth {
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
	if rule < 0 || ansi.StringWidth(lines[rule]) != m.width || !strings.HasPrefix(lines[rule+1], "coder ·") {
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
	ev(tea.MouseMsg{X: 2, Y: 3, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	ev(tea.MouseMsg{X: 2, Y: 3, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft})
	if m.focus != focusSidebar {
		t.Fatalf("click in the sidebar: focus=%v", m.focus)
	}
	// hovering the chat, right of the sidebar, still selects an item
	m.setFocus(focusInput)
	r := m.itemRows[0]
	ev(tea.MouseMsg{X: sidebarWidth + 1 + 3, Y: r.first - m.vp.YOffset, Action: tea.MouseActionMotion})
	if m.focus != focusChat || m.chatCursor != 0 {
		t.Fatalf("hover over the chat with the sidebar open: focus=%v cursor=%d", m.focus, m.chatCursor)
	}
	// hovering over the sidebar hands focus back
	ev(tea.MouseMsg{X: 2, Y: r.first - m.vp.YOffset, Action: tea.MouseActionMotion})
	if m.focus != focusInput {
		t.Fatalf("hover over the sidebar should release the chat: %v", m.focus)
	}
}

func TestSidebarRowsLeaveOneColumn(t *testing.T) {
	m := sessionModel()
	m.showTree = true
	m.width, m.height = 120, 40
	m.agents = []protocol.AgentInfo{{ID: "a", Label: "a-very-long-agent-label-that-will-not-fit-here", Archetype: "general", State: "idle"}}
	m.selected = 0
	m.layout()
	for _, row := range m.treeRows(sidebarWidth - 1) {
		if w := ansi.StringWidth(stripANSI(row)); w != sidebarWidth-1 {
			t.Fatalf("a truncated row should be %d wide, got %d: %q", sidebarWidth-1, w, stripANSI(row))
		}
	}
}
