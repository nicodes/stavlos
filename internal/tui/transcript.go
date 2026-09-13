package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
)

// LineKind selects the style a transcript line is rendered with.
type LineKind int

const (
	LineText     LineKind = iota // assistant prose, or text inside a block
	LineHeading                  // markdown heading (bold)
	LineCode                     // fenced code (dim, indented)
	LineDim                      // secondary information ("◌ thinking…", "· turn cancelled")
	LineTool                     // tool call: "Bash  git status" (glyph added by Render)
	LineToolOut                  // tool output (indented, dim)
	LineNotice                   // local notice such as /help output
	LineLabel                    // dim label inside a block ("steer", "agent response", "task")
	LineFinished                 // "finished · success" (green, bold)
	LineRule                     // centered rule ("── compacted ──")
	LineError                    // error text
	LineStream                   // in-progress streaming text (rendered like LineText)
	LineModel                    // "· <model>" trailer after the final assistant text
	LineThink                    // thinking summary ("◌ thinking…"); always its own item
	LineToolNote                 // permission notice nested under its tool call (indented like output)
	LineBlank                    // spacer
)

// BlockKind marks lines that belong to a left-bordered block.
type BlockKind int

const (
	BlockNone     BlockKind = iota
	BlockUser               // accent border: a prompt
	BlockSteer              // warning border: a steer
	BlockChild              // muted border: a child result / task
	BlockError              // error border
	BlockFinished           // success border
)

// Visibility ties a line to the /details toggle.
type Visibility int

const (
	VisAlways    Visibility = iota
	VisCollapsed            // shown only while details are off ("… +N lines")
	VisExpanded             // shown only while details are on
)

// Line is one logical transcript line. Rendering (color, indentation,
// wrapping) is applied later by Render so the builder stays a pure function
// of events.
type Line struct {
	Kind    LineKind
	Text    string
	Block   BlockKind
	Vis     Visibility
	Running bool   // tool call still in progress (spinner glyph)
	Err     bool   // tool call failed (red gear)
	Suffix  string // dim trailer, e.g. "(cancelled)"
	Item    int    // index of the item (event group) this line belongs to
	Lead    bool   // first text line of a user/steer block: carries the "›" glyph
	Glyph   string // leader glyph for this line (Render styles it by Tone)
	Tone    Tone   // in progress / error; zero means "as is"
	callID  string
	tool    string // raw tool name on a LineTool line
}

// Tone colours a line's glyph by lifecycle: yellow while in progress, red
// when it errored or was terminated, the glyph's own colour otherwise.
type Tone int

const (
	ToneNone    Tone = iota
	ToneWorking      // in progress (yellow)
	ToneError        // error or terminated (red)
)

// Leader glyphs for chat items (see the glyph table in docs).
const (
	GlyphChild     = "⑂" // a child agent reported back (same fork as spawn)
	GlyphSpawn     = "⑂" // a child agent was spawned (fork)
	GlyphTask      = "▹" // the task handed to a child
	GlyphFinished  = "✓" // an agent finished
	GlyphError     = "!" // a turn error
	GlyphKilled    = "⊘" // an agent was killed
	GlyphTurn      = "◦" // a turn notice (cancelled, stopped, aborted)
	GlyphModel     = "⇄" // model changed
	GlyphNotice    = "»" // a local notice (/help, lists)
	GlyphPrompt    = "?" // a question for the user
	GlyphAnswer    = "?" // the user's answer (same mark as the question)
	GlyphFailed    = "✗" // a failed finish
	GlyphCompacted = "┄┄ compacted ┄┄"
)

const (
	maxArgChars        = 100
	maxOutputCollapsed = 3
	maxOutputExpanded  = 40
	maxSummaryLine     = 3
	maxThinkChars      = 100
)

// streamSeg is one piece of the live buffer for the current turn.
type streamSeg struct {
	kind LineKind // LineStream (text), LineDim (thinking), LineTool (tool name), LineToolOut (tool output)
	text string
	tool string // raw tool name for LineTool
}

// Transcript accumulates rendered lines for one agent. Lines are grouped
// into items, one per rendered event group (a user block, an assistant
// message, a tool call with its output, …); the chat cursor walks items.
// ShowThinking controls whether the model's thinking (summaries and
// streaming deltas) appears in the chat as "◌ thinking…" items. It is
// off: the events are still logged and the rendering path is kept, so it
// can be switched back on later.
var ShowThinking = false

type Transcript struct {
	Lines []Line

	calls      map[string]int    // tool call id → index of its LineTool
	prompts    map[string]int    // prompt id → item of the tool call it gates
	promptLine map[string]int    // prompt id → index of its "?" line (tone flips on answer)
	monitors   map[string]int    // monitor id → index of its "started" line
	children   map[string]int    // child agent id → index of the agent_create line that spawned it
	askTarget  map[string]string // agent_prompt call id → the agent it asked (until the call finishes)
	asks       map[string][]int  // agent id → indices of agent_prompt lines still waiting for its answer
	monKinds   map[string]string // monitor id → kind, for the glyph on later events
	items      int               // committed items so far
	streamTurn int
	stream     []streamSeg
	turn       bool      // a turn is in progress (TurnStarted seen, not yet ended)
	turnStart  time.Time // when the current turn began
	turnTokens int       // input + output tokens used so far this turn
	turnVerb   string    // the indicator's verb for this turn ("Galloping")
}

