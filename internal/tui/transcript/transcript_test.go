package transcript

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

func mk(seq int64, agent string, typ event.Type, payload any) event.Event {
	return event.Event{Seq: seq, Session: "s1", Agent: agent, Type: typ, Payload: event.MustPayload(payload)}
}

// showThinkingForTest turns the (off by default) thinking display on for
// one test.
func showThinkingForTest(t *testing.T) {
	t.Helper()
	ShowThinking = true
	t.Cleanup(func() { ShowThinking = false })
}

func TestToolLine(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"shell", `{"command":"git status"}`, "Shell  git status"},
		{"shell", `{"command":"ls\nfoo"}`, "Shell  ls foo"},
		{"read", `{"path":"internal/agent/turn.go","offset":1}`, "Read  internal/agent/turn.go"},
		{"bash", `{"command":"ls"}`, "Shell  ls"}, // a log from before the rename reads as the current tool
		{"shell", `{"command":"go test ./..."}`, "Shell  go test ./..."},
		{"shell_kill", `{"id":"m1"}`, "Shell kill  m1"},
		{"apply_patch", `{"patch":"*** Begin Patch\n*** Update File: a.go\n-x\n+y\n*** Add File: b.md\n+hi\n*** Delete File: c.txt\n*** End Patch"}`, "Apply patch  a.go, b.md (+1 more)"},
		{"agent_create", `{"archetype":"explorer","label":"scout","task":"look"}`, "Agent create  scout (explorer)"},
		{"message", `{"to":"scout","text":"go"}`, "@scout go"},
		{"message", `{"to":"user","text":"done\nand more"}`, "@user done"},
		{"agent_message", `{"id":"ag_1","text":"go"}`, "@ag_1 go"}, // logs from before message
		{"agent_cancel", `{"id":"ag_1"}`, "Agent cancel  ag_1"},
		{"agent_response", `{"to":"ag_2","text":"found it"}`, "@ag_2 found it"},
		{"skill", `{"name":"deploy"}`, "Skill  deploy"},
		{"mystery", `{"a":1}`, `Mystery  {"a":1}`},
		{"shell", ``, "Shell"},
	}
	for _, c := range cases {
		if got := toolLine(c.name, json.RawMessage(c.input)); got != c.want {
			t.Errorf("toolLine(%s, %s) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
	long := toolLine("shell", json.RawMessage(`{"command":"`+strings.Repeat("x", 150)+`"}`))
	if !strings.HasSuffix(long, "…") || len([]rune(long)) > len([]rune("Shell  "))+maxArgChars+1 {
		t.Fatalf("args not truncated: %q", long)
	}
}

func TestTranscriptItemsGroupEventLines(t *testing.T) {
	tr := NewTranscript()
	evs := []event.Event{
		mk(1, "c1", event.AgentSpawned, event.AgentSpawnedPayload{ID: "c1", Parent: "a1", Archetype: "explorer", Label: "scout", Model: "m", Task: "look"}),
		mk(2, "c1", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "look", From: "main"}), // the task: drawn with the spawn
		mk(2, "c1", event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "hello\nworld"}),
		mk(3, "c1", event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "k1", Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}),
		mk(4, "c1", event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "k1", Name: "shell", Output: "a\nb\nc\nd\ne"}),
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
	if k := kinds(0); k[LineText] != 2 || k[LineDim] != 0 || k[LineLabel] != 0 || !strings.HasPrefix(lines[1].Text, "**Spawned by main** as scout (explorer) · m") {
		t.Fatalf("spawn item (the spawn over its task): %v %+v", k, lines[1])
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

func TestAsyncJobJoinsItsCallLine(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "run the tests"}))
	tr.Apply(mk(2, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c1", Name: "shell", Input: json.RawMessage(`{"command":"go test ./..."}`)}))
	tr.Apply(mk(3, event.MonitorStarted, event.MonitorStartedPayload{ID: "m1", Kind: "command", Label: "go test ./...", Spec: "go test ./..."}))
	tr.Apply(mk(4, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c1", Name: "shell", Output: "started job m1"}))
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
			if l.Kind == LineTool && l.Tool == "agent_create" {
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
	tr.Apply(mk(4, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: "c2", Name: "shell", Input: json.RawMessage(`{"command":"ls"}`)}))
	tr.Apply(mk(5, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: "c2", Name: "shell", Output: "ok"}))
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
		if l.Kind == LineTool && l.Tool == "agent_create" {
			tones = append(tones, l.Tone)
		}
	}
	if len(tones) != 2 || tones[0] != ToneNone || tones[1] != ToneError {
		t.Fatalf("tones %v", tones)
	}
}

func TestMessageLineWaitsForTheAnswer(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	send := func(seq int64, callID, to, output string, isErr bool) {
		tr.Apply(mk(seq, event.ToolCallStarted, event.ToolStartedPayload{Turn: 1, CallID: callID, Name: "message", Input: json.RawMessage(`{"to":"` + to + `","text":"?"}`)}))
		tr.Apply(mk(seq+1, event.ToolCallFinished, event.ToolFinishedPayload{Turn: 1, CallID: callID, Name: "message", Output: output, IsError: isErr}))
	}
	delivered := func(name string) string {
		return "message delivered to " + name + "; its answer wakes you between turns"
	}
	tone := func(to string) Tone {
		for _, l := range tr.All() {
			if l.Kind == LineTool && l.Tool == "message" && strings.HasPrefix(l.Text, "@"+to+" ") {
				return l.Tone
			}
		}
		t.Fatalf("no message line to %s", to)
		return ToneNone
	}
	tr.Apply(mk(1, event.TurnStarted, event.TurnPayload{Turn: 1}))
	send(2, "p1", "scout", delivered("scout"), false)
	if tone("scout") != ToneWorking {
		t.Fatalf("a delivered message waits for its answer: %v", tone("scout"))
	}
	send(4, "p2", "lookout", delivered("lookout"), false)
	tr.Apply(mk(6, event.TurnEnded, event.TurnEndedPayload{Turn: 1}))
	// the first agent answers: only its line settles
	tr.Apply(mk(7, event.TurnStarted, event.TurnPayload{Turn: 2}))
	tr.Apply(mk(8, event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: "agent_response", From: "scout", Text: "main"}))
	if tone("scout") != ToneNone || tone("lookout") != ToneWorking {
		t.Fatalf("answered → grey, unanswered → still yellow: %v %v", tone("scout"), tone("lookout"))
	}
	// the second agent is killed before answering: red
	tr.AskerGone("lookout")
	if tone("lookout") != ToneError {
		t.Fatalf("killed before answering → red: %v", tone("lookout"))
	}
	// two messages to one agent, one answer: both settle (a re-prompt is
	// covered by the same reply)
	send(9, "p4", "inspector", delivered("inspector"), false)
	send(11, "p5", "inspector", delivered("inspector"), false)
	tr.Apply(mk(13, event.UserMessage, event.UserMessagePayload{Turn: 3, Kind: "agent_response", From: "inspector", Text: "here"}))
	for _, l := range tr.All() {
		if l.Kind == LineTool && l.Tool == "message" && strings.HasPrefix(l.Text, "@inspector ") && l.Tone != ToneNone {
			t.Fatalf("one answer should settle both messages to that agent: %q tone %v", l.Text, l.Tone)
		}
	}
	// nothing to wait for: a refused message, an answer, a message to the user
	send(14, "p3", "ghost", `unknown agent "ghost"`, true)
	send(16, "p6", "helper", "answer delivered to helper", false)
	send(18, "p7", "user", "message delivered to the user", false)
	for _, to := range []string{"ghost", "helper", "user"} {
		if tone(to) == ToneWorking {
			t.Fatalf("a message to %s has nothing to wait for", to)
		}
	}
}

