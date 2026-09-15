package render

import (
	"encoding/json"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/internal/tui/transcript"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func mk(seq int64, agent string, typ event.Type, payload any) event.Event {
	return event.Event{Seq: seq, Session: "s1", Agent: agent, Type: typ, Payload: event.MustPayload(payload)}
}

// renderLines renders (collapsed) and returns trimmed, ANSI-free lines.
func renderLines(lines []transcript.Line) []string {
	return renderWith(lines, Options{Width: 80, Spinner: "⠋", NoFold: true})
}

func renderWith(lines []transcript.Line, o Options) []string {
	out := strings.Split(stripANSI(firstOf(Lines(lines, o))), "\n")
	for i := range out {
		out[i] = strings.TrimRight(out[i], " ")
	}
	return out
}

// assertSubsequence checks that want appears in got, in order, as exact lines.
func assertSubsequence(t *testing.T, got, want []string) {
	t.Helper()
	j := 0
	for _, g := range got {
		if j < len(want) && g == want[j] {
			j++
		}
	}
	if j != len(want) {
		t.Fatalf("missing line %q\ngot:\n%s", want[j], strings.Join(got, "\n"))
	}
}

func contains(got []string, s string) bool {
	for _, g := range got {
		if g == s {
			return true
		}
	}
	return false
}

func TestBuildTranscript(t *testing.T) {
	showThinkingForTest(t)
	evs := []event.Event{
		mk(1, "a1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a1", Archetype: "coder", Label: "root", Model: "anthropic/claude-x"}),
		mk(2, "a1", event.TurnStarted, event.TurnPayload{Turn: 1}),
		mk(3, "a1", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "hello\nworld"}),
		mk(4, "a1", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command": "sleep 100"}`)}),
		mk(5, "a1", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "partial\n", Cancelled: true}),
		mk(6, "a1", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Model: "anthropic/claude-x", StopReason: "end_turn", Blocks: []model.Block{
			{Type: model.BlockThinking},
			{Type: model.BlockText, Text: "Done."},
		}}),
		mk(7, "a1", event.TurnEnded, event.TurnEndedPayload{Turn: 1, Reason: "cancelled"}),
		mk(8, "a1", event.AgentFinished, event.AgentFinishedPayload{Summary: "all good", Status: "success"}),
	}
	got := renderLines(transcript.Build(evs))
	assertSubsequence(t, got, []string{
		"› @user hello",
		"  world",
		"$ Shell  sleep 100 (cancelled)",
		"  partial",
		"◌ thinking…",
		"§ Done.",
		"◦ Turn cancelled",
		"✓ Finished success",
		"all good",
	})
	for _, g := range got {
		if strings.Contains(g, "usage") || strings.Contains(g, "turn.started") || strings.Contains(g, "spawned") {
			t.Fatalf("unexpected line %q", g)
		}
	}
}

func TestRootSpawnKeepsTranscriptEmpty(t *testing.T) {
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a1", Archetype: "coder", Label: "root"}))
	if !tr.Empty() {
		t.Fatalf("root spawn should not produce lines: %+v", tr.All())
	}
	tr.Apply(mk(2, "c1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "c1", Parent: "a1", Archetype: "explorer", Label: "scout", Model: "m", Task: "look around"}))
	got := renderLines(tr.All())
	if !tr.Empty() {
		t.Fatalf("a spawn with a task waits for its first prompt: %q", got)
	}
	// the first prompt, the task from the creator, draws the spawn and the
	// task as one item, not "Spawned" then "Prompt from"
	tr.Apply(mk(3, "c1", event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(4, "c1", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "look around", From: "root"}))
	got = renderLines(tr.All())
	assertSubsequence(t, got, []string{"» @root as scout (explorer) · m", "  look around"})
	for _, g := range got {
		if strings.Contains(g, "Prompt from") || strings.Contains(g, "▹") {
			t.Fatalf("the spawn and its task are one item: %q", got)
		}
	}
	// a later prompt from the creator reads as a prompt
	tr.Apply(mk(5, "c1", event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: "prompt", Text: "look around", From: "root"}))
	if got := renderLines(tr.All()); !strings.Contains(strings.Join(got, "\n"), "› @root look around") {
		t.Fatalf("a later prompt: %q", got)
	}
}

func TestUserMessageKinds(t *testing.T) {
	lines := transcript.Build([]event.Event{
		mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "steer", Text: "focus"}),
		mk(2, "a", event.UserMessage, event.UserMessagePayload{Kind: "child_finished", Text: "child done"}),
		mk(3, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi"}),
	})
	got := renderLines(lines)
	assertSubsequence(t, got, []string{"› @user focus", "agent response", "⑂ child done", "› @user hi"})
	for _, l := range got {
		if strings.TrimSpace(l) == "steer" {
			t.Fatalf("a steer should carry no title:\n%s", strings.Join(got, "\n"))
		}
	}

	// Blocks carry their kind so Render can pick the border color.
	var blocks []transcript.BlockKind
	for _, l := range lines {
		if l.Kind == transcript.LineText {
			blocks = append(blocks, l.Block)
		}
	}
	if len(blocks) != 3 || blocks[0] != transcript.BlockUser || blocks[1] != transcript.BlockChild || blocks[2] != transcript.BlockUser {
		t.Fatalf("block kinds: %v", blocks)
	}
	// Blank line before and after each block.
	if lines[0].Kind != transcript.LineBlank || lines[len(lines)-1].Kind != transcript.LineBlank {
		t.Fatalf("blocks must be padded with blank lines: %+v", lines)
	}
}

func TestUserBlockBorderAndWrap(t *testing.T) {
	text := "one two three four five six seven eight nine ten"
	got := renderWith(transcript.Build([]event.Event{
		mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi\n" + text}),
	}), Options{Width: 30, NoFold: true})
	if got[0] != "› @user hi" {
		t.Fatalf("prompt glyph + padding: %q", got[0])
	}
	// Long lines wrap inside the border; every continuation keeps it.
	body := got[1:]
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
	}
	if len(body) < 2 {
		t.Fatalf("expected wrapped lines:\n%s", strings.Join(got, "\n"))
	}
	for _, l := range body {
		if !strings.HasPrefix(l, "  ") || len([]rune(l)) > 30 {
			t.Fatalf("bad wrapped line %q", l)
		}
	}
}