// turnVerbs are the horse-flavoured labels the turn indicator cycles
// through, one per turn (stable within a turn so the line does not
// flicker).
var turnVerbs = []string{
	"Galloping", "Trotting", "Prancing", "Hoofing it", "Grazing", "Horsing around",
}

// TurnVerb is the indicator label for the current turn, "" when idle.
func (t *Transcript) TurnVerb() string {
	if !t.turn {
		return ""
	}
	return t.turnVerb
}

// InTurn reports whether the agent is mid-turn: the chat shows an
// ephemeral "working…" line with a spinner while this is true.
func (t *Transcript) InTurn() bool { return t.turn }

// TurnStats is the current turn's elapsed time (as of now) and tokens.
func (t *Transcript) TurnStats(now time.Time) (time.Duration, int) {
	if !t.turn {
		return 0, 0
	}
	return now.Sub(t.turnStart), t.turnTokens
}

// NewTranscript returns an empty transcript.
func NewTranscript() *Transcript {
	return &Transcript{calls: map[string]int{}, prompts: map[string]int{}, promptLine: map[string]int{}, monitors: map[string]int{}, monKinds: map[string]string{}, children: map[string]int{}, askTarget: map[string]string{}, asks: map[string][]int{}}
}

// Apply appends the rendering of ev. An assistant.message (or the end of a
// turn) replaces the in-progress streaming buffer; tool.call.finished updates
// the matching tool line in place.
func (t *Transcript) Apply(ev event.Event) {
	item := t.items // a new item, unless the lines extend an earlier one
	switch ev.Type {
	case event.ToolCallStarted:
		var p event.ToolStartedPayload
		if ev.Decode(&p) == nil && p.CallID != "" {
			t.calls[p.CallID] = len(t.Lines)
			if p.Name == "agent_prompt" {
				var in struct{ ID string }
				if json.Unmarshal(p.Input, &in) == nil && in.ID != "" {
					t.askTarget[p.CallID] = in.ID
				}
			}
		}
	case event.PromptRequested:
		// A permission prompt belongs to the call it gates: the open call
		// with the same tool name (the latest one if several).
		var p event.PromptRequestedPayload
		if ev.Decode(&p) == nil {
			if p.Kind == "permission" {
				if item, ok := t.openCallItem(p.Tool); ok {
					t.prompts[p.ID] = item
					_, last := itemRange(t.Lines, item)
					t.promptLine[p.ID] = last + 1 // where insertIntoItem puts it
					t.insertIntoItem(item, nested(EventLines(ev)))
					return
				}
			}
			t.promptLine[p.ID] = len(t.Lines)
		}
	case event.PromptClaimed, event.PromptAnswered, event.PromptWithdrawn, event.PromptDefaulted:
		var p event.PromptRefPayload
		if ev.Decode(&p) == nil {
			if ev.Type != event.PromptClaimed {
				t.settlePrompt(p.ID, ev.Type != event.PromptAnswered)
			}
			if item, ok := t.prompts[p.ID]; ok {
				if ev.Type != event.PromptClaimed {
					delete(t.prompts, p.ID)
				}
				t.insertIntoItem(item, nested(EventLines(ev)))
				return
			}
		}
	case event.MonitorStarted:
		var p event.MonitorStartedPayload
		if ev.Decode(&p) == nil && p.ID != "" {
			t.monKinds[p.ID] = p.Kind
			// A job started by bash_async is represented by that call's own
			// line: it stays yellow while the job runs and the outcome nests
			// under it. Only a job with no such call gets its own notice.
			if i := t.lastAsyncCall(); i >= 0 {
				t.monitors[p.ID] = i
				t.Lines[i].Tone = ToneWorking
				t.stream = nil
				return
			}
			t.monitors[p.ID] = len(t.Lines)
		}
	case event.MonitorFired:
		// The outcome joins the "started" notice's item, like tool output
		// joins its call.
		var p event.MonitorFiredPayload
		if ev.Decode(&p) == nil {
			if p.Kind == "" {
				p.Kind = t.monKinds[p.ID]
			}
			lines := monitorFiredLines(p)
			if i, ok := t.monitors[p.ID]; ok && i < len(t.Lines) {
				t.Lines[i].Tone = ToneNone
				if p.IsError {
					t.Lines[i].Tone = ToneError
				}
				delete(t.monitors, p.ID)
				if t.Lines[i].Kind == LineTool {
					lines = nested(lines) // under the bash_async call, like tool output
				}
				t.insertIntoItem(t.Lines[i].Item, lines)
				return
			}
			delete(t.monitors, p.ID)
			t.appendItem(item, lines)
			return
		}
	case event.MonitorStopped:
		var p event.MonitorRefPayload
		if ev.Decode(&p) == nil {
			lines := monitorStoppedLines(t.monKinds[p.ID], p.Reason)
			if i, ok := t.monitors[p.ID]; ok && i < len(t.Lines) {
				t.Lines[i].Tone = ToneError
				delete(t.monitors, p.ID)
				if t.Lines[i].Kind == LineTool {
					lines = nested(lines)
				}
				t.insertIntoItem(t.Lines[i].Item, lines)
				return
			}
			delete(t.monitors, p.ID)
			t.appendItem(item, lines)
			return
		}
	case event.ToolCallFinished:
		var p event.ToolFinishedPayload
		if ev.Decode(&p) == nil {
			if i, ok := t.calls[p.CallID]; ok && i < len(t.Lines) {
				// Output joins the call's item and is placed right under the
				// call, even when other items (a permission notice, say)
				// were committed while the call ran.
				item = t.Lines[i].Item
				t.finishCall(p)
				t.stream = nil
				t.insertIntoItem(item, EventLines(ev))
				return
			}
			t.finishCall(p)
		}
		t.stream = nil
	}
	t.appendItem(item, EventLines(ev))
	switch ev.Type {
	case event.TurnStarted:
		t.turn, t.turnStart, t.turnTokens = true, ev.Time, 0
		var p event.TurnPayload
		_ = ev.Decode(&p)
		t.turnVerb = turnVerbs[((p.Turn-1)%len(turnVerbs)+len(turnVerbs))%len(turnVerbs)]
	case event.Usage:
		var p event.UsagePayload
		if ev.Decode(&p) == nil {
			t.turnTokens += p.Usage.InputTokens + p.Usage.OutputTokens
		}
	case event.UserMessage:
		var p event.UserMessagePayload
		if ev.Decode(&p) == nil && p.Kind == "agent_response" {
			t.answered(p.From)
		}
	case event.AssistantMessage:
		t.stream = nil
	case event.TurnEnded, event.TurnAborted, event.AgentFinished, event.AgentKilled:
		t.stream = nil
		t.turn = false
		t.stopRunning()
	}
}

