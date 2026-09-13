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
	return renderWith(lines, RenderOpts{Width: 80, Spinner: "⠋"})
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
		" │  hello",
		" │  world",
		"   ↳ Bash  sleep 100 (cancelled)",
		"       partial",
		"   ∴ thinking…",
		"   Done.",
		"   · claude-x",
		"   · turn cancelled",
		" │  finished · success",
		" │  all good",
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
	assertSubsequence(t, got, []string{"   spawned scout (explorer) · m", " │  task", " │  look around"})
}

func TestUserMessageKinds(t *testing.T) {
	lines := Build([]event.Event{
		mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "steer", Text: "focus"}),
		mk(2, "a", event.UserMessage, event.UserMessagePayload{Kind: "child_finished", Text: "child done"}),
		mk(3, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi"}),
	})
	got := renderLines(lines)
	assertSubsequence(t, got, []string{" │  steer", " │  focus", " │  child", " │  child done", " │  hi"})

	// Blocks carry their kind so Render can pick the border color.
	var blocks []BlockKind
	for _, l := range lines {
		if l.Kind == LineText {
			blocks = append(blocks, l.Block)
		}
	}
	if len(blocks) != 3 || blocks[0] != BlockSteer || blocks[1] != BlockChild || blocks[2] != BlockUser {
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
	}), RenderOpts{Width: 30})
	if got[1] != " │  hi" {
		t.Fatalf("border + 2-space padding: %q", got[1])
	}
	// Long lines wrap inside the border; every continuation keeps it.
	body := got[2:]
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
	}
	if len(body) < 2 {
		t.Fatalf("expected wrapped lines:\n%s", strings.Join(got, "\n"))
	}
	for _, l := range body {
		if !strings.HasPrefix(l, " │  ") || len([]rune(l)) > 30 {
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
		{"grep", `{"pattern":"TODO","path":"."}`, "Grep  TODO"},
		{"glob", `{"pattern":"**/*.go"}`, "Glob  **/*.go"},
		{"spawn", `{"archetype":"explorer","label":"scout","task":"look"}`, "Spawn  scout (explorer)"},
		{"send", `{"id":"ag_1","text":"go"}`, "Send  ag_1"},
		{"steer", `{"id":"ag_1","text":"go"}`, "Steer  ag_1"},
		{"cancel", `{"id":"ag_1"}`, "Cancel  ag_1"},
		{"kill", `{"id":"ag_1"}`, "Kill  ag_1"},
		{"monitor", `{"ids":["a","b"]}`, "Monitor  a, b"},
		{"monitor", `{}`, "Monitor"},
		{"result", `{"id":"ag_2"}`, "Result  ag_2"},
		{"skill", `{"name":"deploy"}`, "Skill  deploy"},
		{"finish", `{"status":"success","summary":"x"}`, "Finish  success"},
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
	if !contains(got, "   ⠋ Read  a.go") {
		t.Fatalf("running tool should show the spinner:\n%s", strings.Join(got, "\n"))
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
	assertSubsequence(t, got, []string{"   ✗ Read  a.go", "       line", "       line", "       line", "       … +17 lines", "   ↳ Write  b.go (denied)"})
	if n := count(got, "       line"); n != maxOutputCollapsed {
		t.Fatalf("collapsed: want %d output lines, got %d", maxOutputCollapsed, n)
	}
	var rule string
	for _, g := range got {
		if strings.Contains(g, "compacted") {
			rule = g
		}
	}
	if strings.TrimSpace(rule) != "── compacted ──" || !strings.HasPrefix(rule, "    ") {
		t.Fatalf("compacted rule should be centered: %q", rule)
	}

	expanded := renderWith(tr.All(), RenderOpts{Width: 80, Details: true})
	if n := count(expanded, "       line"); n != 20 {
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
	collapsed := renderWith(lines, RenderOpts{Width: 80})
	expanded := renderWith(lines, RenderOpts{Width: 80, Details: true})
	if count(collapsed, "       x") != maxOutputCollapsed || !contains(collapsed, "       … +47 lines") {
		t.Fatalf("collapsed:\n%s", strings.Join(collapsed, "\n"))
	}
	if count(expanded, "       x") != maxOutputExpanded || !contains(expanded, "       … +10 lines") {
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
	assertSubsequence(t, got, []string{"   Plan", "   Some bold text", "     fmt.Println()", "   - item", " │  boom", " │  killed"})
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
	tr := NewTranscript()
	tr.Apply(mk(1, "a", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "hi"}))
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Thinking: "hmm"})
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Text: "Hel"})
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, Text: "lo"})
	tr.ApplyStream(protocol.StreamNotification{Agent: "a", Turn: 1, ToolName: "bash"})

	got := renderLines(tr.All())
	assertSubsequence(t, got, []string{" │  hi", "   ∴ thinking…", "   Hello", "   ⠋ Bash"})
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
	assertSubsequence(t, got, []string{"   ∴ one", "   Hello"})
	for _, g := range got {
		if g == "   ⠋ Bash" || g == "   ∴ thinking…" || g == "   two" {
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
	assertSubsequence(t, got, []string{"   ⠋ Bash  make", "       out", "       … +2 lines"})
	if count(got, "       out") != maxOutputCollapsed {
		t.Fatalf("live output should be collapsed:\n%s", strings.Join(got, "\n"))
	}
	tr.Apply(mk(2, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "bash", Output: "final"}))
	got = renderLines(tr.All())
	assertSubsequence(t, got, []string{"   ↳ Bash  make", "       final"})
	if count(got, "       out") != 0 {
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
	if k := kinds(3); k[LineText] != 1 || k[LineModel] != 1 {
		t.Fatalf("assistant item: %v", k)
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
	tr := NewTranscript()
	tr.Apply(mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi"}))
	tr.Apply(mk(2, "a", event.ToolCallStarted, event.ToolStartedPayload{CallID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}))
	tr.Apply(mk(3, "a", event.ToolCallFinished, event.ToolFinishedPayload{CallID: "c1", Name: "bash", Output: strings.TrimRight(strings.Repeat("x\n", 6), "\n")}))
	lines := tr.All()

	plain := renderWith(lines, RenderOpts{Width: 80})
	for _, l := range plain {
		if strings.Contains(l, gutterMark) {
			t.Fatalf("no cursor without focus: %q", l)
		}
	}
	got := renderWith(lines, RenderOpts{Width: 80, Cursor: 1, Focused: true})
	if !contains(got, gutterMark+"  ↳ Bash  ls") || !contains(got, gutterMark+"      x") || contains(got, gutterMark+"│  hi") {
		t.Fatalf("cursor marks only item 1:\n%s", strings.Join(got, "\n"))
	}
	if !contains(got, " │  hi") {
		t.Fatalf("non-cursor lines keep the gutter space:\n%s", strings.Join(got, "\n"))
	}
	// Per-item override expands item 1 while /details is off, and vice versa.
	exp := renderWith(lines, RenderOpts{Width: 80, Expanded: map[int]bool{1: true}})
	if count(exp, "       x") != 6 || contains(exp, "       … +3 lines") {
		t.Fatalf("expanded override:\n%s", strings.Join(exp, "\n"))
	}
	col := renderWith(lines, RenderOpts{Width: 80, Details: true, Expanded: map[int]bool{1: false}})
	if count(col, "       x") != maxOutputCollapsed {
		t.Fatalf("collapsed override:\n%s", strings.Join(col, "\n"))
	}
	_, rows := renderAll(lines, RenderOpts{Width: 80})
	if r := rows[1]; r.first != 3 || r.last != 7 {
		t.Fatalf("tool rows: %+v (want 3..7: tool line, 3 output lines, trailer)", r)
	}
	if r := rows[0]; r.first != 0 || r.last != 2 {
		t.Fatalf("user rows: %+v (want 0..2, blank padding included)", r)
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
	// every line in the item's range belongs to the item: output was spliced
	// in right after the call, ahead of the permission notices
	for i := first; i <= last; i++ {
		if lines[i].Item != toolItem {
			t.Fatalf("line %d (%q) inside tool item range belongs to item %d", i, lines[i].Text, lines[i].Item)
		}
	}
	if last-first < 2 || !strings.Contains(lines[last].Text, "all passed") {
		t.Fatalf("output not under the call: range %d..%d, last %q", first, last, lines[last].Text)
	}
	// and the notices come after the whole tool item
	for i := last + 1; i < len(lines); i++ {
		if lines[i].Kind == LineTool || lines[i].Item == toolItem {
			t.Fatalf("tool item content after its range at %d", i)
		}
	}
}