// TestNotesAndReminders: an agent's own text is marked as notes (it reaches
// no one), input is not, and the reminder bookkeeping reads as notices with
// the human as "you".
func TestNotesAndReminders(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "check it"}))
	tr.Apply(mk(2, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "# Result\nall good"}}}))
	tr.Apply(mk(3, event.ReminderQueued, event.RepliesPayload{Parties: []string{"user", "a1"}, Names: []string{"user", "scout"}}))
	tr.Apply(mk(4, event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: event.MsgReminder, Text: "[reminder from the harness] ..."}))
	tr.Apply(mk(5, event.ReplyMissing, event.RepliesPayload{Parties: []string{"user"}, Names: []string{"user"}}))
	var notes, notices []string
	for _, l := range tr.All() {
		switch {
		case l.Note:
			notes = append(notes, l.Text)
		case l.Kind == LineNotice || l.Kind == LineDim:
			notices = append(notices, l.Text)
		case strings.Contains(l.Text, "reminder from the harness"):
			t.Fatalf("the reminder input is not shown: %+v", l)
		}
	}
	if strings.Join(notes, "|") != "Result|all good" {
		t.Fatalf("notes %q", notes)
	}
	if strings.Join(notices, "|") != "**Nudged** owes a reply to you, scout|**Ended without replying** to you" {
		t.Fatalf("notices %q", notices)
	}
}