// appendItem commits lines under item; a fresh item index bumps the count.
// Thinking is always its own item: each contiguous run of LineThink lines,
// and the run of other lines after it, get successive item indices.
func (t *Transcript) appendItem(item int, lines []Line) {
	if len(lines) == 0 {
		return
	}
	if item != t.items {
		for i := range lines {
			lines[i].Item = item
		}
		t.Lines = append(t.Lines, lines...)
		return
	}
	for _, run := range splitThinking(lines) {
		for i := range run {
			run[i].Item = t.items
		}
		t.Lines = append(t.Lines, run...)
		t.items++
	}
}

// splitThinking cuts lines at every boundary between thinking and
// non-thinking lines, preserving order.
func splitThinking(lines []Line) [][]Line {
	var runs [][]Line
	start := 0
	for i := 1; i <= len(lines); i++ {
		if i == len(lines) || (lines[i].Kind == LineThink) != (lines[start].Kind == LineThink) {
			runs = append(runs, lines[start:i])
			start = i
		}
	}
	return runs
}

// nested re-kinds notice lines so they render indented under a tool call.
func nested(lines []Line) []Line {
	for i := range lines {
		if lines[i].Kind == LineDim || lines[i].Kind == LineNotice {
			lines[i].Kind = LineToolNote
		}
	}
	return lines
}

// settlePrompt ends the "in progress" tone on a prompt's "?" line: as-is
// when answered, red when denied by default or withdrawn.
func (t *Transcript) settlePrompt(id string, terminated bool) {
	i, ok := t.promptLine[id]
	delete(t.promptLine, id)
	if !ok || i >= len(t.Lines) || t.Lines[i].Glyph != GlyphPrompt {
		return
	}
	t.Lines[i].Tone = ToneNone
	if terminated {
		t.Lines[i].Tone = ToneError
	}
}

// monitorKindFromText recovers the kind from a monitor wake message
// ("Monitor "x" (command, m…): …").
func monitorKindFromText(text string) string {
	for _, k := range []string{"command", "watch", "timer"} {
		if strings.Contains(text, "("+k+", ") {
			return k
		}
	}
	return "command"
}

// lastAsyncCall returns the index of the most recent bash_async call line
// that is not yet tied to a job, or -1.
func (t *Transcript) lastAsyncCall() int {
	tied := map[int]bool{}
	for _, idx := range t.monitors {
		tied[idx] = true
	}
	for i := len(t.Lines) - 1; i >= 0; i-- {
		l := t.Lines[i]
		if l.Kind == LineTool && l.tool == "bash_async" && !tied[i] {
			return i
		}
	}
	return -1
}

// openCallItem returns the item of the most recently started, still-open
// call of tool name.
func (t *Transcript) openCallItem(name string) (int, bool) {
	best, item := -1, 0
	for _, idx := range t.calls {
		if idx < len(t.Lines) && idx > best && strings.EqualFold(t.Lines[idx].tool, name) {
			best, item = idx, t.Lines[idx].Item
		}
	}
	return item, best >= 0
}

