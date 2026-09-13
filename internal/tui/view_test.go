package tui

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicodes/stavlos/internal/event"
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
	if got != "Get started /provider" {
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
	if got := stripANSI(metaLine("coder", "anthropic/claude-opus-5", "", 0, false)); got != "Coder · claude-opus-5 anthropic · default" {
		t.Fatalf("with model: %q", got)
	}
	if got := stripANSI(metaLine("coder", "", "", 0, false)); got != "Coder · no model — /models" {
		t.Fatalf("no model: %q", got)
	}
	if got := stripANSI(metaLine("scout", "ollama/llama3", "", 2, false)); got != "Scout · llama3 ollama · default · 2 queued" {
		t.Fatalf("queued: %q", got)
	}
	if got := stripANSI(metaLine("coder", "openai/gpt-5", "high", 0, false)); got != "Coder · gpt-5 openai · high" {
		t.Fatalf("variant: %q", got)
	}
	if got := stripANSI(metaLine("coder", "openai/gpt-5", "", 0, true)); got != "YOLO · Coder · gpt-5 openai · default" {
		t.Fatalf("yolo: %q", got)
	}
}

func TestInputBoxAndPromptWidth(t *testing.T) {
	box := stripANSI(inputBox("› hi", "Coder · x"))
	if box != "› hi\nCoder · x" {
		t.Fatalf("input box: %q", box)
	}

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
	if !strings.Contains(plain, "▀") || !strings.Contains(plain, "/provider   sign in with ChatGPT or Grok") || !strings.Contains(plain, "Get started /provider") {
		t.Fatalf("home view:\n%s", plain)
	}
	if strings.Contains(plain, "session ") {
		t.Fatal("home view must not show the sidebar")
	}
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
		{ID: "c3", Parent: "root", Label: "done", Archetype: "explorer", State: "finished"},
		{ID: "g1", Parent: "c1", Label: "grandchild", Archetype: "explorer", State: "running"},
	}
	rows := agentRows(agents, "root", spawned, now, 100)
	if len(rows) != 2 {
		t.Fatalf("rows %d: %q", len(rows), rows)
	}
	if !strings.Contains(rows[0], "scout (explorer)") || !strings.Contains(rows[0], "1m15s") || !strings.Contains(rows[0], "⑂") {
		t.Fatalf("%q", rows[0])
	}
	if strings.Contains(rows[0], "wakes parent") {
		t.Fatalf("armed marker: %q", rows)
	}
	if !strings.Contains(rows[1], "tester") || !strings.Contains(rows[1], "3s") || strings.Contains(rows[1], "turn") {
		t.Fatalf("%q", rows[1])
	}
	if rows := agentRows(agents, "c2", spawned, now, 100); len(rows) != 0 {
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
	view = stripANSI(m.sectionsView(100))
	if !strings.Contains(view, "scout") || strings.Contains(view, "grandchild") {
		t.Fatalf("agents tab:\n%s", view)
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
	rows := monitorRows(monitors, "coder", now, 100)
	if len(rows) != 3 {
		t.Fatalf("rows %d: %q", len(rows), rows)
	}
	plain := make([]string, len(rows))
	for i, r := range rows {
		plain[i] = stripANSI(r)
	}
	// command: glyph, bold label, kind, progress, elapsed
	if !strings.HasPrefix(plain[0], "  ⚙") || !strings.Contains(plain[0], "go test (coder)") {
		t.Fatalf("command row: %q", plain[0])
	}
	for _, want := range []string{"42 lines", "1m15s"} {
		if !strings.Contains(plain[0], want) {
			t.Fatalf("command row lacks %q: %q", want, plain[0])
		}
	}
	// a second running job: no wake tag, elapsed
	if !strings.HasPrefix(plain[1], "  ⚙  src changes") || !strings.HasSuffix(strings.TrimRight(plain[1], " "), "src changes (coder)  3s") {
		t.Fatalf("second job row: %q", plain[1])
	}
	// progress and hours elapsed
	if !strings.HasPrefix(plain[2], "  ⚙  cooldown") || !strings.Contains(plain[2], "3m left · 2h00m") {
		t.Fatalf("third job row: %q", plain[2])
	}
	// a bad Started stamp just drops the elapsed field
	rows = monitorRows([]protocol.MonitorInfo{{ID: "x", Kind: "command", Label: "w", State: "running", Started: "nope"}}, "", now, 100)
	if len(rows) != 1 || strings.Contains(rows[0], "command") || !strings.HasSuffix(strings.TrimRight(stripANSI(rows[0]), " "), "w") {
		t.Fatalf("bad stamp: %q", rows)
	}
	if monitorRows(nil, "coder", now, 100) != nil {
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
	if view := stripANSI(m.sectionsView(100)); strings.Count(view, "\n") != 3 {
		t.Fatalf("expanded async tab:\n%s", view)
	}
	m.focus = focusInput
	// Both blocks are budgeted out of the transcript height.
	m.width, m.height = 120, 40
	m.showTree = false
	m.layout()
	collapsed := m.vp.Height
	_, kbCollapsed := m.keyBarView()
	m.focus = focusAsync
	m.layout()
	_, kbOpen := m.keyBarView()
	// open, the section costs one line per job on top of the strip (the key
	// bar legend may also change height with the focus)
	if want := collapsed - 3 - (kbOpen - kbCollapsed); m.vp.Height != want {
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
	// No prompt, sidebar hidden: input → chat → tabs → input. The strip is
	// one stop; all empty, it opens on permission, and ←/→ walk the tabs.
	right := tea.KeyMsg{Type: tea.KeyRight}
	left := tea.KeyMsg{Type: tea.KeyLeft}
	press(&m, tab)
	if m.focus != focusChat || m.follow || m.input.Focused() {
		t.Fatalf("tab: focus=%v follow=%v", m.focus, m.follow)
	}
	press(&m, tab)
	if m.focus != focusPermission || !strings.Contains(stripANSI(m.sectionsView(100)), "no prompts waiting") {
		t.Fatalf("tab tab: focus=%v\n%s", m.focus, stripANSI(m.sectionsView(100)))
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
	if m.focus != focusAgents || !strings.Contains(stripANSI(m.sectionsView(100)), "no subagents running") {
		t.Fatalf("right: focus=%v\n%s", m.focus, stripANSI(m.sectionsView(100)))
	}
	press(&m, right)
	if m.focus != focusAsync || !strings.Contains(stripANSI(m.sectionsView(100)), "no async jobs running") {
		t.Fatalf("right right: focus=%v\n%s", m.focus, stripANSI(m.sectionsView(100)))
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

	// Sidebar shown: input → sidebar → chat → tabs → input.
	m.showTree = true
	m.layout()
	var seen []focus
	for i := 0; i < 4; i++ {
		press(&m, tab)
		seen = append(seen, m.focus)
	}
	if want := []focus{focusSidebar, focusChat, focusPermission, focusInput}; !equalFocus(seen, want) {
		t.Fatalf("with sidebar: %v, want %v", seen, want)
	}
	// Hiding the sidebar while it has focus falls back to the input.
	press(&m, tab)
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
	for i := 0; i < 3; i++ {
		press(&m, tab)
		seen = append(seen, m.focus)
	}
	if want := []focus{focusChat, focusPermission, focusInput}; !equalFocus(seen, want) {
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

	press(&m, tea.KeyMsg{Type: tea.KeyTab})
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
	if got := marked(); !strings.HasPrefix(got, "⚙  Bash") {
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
	press(&m, tea.KeyMsg{Type: tea.KeyEsc}, tea.KeyMsg{Type: tea.KeyTab})
	if m.focus != focusChat || len(m.expanded["a"]) != 0 || strings.Count(view(), "out") != previewLines-1 {
		t.Fatalf("re-entering the chat should show the preview: focus=%v %v\n%s", m.focus, m.expanded["a"], view())
	}

	// Leaving the chat resumes following and drops the marker.
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusInput || !m.follow || !m.vp.AtBottom() || strings.Contains(stripANSI(m.vp.View()), gutterMark) {
		t.Fatalf("leave chat: focus=%v follow=%v bottom=%v", m.focus, m.follow, m.vp.AtBottom())
	}
	if hs := m.keyHints(); hs[2].key != "tab" || hs[2].desc != "next section" {
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
		{protocol.AgentInfo{State: "finished", Status: "success"}, "complete"},
		{protocol.AgentInfo{State: "finished", Status: "failure"}, "error"},
		{protocol.AgentInfo{State: "killed"}, "complete"},
	}
	for _, c := range cases {
		if got := agentOutcome(c.a); got != c.want {
			t.Errorf("%+v: got %s want %s", c.a, got, c.want)
		}
	}
	if agentDot(protocol.AgentInfo{State: "idle"}) == agentDot(protocol.AgentInfo{State: "finished"}) {
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

	// tab order: chat is skipped on the home view; the strip, then input
	order := m.focusOrder()
	if len(order) != 2 || order[0] != focusTabs || order[1] != focusInput {
		t.Fatalf("order %v", order)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyTab}) // input → permission (a prompt waits)
	if pv := stripANSI(m.sectionsView(100)); m.focus != focusPermission || strings.Count(pv, "\n") != 2 || !strings.Contains(pv, "⚙  Bash · coder\n       make test") || strings.Contains(pv, "{") {
		t.Fatalf("permission should open as a tool row over its command: focus=%v\n%s", m.focus, pv)
	}
	// the permission box is drawn under the strip, right above the input
	full := stripANSI(m.View())
	if bi, pi := strings.Index(full, "agents ("), strings.Index(full, "make test"); bi < 0 || pi < 0 || bi > pi {
		t.Fatalf("permission box should render below the strip:\n%s", full)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRight}) // permission → agents
	if m.focus != focusAgents {
		t.Fatalf("focus %v", m.focus)
	}
	sv = stripANSI(m.sectionsView(100))
	if strings.Count(sv, "\n") != 2 || !strings.Contains(sv, "▶") || !strings.Contains(sv, "scout") {
		t.Fatalf("expanded agents:\n%s", sv)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyRight}) // agents → async
	if m.focus != focusAsync || !strings.Contains(stripANSI(m.sectionsView(100)), "no async jobs running") {
		t.Fatalf("async: focus=%v\n%s", m.focus, stripANSI(m.sectionsView(100)))
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
	// agents focused: same strip, then its rows
	m.focus = focusAgents
	v = stripANSI(m.sectionsView(100))
	lines := strings.Split(v, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "permission (1) · agents (1) · async (1)") || !strings.Contains(lines[1], "▶") || !strings.Contains(lines[1], "scout") {
		t.Fatalf("agents focused:\n%s", v)
	}
	// async focused: the job row
	m.focus = focusAsync
	v = stripANSI(m.sectionsView(100))
	lines = strings.Split(v, "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "async (1)") || !strings.Contains(lines[1], "▶") || !strings.Contains(lines[1], "go test") || strings.Contains(v, "scout") {
		t.Fatalf("async focused:\n%s", v)
	}
	// permission focused: same strip, then the box
	m.focus = focusPermission
	v = stripANSI(m.sectionsView(100))
	lines = strings.Split(v, "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "permission (1)") || strings.Contains(lines[0], "▾") || !strings.Contains(lines[1], "Bash") || lines[2] != "       make test" || strings.Contains(v, "scout") {
		t.Fatalf("permission focused:\n%s", v)
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
		if si < 1 || !strings.HasPrefix(lines[si-1], "─") {
			t.Fatalf("focus %v: the strip should sit right under the chat rule:\n%s", f, stripANSI(v))
		}
	}
}

func TestPermissionShowsWholeCommand(t *testing.T) {
	m := sessionModel()
	long := "for f in $(ls /very/long/path/to/somewhere/deep/in/the/tree); do echo processing \"$f\" && sleep 1 && rm -f \"$f\".bak; done"
	m.prompts = []protocol.PromptInfo{{ID: "p1", Kind: "permission", Tool: "bash", Agent: "a", Input: []byte(`{"command":` + strconv.Quote(long+"\necho second line") + `}`)}}
	m.focus = focusPermission
	v := stripANSI(m.sectionsView(60))
	// every line fits the width, nothing is elided, and the newline is kept
	for _, l := range strings.Split(v, "\n") {
		if ansi.StringWidth(l) > 60 || strings.Contains(l, "…") {
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
	m.session.Dir = "/repo/project"
	m.session.Model, m.agents[0].Model = "chatgpt/gpt-5", "chatgpt/gpt-5"
	m.agents[0].Tokens, m.agents[0].CostUSD = 1500, 0.02
	m.width, m.height = 100, 30
	m.layout()
	lines := strings.Split(stripANSI(m.View()), "\n")
	meta, strip := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "Coder · "):
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
	if !strings.HasPrefix(lines[meta+1], "─") {
		t.Fatalf("the key bar rule should follow the meta row:\n%s", strings.Join(lines, "\n"))
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