// TestOnlyHumanInputIsBlue: a prompt or steer from another agent reads like
// a received message ("› @main look at the parser"), never as the blue user
// block the human's own input gets.
func TestOnlyHumanInputIsBlue(t *testing.T) {
	mk := func(p event.UserMessagePayload) []Line {
		return EventLines(event.Event{Type: event.UserMessage, Time: time.Now(), Payload: event.MustPayload(p)})
	}
	for _, kind := range []event.MessageKind{event.MsgPrompt, event.MsgSteer} {
		lines := mk(event.UserMessagePayload{Kind: kind, Text: "look at the parser", From: "main"})
		var texts []string
		for _, l := range lines {
			if l.Block == BlockUser || l.Block == BlockSteer {
				t.Fatalf("%s from an agent drawn as user input: %+v", kind, l)
			}
			if l.Text != "" {
				texts = append(texts, l.Text)
			}
		}
		if strings.Join(texts, "|") != "**@main** look at the parser" || lines[1].Glyph != GlyphAsk {
			t.Fatalf("%s from an agent: %+v", kind, lines)
		}
	}
	human := mk(event.UserMessagePayload{Kind: event.MsgSteer, Text: "and the tests"})
	if human[1].Block != BlockUser || !human[1].Lead {
		t.Fatalf("the human's input stays the blue user block: %+v", human)
	}
}

// TestMessageArrows: what an agent sends reads ‹ and what it receives ›;
// other tools keep their glyph.
func TestMessageArrows(t *testing.T) {
	for _, c := range []struct {
		line Line
		want string
	}{
		{Line{Kind: LineTool, Tool: "message", Text: "@scout look"}, GlyphReply},
		{Line{Kind: LineTool, Tool: "message", Text: "@user done"}, GlyphReply},
		{Line{Kind: LineTool, Tool: "shell", Text: "Shell  ls"}, GlyphToolShell},
		{Line{Kind: LineTool, Tool: "agent_create", Text: "Agent create  scout (general)"}, GlyphToolCreate},
		{Line{Kind: LineTool, Tool: "agent_status", Text: "Agent status"}, GlyphToolAgents},
	} {
		if g, _ := CallGlyph(c.line); g != c.want {
			t.Errorf("%q: glyph %q, want %q", c.line.Text, g, c.want)
		}
	}
	resp := EventLines(event.Event{Type: event.UserMessage, Time: time.Now(), Payload: event.MustPayload(event.UserMessagePayload{Kind: event.MsgAgentResponse, Text: "done", From: "scout"})})
	if resp[1].Glyph != GlyphAsk || resp[1].Text != "**@scout** done" {
		t.Fatalf("a response from an agent reads › @scout: %+v", resp)
	}
}

// TestTurnStartMarksItems: the first item after a turn starts or ends is
// marked, and nothing else.
func TestTurnStartMarksItems(t *testing.T) {
	tr := NewTranscript()
	mk := func(seq int64, typ event.Type, p any) event.Event {
		return event.Event{Seq: seq, Agent: "a", Type: typ, Time: time.Now(), Payload: event.MustPayload(p)}
	}
	tr.Apply(mk(1, event.TurnStarted, event.TurnPayload{Turn: 1}))
	tr.Apply(mk(2, event.UserMessage, event.UserMessagePayload{Turn: 1, Kind: "prompt", Text: "one"}))
	tr.Apply(mk(3, event.AssistantMessage, event.AssistantMessagePayload{Turn: 1, Blocks: []model.Block{{Type: model.BlockText, Text: "notes"}}}))
	tr.Apply(mk(4, event.TurnEnded, event.TurnEndedPayload{Turn: 1, Reason: "end_turn"}))
	tr.Apply(mk(5, event.TurnStarted, event.TurnPayload{Turn: 2}))
	tr.Apply(mk(6, event.UserMessage, event.UserMessagePayload{Turn: 2, Kind: "prompt", Text: "two"}))
	var marked []string
	for _, l := range tr.All() {
		if l.TurnStart {
			for _, x := range tr.All() {
				if x.Item == l.Item && x.Text != "" {
					marked = append(marked, x.Text)
					break
				}
			}
		}
	}
	if strings.Join(marked, "|") != "one|two" {
		t.Fatalf("turn starts: %q", marked)
	}
}