// insertIntoItem places lines immediately after the last line of item so
// the item stays contiguous. Tracked call and monitor indices past the
// splice shift.
func (t *Transcript) insertIntoItem(item int, lines []Line) {
	if len(lines) == 0 {
		return
	}
	for i := range lines {
		lines[i].Item = item
	}
	_, last := itemRange(t.Lines, item)
	if last < 0 || last == len(t.Lines)-1 {
		t.Lines = append(t.Lines, lines...)
		return
	}
	at := last + 1
	out := make([]Line, 0, len(t.Lines)+len(lines))
	out = append(out, t.Lines[:at]...)
	out = append(out, lines...)
	out = append(out, t.Lines[at:]...)
	t.Lines = out
	for id, idx := range t.calls {
		if idx >= at {
			t.calls[id] = idx + len(lines)
		}
	}
	for id, idx := range t.promptLine {
		if idx >= at {
			t.promptLine[id] = idx + len(lines)
		}
	}
	for id, idx := range t.monitors {
		if idx >= at {
			t.monitors[id] = idx + len(lines)
		}
	}
	for id, idx := range t.children {
		if idx >= at {
			t.children[id] = idx + len(lines)
		}
	}
	for id, idxs := range t.asks {
		for k, idx := range idxs {
			if idx >= at {
				t.asks[id][k] = idx + len(lines)
			}
		}
	}
}

// ChildSpawned ties a just-spawned child to the agent_create call that
// made it: the call line reads as in progress (yellow) until ChildDone,
// the way a bash_async call tracks its job.
func (t *Transcript) ChildSpawned(childID string) {
	if i := t.lastAgentCreateCall(); i >= 0 {
		t.Lines[i].Tone = ToneWorking
		t.children[childID] = i
	}
}

// ChildState colours the agent_create line of a child by its current
// state: working (yellow) while it runs or is blocked, red once killed,
// grey when it idles. Children no longer finish; they answer and wait.
func (t *Transcript) ChildState(childID, state string) {
	i, ok := t.children[childID]
	if !ok || i >= len(t.Lines) {
		return
	}
	switch state {
	case "running", "blocked":
		t.Lines[i].Tone = ToneWorking
	case "killed":
		t.Lines[i].Tone = ToneError
		delete(t.children, childID)
	default:
		t.Lines[i].Tone = ToneNone
	}
}

// lastAgentCreateCall returns the index of the most recent agent_create
// call line not yet tied to a child, or -1.
func (t *Transcript) lastAgentCreateCall() int {
	tied := map[int]bool{}
	for _, idx := range t.children {
		tied[idx] = true
	}
	for i := len(t.Lines) - 1; i >= 0; i-- {
		l := t.Lines[i]
		if l.Kind == LineTool && l.tool == "agent_create" && !tied[i] {
			return i
		}
	}
	return -1
}

func (t *Transcript) finishCall(p event.ToolFinishedPayload) {
	i, ok := t.calls[p.CallID]
	if !ok || i >= len(t.Lines) {
		return
	}
	l := &t.Lines[i]
	l.Running = false
	l.Err = p.IsError
	switch {
	case p.Cancelled:
		l.Suffix = "(cancelled)"
	case p.Denied:
		l.Suffix = "(denied)"
	}
	// A delivered agent_prompt waits for that agent's answer: yellow until
	// its agent_response lands (see answered), like bash_async and its job.
	if target, ok := t.askTarget[p.CallID]; ok {
		delete(t.askTarget, p.CallID)
		if !p.IsError && !p.Cancelled && !p.Denied {
			l.Tone = ToneWorking
			t.asks[target] = append(t.asks[target], i)
		}
	}
	delete(t.calls, p.CallID)
}

// answered settles the oldest outstanding agent_prompt to the agent named
// in a response's From ("label (shortid)" or a bare id).
func (t *Transcript) answered(from string) {
	for target, idxs := range t.asks {
		if len(idxs) == 0 || (from != target && !strings.Contains(from, "("+shortID(target)+")")) {
			continue
		}
		if i := idxs[0]; i < len(t.Lines) {
			t.Lines[i].Tone = ToneNone
		}
		t.asks[target] = idxs[1:]
		return
	}
}

// AskerGone marks every outstanding agent_prompt to a killed agent red:
// no answer is coming.
func (t *Transcript) AskerGone(id string) {
	for _, i := range t.asks[id] {
		if i < len(t.Lines) {
			t.Lines[i].Tone = ToneError
		}
	}
	delete(t.asks, id)
}

func (t *Transcript) stopRunning() {
	for i := range t.Lines {
		t.Lines[i].Running = false
	}
	t.calls = map[string]int{}
}

// ApplyStream folds a transient stream notification into the live buffer.
// ToolName+Text is partial tool output; ToolName alone is a tool_use block
// starting in the model's response.
func (t *Transcript) ApplyStream(n protocol.StreamNotification) {
	if n.Turn != t.streamTurn {
		t.streamTurn = n.Turn
		t.stream = nil
	}
	k := len(t.stream)
	switch {
	case n.ToolName != "" && n.Text != "":
		if k > 0 && t.stream[k-1].kind == LineToolOut {
			t.stream[k-1].text += n.Text
		} else {
			t.stream = append(t.stream, streamSeg{LineToolOut, n.Text, ""})
		}
	case n.Text != "":
		if k > 0 && t.stream[k-1].kind == LineStream {
			t.stream[k-1].text += n.Text
		} else {
			t.stream = append(t.stream, streamSeg{LineStream, n.Text, ""})
		}
	case n.Thinking != "":
		if ShowThinking && (k == 0 || t.stream[k-1].kind != LineThink) {
			t.stream = append(t.stream, streamSeg{LineThink, "◌ thinking…", ""})
		}
	case n.ToolName != "":
		t.stream = append(t.stream, streamSeg{LineTool, toolTitle(n.ToolName), n.ToolName})
	}
}

