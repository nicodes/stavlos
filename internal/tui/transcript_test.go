package tui

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func mk(seq int64, agent string, typ event.Type, payload any) event.Event {
	return event.Event{Seq: seq, Session: "s1", Agent: agent, Type: typ, Payload: event.MustPayload(payload)}
}

// renderLines renders (collapsed) and returns trimmed, ANSI-free lines.
func renderLines(lines []Line) []string {
	return renderWith(lines, RenderOpts{Width: 80, Spinner: "⠋", NoFold: true})
}

func renderWith(lines []Line, o RenderOpts) []string {
	out := strings.Split(stripANSI(Render(lines, o)), "\n")
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
		mk(4, "a1", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "bash", Input: json.RawMessage(`{"command": "sleep 100"}`)}),
		mk(5, "a1", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "bash", Output: "partial\n", Cancelled: true}),
		mk(6, "a1", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Model: "anthropic/claude-x", StopReason: "end_turn", Blocks: []model.Block{
			{Type: model.BlockThinking},
			{Type: model.BlockText, Text: "Done."},
		}}),
		mk(7, "a1", event.TurnEnded, event.TurnEndedPayload{Turn: 1, Reason: "cancelled"}),
		mk(8, "a1", event.AgentFinished, event.AgentFinishedPayload{Summary: "all good", Status: "success"}),
	}
	got := renderLines(Build(evs))
	assertSubsequence(t, got, []string{
		"› hello",
		"  world",
		"$ Bash  sleep 100 (cancelled)",
		"  partial",
		"◌ thinking…",
		"Done.",
		"◦ turn cancelled",
		"✓ finished · success",
		"all good",
	})
	for _, g := range got {
		if strings.Contains(g, "usage") || strings.Contains(g, "turn.started") || strings.Contains(g, "spawned") {
			t.Fatalf("unexpected line %q", g)
		}
	}
}