func TestToolStatesAndCollapsedOutput(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		sb.WriteString("line\n")
	}
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)}))
	got := renderLines(tr.All())
	if !contains(got, "⌕ Read  a.go") {
		t.Fatalf("running tool shows its glyph (yellow):\n%s", strings.Join(got, "\n"))
	}
	if !tr.Running() {
		t.Fatal("Running() should be true while a call is open")
	}

	tr.Apply(mk(2, "a", event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c1", Name: "read", Output: sb.String(), IsError: true}))
	tr.Apply(mk(3, "a", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c2", Name: "read", Input: json.RawMessage(`{"path":"b.go"}`)}))
	tr.Apply(mk(4, "a", event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c2", Name: "read", Denied: true}))
	tr.Apply(mk(5, "a", event.Compacted, event.CompactedPayload{FromSeq: 1, ToSeq: 3}))
	if tr.Running() {
		t.Fatal("Running() should be false after all calls finished")
	}

	got = renderLines(tr.All())
	assertSubsequence(t, got, []string{"⌕ Read  a.go", "  line", "  line", "  line", "  … +17 lines", "✗ Read  b.go"})
	if n := count(got, "  line"); n != transcript.MaxOutputCollapsed {
		t.Fatalf("collapsed: want %d output lines, got %d", transcript.MaxOutputCollapsed, n)
	}
	var rule string
	for _, g := range got {
		if strings.Contains(g, "compacted") {
			rule = g
		}
	}
	if strings.TrimSpace(rule) != "┄┄ compacted ┄┄" || !strings.HasPrefix(rule, "  ") {
		t.Fatalf("compacted rule should be centered: %q", rule)
	}

	expanded := renderWith(tr.All(), Options{Width: 80, Details: true})
	if n := count(expanded, "  line"); n != 20 {
		t.Fatalf("expanded: want 20 output lines, got %d", n)
	}
	for _, g := range expanded {
		if strings.Contains(g, "+17 lines") {
			t.Fatalf("expanded view must not show the collapsed trailer: %q", g)
		}
	}
}

func TestToolOutputExpandedCap(t *testing.T) {
	lines := transcript.OutputLines(strings.TrimRight(strings.Repeat("x\n", 50), "\n"))
	collapsed := renderWith(lines, Options{Width: 80, NoFold: true})
	expanded := renderWith(lines, Options{Width: 80, Details: true})
	if count(collapsed, "  x") != transcript.MaxOutputCollapsed || !contains(collapsed, "  … +47 lines") {
		t.Fatalf("collapsed:\n%s", strings.Join(collapsed, "\n"))
	}
	if count(expanded, "  x") != transcript.MaxOutputExpanded || !contains(expanded, "  … +10 lines") {
		t.Fatalf("expanded:\n%s", strings.Join(expanded, "\n"))
	}
}

func count(got []string, s string) int {
	n := 0
	for _, g := range got {
		if g == s {
			n++
		}
	}
	return n
}

func TestAssistantMarkdownAndErrors(t *testing.T) {
	got := renderLines(transcript.Build([]event.Event{
		mk(1, "a", event.AssistantMessage, event.AssistantMessagePayload{Model: "openai/gpt-x", StopReason: "tool_use", Blocks: []model.Block{
			{Type: model.BlockText, Text: "# Plan\nSome **bold** text\n```go\nfmt.Println()\n```\n- item"},
		}}),
		mk(2, "a", event.TurnEnded, event.TurnEndedPayload{Reason: "error", Error: "boom"}),
		mk(3, "a", event.AgentKilled, nil),
	}))
	assertSubsequence(t, got, []string{"§ Plan", "  Some bold text", "   fmt.Println()", "  - item", "! boom", "⊘ killed"})
	for _, g := range got {
		if strings.Contains(g, "```") || strings.Contains(g, "· gpt-x") {
			t.Fatalf("unexpected line %q (fences dropped; no model trailer on tool_use)", g)
		}
	}
	// Balanced markers are consumed; unbalanced ones are left alone.
	if got := stripANSI(inlineMarkdown("a **b** c", theme.StyleDim)); got != "a b c" {
		t.Fatalf("inline bold: %q", got)
	}
	if got := stripANSI(inlineMarkdown("a **b c", theme.StyleDim)); got != "a **b c" {
		t.Fatalf("unbalanced: %q", got)
	}
}

func TestStreamingBufferReplacedByAssistantMessage(t *testing.T) {
	showThinkingForTest(t)
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "hi"}))
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Thinking: "hmm"})
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Text: "Hel"})
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Text: "lo"})
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, ToolName: "shell"})

	got := renderLines(tr.All())
	assertSubsequence(t, got, []string{"› @user hi", "◌ thinking…", "§ Hello", "$ Shell"})
	if len(tr.Tail()) == 0 || !tr.Running() {
		t.Fatal("expected a streaming buffer with a running tool")
	}

	tr.Apply(mk(2, "a", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{
		{Type: model.BlockThinking, Text: "one\ntwo\nthree\nfour"},
		{Type: model.BlockText, Text: "Hello"},
	}}))
	if len(tr.Tail()) > 0 {
		t.Fatal("buffer should be cleared by assistant.message")
	}
	got = renderLines(tr.All())
	assertSubsequence(t, got, []string{"◌ one", "§ Hello"})
	for _, g := range got {
		if g == "$ Shell" || g == "◌ thinking…" || g == "two" {
			t.Fatalf("stale stream line %q", g)
		}
	}
}