// Notice appends a local (non-event) notice, e.g. /help output, as one item.
func (t *Transcript) Notice(lines ...string) {
	ls := make([]Line, 0, len(lines))
	for i, l := range lines {
		ln := Line{Kind: LineNotice, Text: l}
		if i == 0 {
			ln.Glyph = GlyphNotice
		}
		ls = append(ls, ln)
	}
	t.appendItem(t.items, ls)
}

// All returns the committed lines followed by the live streaming buffer.
// Buffer lines belong to the in-progress item: the running tool call when
// the buffer continues one, otherwise a new item after the committed ones.
func (t *Transcript) All() []Line {
	if len(t.stream) == 0 {
		return t.Lines
	}
	item := t.items
	if n := len(t.Lines); n > 0 && t.Lines[n-1].Kind == LineTool && t.Lines[n-1].Running {
		item = t.Lines[n-1].Item
	}
	out := make([]Line, 0, len(t.Lines)+8)
	out = append(out, t.Lines...)
	start := len(out)
	for _, s := range t.stream {
		switch s.kind {
		case LineStream:
			for _, l := range strings.Split(strings.TrimRight(s.text, "\n"), "\n") {
				out = append(out, Line{Kind: LineStream, Text: l})
			}
		case LineToolOut:
			out = append(out, outputLines(strings.TrimRight(s.text, "\n"))...)
		case LineTool:
			out = append(out, Line{Kind: LineTool, Text: s.text, Running: true, tool: s.tool})
		default:
			out = append(out, Line{Kind: s.kind, Text: s.text})
		}
	}
	cur := item
	for i := start; i < len(out); i++ {
		if i > start && (out[i].Kind == LineThink) != (out[i-1].Kind == LineThink) && !(cur == item && item != t.items) {
			cur++
		}
		out[i].Item = cur
	}
	return out
}

// Items is the number of items All() spans (committed plus the in-progress
// one when the streaming buffer starts a new item).
func (t *Transcript) Items() int { return itemCount(t.All()) }

// ItemRange returns the first and last index into All() of item i, or
// (-1, -1) when there is no such item. Items are contiguous: tool output is
// spliced in under its call even when other events landed in between.
func (t *Transcript) ItemRange(i int) (first, last int) { return itemRange(t.All(), i) }

func itemCount(lines []Line) int {
	n := 0
	for _, l := range lines {
		if l.Item+1 > n {
			n = l.Item + 1
		}
	}
	return n
}

