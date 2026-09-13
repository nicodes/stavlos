package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

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
	box := stripANSI(inputBox("› hi", "Coder  ·  x"))
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

func TestMonitorRows(t *testing.T) {
	now := time.Now()
	spawned := map[string]time.Time{"c1": now.Add(-75 * time.Second), "c2": now.Add(-3 * time.Second)}
	agents := []protocol.AgentInfo{
		{ID: "root", Label: "coder", Archetype: "coder", State: "idle"},
		{ID: "c1", Parent: "root", Label: "scout", Archetype: "explorer", State: "running", Turn: 2, CostUSD: 0.0012},
		{ID: "c2", Parent: "root", Label: "tester", Archetype: "tester", State: "idle"},
		{ID: "c3", Parent: "root", Label: "done", Archetype: "explorer", State: "finished"},
		{ID: "g1", Parent: "c1", Label: "grandchild", Archetype: "explorer", State: "running"},
	}
	rows := monitorRows(agents, "root", spawned, now, "⠋", 100)
	if len(rows) != 2 {
		t.Fatalf("rows %d: %q", len(rows), rows)
	}
	if !strings.Contains(rows[0], "scout (explorer)") || !strings.Contains(rows[0], "turn 2") || !strings.Contains(rows[0], "1m15s") || !strings.Contains(rows[0], "⠋") {
		t.Fatalf("%q", rows[0])
	}
	if !strings.Contains(rows[1], "tester") || !strings.Contains(rows[1], "3s") || strings.Contains(rows[1], "turn") {
		t.Fatalf("%q", rows[1])
	}
	if rows := monitorRows(agents, "c2", spawned, now, "", 100); len(rows) != 0 {
		t.Fatalf("no children expected: %q", rows)
	}
	if got := fmtElapsed(3725 * time.Second); got != "1h02m" {
		t.Fatalf("%s", got)
	}
}