func TestRootSpawnKeepsTranscriptEmpty(t *testing.T) {
	tr := NewTranscript()
	tr.Apply(mk(1, "a1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "a1", Archetype: "coder", Label: "root"}))
	if !tr.Empty() {
		t.Fatalf("root spawn should not produce lines: %+v", tr.Lines)
	}
	tr.Apply(mk(2, "c1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "c1", Parent: "a1", Archetype: "explorer", Label: "scout", Model: "m", Task: "look around"}))
	got := renderLines(tr.All())
	assertSubsequence(t, got, []string{"⑂ spawned scout (explorer) · m", "task", "▹ look around"})
}

func TestUserMessageKinds(t *testing.T) {
	lines := Build([]event.Event{
		mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "steer", Text: "focus"}),
		mk(2, "a", event.UserMessage, event.UserMessagePayload{Kind: "child_finished", Text: "child done"}),
		mk(3, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi"}),
	})
	got := renderLines(lines)
	assertSubsequence(t, got, []string{"› focus", "agent response", "⑂ child done", "› hi"})
	for _, l := range got {
		if strings.TrimSpace(l) == "steer" {
			t.Fatalf("a steer should carry no title:\n%s", strings.Join(got, "\n"))
		}
	}

	// Blocks carry their kind so Render can pick the border color.
	var blocks []BlockKind
	for _, l := range lines {
		if l.Kind == LineText {
			blocks = append(blocks, l.Block)
		}
	}
	if len(blocks) != 3 || blocks[0] != BlockUser || blocks[1] != BlockChild || blocks[2] != BlockUser {
		t.Fatalf("block kinds: %v", blocks)
	}
	// Blank line before and after each block.
	if lines[0].Kind != LineBlank || lines[len(lines)-1].Kind != LineBlank {
		t.Fatalf("blocks must be padded with blank lines: %+v", lines)
	}
}

func TestUserBlockBorderAndWrap(t *testing.T) {
	text := "one two three four five six seven eight nine ten"
	got := renderWith(Build([]event.Event{
		mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi\n" + text}),
	}), RenderOpts{Width: 30, NoFold: true})
	if got[0] != "› hi" {
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

func TestToolLine(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"bash", `{"command":"git status"}`, "Bash  git status"},
		{"bash", `{"command":"ls\nfoo"}`, "Bash  ls foo"},
		{"read", `{"path":"internal/agent/turn.go","offset":1}`, "Read  internal/agent/turn.go"},
		{"edit", `{"path":"file.go","old_string":"a","new_string":"b"}`, "Edit  file.go"},
		{"write", `{"path":"x.go","content":"..."}`, "Write  x.go"},
		{"bash_async", `{"command":"go test ./..."}`, "Bash async  go test ./..."},
		{"bash_async_kill", `{"id":"m1"}`, "Bash async kill  m1"},
		{"apply_patch", `{"patch":"*** Begin Patch\n*** Update File: a.go\n-x\n+y\n*** Add File: b.md\n+hi\n*** Delete File: c.txt\n*** End Patch"}`, "Apply patch  a.go, b.md (+1 more)"},
		{"agent_create", `{"archetype":"explorer","label":"scout","task":"look"}`, "Agent create  scout (explorer)"},
		{"agent_prompt", `{"id":"ag_1","text":"go"}`, "Agent prompt  ag_1"},
		{"agent_steer", `{"id":"ag_1","text":"go"}`, "Agent steer  ag_1"},
		{"agent_cancel", `{"id":"ag_1"}`, "Agent cancel  ag_1"},
		{"agent_kill", `{"id":"ag_1"}`, "Agent kill  ag_1"},
		{"agent_response", `{"to":"ag_2","text":"found it"}`, "Agent response delivered  → ag_2"},
		{"skill", `{"name":"deploy"}`, "Skill  deploy"},
		{"agent_finish", `{"status":"success","summary":"x"}`, "Agent complete  success"},
		{"mystery", `{"a":1}`, `Mystery  {"a":1}`},
		{"bash", ``, "Bash"},
	}
	for _, c := range cases {
		if got := toolLine(c.name, json.RawMessage(c.input)); got != c.want {
			t.Errorf("toolLine(%s, %s) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
	long := toolLine("bash", json.RawMessage(`{"command":"`+strings.Repeat("x", 150)+`"}`))
	if !strings.HasSuffix(long, "…") || len([]rune(long)) > len([]rune("Bash  "))+maxArgChars+1 {
		t.Fatalf("args not truncated: %q", long)
	}
}

func TestToolStatesAndCollapsedOutput(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		sb.WriteString("line\n")
	}
	tr := NewTranscript()
	tr.Apply(mk(1, "a", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "read", Input: json.RawMessage(`{"path":"a.go"}`)}))
	got := renderLines(tr.All())
	if !contains(got, "◆ Read  a.go") {
		t.Fatalf("running tool shows its glyph (yellow):\n%s", strings.Join(got, "\n"))
	}
	if !tr.Running() {
		t.Fatal("Running() should be true while a call is open")
	}

	tr.Apply(mk(2, "a", event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c1", Name: "read", Output: sb.String(), IsError: true}))
	tr.Apply(mk(3, "a", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c2", Name: "write", Input: json.RawMessage(`{"path":"b.go"}`)}))
	tr.Apply(mk(4, "a", event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c2", Name: "write", Denied: true}))
	tr.Apply(mk(5, "a", event.Compacted, event.CompactedPayload{FromSeq: 1, ToSeq: 3}))
	if tr.Running() {
		t.Fatal("Running() should be false after all calls finished")
	}

	got = renderLines(tr.All())
	assertSubsequence(t, got, []string{"◆ Read  a.go", "  line", "  line", "  line", "  … +17 lines", "◆ Write  b.go (denied)"})
	if n := count(got, "  line"); n != maxOutputCollapsed {
		t.Fatalf("collapsed: want %d output lines, got %d", maxOutputCollapsed, n)
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

	expanded := renderWith(tr.All(), RenderOpts{Width: 80, Details: true})
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
	lines := outputLines(strings.TrimRight(strings.Repeat("x\n", 50), "\n"))
	collapsed := renderWith(lines, RenderOpts{Width: 80, NoFold: true})
	expanded := renderWith(lines, RenderOpts{Width: 80, Details: true})
	if count(collapsed, "  x") != maxOutputCollapsed || !contains(collapsed, "  … +47 lines") {
		t.Fatalf("collapsed:\n%s", strings.Join(collapsed, "\n"))
	}
	if count(expanded, "  x") != maxOutputExpanded || !contains(expanded, "  … +10 lines") {
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
	got := renderLines(Build([]event.Event{
		mk(1, "a", event.AssistantMessage, event.AssistantMessagePayload{Model: "openai/gpt-x", StopReason: "tool_use", Blocks: []model.Block{
			{Type: model.BlockText, Text: "# Plan\nSome **bold** text\n```go\nfmt.Println()\n```\n- item"},
		}}),
		mk(2, "a", event.TurnEnded, event.TurnEndedPayload{Reason: "error", Error: "boom"}),
		mk(3, "a", event.AgentKilled, nil),
	}))
	assertSubsequence(t, got, []string{"Plan", "Some bold text", " fmt.Println()", "- item", "! boom", "⊘ killed"})
	for _, g := range got {
		if strings.Contains(g, "```") || strings.Contains(g, "· gpt-x") {
			t.Fatalf("unexpected line %q (fences dropped; no model trailer on tool_use)", g)
		}
	}
	// Balanced markers are consumed; unbalanced ones are left alone.
	if got := stripANSI(inlineMarkdown("a **b** c", styleDim)); got != "a b c" {
		t.Fatalf("inline bold: %q", got)
	}
	if got := stripANSI(inlineMarkdown("a **b c", styleDim)); got != "a **b c" {
		t.Fatalf("unbalanced: %q", got)
	}
}

func TestStreamingBufferReplacedByAssistantMessage(t *testing.T) {
	showThinkingForTest(t)
	tr := NewTranscript()
	tr.Apply(mk(1, "a", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "hi"}))
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Thinking: "hmm"})
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Text: "Hel"})
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Text: "lo"})
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, ToolName: "bash"})

	got := renderLines(tr.All())
	assertSubsequence(t, got, []string{"› hi", "◌ thinking…", "Hello", "$ Bash"})
	if !tr.Streaming() || !tr.Running() {
		t.Fatal("expected a streaming buffer with a running tool")
	}

	tr.Apply(mk(2, "a", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{
		{Type: model.BlockThinking, Text: "one\ntwo\nthree\nfour"},
		{Type: model.BlockText, Text: "Hello"},
	}}))
	if tr.Streaming() {
		t.Fatal("buffer should be cleared by assistant.message")
	}
	got = renderLines(tr.All())
	assertSubsequence(t, got, []string{"◌ one", "Hello"})
	for _, g := range got {
		if g == "$ Bash" || g == "◌ thinking…" || g == "two" {
			t.Fatalf("stale stream line %q", g)
		}
	}
}

func TestStreamingToolOutputCollapses(t *testing.T) {
	tr := NewTranscript()
	tr.Apply(mk(1, "a", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"make"}`)}))
	for i := 0; i < 5; i++ {
		tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, ToolName: "bash", Text: "out\n"})
	}
	got := renderLines(tr.All())
	assertSubsequence(t, got, []string{"$ Bash  make", "  out", "  … +2 lines"})
	if count(got, "  out") != maxOutputCollapsed {
		t.Fatalf("live output should be collapsed:\n%s", strings.Join(got, "\n"))
	}
	tr.Apply(mk(2, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "bash", Output: "final"}))
	got = renderLines(tr.All())
	assertSubsequence(t, got, []string{"$ Bash  make", "  final"})
	if count(got, "  out") != 0 {
		t.Fatal("live output should be replaced by the final output")
	}
}

func TestTranscriptItemsGroupEventLines(t *testing.T) {
	tr := NewTranscript()
	evs := []event.Event{
		mk(1, "c1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "c1", Parent: "a1", Archetype: "explorer", Label: "scout", Model: "m", Task: "look"}),
		mk(2, "c1", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "hello\nworld"}),
		mk(3, "c1", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "k1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}),
		mk(4, "c1", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "k1", Name: "bash", Output: "a\nb\nc\nd\ne"}),
		mk(5, "c1", event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Model: "p/m", StopReason: "end_turn", Blocks: []model.Block{{Type: model.BlockText, Text: "Done."}}}),
		mk(6, "c1", event.AgentFinished, event.AgentFinishedPayload{Summary: "ok", Status: "success"}),
	}
	for _, ev := range evs {
		tr.Apply(ev)
	}
	if n := tr.Items(); n != 5 {
		t.Fatalf("items: got %d, want 5 (spawn, user, tool, assistant, finished)", n)
	}
	lines := tr.All()
	kinds := func(item int) map[LineKind]int {
		out := map[LineKind]int{}
		for _, l := range lines {
			if l.Item == item {
				out[l.Kind]++
			}
		}
		return out
	}
	if k := kinds(0); k[LineDim] != 1 || k[LineLabel] != 1 || k[LineText] != 1 {
		t.Fatalf("spawn item: %v", k)
	}
	if k := kinds(1); k[LineText] != 2 || k[LineBlank] != 2 {
		t.Fatalf("user item: %v", k)
	}
	// The tool's output lines share the tool's item.
	if k := kinds(2); k[LineTool] != 1 || k[LineToolOut] != 6 {
		t.Fatalf("tool item: %v", k)
	}
	if k := kinds(3); k[LineText] != 1 || k[LineModel] != 0 || k[LineBlank] != 1 {
		t.Fatalf("assistant item (no model trailer): %v", k)
	}
	if k := kinds(4); k[LineFinished] != 1 || k[LineText] != 1 {
		t.Fatalf("finished item: %v", k)
	}
	if first, last := tr.ItemRange(2); first < 0 || lines[first].Kind != LineTool || lines[last].Kind != LineToolOut || last-first != 6 {
		t.Fatalf("tool range: %d..%d", first, last)
	}
	if first, last := tr.ItemRange(9); first != -1 || last != -1 {
		t.Fatalf("missing item range: %d..%d", first, last)
	}
	// Items are contiguous, in order, and never skip an index.
	prev := -1
	for _, l := range lines {
		if l.Item < 0 || l.Item > prev+1 {
			t.Fatalf("item %d after %d", l.Item, prev)
		}
		if l.Item > prev {
			prev = l.Item
		}
	}

	// The streaming buffer is the in-progress item after the committed ones.
	tr.ApplyStream(protocol.StreamNotification{Agent: "c1", Turn: 2, Text: "more"})
	if tr.Items() != 6 || tr.All()[len(tr.All())-1].Item != 5 {
		t.Fatalf("stream item: items=%d", tr.Items())
	}
	// A notice is one item regardless of its line count.
	tr.Apply(mk(7, "c1", event.TurnEnded, event.TurnEndedPayload{Turn: 2}))
	tr.Notice("a", "b", "c")
	if tr.Items() != 6 {
		t.Fatalf("notice item: items=%d", tr.Items())
	}
}

func TestRenderCursorAndPerItemExpand(t *testing.T) {
	markCursorForTest(t)
	tr := NewTranscript()
	tr.Apply(mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi"}))
	tr.Apply(mk(2, "a", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}))
	tr.Apply(mk(3, "a", event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c1", Name: "bash", Output: strings.TrimRight(strings.Repeat("x\n", 6), "\n")}))
	lines := tr.All()

	plain := renderWith(lines, RenderOpts{Width: 80, NoFold: true})
	for _, l := range plain {
		if strings.Contains(l, gutterMark) {
			t.Fatalf("no cursor without focus: %q", l)
		}
	}
	got := renderWith(lines, RenderOpts{Width: 80, Cursor: 1, Focused: true})
	if !contains(got, gutterMark+"$ Bash  ls") || !contains(got, gutterMark+"  x") || contains(got, gutterMark+"│  hi") {
		t.Fatalf("cursor marks only item 1:\n%s", strings.Join(got, "\n"))
	}
	if !contains(got, "› hi") {
		t.Fatalf("non-cursor lines keep the gutter space:\n%s", strings.Join(got, "\n"))
	}
	// Per-item override expands item 1 while /details is off, and vice versa.
	exp := renderWith(lines, RenderOpts{Width: 80, NoFold: true, Expanded: map[int]bool{1: true}})
	if count(exp, "  x") != 6 || contains(exp, "    … +3 lines") {
		t.Fatalf("expanded override:\n%s", strings.Join(exp, "\n"))
	}
	col := renderWith(lines, RenderOpts{Width: 80, Details: true, Expanded: map[int]bool{1: false}})
	if count(col, "  x") != maxOutputCollapsed {
		t.Fatalf("collapsed override:\n%s", strings.Join(col, "\n"))
	}
	_, rows := renderAll(lines, RenderOpts{Width: 80, NoFold: true})
	// user block on row 0 (no leading blank at the top), one blank row of
	// spacing, then the tool item: line, 3 output lines, trailer
	if r := rows[0]; r.first != 0 || r.last != 0 {
		t.Fatalf("user rows: %+v (want 0..0)", r)
	}
	if r := rows[1]; r.first != 2 || r.last != 6 {
		t.Fatalf("tool rows: %+v (want 2..6: tool line, 3 output lines, trailer)", r)
	}
}

func TestToolOutputStaysWithItsCall(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(2, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "run it"}))
	tr.Apply(mk(3, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"make test"}`)}))
	tr.Apply(mk(4, event.PromptRequested, event.PromptRequestedPayload{ID: "p1", Kind: "permission", Tool: "bash"}))
	tr.Apply(mk(5, event.PromptAnswered, event.PromptAnsweredPayload{ID: "p1", Answer: "allow"}))
	tr.Apply(mk(6, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "bash", Output: "ok\nall passed"}))
	tr.Apply(mk(7, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "done"}}}))

	lines := tr.All()
	toolIdx := -1
	for i, l := range lines {
		if l.Kind == LineTool {
			toolIdx = i
		}
	}
	if toolIdx < 0 {
		t.Fatal("no tool line")
	}
	toolItem := lines[toolIdx].Item
	first, last := tr.ItemRange(toolItem)
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
	reqAt, ansAt, outAt := strings.Index(joined, "permission"), strings.Index(joined, "allow"), strings.Index(joined, "all passed")
	if reqAt < 0 || ansAt < 0 || outAt < 0 || !(reqAt < ansAt && ansAt < outAt) {
		t.Fatalf("order within tool item wrong (req %d, ans %d, out %d):\n%s", reqAt, ansAt, outAt, joined)
	}
	// the notices are nested under the call at the output's indent
	rendered := renderWith(lines, RenderOpts{Width: 80, Focused: true, Cursor: toolItem})
	for _, r := range rendered {
		if strings.Contains(r, "permission") || strings.Contains(r, "answered") {
			if !strings.HasPrefix(strings.TrimLeft(r, "▍"), "  ") {
				t.Fatalf("notice not indented under the call: %q", r)
			}
		}
	}
	// nothing of the tool item, and no permission notice, lives outside it
	for i := last + 1; i < len(lines); i++ {
		if lines[i].Item == toolItem || strings.Contains(lines[i].Text, "permission") {
			t.Fatalf("tool item content after its range at %d: %q", i, lines[i].Text)
		}
	}
}