func TestStreamingToolOutputCollapses(t *testing.T) {
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"make"}`)}))
	for i := 0; i < 5; i++ {
		tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, ToolName: "shell", Text: "out\n"})
	}
	got := renderLines(tr.All())
	assertSubsequence(t, got, []string{"$ Shell  make", "  out", "  … +2 lines"})
	if count(got, "  out") != transcript.MaxOutputCollapsed {
		t.Fatalf("live output should be collapsed:\n%s", strings.Join(got, "\n"))
	}
	tr.Apply(mk(2, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "final"}))
	got = renderLines(tr.All())
	assertSubsequence(t, got, []string{"$ Shell  make", "  final"})
	if count(got, "  out") != 0 {
		t.Fatal("live output should be replaced by the final output")
	}
}

func TestRenderCursorAndPerItemExpand(t *testing.T) {
	markCursorForTest(t)
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi"}))
	tr.Apply(mk(2, "a", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}))
	tr.Apply(mk(3, "a", event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c1", Name: "shell", Output: strings.TrimRight(strings.Repeat("x\n", 6), "\n")}))
	lines := tr.All()

	plain := renderWith(lines, Options{Width: 80, NoFold: true})
	for _, l := range plain {
		if strings.Contains(l, GutterMark) {
			t.Fatalf("no cursor without focus: %q", l)
		}
	}
	got := renderWith(lines, Options{Width: 80, Cursor: 1, Focused: true})
	if !contains(got, GutterMark+"$ Shell  ls") || !contains(got, GutterMark+"  x") || contains(got, GutterMark+"│  hi") {
		t.Fatalf("cursor marks only item 1:\n%s", strings.Join(got, "\n"))
	}
	if !contains(got, "› @user hi") {
		t.Fatalf("non-cursor lines keep the gutter space:\n%s", strings.Join(got, "\n"))
	}
	// Per-item override expands item 1 while /details is off, and vice versa.
	exp := renderWith(lines, Options{Width: 80, NoFold: true, Expanded: map[int]bool{1: true}})
	if count(exp, "  x") != 6 || contains(exp, "    … +3 lines") {
		t.Fatalf("expanded override:\n%s", strings.Join(exp, "\n"))
	}
	col := renderWith(lines, Options{Width: 80, Details: true, Expanded: map[int]bool{1: false}})
	if count(col, "  x") != transcript.MaxOutputCollapsed {
		t.Fatalf("collapsed override:\n%s", strings.Join(col, "\n"))
	}
	_, rows := Lines(lines, Options{Width: 80, NoFold: true})
	// user block on row 0 (no leading blank at the top), one blank row of
	// spacing, then the tool item: line, 3 output lines, trailer
	if r := rows[0]; r.First != 0 || r.Last != 0 {
		t.Fatalf("user rows: %+v (want 0..0)", r)
	}
	if r := rows[1]; r.First != 2 || r.Last != 6 {
		t.Fatalf("tool rows: %+v (want 2..6: tool line, 3 output lines, trailer)", r)
	}
}

func TestToolOutputStaysWithItsCall(t *testing.T) {
	tr := transcript.NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(2, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "run it"}))
	tr.Apply(mk(3, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"make test"}`)}))
	tr.Apply(mk(4, event.PromptRequested, event.PromptRequestedPayload{ID: "p1", Kind: "permission", Tool: "shell"}))
	tr.Apply(mk(5, event.PromptAnswered, event.PromptAnsweredPayload{ID: "p1", Answer: "allow"}))
	tr.Apply(mk(6, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "ok\nall passed"}))
	tr.Apply(mk(7, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "done"}}}))

	lines := tr.All()
	toolIdx := -1
	for i, l := range lines {
		if l.Kind == transcript.LineTool {
			toolIdx = i
		}
	}
	if toolIdx < 0 {
		t.Fatal("no tool line")
	}
	toolItem := lines[toolIdx].Item
	first, last := transcript.ItemRange(tr.All(), toolItem)
	// every line in the item's range belongs to the item, and the item holds,
	// in order: the call, the permission notices it gated, then the output
	for i := first; i <= last; i++ {
		if lines[i].Item != toolItem {
			t.Fatalf("line %d (%q) inside tool item range belongs to item %d", i, lines[i].Text, lines[i].Item)
		}
	}
	joined := ""
	for i := first; i <= last; i++ {
		joined += lines[i].Text + "\n"
	}
	reqAt, ansAt, outAt := strings.Index(joined, "Permission"), strings.Index(joined, "allow"), strings.Index(joined, "all passed")
	if reqAt < 0 || ansAt < 0 || outAt < 0 || !(reqAt < ansAt && ansAt < outAt) {
		t.Fatalf("order within tool item wrong (req %d, ans %d, out %d):\n%s", reqAt, ansAt, outAt, joined)
	}
	// the notices are nested under the call at the output's indent
	rendered := renderWith(lines, Options{Width: 80, Focused: true, Cursor: toolItem})
	for _, r := range rendered {
		if strings.Contains(r, "Permission") || strings.Contains(r, "answered") {
			if !strings.HasPrefix(strings.TrimLeft(r, "▍"), "  ") {
				t.Fatalf("notice not indented under the call: %q", r)
			}
		}
	}
	// nothing of the tool item, and no permission notice, lives outside it
	for i := last + 1; i < len(lines); i++ {
		if lines[i].Item == toolItem || strings.Contains(lines[i].Text, "Permission") {
			t.Fatalf("tool item content after its range at %d: %q", i, lines[i].Text)
		}
	}
}

