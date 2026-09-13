package tui

import (
	"context"
	"encoding/json"
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
	if got != "● coder · anthropic/claude-opus-5  /help" {
		t.Fatalf("connected home: %q", got)
	}
	got = stripANSI(footerRight(footerInfo{connected: true, label: "coder", model: "anthropic/claude-opus-5", tokens: 12_345, cost: 0.0123}))
	if got != "12k tokens · $0.0123  /help" {
		t.Fatalf("session: %q", got)
	}
	got = stripANSI(footerRight(footerInfo{connected: true, tokens: 500, cost: 1.5, pending: 2}))
	if got != "△ 2 Permissions  500 tokens · $1.50  /help" {
		t.Fatalf("pending: %q", got)
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
	if got := stripANSI(metaLine("coder", "anthropic/claude-opus-5", 0)); got != "Coder  ·  claude-opus-5 anthropic" {
		t.Fatalf("with model: %q", got)
	}
	if got := stripANSI(metaLine("coder", "", 0)); got != "Coder  ·  no model — /models" {
		t.Fatalf("no model: %q", got)
	}
	if got := stripANSI(metaLine("scout", "ollama/llama3", 2)); got != "Scout  ·  llama3 ollama  ·  2 queued" {
		t.Fatalf("queued: %q", got)
	}
}

func TestInputBoxAndPromptWidth(t *testing.T) {
	box := stripANSI(inputBox("› hi", "Coder  ·  x", true))
	if box != "│  › hi\n│  Coder  ·  x" {
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
		{ID: "c1", Parent: "root", Label: "scout", Archetype: "explorer", State: "running", Turn: 2, CostUSD: 0.0012, Monitored: true},
		{ID: "c2", Parent: "root", Label: "tester", Archetype: "tester", State: "idle"},
		{ID: "c3", Parent: "root", Label: "done", Archetype: "explorer", State: "finished"},
		{ID: "g1", Parent: "c1", Label: "grandchild", Archetype: "explorer", State: "running"},
	}
	rows := agentRows(agents, "root", spawned, now, "⠋", 100)
	if len(rows) != 2 {
		t.Fatalf("rows %d: %q", len(rows), rows)
	}
	if !strings.Contains(rows[0], "scout (explorer)") || !strings.Contains(rows[0], "turn 2") || !strings.Contains(rows[0], "1m15s") || !strings.Contains(rows[0], "⠋") {
		t.Fatalf("%q", rows[0])
	}
	if !strings.Contains(rows[0], "wakes parent") || strings.Contains(rows[1], "wakes parent") {
		t.Fatalf("armed marker: %q", rows)
	}
	if !strings.Contains(rows[1], "tester") || !strings.Contains(rows[1], "3s") || strings.Contains(rows[1], "turn") {
		t.Fatalf("%q", rows[1])
	}
	if rows := agentRows(agents, "c2", spawned, now, "", 100); len(rows) != 0 {
		t.Fatalf("no children expected: %q", rows)
	}
	if got := fmtElapsed(3725 * time.Second); got != "1h02m" {
		t.Fatalf("%s", got)
	}

	// The block is titled "agents" and lists only the selected agent's children.
	m := sessionModel()
	m.spawned = spawned
	m.agents = agents
	m.selected = 0
	view := stripANSI(m.agentsView(100))
	if !strings.HasPrefix(view, "agents (2)") || !strings.Contains(view, "scout") || strings.Contains(view, "grandchild") {
		t.Fatalf("agents block:\n%s", view)
	}
	if strings.Contains(view, "monitors") {
		t.Fatalf("agents block must not be titled monitors:\n%s", view)
	}
}

func TestMonitorRows(t *testing.T) {
	now := time.Now().Truncate(time.Second) // Started is RFC3339: whole seconds
	monitors := []protocol.MonitorInfo{
		{ID: "m1", Agent: "root", Kind: "command", Label: "go test", Spec: "go test ./...", State: "running", Started: now.Add(-75 * time.Second).Format(time.RFC3339), Progress: "42 lines", Monitored: true},
		{ID: "m2", Agent: "root", Kind: "command", Label: "src changes", Spec: "./watch.sh", State: "running", Started: now.Add(-3 * time.Second).Format(time.RFC3339)},
		{ID: "m3", Agent: "root", Kind: "command", Label: "cooldown", Spec: "sleep 300", State: "running", Started: now.Add(-2 * time.Hour).Format(time.RFC3339), Progress: "3m left"},
		{ID: "m4", Agent: "root", Kind: "command", Label: "old", State: "fired", Started: now.Format(time.RFC3339)},
	}
	rows := monitorRows(monitors, now, "⠋", 100)
	if len(rows) != 3 {
		t.Fatalf("rows %d: %q", len(rows), rows)
	}
	plain := make([]string, len(rows))
	for i, r := range rows {
		plain[i] = stripANSI(r)
	}
	// command: clock glyph, bold label, spinner, kind, progress, elapsed, wakes parent
	if !strings.HasPrefix(plain[0], "  ◷") || !strings.Contains(plain[0], "go test ⠋") {
		t.Fatalf("command row: %q", plain[0])
	}
	for _, want := range []string{"command", "42 lines", "1m15s", "wakes parent"} {
		if !strings.Contains(plain[0], want) {
			t.Fatalf("command row lacks %q: %q", want, plain[0])
		}
	}
	// a second running job: spinner, no wake tag, elapsed
	if !strings.HasPrefix(plain[1], "  ◷ src changes") || !strings.Contains(plain[1], "⠋") || strings.Contains(plain[1], "wakes parent") || !strings.Contains(plain[1], "command · 3s") {
		t.Fatalf("second job row: %q", plain[1])
	}
	// progress and hours elapsed
	if !strings.HasPrefix(plain[2], "  ◷ cooldown") || !strings.Contains(plain[2], "command · 3m left · 2h00m") {
		t.Fatalf("third job row: %q", plain[2])
	}
	// a bad Started stamp just drops the elapsed field
	rows = monitorRows([]protocol.MonitorInfo{{ID: "x", Kind: "command", Label: "w", State: "running", Started: "nope"}}, now, "", 100)
	if len(rows) != 1 || !strings.HasSuffix(strings.TrimRight(stripANSI(rows[0]), " "), "command") {
		t.Fatalf("bad stamp: %q", rows)
	}
	if monitorRows(nil, now, "", 100) != nil {
		t.Fatal("no monitors should give no rows")
	}

	// The block reads the selected agent's Monitors and is titled "monitors".
	m := sessionModel()
	m.agents = []protocol.AgentInfo{{ID: "root", Label: "coder", Archetype: "coder", State: "idle", Monitors: monitors[:3]}}
	m.selected = 0
	view := stripANSI(m.monitorsView(100))
	if !strings.HasPrefix(view, "monitors (3)") || strings.Count(view, "\n") != 3 {
		t.Fatalf("monitors block:\n%s", view)
	}
	if m.agentsView(100) != "" {
		t.Fatal("no children: agents block should be empty")
	}
	// Both blocks are budgeted out of the transcript height.
	m.width, m.height = 120, 40
	m.showTree = false
	m.layout()
	withBlock := m.vp.Height
	m.agents[0].Monitors = nil
	m.layout()
	if m.vp.Height != withBlock+4 {
		t.Fatalf("layout: viewport %d with monitors, %d without", withBlock, m.vp.Height)
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
	// No prompt, sidebar hidden: input → chat → input.
	press(&m, tab)
	if m.focus != focusChat || m.follow || m.input.Focused() {
		t.Fatalf("tab: focus=%v follow=%v", m.focus, m.follow)
	}
	press(&m, tab)
	if m.focus != focusInput || !m.follow || !m.input.Focused() {
		t.Fatalf("tab tab: focus=%v follow=%v", m.focus, m.follow)
	}
	press(&m, stab)
	if m.focus != focusChat {
		t.Fatalf("shift+tab: focus=%v", m.focus)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.focus != focusInput {
		t.Fatalf("esc: focus=%v", m.focus)
	}

	// Sidebar shown: input → sidebar → chat → input.
	m.showTree = true
	m.layout()
	var seen []focus
	for i := 0; i < 3; i++ {
		press(&m, tab)
		seen = append(seen, m.focus)
	}
	if want := []focus{focusSidebar, focusChat, focusInput}; !equalFocus(seen, want) {
		t.Fatalf("with sidebar: %v, want %v", seen, want)
	}
	// Hiding the sidebar while it has focus falls back to the input.
	press(&m, tab)
	m.showTree = false
	m.ensureFocus()
	if m.focus != focusInput {
		t.Fatalf("sidebar hidden: focus=%v", m.focus)
	}

	// Pending prompt: chat → permission → input → chat.
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
	press(&m, stab)
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
	press(&m, tea.KeyMsg{Type: tea.KeyShiftTab})
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
	if strings.Contains(stripANSI(m.promptView(80)), "tab to focus") {
		t.Fatal("focused box should show the hotkeys directly")
	}
	m.focus = focusInput
	if pv := stripANSI(m.promptView(80)); !strings.Contains(pv, "tab to answer") || strings.Count(pv, "\n") != 0 {
		t.Fatalf("unfocused prompt should be one line pointing at tab: %q", pv)
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
	// Enter on a non-tool item is inert.
	press(&m, tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.expanded["a"]) != 1 {
		t.Fatalf("enter on a user item changed overrides: %v", m.expanded["a"])
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

	// unfocused: one line each
	av := stripANSI(m.agentsView(100))
	if strings.Count(av, "\n") != 0 || !strings.Contains(av, "agents (2)") || !strings.Contains(av, "scout") || !strings.Contains(av, "checks") {
		t.Fatalf("collapsed agents:\n%s", av)
	}
	pv := stripANSI(m.promptView(100))
	if strings.Count(pv, "\n") != 0 || !strings.Contains(pv, "permission: bash") || !strings.Contains(pv, "make test") || !strings.Contains(pv, "tab to answer") {
		t.Fatalf("collapsed prompt:\n%s", pv)
	}

	// tab order: chat is skipped on the home view; permission, agents, input
	order := m.focusOrder()
	if len(order) != 3 || order[0] != focusPermission || order[1] != focusAgents || order[2] != focusInput {
		t.Fatalf("order %v", order)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyTab}) // input → permission
	if m.focus != focusPermission || strings.Count(stripANSI(m.promptView(100)), "\n") < 2 {
		t.Fatalf("permission should expand when focused: focus=%v\n%s", m.focus, stripANSI(m.promptView(100)))
	}
	press(&m, tea.KeyMsg{Type: tea.KeyTab}) // permission → agents
	if m.focus != focusAgents {
		t.Fatalf("focus %v", m.focus)
	}
	av = stripANSI(m.agentsView(100))
	if strings.Count(av, "\n") != 2 || !strings.Contains(av, "▶") {
		t.Fatalf("expanded agents:\n%s", av)
	}
	press(&m, tea.KeyMsg{Type: tea.KeyDown})
	press(&m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.selectedID() != "c2" || m.focus != focusInput {
		t.Fatalf("enter should select the child under the cursor: %s focus %v", m.selectedID(), m.focus)
	}
	// now the selected agent has no children: the agents section leaves the cycle
	for _, f := range m.focusOrder() {
		if f == focusAgents {
			t.Fatal("agents section should not be in the order without live children")
		}
	}
}