func TestThinkingIsItsOwnItem(t *testing.T) {
	showThinkingForTest(t)
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "hi"}))
	tr.Apply(mk(2, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Model: "openai/gpt-5.4", Blocks: []model.Block{
		{Type: model.BlockThinking, Text: "let me see"},
		{Type: model.BlockText, Text: "here is the answer"},
	}}))
	lines := tr.All()
	var thinkItem, textItem, userItem = -1, -1, -1
	for _, l := range lines {
		switch {
		case l.Kind == LineThink:
			thinkItem = l.Item
		case l.Kind == LineText && strings.Contains(l.Text, "answer"):
			textItem = l.Item
		case l.Block == BlockUser && strings.Contains(l.Text, "hi"):
			userItem = l.Item
		}
	}
	if thinkItem < 0 || textItem < 0 || userItem < 0 {
		t.Fatalf("missing lines: think=%d text=%d user=%d", thinkItem, textItem, userItem)
	}
	if !(userItem < thinkItem && thinkItem < textItem) {
		t.Fatalf("items not separate/in order: user=%d think=%d text=%d", userItem, thinkItem, textItem)
	}
	if tr.Items() != 3 {
		t.Fatalf("items %d", tr.Items())
	}
	// streaming: a thinking delta then text form two in-progress items
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 2, Thinking: "hmm"})
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 2, Text: "so far"})
	all := tr.All()
	last := all[len(all)-1]
	prev := all[len(all)-2]
	if prev.Kind != LineThink || last.Kind != LineStream || prev.Item == last.Item {
		t.Fatalf("stream items: prev=%+v last=%+v", prev, last)
	}
}