func itemRange(lines []Line, item int) (first, last int) {
	first, last = -1, -1
	for i, l := range lines {
		if l.Item != item {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	return first, last
}

// itemIsTool reports whether item is a tool call (has output to expand).
func itemIsTool(lines []Line, item int) bool {
	for _, l := range lines {
		if l.Item == item && l.Kind == LineTool {
			return true
		}
	}
	return false
}

// Streaming reports whether a live buffer is being shown.
func (t *Transcript) Streaming() bool { return len(t.stream) > 0 }

// Empty reports whether nothing at all would be shown (the home state).
func (t *Transcript) Empty() bool { return len(t.Lines) == 0 && len(t.stream) == 0 }

// Running reports whether a tool call is in progress (spinner needs redraws).
func (t *Transcript) Running() bool {
	if len(t.calls) > 0 {
		return true
	}
	for _, s := range t.stream {
		if s.kind == LineTool {
			return true
		}
	}
	return false
}

// Build folds a full event sequence into lines. Pure; used by tests.
func Build(evs []event.Event) []Line {
	t := NewTranscript()
	for _, ev := range evs {
		t.Apply(ev)
	}
	return t.All()
}

// EventLines renders a single event. Unknown or silent event types (usage,
// turn.started, prompt.queued, …) yield no lines.
func EventLines(ev event.Event) []Line {
	switch ev.Type {
	case event.AgentSpawned:
		var p event.AgentSpawnedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		if p.Parent == "" {
			return nil // the root's own spawn is not a message; keeps the home state empty
		}
		lines := []Line{{Kind: LineDim, Glyph: GlyphSpawn, Text: fmt.Sprintf("spawned %s (%s) · %s", p.Label, p.Archetype, p.Model)}}
		if p.Task != "" {
			lines = append(lines, blockWith(BlockChild, "task", p.Task, GlyphTask)...)
		}
		return lines

	case event.UserMessage:
		var p event.UserMessagePayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		switch p.Kind {
		case "prompt", "":
			if p.From != "" {
				return block(BlockUser, "from "+p.From, p.Text)
			}
			return block(BlockUser, "", p.Text)
		case "steer": // shown exactly like a prompt: blue ›, no title
			if p.From != "" {
				return block(BlockUser, "from "+p.From, p.Text)
			}
			return block(BlockUser, "", p.Text)
		case "agent_response":
			// Reads like a tool line so it is obvious what it is:
			// "⑂ Response from scout (a1b2c3d4)" over the answer's text. The
			// agent's own agent_response call reads "Agent response  → id",
			// so incoming and outgoing never look alike.
			head := "**Agent response received**"
			if p.From != "" {
				head += " · " + p.From
			}
			lines := []Line{{Kind: LineBlank}, {Kind: LineText, Text: head, Block: BlockChild, Glyph: GlyphChild}}
			for _, l := range strings.Split(strings.TrimRight(p.Text, "\n"), "\n") {
				lines = append(lines, Line{Kind: LineText, Text: l, Block: BlockChild})
			}
			return append(lines, Line{Kind: LineBlank})
		case "child_finished": // legacy: finished children from old logs
			return blockWith(BlockChild, "agent response", p.Text, GlyphChild)
		case "monitor_fired":
			return blockWith(BlockChild, "bash async result", p.Text, monitorGlyph("command"))
		default:
			return block(BlockUser, p.Kind, p.Text)
		}

	case event.AssistantMessage:
		var p event.AssistantMessagePayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		var lines []Line
		hasText := false
		for _, b := range p.Blocks {
			switch b.Type {
			case model.BlockText:
				text := strings.TrimRight(b.Text, "\n")
				if text == "" {
					continue
				}
				hasText = true
				lines = append(lines, markdownLines(text)...)
			case model.BlockThinking:
				if ShowThinking {
					lines = append(lines, thinkingLine(b.Text))
				}
			}
		}
		if hasText {
			// The model that produced the response is not shown (it is on
			// the meta row and in the sidebar); LineModel stays for notices
			// style notices that name a model.
			lines = append(lines, Line{Kind: LineBlank})
		}
		return lines

	case event.ToolCallStarted:
		var p event.ToolStartedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		if p.Name == "agent_response" {
			// The message itself is the interesting part: show it under the
			// call the way an incoming answer shows its text.
			var in struct{ Text string }
			_ = json.Unmarshal(p.Input, &in)
			lines := []Line{{Kind: LineTool, Text: toolLine(p.Name, p.Input), Running: true, tool: p.Name}}
			return append(lines, outputLines(strings.TrimRight(in.Text, "\n"))...)
		}
		return []Line{{Kind: LineTool, Text: toolLine(p.Name, p.Input), Running: true, callID: p.CallID, tool: p.Name}}

	case event.ToolCallFinished:
		var p event.ToolFinishedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		if p.Name == "agent_response" && !p.IsError {
			return nil // the message already sits under the call; "response delivered" adds nothing
		}
		return outputLines(strings.TrimRight(p.Output, "\n"))

	case event.TurnEnded:
		var p event.TurnEndedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		switch p.Reason {
		case "cancelled":
			return []Line{{Kind: LineDim, Glyph: GlyphTurn, Tone: ToneError, Text: "turn cancelled"}, {Kind: LineBlank}}
		case "error":
			msg := p.Error
			if msg == "" {
				msg = "turn error"
			}
			return errorBlock(msg)
		case "max_tokens":
			return []Line{{Kind: LineDim, Glyph: GlyphTurn, Tone: ToneError, Text: "turn stopped: max_tokens"}, {Kind: LineBlank}}
		}
		return nil

	case event.TurnAborted:
		return []Line{{Kind: LineDim, Glyph: GlyphTurn, Tone: ToneError, Text: "turn aborted (daemon restart)"}, {Kind: LineBlank}}

	case event.AgentFinished:
		var p event.AgentFinishedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		head := Line{Kind: LineFinished, Text: "finished · " + p.Status, Block: BlockFinished, Glyph: GlyphFinished}
		switch p.Status {
		case "failure":
			head.Glyph, head.Tone = GlyphFailed, ToneError
		case "partial":
			head.Tone = ToneWorking
		}
		lines := []Line{{Kind: LineBlank}, head}
		if s := strings.TrimRight(p.Summary, "\n"); s != "" {
			for _, l := range strings.Split(s, "\n") {
				lines = append(lines, Line{Kind: LineText, Text: l, Block: BlockFinished})
			}
		}
		for _, a := range p.Artifacts {
			s := "• " + a.Path
			if a.Description != "" {
				s += " — " + a.Description
			}
			lines = append(lines, Line{Kind: LineDim, Text: s, Block: BlockFinished})
		}
		return append(lines, Line{Kind: LineBlank})

	case event.AgentKilled:
		return errorBlockWith("killed", GlyphKilled)

	case event.AgentRoleChanged:
		var p event.RoleChangedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "role → " + p.Role}}

	case event.AgentModelChanged:
		var p event.ModelChangedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "model → " + p.Model}}

	case event.SessionYoloChanged:
		var p event.YoloPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		state := "off · permissions are asked"
		if p.On {
			state = "on · every permission is approved"
		}
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "yolo → " + state}}

	case event.AgentVariantChanged:
		var p event.VariantChangedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		v := p.Variant
		if v == "" {
			v = "default"
		}
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "variant → " + v}}

	case event.MonitorStarted:
		var p event.MonitorStartedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineDim, Glyph: monitorGlyph(p.Kind), Tone: ToneWorking, Text: "job: " + p.Label}}

	case event.MonitorFired:
		var p event.MonitorFiredPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return monitorFiredLines(p)

	case event.MonitorStopped:
		// Without the transcript's id → kind memory the glyph defaults to ◆;
		// Transcript.Apply looks it up.
		var p event.MonitorRefPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return monitorStoppedLines("", p.Reason)

	case event.Compacted:
		var p event.CompactedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		lines := []Line{{Kind: LineBlank}, {Kind: LineRule, Text: GlyphCompacted}}
		if s := strings.TrimSpace(p.Summary); s != "" {
			lines = append(lines, truncLines(s, maxSummaryLine, LineDim)...)
		}
		return append(lines, Line{Kind: LineBlank})

	case event.PromptRequested:
		var p event.PromptRequestedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		switch p.Kind {
		case "question":
			return []Line{{Kind: LineNotice, Glyph: GlyphPrompt, Tone: ToneWorking, Text: "question: " + firstLine(p.Question)}}
		case "trust":
			return []Line{{Kind: LineNotice, Glyph: GlyphPrompt, Tone: ToneWorking, Text: "trust requested"}}
		default:
			return []Line{{Kind: LineNotice, Glyph: GlyphPrompt, Tone: ToneWorking, Text: "permission: " + p.Tool}}
		}

	case event.PromptAnswered:
		var p event.PromptAnsweredPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		ln := Line{Kind: LineNotice, Glyph: GlyphAnswer, Text: "answered: " + firstLine(p.Answer)}
		if strings.HasPrefix(strings.ToLower(p.Answer), "deny") {
			ln.Tone = ToneError
		}
		return []Line{ln}

	case event.PromptDefaulted:
		var p event.PromptAnsweredPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineNotice, Glyph: GlyphAnswer, Tone: ToneError, Text: "defaulted: " + firstLine(p.Answer)}}

	case event.PromptWithdrawn:
		return []Line{{Kind: LineNotice, Glyph: GlyphAnswer, Tone: ToneError, Text: "prompt withdrawn"}}
	}
	return nil
}

