package tui

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

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
		"│  hello",
		"│  world",
		"  ↳ Bash  sleep 100 (cancelled)",
		"      partial",
		"  ∴ thinking…",
		"  Done.",
		"  · claude-x",
		"  · turn cancelled",
		"│  finished · success",
		"│  all good",
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
	assertSubsequence(t, got, []string{"  spawned scout (explorer) · m", "│  task", "│  look around"})
}

func TestUserMessageKinds(t *testing.T) {
	lines := Build([]event.Event{
		mk(1, "a", event.UserMessage, event.UserMessagePayload{Kind: "steer", Text: "focus"}),
		mk(2, "a", event.UserMessage, event.UserMessagePayload{Kind: "child_finished", Text: "child done"}),
		mk(3, "a", event.UserMessage, event.UserMessagePayload{Kind: "prompt", Text: "hi"}),
	})
	got := renderLines(lines)
	assertSubsequence(t, got, []string{"│  steer", "│  focus", "│  child", "│  child done", "│  hi"})

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
	if got[1] != "│  hi" {
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
		if !strings.HasPrefix(l, "│  ") || len([]rune(l)) > 30 {
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
		{"monitor", `{"ids":["a","b"]}`, "Wait  a, b"},
		{"monitor", `{}`, "Wait"},
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
	if !contains(got, "  ⠋ Read  a.go") {
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
	assertSubsequence(t, got, []string{"  ✗ Read  a.go", "      line", "      line", "      line", "      … +17 lines", "  ↳ Write  b.go (denied)"})
	if n := count(got, "      line"); n != maxOutputCollapsed {
		t.Fatalf("collapsed: want %d output lines, got %d", maxOutputCollapsed, n)
	}
	var rule string
	for _, g := range got {
		if strings.Contains(g, "compacted") {
			rule = g
		}
	}
	if strings.TrimSpace(rule) != "── compacted ──" || !strings.HasPrefix(rule, "   ") {
		t.Fatalf("compacted rule should be centered: %q", rule)
	}

	expanded := renderWith(tr.All(), RenderOpts{Width: 80, Details: true})
	if n := count(expanded, "      line"); n != 20 {
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
	if count(collapsed, "      x") != maxOutputCollapsed || !contains(collapsed, "      … +47 lines") {
		t.Fatalf("collapsed:\n%s", strings.Join(collapsed, "\n"))
	}
	if count(expanded, "      x") != maxOutputExpanded || !contains(expanded, "      … +10 lines") {
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
	assertSubsequence(t, got, []string{"  Plan", "  Some bold text", "    fmt.Println()", "  - item", "│  boom", "│  killed"})
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
	assertSubsequence(t, got, []string{"│  hi", "  ∴ thinking…", "  Hello", "  ⠋ Bash"})
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
	assertSubsequence(t, got, []string{"  ∴ one", "  Hello"})
	for _, g := range got {
		if g == "  ⠋ Bash" || g == "  ∴ thinking…" || g == "  two" {
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
	assertSubsequence(t, got, []string{"  ⠋ Bash  make", "      out", "      … +2 lines"})
	if count(got, "      out") != maxOutputCollapsed {
		t.Fatalf("live output should be collapsed:\n%s", strings.Join(got, "\n"))
	}
	tr.Apply(mk(2, "a", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "bash", Output: "final"}))
	got = renderLines(tr.All())
	assertSubsequence(t, got, []string{"  ↳ Bash  make", "      final"})
	if count(got, "      out") != 0 {
		t.Fatal("live output should be replaced by the final output")
	}
}