func TestFoldingToOneLine(t *testing.T) {
	showThinkingForTest(t)
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "line one\nline two"}))
	tr.Apply(mk(2, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockThinking, Text: "first thought\nsecond thought"}}}))
	tr.Apply(mk(3, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}))
	tr.Apply(mk(4, event.PromptRequested, event.PromptRequestedPayload{ID: "p1", Kind: "permission", Tool: "bash"}))
	tr.Apply(mk(5, event.PromptAnswered, event.PromptAnsweredPayload{ID: "p1", Answer: "allow"}))
	tr.Apply(mk(6, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "bash", Output: "a\nb\nc\nd\ne"}))
	tr.Apply(mk(7, event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: "child_finished", Text: "Child agent \"scout\"finished.\n\nfound it"}))
	tr.Apply(mk(8, event.AssistantMessage, event.AssistantMessagePayload{Turn: 2, Model: "openai/gpt-5.4", Blocks: []model.Block{{Type: model.BlockText, Text: "final answer\nwith two lines"}}}))
	lines := tr.All()
	toolItem, childItem := -1, -1
	for _, l := range lines {
		if l.Kind == LineTool {
			toolItem = l.Item
		}
		if l.Block == BlockChild && childItem < 0 {
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
	plain := nonblank(renderWith(lines, RenderOpts{Width: 80}))
	joined := strings.Join(plain, "\n")
	// user input and final response in full
	for _, want := range []string{"line one", "line two", "final answer", "with two lines"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in\n%s", want, joined)
		}
	}
	// thinking, tool (with its notices and output) and child result fold to one line each
	if strings.Contains(joined, "second thought") || strings.Contains(joined, "permission") || strings.Contains(joined, "found it") {
		t.Fatalf("folded items leaked lines:\n%s", joined)
	}
	toolRows := 0
	for _, l := range plain {
		if strings.Contains(l, "Bash") {
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
	prev := nonblank(renderWith(lines, RenderOpts{Width: 80, Focused: true, Cursor: toolItem}))
	joinedPrev := strings.Join(prev, "\n")
	if !strings.Contains(joinedPrev, "permission") || !strings.Contains(joinedPrev, "allow") || strings.Contains(joinedPrev, "found it") {
		t.Fatalf("cursor on tool:\n%s", joinedPrev)
	}
	toolPrev := 0
	for _, l := range prev {
		if strings.Contains(l, "Bash") || strings.Contains(l, "permission") || strings.Contains(l, "allow") || strings.HasPrefix(strings.TrimLeft(l, "▍ "), "a") {
			toolPrev++
		}
	}
	if toolPrev > previewLines || !strings.Contains(joinedPrev, "+") {
		t.Fatalf("preview should be at most %d lines with a +N marker:\n%s", previewLines, joinedPrev)
	}
	full := strings.Join(nonblank(renderWith(lines, RenderOpts{Width: 80, Focused: true, Cursor: toolItem, Expanded: map[int]bool{toolItem: true}})), "\n")
	if !strings.Contains(full, "permission") || !strings.Contains(full, "allow") || !strings.Contains(full, "\n") || strings.Count(full, "\n") < 5 {
		t.Fatalf("expanded tool:\n%s", full)
	}
	child := strings.Join(nonblank(renderWith(lines, RenderOpts{Width: 80, Focused: true, Cursor: childItem})), "\n")
	if !strings.Contains(child, "found it") || strings.Contains(child, "permission") {
		t.Fatalf("cursor on child:\n%s", child)
	}
	// /details shows everything
	all := strings.Join(nonblank(renderWith(lines, RenderOpts{Width: 80, Details: true})), "\n")
	if !strings.Contains(all, "found it") || !strings.Contains(all, "permission") || !strings.Contains(all, "e\n") && !strings.HasSuffix(all, "e") {
		t.Fatalf("details:\n%s", all)
	}
}

func TestMonitorEventsGroupAndFold(t *testing.T) {
	tr := NewTranscript()
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
	find := func(text string) (int, Line) {
		for i, l := range lines {
			if strings.Contains(l.Text, text) {
				return i, l
			}
		}
		t.Fatalf("no line containing %q in:\n%s", text, strings.Join(renderLines(lines), "\n"))
		return -1, Line{}
	}
	// started notices
	_, cmdStart := find("job: go test")
	_, watchStart := find("job: src")
	_, timerStart := find("job: cooldown")
	for _, l := range []Line{cmdStart, watchStart, timerStart} {
		if l.Kind != LineDim || l.Running {
			t.Fatalf("started notice should be a static dim line: %+v", l)
		}
	}
	// fired output joins the started item and sits right under it, collapsed
	firedAt, fired := find("go test exited 0")
	if fired.Item != cmdStart.Item || fired.Kind != LineDim {
		t.Fatalf("fired line item %d != started item %d (%+v)", fired.Item, cmdStart.Item, fired)
	}
	first, last := tr.ItemRange(cmdStart.Item)
	for i := first; i <= last; i++ {
		if lines[i].Item != cmdStart.Item {
			t.Fatalf("started item not contiguous at %d: %+v", i, lines[i])
		}
	}
	outAt, out := find("ok  a")
	if out.Kind != LineToolOut || out.Item != cmdStart.Item || outAt != firedAt+1 {
		t.Fatalf("output line: %+v at %d (fired at %d)", out, outAt, firedAt)
	}
	_, more := find("… +2 lines")
	if more.Vis != VisCollapsed || more.Item != cmdStart.Item {
		t.Fatalf("collapsed trailer: %+v", more)
	}
	// the assistant text between them stays its own item, after the group
	_, waiting := find("waiting")
	if waiting.Item == cmdStart.Item || waiting.Item < cmdStart.Item {
		t.Fatalf("assistant item %d vs started item %d", waiting.Item, cmdStart.Item)
	}
	// stopped: glyph from the remembered kind, grouped with its start
	_, stopped := find("job stopped (unmonitor)")
	if stopped.Item != watchStart.Item || stopped.Kind != LineDim {
		t.Fatalf("stopped: %+v (watch item %d)", stopped, watchStart.Item)
	}
	// error outcome swaps the glyph for ✗
	_, errFired := find("timer elapsed")
	if errFired.Item != timerStart.Item {
		t.Fatalf("error fired: %+v (timer item %d)", errFired, timerStart.Item)
	}
	// the monitor_fired user message is a muted block labelled "bash async result"
	var label Line
	for _, l := range lines {
		if l.Kind == LineLabel && l.Text == "bash async result" {
			label = l
		}
	}
	if label.Kind != LineLabel || label.Block != BlockChild {
		t.Fatalf("monitor block label: %+v", label)
	}
	_, summary := find("Job \"go test\"")
	if summary.Kind != LineText || summary.Block != BlockChild || summary.Item != label.Item || summary.Lead {
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
	plain := nonblank(renderWith(lines, RenderOpts{Width: 80}))
	joined := strings.Join(plain, "\n")
	for _, want := range []string{"run the tests", "waiting", "all green", "job: go test", "job: src", "job: cooldown", "Job \"go test\" (m1): go test exited 0"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in\n%s", want, joined)
		}
	}
	for _, leak := range []string{"$ go test exited 0", "ok  a", "monitor stopped", "timer elapsed", "ok  b"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("folded monitor item leaked %q:\n%s", leak, joined)
		}
	}
	// the block label never becomes the folded line
	for _, l := range plain {
		if strings.TrimSpace(l) == "bash async result" {
			t.Fatalf("folded to the label line:\n%s", joined)
		}
	}
	// cursor on the command monitor previews the start, the fired line and
	// the first output line with a +N marker; expanded shows the collapsed
	// output rule (3 lines + "… +N lines")
	prev := strings.Join(nonblank(renderWith(lines, RenderOpts{Width: 80, Focused: true, Cursor: cmdStart.Item})), "\n")
	for _, want := range []string{"$ job: go test", "$ go test exited 0", "ok  a", "+"} {
		if !strings.Contains(prev, want) {
			t.Fatalf("cursor on monitor lacks %q:\n%s", want, prev)
		}
	}
	if strings.Contains(prev, "ok  c") {
		t.Fatalf("preview shows too much:\n%s", prev)
	}
	full := strings.Join(nonblank(renderWith(lines, RenderOpts{Width: 80, Focused: true, Cursor: cmdStart.Item, Expanded: map[int]bool{cmdStart.Item: true}})), "\n")
	for _, want := range []string{"$ go test exited 0", "ok  a", "ok  c", "ok  d", "ok  e"} {
		if !strings.Contains(full, want) {
			t.Fatalf("expanded monitor lacks %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "job stopped") {
		t.Fatalf("expanded monitor shows other items:\n%s", full)
	}
	// /details shows the whole output and the user block's output lines
	all := strings.Join(nonblank(renderWith(lines, RenderOpts{Width: 80, Details: true})), "\n")
	for _, want := range []string{"ok  e", "job stopped (unmonitor)", "timer elapsed", "ok  b"} {
		if !strings.Contains(all, want) {
			t.Fatalf("details lacks %q:\n%s", want, all)
		}
	}

	// a fired event for an unknown monitor is its own item, not lost
	tr2 := NewTranscript()
	tr2.Apply(mk(1, event.MonitorFired, event.MonitorFiredPayload{ID: "zz", Kind: "watch", Summary: "3 files changed", Output: "a.go"}))
	tr2.Apply(mk(2, event.MonitorStopped, event.MonitorRefPayload{ID: "yy", Reason: "kill"}))
	got := renderLines(tr2.All())
	assertSubsequence(t, got, []string{"$ 3 files changed", "  a.go", "$ job stopped (kill)"})
	if tr2.Items() != 2 {
		t.Fatalf("items %d", tr2.Items())
	}
}

func TestAsyncJobJoinsItsCallLine(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "run the tests"}))
	tr.Apply(mk(2, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "bash_async", Input: json.RawMessage(`{"command":"go test ./..."}`)}))
	tr.Apply(mk(3, event.MonitorStarted, event.MonitorStartedPayload{ID: "m1", Kind: "command", Label: "go test ./...", Spec: "go test ./..."}))
	tr.Apply(mk(4, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "bash_async", Output: "started job m1"}))
	tr.Apply(mk(5, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "waiting"}}}))
	lines := tr.All()
	call := -1
	for i, l := range lines {
		if l.Kind == LineTool {
			call = i
		}
		if strings.HasPrefix(l.Text, "job:") {
			t.Fatalf("separate job line should not exist: %q", l.Text)
		}
	}
	if call < 0 || lines[call].Tone != ToneWorking {
		t.Fatalf("call line should be marked working: %+v", lines[call])
	}
	tr.Apply(mk(6, event.MonitorFired, event.MonitorFiredPayload{ID: "m1", Kind: "command", Label: "go test ./...", Summary: "exited 1", Output: "FAIL", IsError: true, ExitCode: 1}))
	lines = tr.All()
	if lines[call].Tone != ToneError {
		t.Fatalf("call line should be red after a failed job: %+v", lines[call])
	}
	first, last := tr.ItemRange(lines[call].Item)
	joined := ""
	for i := first; i <= last; i++ {
		joined += lines[i].Text + "\n"
	}
	if !strings.Contains(joined, "exited 1") || !strings.Contains(joined, "FAIL") {
		t.Fatalf("job outcome should nest under the call:\n%s", joined)
	}
	if strings.Contains(joined, "waiting") {
		t.Fatalf("assistant text leaked into the call item:\n%s", joined)
	}
}