func TestFoldingToOneLine(t *testing.T) {
	showThinkingForTest(t)
	tr := transcript.NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "line one\nline two"}))
	tr.Apply(mk(2, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockThinking, Text: "first thought\nsecond thought"}}}))
	tr.Apply(mk(3, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}))
	tr.Apply(mk(4, event.PromptRequested, event.PromptRequestedPayload{ID: "p1", Kind: "permission", Tool: "shell"}))
	tr.Apply(mk(5, event.PromptAnswered, event.PromptAnsweredPayload{ID: "p1", Answer: "allow"}))
	tr.Apply(mk(6, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "a\nb\nc\nd\ne"}))
	tr.Apply(mk(7, event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: "child_finished", Text: "Child agent \"scout\"finished.\n\nfound it"}))
	tr.Apply(mk(8, event.AssistantMessage, event.AssistantMessagePayload{Turn: 2, Model: "openai/gpt-5.4", Blocks: []model.Block{{Type: model.BlockText, Text: "final answer\nwith two lines"}}}))
	lines := tr.All()
	toolItem, childItem := -1, -1
	for _, l := range lines {
		if l.Kind == transcript.LineTool {
			toolItem = l.Item
		}
		if l.Block == transcript.BlockChild && childItem < 0 {
			childItem = l.Item
		}
	}
	nonblank := func(out []string) []string {
		var r []string
		for _, l := range out {
			if strings.TrimSpace(l) != "" {
				r = append(r, l)
			}
		}
		return r
	}
	plain := nonblank(renderWith(lines, Options{Width: 80}))
	joined := strings.Join(plain, "\n")
	// the human's input in full; the agent's own text folds like the rest
	for _, want := range []string{"line one", "line two", "final answer +1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "with two lines") {
		t.Fatalf("the agent's text should fold to its first line:\n%s", joined)
	}
	// thinking, tool (with its notices and output) and child result fold to one line each
	if strings.Contains(joined, "second thought") || strings.Contains(joined, "Permission") || strings.Contains(joined, "found it") {
		t.Fatalf("folded items leaked lines:\n%s", joined)
	}
	toolRows := 0
	for _, l := range plain {
		if strings.Contains(l, "Shell") {
			toolRows++
			if !strings.Contains(l, "+") {
				t.Fatalf("folded tool row lacks +N: %q", l)
			}
		}
	}
	if toolRows != 1 {
		t.Fatalf("tool rows %d:\n%s", toolRows, joined)
	}
	// cursor on the tool item previews its first lines (call, permission,
	// answer) with a +N marker; enter (Expanded) shows everything
	prev := nonblank(renderWith(lines, Options{Width: 80, Focused: true, Cursor: toolItem}))
	joinedPrev := strings.Join(prev, "\n")
	if !strings.Contains(joinedPrev, "Permission") || !strings.Contains(joinedPrev, "allow") || strings.Contains(joinedPrev, "found it") {
		t.Fatalf("cursor on tool:\n%s", joinedPrev)
	}
	toolPrev := 0
	for _, l := range prev {
		if strings.Contains(l, "Shell") || strings.Contains(l, "Permission") || strings.Contains(l, "allow") || strings.HasPrefix(strings.TrimLeft(l, "▍ "), "a") {
			toolPrev++
		}
	}
	if toolPrev > PreviewLines || !strings.Contains(joinedPrev, "+") {
		t.Fatalf("preview should be at most %d lines with a +N marker:\n%s", PreviewLines, joinedPrev)
	}
	full := strings.Join(nonblank(renderWith(lines, Options{Width: 80, Focused: true, Cursor: toolItem, Expanded: map[int]bool{toolItem: true}})), "\n")
	if !strings.Contains(full, "Permission") || !strings.Contains(full, "allow") || !strings.Contains(full, "\n") || strings.Count(full, "\n") < 5 {
		t.Fatalf("expanded tool:\n%s", full)
	}
	child := strings.Join(nonblank(renderWith(lines, Options{Width: 80, Focused: true, Cursor: childItem})), "\n")
	if !strings.Contains(child, "found it") || strings.Contains(child, "Permission") {
		t.Fatalf("cursor on child:\n%s", child)
	}
	// /details shows everything
	all := strings.Join(nonblank(renderWith(lines, Options{Width: 80, Details: true})), "\n")
	if !strings.Contains(all, "found it") || !strings.Contains(all, "Permission") || !strings.Contains(all, "e\n") && !strings.HasSuffix(all, "e") {
		t.Fatalf("details:\n%s", all)
	}
}