// --- helpers ---

func decodeErr(ev event.Event, err error) []Line {
	return []Line{{Kind: LineError, Text: fmt.Sprintf("(bad %s payload: %v)", ev.Type, err)}}
}

// monitorKindWord is the word after the glyph in a monitor.started notice.
func monitorKindWord(kind string) string {
	switch kind {
	case "watch", "timer":
		return kind
	}
	return "background"
}

// monitorFiredLines renders "<glyph> <summary>" (red on error) followed by
// the output collapsed like tool output.
func monitorFiredLines(p event.MonitorFiredPayload) []Line {
	head := Line{Kind: LineDim, Glyph: monitorGlyph(p.Kind)}
	if p.IsError {
		head.Tone = ToneError
	}
	summary := strings.TrimSpace(p.Summary)
	if summary == "" {
		summary = "monitor fired"
	}
	head.Text = summary
	lines := []Line{head}
	return append(lines, outputLines(strings.TrimRight(p.Output, "\n"))...)
}

// monitorStoppedLines renders "<glyph> monitor stopped (<reason>)".
func monitorStoppedLines(kind, reason string) []Line {
	text := "job stopped"
	if reason = strings.TrimSpace(reason); reason != "" {
		text += " (" + reason + ")"
	}
	return []Line{{Kind: LineDim, Glyph: monitorGlyph(kind), Tone: ToneError, Text: text}}
}

// block renders text as a left-bordered block (blank line before and after)
// with an optional dim label as its first line.
func block(kind BlockKind, label, text string) []Line {
	return blockWith(kind, label, text, "")
}

// blockWith is block with a leader glyph on the first text line (user and
// steer blocks always use the shell prompt "›").
func blockWith(kind BlockKind, label, text, glyph string) []Line {
	lines := []Line{{Kind: LineBlank}}
	if label != "" {
		lines = append(lines, Line{Kind: LineLabel, Text: label, Block: kind})
	}
	for i, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		lead := i == 0 && (kind == BlockUser || kind == BlockSteer)
		ln := Line{Kind: LineText, Text: l, Block: kind, Lead: lead}
		if i == 0 && glyph != "" {
			ln.Glyph = glyph
		}
		lines = append(lines, ln)
	}
	return append(lines, Line{Kind: LineBlank})
}

func errorBlock(text string) []Line { return errorBlockWith(text, GlyphError) }

// errorBlockWith is errorBlock with a specific leader glyph (red).
func errorBlockWith(text, glyph string) []Line {
	lines := []Line{{Kind: LineBlank}}
	for i, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		ln := Line{Kind: LineError, Text: l, Block: BlockError, Tone: ToneError}
		if i == 0 {
			ln.Glyph = glyph
		}
		lines = append(lines, ln)
	}
	return append(lines, Line{Kind: LineBlank})
}

// markdownLines applies very light markdown: fenced code becomes LineCode
// (fence markers dropped), "# heading" becomes LineHeading, everything else
// stays prose (inline **bold** is handled by Render).
func markdownLines(text string) []Line {
	var out []Line
	code := false
	for _, l := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(l)
		if strings.HasPrefix(trim, "```") {
			code = !code
			continue
		}
		switch {
		case code:
			out = append(out, Line{Kind: LineCode, Text: l})
		case headingLevel(trim) > 0:
			out = append(out, Line{Kind: LineHeading, Text: strings.TrimSpace(trim[headingLevel(trim):])})
		default:
			out = append(out, Line{Kind: LineText, Text: l})
		}
	}
	return out
}

// headingLevel returns the number of leading '#' of an ATX heading, or 0.
func headingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(s) || s[n] != ' ' {
		return 0
	}
	return n
}