func TestWorkingIndicatorOnlyDuringTurn(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	render := func() string {
		s, _ := renderAll(tr.All(), RenderOpts{Width: 80, NoFold: true, Spinner: "⠋", Working: tr.InTurn()})
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
	if s, _ := renderAll(tr.All(), RenderOpts{Width: 80, NoFold: true, Spinner: "⠋", Working: true, Waiting: true}); !strings.HasSuffix(stripANSI(s), "\n\n! permission requested") || strings.Contains(s, "working") {
		t.Fatalf("waiting indicator:\n%s", stripANSI(s))
	}
	// the indicator is not an item: the cursor/expand bookkeeping ignores it
	if _, rows := renderAll(tr.All(), RenderOpts{Width: 80, NoFold: true, Working: true}); len(rows) != tr.Items() {
		t.Fatalf("rows %d, items %d", len(rows), tr.Items())
	}
	// elapsed time and tokens for the turn
	tr.Apply(mk(21, event.Usage, event.UsagePayload{Turn: 1, Usage: model.Usage{InputTokens: 900, OutputTokens: 400, CacheReadTokens: 5000}}))
	tr.Apply(mk(22, event.Usage, event.UsagePayload{Turn: 1, Usage: model.Usage{InputTokens: 100, OutputTokens: 100}}))
	if el, tok := tr.TurnStats(tr.turnStart.Add(75 * time.Second)); el != 75*time.Second || tok != 1500 {
		t.Fatalf("turn stats: %v %d", el, tok)
	}
	if got := turnStats(75*time.Second, 1500); got != "(1m15s · 2k tokens)" {
		t.Fatalf("stats: %q", got)
	}
	if s, _ := renderAll(tr.All(), RenderOpts{Width: 80, NoFold: true, Spinner: "⠋", Working: true, Stats: "(3s · 0 tokens)"}); !strings.HasSuffix(stripANSI(s), "⠋ working… (3s · 0 tokens)") {
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
	if v := tr.TurnVerb(); v != turnVerbs[1] {
		t.Fatalf("turn 2 verb %q, want %q", v, turnVerbs[1])
	}
	if s, _ := renderAll(tr.All(), RenderOpts{Width: 80, NoFold: true, Spinner: "⠋", Working: true, Verb: tr.TurnVerb()}); !strings.HasSuffix(stripANSI(s), "⠋ "+turnVerbs[1]+"…") {
		t.Fatalf("verb on the indicator:\n%s", stripANSI(s))
	}
	tr.Apply(mk(5, event.TurnAborted, event.TurnPayload{Turn: 2}))
	if tr.InTurn() {
		t.Fatal("aborted turn should clear the indicator")
	}
}

// showThinkingForTest turns the (off by default) thinking display on for
// one test.
func showThinkingForTest(t *testing.T) {
	t.Helper()
	ShowThinking = true
	t.Cleanup(func() { ShowThinking = false })
}

func TestThinkingHiddenByDefault(t *testing.T) {
	if ShowThinking {
		t.Fatal("thinking should be hidden by default")
	}
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Thinking: "hmm"})
	if !tr.Empty() {
		t.Fatalf("a thinking delta should add nothing: %+v", tr.All())
	}
	tr.Apply(mk(2, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockThinking, Text: "let me see"}, {Type: model.BlockText, Text: "Hello"}}}))
	sawText := false
	for _, l := range tr.All() {
		if l.Kind == LineThink || strings.Contains(l.Text, "let me see") {
			t.Fatalf("thinking leaked into the chat: %+v", l)
		}
		if strings.Contains(l.Text, "Hello") {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("text should still show: %+v", tr.All())
	}
}