func TestMonitorEventsGroupAndFold(t *testing.T) {
	tr := transcript.NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "run the tests in the background"}))
	tr.Apply(mk(2, event.MonitorStarted, event.MonitorStartedPayload{ID: "m1", Kind: "command", Label: "go test", Spec: "go test ./..."}))
	tr.Apply(mk(3, event.MonitorStarted, event.MonitorStartedPayload{ID: "m2", Kind: "command", Label: "src", Spec: "./watch.sh"}))
	tr.Apply(mk(4, event.MonitorStarted, event.MonitorStartedPayload{ID: "m3", Kind: "command", Label: "cooldown", Spec: "sleep 300"}))
	tr.Apply(mk(5, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Model: "openai/gpt-5.4", Blocks: []model.Block{{Type: model.BlockText, Text: "waiting"}}}))
	tr.Apply(mk(6, event.MonitorFired, event.MonitorFiredPayload{ID: "m1", Kind: "command", Label: "go test", Summary: "go test exited 0", Output: "ok  a\nok  b\nok  c\nok  d\nok  e"}))
	tr.Apply(mk(7, event.MonitorStopped, event.MonitorRefPayload{ID: "m2", Reason: "unmonitor"}))
	tr.Apply(mk(8, event.MonitorFired, event.MonitorFiredPayload{ID: "m3", Kind: "command", Label: "cooldown", Summary: "timer elapsed", IsError: true}))
	tr.Apply(mk(9, event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: "monitor_fired", Text: "Job \"go test\" (m1): go test exited 0\n\nok  a\nok  b"}))
	tr.Apply(mk(10, event.AssistantMessage, event.AssistantMessagePayload{Turn: 2, Model: "openai/gpt-5.4", Blocks: []model.Block{{Type: model.BlockText, Text: "all green"}}}))

	lines := tr.All()
	find := func(text string) (int, transcript.Line) {
		for i, l := range lines {
			if strings.Contains(l.Text, text) {
				return i, l
			}
		}
		t.Fatalf("no line containing %q in:\n%s", text, strings.Join(renderLines(lines), "\n"))
		return -1, transcript.Line{}
	}
	// started notices
	_, cmdStart := find("**Job** go test")
	_, watchStart := find("**Job** src")
	_, timerStart := find("**Job** cooldown")
	for _, l := range []transcript.Line{cmdStart, watchStart, timerStart} {
		if l.Kind != transcript.LineDim || l.Running {
			t.Fatalf("started notice should be a static dim line: %+v", l)
		}
	}
	// fired output joins the started item and sits right under it, collapsed
	firedAt, fired := find("go test exited 0")
	if fired.Item != cmdStart.Item || fired.Kind != transcript.LineDim {
		t.Fatalf("fired line item %d != started item %d (%+v)", fired.Item, cmdStart.Item, fired)
	}
	first, last := transcript.ItemRange(tr.All(), cmdStart.Item)
	for i := first; i <= last; i++ {
		if lines[i].Item != cmdStart.Item {
			t.Fatalf("started item not contiguous at %d: %+v", i, lines[i])
		}
	}
	outAt, out := find("ok  a")
	if out.Kind != transcript.LineToolOut || out.Item != cmdStart.Item || outAt != firedAt+1 {
		t.Fatalf("output line: %+v at %d (fired at %d)", out, outAt, firedAt)
	}
	_, more := find("… +2 lines")
	if more.Vis != transcript.VisCollapsed || more.Item != cmdStart.Item {
		t.Fatalf("collapsed trailer: %+v", more)
	}
	// the assistant text between them stays its own item, after the group
	_, waiting := find("waiting")
	if waiting.Item == cmdStart.Item || waiting.Item < cmdStart.Item {
		t.Fatalf("assistant item %d vs started item %d", waiting.Item, cmdStart.Item)
	}
	// stopped: glyph from the remembered kind, grouped with its start
	_, stopped := find("**Job stopped** (unmonitor)")
	if stopped.Item != watchStart.Item || stopped.Kind != transcript.LineDim {
		t.Fatalf("stopped: %+v (watch item %d)", stopped, watchStart.Item)
	}
	// error outcome swaps the glyph for ✗
	_, errFired := find("timer elapsed")
	if errFired.Item != timerStart.Item {
		t.Fatalf("error fired: %+v (timer item %d)", errFired, timerStart.Item)
	}
	// the monitor_fired user message is a muted block labelled "job result"
	var label transcript.Line
	for _, l := range lines {
		if l.Kind == transcript.LineLabel && l.Text == "job result" {
			label = l
		}
	}
	if label.Kind != transcript.LineLabel || label.Block != transcript.BlockChild {
		t.Fatalf("monitor block label: %+v", label)
	}
	_, summary := find("Job \"go test\"")
	if summary.Kind != transcript.LineText || summary.Block != transcript.BlockChild || summary.Item != label.Item || summary.Lead {
		t.Fatalf("monitor block summary: %+v", summary)
	}

	// folding: every monitor item collapses to its started line (only tool
	// lines carry a +N tag); the monitor_fired block folds to the summary line.
	nonblank := func(out []string) []string {
		var r []string
		for _, l := range out {
			if strings.TrimSpace(l) != "" {
				r = append(r, l)
			}
		}
		return r
	}
	plain := nonblank(renderWith(lines, Options{Width: 80}))
	joined := strings.Join(plain, "\n")
	for _, want := range []string{"run the tests", "waiting", "all green", "Job go test", "Job src", "Job cooldown", "Job \"go test\" (m1): go test exited 0"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in\n%s", want, joined)
		}
	}
	for _, leak := range []string{"$ Job go test exited 0", "ok  a", "monitor stopped", "timer elapsed", "ok  b"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("folded monitor item leaked %q:\n%s", leak, joined)
		}
	}
	// the block label never becomes the folded line
	for _, l := range plain {
		if strings.TrimSpace(l) == "job result" {
			t.Fatalf("folded to the label line:\n%s", joined)
		}
	}
	// cursor on the command monitor previews the start, the fired line and
	// the first output line with a +N marker; expanded shows the collapsed
	// output rule (3 lines + "… +N lines")
	prev := strings.Join(nonblank(renderWith(lines, Options{Width: 80, Focused: true, Cursor: cmdStart.Item})), "\n")
	for _, want := range []string{"$ Job go test", "$ Job go test exited 0", "ok  a", "+"} {
		if !strings.Contains(prev, want) {
			t.Fatalf("cursor on monitor lacks %q:\n%s", want, prev)
		}
	}
	if strings.Contains(prev, "ok  c") {
		t.Fatalf("preview shows too much:\n%s", prev)
	}
	full := strings.Join(nonblank(renderWith(lines, Options{Width: 80, Focused: true, Cursor: cmdStart.Item, Expanded: map[int]bool{cmdStart.Item: true}})), "\n")
	for _, want := range []string{"$ Job go test exited 0", "ok  a", "ok  c", "ok  d", "ok  e"} {
		if !strings.Contains(full, want) {
			t.Fatalf("expanded monitor lacks %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "Job stopped") {
		t.Fatalf("expanded monitor shows other items:\n%s", full)
	}
	// /details shows the whole output and the user block's output lines
	all := strings.Join(nonblank(renderWith(lines, Options{Width: 80, Details: true})), "\n")
	for _, want := range []string{"ok  e", "Job stopped (unmonitor)", "timer elapsed", "ok  b"} {
		if !strings.Contains(all, want) {
			t.Fatalf("details lacks %q:\n%s", want, all)
		}
	}

	// a fired event for an unknown monitor is its own item, not lost
	tr2 := transcript.NewTranscript()
	tr2.Apply(mk(1, event.MonitorFired, event.MonitorFiredPayload{ID: "zz", Kind: "watch", Summary: "3 files changed", Output: "a.go"}))
	tr2.Apply(mk(2, event.MonitorStopped, event.MonitorRefPayload{ID: "yy", Reason: "kill"}))
	got := renderLines(tr2.All())
	assertSubsequence(t, got, []string{"$ Job 3 files changed", "  a.go", "$ Job stopped (kill)"})
	if tr2.Items() != 2 {
		t.Fatalf("items %d", tr2.Items())
	}
}

func TestWorkingIndicatorOnlyDuringTurn(t *testing.T) {
	tr := transcript.NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	render := func() string {
		s, _ := Lines(tr.All(), Options{Width: 80, NoFold: true, Spinner: "⠋", Working: tr.InTurn()})
		return stripANSI(s)
	}
	if tr.InTurn() || strings.Contains(render(), "working…") {
		t.Fatal("no turn yet")
	}
	tr.Apply(mk(1, event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(2, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "go"}))
	out := render()
	if !tr.InTurn() || !strings.HasSuffix(out, "\n\n⠋ working…") {
		t.Fatalf("mid-turn should end with the indicator:\n%s", out)
	}
	// blocked on a permission: an exclamation mark and a different label
	if s, _ := Lines(tr.All(), Options{Width: 80, NoFold: true, Spinner: "⠋", Working: true, Waiting: true}); !strings.HasSuffix(stripANSI(s), "\n\n! permission requested") || strings.Contains(s, "working") {
		t.Fatalf("waiting indicator:\n%s", stripANSI(s))
	}
	// the indicator is not an item: the cursor/expand bookkeeping ignores it
	if _, rows := Lines(tr.All(), Options{Width: 80, NoFold: true, Working: true}); len(rows) != tr.Items() {
		t.Fatalf("rows %d, items %d", len(rows), tr.Items())
	}
	// elapsed time grows with the clock; tokens add up over the turn
	tr.Apply(mk(21, event.Usage, event.UsagePayload{Turn: 1, Usage: model.Usage{InputTokens: 900, OutputTokens: 400, CacheReadTokens: 5000}}))
	tr.Apply(mk(22, event.Usage, event.UsagePayload{Turn: 1, Usage: model.Usage{InputTokens: 100, OutputTokens: 100}}))
	ref := time.Now()
	base, _ := tr.TurnStats(ref)
	if el, tok := tr.TurnStats(ref.Add(75 * time.Second)); el-base != 75*time.Second || tok != 1500 {
		t.Fatalf("turn stats: %v %d", el-base, tok)
	}
	if got := TurnStats(75*time.Second, 1500); got != "(1m15s · 2k tokens)" {
		t.Fatalf("stats: %q", got)
	}
	if s, _ := Lines(tr.All(), Options{Width: 80, NoFold: true, Spinner: "⠋", Working: true, Stats: "(3s · 0 tokens)"}); !strings.HasSuffix(stripANSI(s), "⠋ working… (3s · 0 tokens)") {
		t.Fatalf("stats suffix:\n%s", stripANSI(s))
	}
	tr.Apply(mk(3, event.TurnEnded, event.TurnEndedPayload{Turn: 1}))
	if tr.InTurn() || strings.Contains(render(), "working…") {
		t.Fatalf("after the turn the indicator must go:\n%s", render())
	}
	// an aborted turn (daemon restart) clears it too
	tr.Apply(mk(4, event.TurnStarted, event.TurnPayload{Turn: 2}))
	if _, tok := tr.TurnStats(time.Now()); tok != 0 {
		t.Fatalf("a new turn starts its token count over: %d", tok)
	}
	// each turn gets a horse-flavoured verb, stable within the turn
	if v := tr.TurnVerb(); v != transcript.TurnVerbs[1] {
		t.Fatalf("turn 2 verb %q, want %q", v, transcript.TurnVerbs[1])
	}
	if s, _ := Lines(tr.All(), Options{Width: 80, NoFold: true, Spinner: "⠋", Working: true, Verb: tr.TurnVerb()}); !strings.HasSuffix(stripANSI(s), "⠋ "+transcript.TurnVerbs[1]+"…") {
		t.Fatalf("verb on the indicator:\n%s", stripANSI(s))
	}
	tr.Apply(mk(5, event.TurnAborted, event.TurnPayload{Turn: 2}))
	if tr.InTurn() {
		t.Fatal("aborted turn should clear the indicator")
	}
}

func TestCursorMarkSkipsSpacingRows(t *testing.T) {
	markCursorForTest(t)
	lines := []transcript.Line{
		{Kind: transcript.LineText, Block: transcript.BlockUser, Lead: true, Text: "hi", Item: 0},
		{Kind: transcript.LineText, Text: "Hello there", Item: 1},
		{Kind: transcript.LineTool, Text: "Shell  ls", Item: 2, Tool: "shell"},
	}
	for cursor := 0; cursor < 3; cursor++ {
		out, _ := Lines(lines, Options{Width: 60, NoFold: true, Focused: true, Cursor: cursor})
		for _, row := range strings.Split(stripANSI(out), "\n") {
			if strings.TrimSpace(row) == GutterMark {
				t.Fatalf("cursor %d: the mark sits on a blank spacing row:\n%s", cursor, stripANSI(out))
			}
		}
		if !strings.Contains(stripANSI(out), GutterMark) {
			t.Fatalf("cursor %d: no mark at all:\n%s", cursor, stripANSI(out))
		}
	}
}

// markCursorForTest swaps the (background colour) cursor highlight for a
// visible gutter mark so assertions can see which rows carry the cursor.
func markCursorForTest(t *testing.T) {
	t.Helper()
	prev := SwapHighlight(func(s string, _ int) string { return GutterMark + s })
	t.Cleanup(func() { SwapHighlight(prev) })
}

func TestAgentResponseBlockReadsLikeAToolLine(t *testing.T) {
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "delegate"}))
	tr.Apply(mk(2, "a", event.UserMessage, event.UserMessagePayload{Kind: "agent_response", From: "scout", Text: "Repository survey complete.\nNo edits were needed."}))
	folded := renderWith(tr.All(), Options{Width: 80})
	if !contains(folded, "› @scout Repository survey complete. +1") {
		t.Fatalf("folded response should name itself and its sender:\n%s", strings.Join(folded, "\n"))
	}
	for _, l := range folded {
		if strings.Contains(l, "No edits were needed") {
			t.Fatalf("a folded response shows only its first line:\n%s", strings.Join(folded, "\n"))
		}
	}
	full := renderWith(tr.All(), Options{Width: 80, NoFold: true})
	assertSubsequence(t, full, []string{"› @scout Repository survey complete.", "  No edits were needed."})
}