func thinkingLine(summary string) Line {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return Line{Kind: LineThink, Text: "◌ thinking…"}
	}
	first := summary
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	return Line{Kind: LineThink, Text: "◌ " + truncRunes(first, maxThinkChars)}
}

// truncLines splits text into at most n lines of the given kind, appending
// "…" when it had to cut.
func truncLines(text string, n int, kind LineKind) []Line {
	parts := strings.Split(strings.TrimRight(text, "\n"), "\n")
	cut := false
	if len(parts) > n {
		parts = parts[:n]
		cut = true
	}
	lines := make([]Line, 0, len(parts))
	for i, l := range parts {
		if cut && i == len(parts)-1 {
			l += "…"
		}
		lines = append(lines, Line{Kind: kind, Text: l})
	}
	return lines
}

// toolLine renders "Bash  git status": the tool name title-cased and its
// most relevant argument.
func toolLine(name string, input json.RawMessage) string {
	title := toolTitle(name)
	arg := toolArg(name, input)
	if arg == "" {
		return title
	}
	return title + "  " + truncRunes(arg, maxArgChars)
}

// toolArg picks the argument worth showing for a tool call.
func toolArg(name string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return compactArgs(raw)
	}
	str := func(k string) string {
		s, _ := in[k].(string)
		return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	}
	switch name {
	case "bash", "bash_async":
		return str("command")
	case "bash_async_kill":
		return str("id")
	case "read", "write", "edit":
		return str("path")
	case "apply_patch":
		if patch, ok := in["patch"].(string); ok {
			return patchFiles(patch)
		}
		return ""
	case "agent_create", "spawn":
		label, arch := str("label"), str("archetype")
		switch {
		case label != "" && arch != "":
			return label + " (" + arch + ")"
		case label != "":
			return label
		default:
			return arch
		}
	case "agent_prompt", "agent_steer", "agent_cancel", "agent_kill", "agent_result", "agent_status", "send", "steer", "cancel", "kill", "result", "status":
		return str("id")
	case "monitor":
		if ids, ok := in["ids"].([]any); ok && len(ids) > 0 {
			parts := make([]string, 0, len(ids))
			for _, v := range ids {
				if s, ok := v.(string); ok {
					parts = append(parts, s)
				}
			}
			return strings.Join(parts, ", ")
		}
		return str("id")
	case "skill":
		return str("name")
	case "agent_finish": // legacy
		return str("status")
	case "agent_response":
		return "→ " + str("to") // outgoing: who it answers
	}
	return compactArgs(raw)
}

// toolTitle is the display name of a tool on its chat line: titleCase of
// the name, except agent_finish, which reads "Agent complete" (the call
// marks the agent's work complete).
func toolTitle(name string) string {
	switch name {
	case "agent_finish": // legacy
		return "Agent complete"
	case "agent_response":
		return "Agent response delivered"
	}
	return titleCase(name)
}

// titleCase capitalises a tool name for display; underscores read as
// spaces, so agent_create shows as "Agent create".
func titleCase(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "_", " ")
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// splitModel splits "provider/model-id" into the short id and the provider.
func splitModel(id string) (short, provider string) {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[i+1:], id[:i]
	}
	return id, ""
}

// compactArgs renders tool input as compact JSON truncated to maxArgChars.
func compactArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	s := string(raw)
	if err := json.Compact(&buf, raw); err == nil {
		s = buf.String()
	}
	s = strings.ReplaceAll(s, "\n", " ")
	return truncRunes(s, maxArgChars)
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "…"
	}
	return s
}

// outputLines renders tool output collapsed by default: the first
// maxOutputCollapsed lines always, up to maxOutputExpanded with /details,
// and a "… +N lines" trailer for whichever mode is hiding something.
func outputLines(out string) []Line {
	if out == "" {
		return nil
	}
	parts := strings.Split(out, "\n")
	total := len(parts)
	var lines []Line
	for i, l := range parts {
		if i >= maxOutputExpanded {
			break
		}
		vis := VisAlways
		if i >= maxOutputCollapsed {
			vis = VisExpanded
		}
		lines = append(lines, Line{Kind: LineToolOut, Text: l, Vis: vis})
	}
	if total > maxOutputCollapsed {
		lines = append(lines, Line{Kind: LineToolOut, Text: fmt.Sprintf("… +%d lines", total-maxOutputCollapsed), Vis: VisCollapsed})
	}
	if total > maxOutputExpanded {
		lines = append(lines, Line{Kind: LineToolOut, Text: fmt.Sprintf("… +%d lines", total-maxOutputExpanded), Vis: VisExpanded})
	}
	return lines
}

// prettyJSON indents raw JSON for display, falling back to the raw text.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// patchFiles summarises the files an apply_patch touches: "a.go, b.md" or
// "a.go, b.md (+2 more)".
func patchFiles(patch string) string {
	var files []string
	for _, l := range strings.Split(patch, "\n") {
		for _, h := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: "} {
			if strings.HasPrefix(l, h) {
				files = append(files, strings.TrimSpace(strings.TrimPrefix(l, h)))
			}
		}
	}
	switch {
	case len(files) == 0:
		return ""
	case len(files) <= 2:
		return strings.Join(files, ", ")
	}
	return strings.Join(files[:2], ", ") + fmt.Sprintf(" (+%d more)", len(files)-2)
}