func TestCursorMarkSkipsSpacingRows(t *testing.T) {
	markCursorForTest(t)
	lines := []Line{
		{Kind: LineText, Block: BlockUser, Lead: true, Text: "hi", Item: 0},
		{Kind: LineText, Text: "Hello there", Item: 1},
		{Kind: LineTool, Text: "Bash  ls", Item: 2, tool: "bash"},
	}
	for cursor := 0; cursor < 3; cursor++ {
		out, _ := renderAll(lines, RenderOpts{Width: 60, NoFold: true, Focused: true, Cursor: cursor})
		for _, row := range strings.Split(stripANSI(out), "\n") {
			if strings.TrimSpace(row) == gutterMark {
				t.Fatalf("cursor %d: the mark sits on a blank spacing row:\n%s", cursor, stripANSI(out))
			}
		}
		if !strings.Contains(stripANSI(out), gutterMark) {
			t.Fatalf("cursor %d: no mark at all:\n%s", cursor, stripANSI(out))
		}
	}
}

// markCursorForTest swaps the (background colour) cursor highlight for a
// visible gutter mark so assertions can see which rows carry the cursor.
func markCursorForTest(t *testing.T) {
	t.Helper()
	prev := highlightRow
	highlightRow = func(s string, _ int) string { return gutterMark + s }
	t.Cleanup(func() { highlightRow = prev })
}