func TestSentMessageShowsItsText(t *testing.T) {
	tr := transcript.NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(2, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "r1", Name: "message", Input: json.RawMessage(`{"to":"main","text":"Concise findings:\n- Go-only module"}`)}))
	tr.Apply(mk(3, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "r1", Name: "message", Output: "answer delivered to main"}))
	full := renderWith(tr.All(), Options{Width: 80, NoFold: true})
	assertSubsequence(t, full, []string{"‹ @main Concise findings:", "  - Go-only module"})
	for _, l := range full {
		if strings.Contains(l, "answer delivered to") {
			t.Fatalf("the bare tool result should not show:\n%s", strings.Join(full, "\n"))
		}
	}
	if folded := renderWith(tr.All(), Options{Width: 80}); !contains(folded, "‹ @main Concise findings: +1") {
		t.Fatalf("folded:\n%s", strings.Join(folded, "\n"))
	}
}

// TestTrackedLinesSurviveLaterItems: a prompt's "?" line, a call's line
// and its output are found by reference, not by index, so items committed
// in between (and output nested under an earlier call) never misplace them;
// a slice from All() is a snapshot later changes do not touch.
func TestTrackedLinesSurviveLaterItems(t *testing.T) {
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a", event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(2, "a", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"make"}`)}))
	tr.Apply(mk(3, "a", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c2", Name: "read", Input: json.RawMessage(`{"path":"x"}`)}))
	tr.Apply(mk(4, "a", event.PromptRequested, event.PromptRequestedPayload{ID: "p1", Kind: "permission", Tool: "shell"}))
	tr.Apply(mk(5, "a", event.PromptRequested, event.PromptRequestedPayload{ID: "q1", Kind: "question", Question: "which?"}))
	snapshot := tr.All()
	tr.Apply(mk(6, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c2", Name: "read", Output: "r1\nr2"}))
	tr.Apply(mk(7, "a", event.PromptAnswered, event.PromptAnsweredPayload{ID: "p1", Answer: "allow"}))
	tr.Apply(mk(8, "a", event.PromptWithdrawn, event.PromptRefPayload{ID: "q1"}))
	tr.Apply(mk(9, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "built"}))

	lines := tr.All()
	find := func(text string) transcript.Line {
		t.Helper()
		for _, l := range lines {
			if strings.Contains(l.Text, text) {
				return l
			}
		}
		t.Fatalf("no line %q in:\n%s", text, strings.Join(renderLines(lines), "\n"))
		return transcript.Line{}
	}
	perm, question := find("**Permission** shell"), find("**Question** which?")
	if perm.Glyph != transcript.GlyphPermission || perm.Tone != transcript.ToneNone {
		t.Fatalf("answered permission prompt: %+v", perm)
	}
	if question.Glyph != transcript.GlyphPrompt || question.Tone != transcript.ToneError {
		t.Fatalf("withdrawn question: %+v", question)
	}
	// an answer draws its prompt's mark: ! for a permission, ? for a question
	if a, w := find("**Answered** allow"), find("**Prompt withdrawn**"); a.Glyph != transcript.GlyphPermission || w.Glyph != transcript.GlyphAnswer {
		t.Fatalf("answer glyphs: answered %q withdrawn %q", a.Glyph, w.Glyph)
	}
	shell, built, read, r2 := find("make"), find("built"), find("x"), find("r2")
	if shell.Running || built.Item != shell.Item || r2.Item != read.Item || shell.Item == read.Item {
		t.Fatalf("output must nest under its own call: shell %+v built %+v read %+v r2 %+v", shell, built, read, r2)
	}
	first, last := transcript.ItemRange(tr.All(), shell.Item)
	if lines[first].Text != shell.Text || lines[last].Text != built.Text {
		t.Fatalf("shell item range %d..%d: %q..%q", first, last, lines[first].Text, lines[last].Text)
	}
	for _, l := range snapshot {
		if strings.Contains(l.Text, "built") || strings.Contains(l.Text, "r2") || (l.Glyph == transcript.GlyphPrompt && l.Tone != transcript.ToneWorking) {
			t.Fatalf("an earlier All() slice changed: %+v", l)
		}
	}
}

// showThinkingForTest turns the (off by default) thinking display on for
// one test.
func showThinkingForTest(t *testing.T) {
	t.Helper()
	transcript.ShowThinking = true
	t.Cleanup(func() { transcript.ShowThinking = false })
}

func firstOf(s string, _ map[int]RowRange) string { return s }

// TestFoldedToolCallIsOneRow: a folded call whose command is wider than the
// chat is cut to one row, so its +N marker ends that row.
func TestFoldedToolCallIsOneRow(t *testing.T) {
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"` + strings.Repeat("echo word ", 12) + `"}`)}))
	tr.Apply(mk(2, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "a\nb\nc\nd"}))
	var rows []string
	for _, r := range renderWith(tr.All(), Options{Width: 60}) {
		if strings.TrimSpace(r) != "" {
			rows = append(rows, r)
		}
	}
	if len(rows) != 1 || !strings.Contains(rows[0], "Shell") || !strings.HasSuffix(strings.TrimRight(rows[0], " "), "+4") || ansi.StringWidth(rows[0]) > 60 {
		t.Fatalf("folded call should be one row ending in +4:\n%s", strings.Join(rows, "\n"))
	}
}

// TestTurnGapsSpaceOnlyTurns: in an agent's chat nothing inside a turn is
// spaced, and one blank row separates turns; what happens between turns
// stays with the turn before, except a nudge, which opens the next.
func TestTurnGapsSpaceOnlyTurns(t *testing.T) {
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a", event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(2, "a", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "list files"}))
	tr.Apply(mk(3, "a", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}))
	tr.Apply(mk(4, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "a.go"}))
	tr.Apply(mk(5, "a", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "one file"}}}))
	tr.Apply(mk(6, "a", event.TurnEnded, event.TurnEndedPayload{Turn: 1, Reason: "end_turn"}))
	tr.Apply(mk(7, "a", event.TurnStarted, event.TurnPayload{Turn: 2}))
	tr.Apply(mk(8, "a", event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: "prompt", Text: "thanks"}))
	got := strings.Join(renderWith(tr.All(), Options{Width: 80, NoFold: true, TurnGaps: true}), "\n")
	want := "› @user list files\n$ Shell  ls\n  a.go\n§ one file\n\n› @user thanks"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// a nudge opens the turn it starts: a gap above it, none under it
	tr.Apply(mk(9, "a", event.TurnEnded, event.TurnEndedPayload{Turn: 2, Reason: "end_turn"}))
	tr.Apply(mk(10, "a", event.ReminderQueued, event.RepliesPayload{Parties: []string{"user"}, Names: []string{"user"}}))
	tr.Apply(mk(11, "a", event.TurnStarted, event.TurnPayload{Turn: 3}))
	tr.Apply(mk(12, "a", event.UserMessage, event.UserMessagePayload{Turn: 3, Kind: event.MsgReminder, Text: "[reminder from the harness] ..."}))
	tr.Apply(mk(13, "a", event.AssistantMessage, event.AssistantMessagePayload{Turn: 3, Blocks: []model.Block{{Type: model.BlockText, Text: "replying now"}}}))
	nudged := strings.Join(renderWith(tr.All(), Options{Width: 80, NoFold: true, TurnGaps: true}), "\n")
	if !strings.HasSuffix(nudged, "› @user thanks\n\n↻ Nudged owes a reply to you\n§ replying now") {
		t.Fatalf("nudge spacing:\n%s", nudged)
	}
	tr = transcript.NewTranscript()
	for _, ev := range []event.Event{
		mk(1, "a", event.TurnStarted, event.TurnPayload{Turn: 1}),
		mk(2, "a", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "list files"}),
		mk(3, "a", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}),
		mk(4, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "a.go"}),
		mk(5, "a", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "one file"}}}),
		mk(6, "a", event.TurnEnded, event.TurnEndedPayload{Turn: 1, Reason: "end_turn"}),
		mk(7, "a", event.TurnStarted, event.TurnPayload{Turn: 2}),
		mk(8, "a", event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: "prompt", Text: "thanks"}),
	} {
		tr.Apply(ev)
	}
	// a mode change between turns stays with the turn before the gap
	between := transcript.NewTranscript()
	for _, ev := range []event.Event{
		mk(1, "a", event.TurnStarted, event.TurnPayload{Turn: 1}),
		mk(2, "a", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "go"}),
		mk(3, "a", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "done"}}}),
		mk(4, "a", event.TurnEnded, event.TurnEndedPayload{Turn: 1, Reason: "end_turn"}),
		mk(5, "", event.SessionModeChanged, event.ModePayload{Mode: "auto"}),
		mk(6, "a", event.TurnStarted, event.TurnPayload{Turn: 2}),
		mk(7, "a", event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: "prompt", Text: "again"}),
	} {
		between.Apply(ev)
	}
	if got := strings.Join(renderWith(between.All(), Options{Width: 100, NoFold: true, TurnGaps: true}), "\n"); got != "› @user go\n§ done\n⇄ Mode → auto · allows inside the agent's directories, denies outside them\n\n› @user again" {
		t.Fatalf("a between-turn mode change:\n%s", got)
	}
	// the loader keeps one blank row above it
	working := strings.Join(renderWith(tr.All(), Options{Width: 80, NoFold: true, TurnGaps: true, Working: true, Spinner: "◐", Verb: "Trotting"}), "\n")
	if !strings.HasSuffix(working, "› @user thanks\n\n◐ Trotting…") {
		t.Fatalf("loader spacing:\n%s", working)
	}
}

// TestWhoColoursGlyphAndName: a line that names someone asks WhoStyle for
// their colour and draws the name without its bold markers.
func TestWhoColoursGlyphAndName(t *testing.T) {
	asked := map[string]bool{}
	whoStyle := func(name string) lipgloss.Style {
		asked[name] = true
		return lipgloss.NewStyle()
	}
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "look", From: "main"}))
	tr.Apply(mk(2, "a", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "message", Input: json.RawMessage(`{"to":"scout","text":"go"}`)}))
	tr.Apply(mk(3, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi"}))
	got := renderWith(tr.All(), Options{Width: 80, NoFold: true, WhoStyle: whoStyle})
	assertSubsequence(t, got, []string{"› @main look", "‹ @scout go", "› @user hi"})
	if !asked["main"] || !asked["scout"] || !asked["user"] {
		t.Fatalf("colours asked for %v", asked)
	}
}

// TestDeniedCallReadsOnItsLine: folded or not, a denied call is one line
// led by ✗, its reason next to the tool's name.
func TestDeniedCallReadsOnItsLine(t *testing.T) {
	tr := transcript.NewTranscript()
	tr.Apply(mk(1, "a", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"rm -rf build"}`)}))
	tr.Apply(mk(2, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "Permission denied by the user: not now", IsError: true, Denied: true}))
	for _, o := range []Options{{Width: 80, NoFold: true}, {Width: 80}} {
		var rows []string
		for _, r := range renderWith(tr.All(), o) {
			if strings.TrimSpace(r) != "" {
				rows = append(rows, r)
			}
		}
		if len(rows) != 1 || rows[0] != "✗ Shell (not now)  rm -rf build" {
			t.Fatalf("denied call rows (nofold %v): %q", o.NoFold, rows)
		}
	}
}