func TestAgentCreateLineTracksChild(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(2, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "agent_create", Input: json.RawMessage(`{"archetype":"explorer","label":"scout","task":"look"}`)}))
	tr.Apply(mk(3, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "agent_create", Output: "spawned scout (explorer) as x1"}))
	line := func() Line {
		for _, l := range tr.All() {
			if l.Kind == LineTool && l.tool == "agent_create" {
				return l
			}
		}
		t.Fatal("no agent_create line")
		return Line{}
	}
	if line().Tone != ToneNone {
		t.Fatalf("before the spawn: %v", line().Tone)
	}
	tr.ChildSpawned("x1")
	if line().Tone != ToneWorking {
		t.Fatalf("while the child runs the line should be working: %v", line().Tone)
	}
	// more lines after it do not lose the tie
	tr.Apply(mk(4, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c2", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}))
	tr.Apply(mk(5, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c2", Name: "bash", Output: "ok"}))
	tr.ChildState("x1", "idle")
	if line().Tone != ToneNone {
		t.Fatalf("once the child idles the line should be grey: %v", line().Tone)
	}
	tr.ChildState("x1", "running")
	if line().Tone != ToneWorking {
		t.Fatalf("a follow-up turn makes it yellow again: %v", line().Tone)
	}
	tr.ChildState("x1", "idle")
	// a second child that is killed turns its own line red; the first stays grey
	tr.Apply(mk(6, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c3", Name: "agent_create", Input: json.RawMessage(`{"archetype":"tester","label":"checks","task":"test"}`)}))
	tr.Apply(mk(7, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c3", Name: "agent_create", Output: "spawned checks (tester) as x2"}))
	tr.ChildSpawned("x2")
	tr.ChildState("x2", "killed")
	var tones []Tone
	for _, l := range tr.All() {
		if l.Kind == LineTool && l.tool == "agent_create" {
			tones = append(tones, l.Tone)
		}
	}
	if len(tones) != 2 || tones[0] != ToneNone || tones[1] != ToneError {
		t.Fatalf("tones %v", tones)
	}
}

func TestAgentResponseBlockReadsLikeAToolLine(t *testing.T) {
	tr := NewTranscript()
	tr.Apply(mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "delegate"}))
	tr.Apply(mk(2, "a", event.UserMessage, event.UserMessagePayload{Kind: "agent_response", From: "scout (a1b2c3d4)", Text: "Repository survey complete.\nNo edits were needed."}))
	folded := renderWith(tr.All(), RenderOpts{Width: 80})
	if !contains(folded, "⑂ Agent response received · scout (a1b2c3d4) +2") {
		t.Fatalf("folded response should name itself and its sender:\n%s", strings.Join(folded, "\n"))
	}
	for _, l := range folded {
		if strings.Contains(l, "Repository survey") {
			t.Fatalf("folded response should not lead with the answer text:\n%s", strings.Join(folded, "\n"))
		}
	}
	full := renderWith(tr.All(), RenderOpts{Width: 80, NoFold: true})
	assertSubsequence(t, full, []string{"⑂ Agent response received · scout (a1b2c3d4)", "Repository survey complete.", "No edits were needed."})
}

func TestAgentPromptLineWaitsForTheAnswer(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tone := func(callID string) Tone {
		for _, l := range tr.All() {
			if l.Kind == LineTool && l.tool == "agent_prompt" && strings.Contains(l.Text, callID) {
				return l.Tone
			}
		}
		t.Fatalf("no agent_prompt line for %s", callID)
		return ToneNone
	}
	tr.Apply(mk(1, event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(2, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "p1", Name: "agent_prompt", Input: json.RawMessage(`{"id":"a1b2c3d4e5f6","text":"which branch?"}`)}))
	tr.Apply(mk(3, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "p1", Name: "agent_prompt", Output: "queued"}))
	if tone("a1b2c3d4e5f6") != ToneWorking {
		t.Fatalf("a delivered prompt waits for its answer: %v", tone("a1b2c3d4e5f6"))
	}
	// a second question to a different agent
	tr.Apply(mk(4, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "p2", Name: "agent_prompt", Input: json.RawMessage(`{"id":"ffff00001111","text":"and you?"}`)}))
	tr.Apply(mk(5, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "p2", Name: "agent_prompt", Output: "queued"}))
	tr.Apply(mk(6, event.TurnEnded, event.TurnEndedPayload{Turn: 1}))
	// the first agent answers: only its line settles
	tr.Apply(mk(7, event.TurnStarted, event.TurnPayload{Turn: 2}))
	tr.Apply(mk(8, event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: "agent_response", From: "scout (a1b2c3d4)", Text: "main"}))
	if tone("a1b2c3d4e5f6") != ToneNone || tone("ffff00001111") != ToneWorking {
		t.Fatalf("answered → grey, unanswered → still yellow: %v %v", tone("a1b2c3d4e5f6"), tone("ffff00001111"))
	}
	// the second agent is killed before answering: red
	tr.AskerGone("ffff00001111")
	if tone("ffff00001111") != ToneError {
		t.Fatalf("killed before answering → red: %v", tone("ffff00001111"))
	}
	// two prompts to one agent, one answer: both settle (a re-prompt is
	// covered by the same reply)
	tr.Apply(mk(11, event.ToolCallStarted, event.ToolStartedPayload{Turn: 2, CallID: "p4", Name: "agent_prompt", Input: json.RawMessage(`{"id":"cafe00000001","text":"report"}`)}))
	tr.Apply(mk(12, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 2, CallID: "p4", Name: "agent_prompt", Output: "queued"}))
	tr.Apply(mk(13, event.ToolCallStarted, event.ToolStartedPayload{Turn: 2, CallID: "p5", Name: "agent_prompt", Input: json.RawMessage(`{"id":"cafe00000001","text":"send it now"}`)}))
	tr.Apply(mk(14, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 2, CallID: "p5", Name: "agent_prompt", Output: "queued"}))
	tr.Apply(mk(15, event.UserMessage, event.UserMessagePayload{Turn: 3, Kind: "agent_response", From: "inspector (cafe0000)", Text: "here"}))
	for _, l := range tr.All() {
		if l.Kind == LineTool && l.tool == "agent_prompt" && strings.Contains(l.Text, "cafe00000001") && l.Tone != ToneNone {
			t.Fatalf("one answer should settle both prompts to that agent: %q tone %v", l.Text, l.Tone)
		}
	}
	// a failed prompt never waits
	tr.Apply(mk(9, event.ToolCallStarted, event.ToolStartedPayload{Turn: 2, CallID: "p3", Name: "agent_prompt", Input: json.RawMessage(`{"id":"deadbeef0000","text":"?"}`)}))
	tr.Apply(mk(10, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 2, CallID: "p3", Name: "agent_prompt", Output: "unknown agent", IsError: true}))
	if tone("deadbeef0000") == ToneWorking {
		t.Fatal("a refused prompt has nothing to wait for")
	}
}

func TestDeliveredResponseShowsItsText(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(2, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "r1", Name: "agent_response", Input: json.RawMessage(`{"to":"a4e33e14942","text":"Concise findings:\n- Go-only module"}`)}))
	tr.Apply(mk(3, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "r1", Name: "agent_response", Output: "response delivered to a4e33e14942"}))
	full := renderWith(tr.All(), RenderOpts{Width: 80, NoFold: true})
	assertSubsequence(t, full, []string{"⑂ Agent response delivered  → a4e33e14942", "  Concise findings:", "  - Go-only module"})
	for _, l := range full {
		if strings.Contains(l, "response delivered to") {
			t.Fatalf("the bare tool result should not show:\n%s", strings.Join(full, "\n"))
		}
	}
	if folded := renderWith(tr.All(), RenderOpts{Width: 80}); !contains(folded, "⑂ Agent response delivered  → a4e33e14942 +2") {
		t.Fatalf("folded:\n%s", strings.Join(folded, "\n"))
	}
}
