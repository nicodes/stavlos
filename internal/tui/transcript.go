package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/model"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/toolname"
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
	// GlyphCompacting marks the rule of a compaction still running; Render
	// draws the sweeping bar into it.
	GlyphCompacting = "┄┄ compacting ┄┄"
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

// lineRef addresses one committed line by its item and its offset inside
// the item. Lines are only ever appended to an item (or the whole item is
// replaced), so a ref stays valid however the transcript grows: nothing is
// shifted when output is spliced in under an earlier call.
type lineRef struct{ item, off int }

func (r lineRef) after(o lineRef) bool {
	return r.item > o.item || r.item == o.item && r.off > o.off
}

type Transcript struct {
	items   [][]Line // committed lines by item; items[i][j].Item == i
	revs    []uint64 // revs[i] changes whenever items[i] does (render cache key)
	version uint64   // source of revisions
	flat    []Line   // items flattened; nil when stale
	start   []int    // start[i]: index of items[i]'s first line in flat
	cache   renderCache

	calls      map[string]lineRef   // tool call id → its LineTool line
	prompts    map[string]int       // prompt id → item of the tool call it gates
	promptLine map[string]lineRef   // prompt id → its "?" line (tone flips when settled)
	monitors   map[string]lineRef   // monitor id → its "started" line, or the shell call it grew from
	children   map[string]lineRef   // child agent id → the agent_create line that spawned it
	askTarget  map[string]string    // agent_message call id → the agent it asked (until the call finishes)
	asks       map[string][]lineRef // agent id → agent_message lines still waiting for its answer
	monKinds   map[string]string    // monitor id → kind, for the glyph on later events

	streamTurn  int
	stream      []streamSeg
	turn        bool      // a turn is in progress (TurnStarted seen, not yet ended)
	turnStart   time.Time // when the current turn began
	turnTokens  int       // input + output tokens used so far this turn
	turnVerb    string    // the indicator's verb for this turn ("Galloping")
	compactItem int       // item of the running compaction's rule (replaced by the result), -1 when none
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
	return &Transcript{
		calls: map[string]lineRef{}, prompts: map[string]int{}, promptLine: map[string]lineRef{},
		monitors: map[string]lineRef{}, monKinds: map[string]string{}, children: map[string]lineRef{},
		askTarget: map[string]string{}, asks: map[string][]lineRef{}, compactItem: -1,
	}
}

// Apply folds ev into the transcript. An assistant.message (or the end of
// a turn) replaces the in-progress streaming buffer; tool.call.finished
// updates the matching tool line and nests the output under it.
func (t *Transcript) Apply(ev event.Event) {
	// A compaction is one chat item: the rule with the sweeping bar while
	// it runs, replaced in place by the result (or by a note when it
	// failed, or when the turn ended without one — the daemon died).
	switch ev.Type {
	case event.CompactionStarted:
		if refs := t.appendItem(cleanLines(EventLines(ev))); len(refs) > 0 {
			t.compactItem = refs[0].item
		}
		return
	case event.Compacted, event.CompactionFailed:
		if t.compactItem >= 0 {
			t.replaceItem(t.compactItem, cleanLines(EventLines(ev)))
			t.compactItem = -1
			return
		}
	case event.TurnEnded, event.TurnAborted:
		if t.compactItem >= 0 {
			t.replaceItem(t.compactItem, []Line{{Kind: LineBlank}, {Kind: LineDim, Text: "compaction interrupted"}, {Kind: LineBlank}})
			t.compactItem = -1
		}
	}
	switch ev.Type {
	case event.ToolCallStarted:
		var p event.ToolStartedPayload
		if ev.Decode(&p) == nil && p.CallID != "" {
			if r, ok := t.find(t.appendItem(cleanLines(EventLines(ev))), isToolLine); ok {
				t.calls[p.CallID] = r
			}
			if toolname.Canonical(p.Name) == toolname.AgentMessage {
				var in struct{ ID string }
				if json.Unmarshal(p.Input, &in) == nil && in.ID != "" {
					t.askTarget[p.CallID] = in.ID
				}
			}
			return
		}
	case event.PromptRequested:
		// A permission prompt belongs to the call it gates: the open call
		// with the same tool name (the latest one if several).
		var p event.PromptRequestedPayload
		if ev.Decode(&p) == nil {
			lines := cleanLines(EventLines(ev))
			var refs []lineRef
			item, gated := t.openCallItem(p.Tool)
			if p.Kind == "permission" && gated {
				t.prompts[p.ID] = item
				refs = t.insertIntoItem(item, nested(lines))
			} else {
				refs = t.appendItem(lines)
			}
			if r, ok := t.find(refs, isPromptLine); ok {
				t.promptLine[p.ID] = r
			}
			return
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
				t.insertIntoItem(item, nested(cleanLines(EventLines(ev))))
				return
			}
		}
	case event.MonitorStarted:
		var p event.MonitorStartedPayload
		if ev.Decode(&p) == nil && p.ID != "" {
			t.monKinds[p.ID] = p.Kind
			// A job that grew out of a shell call is represented by that
			// call's own line: it stays yellow while the job runs and the
			// outcome nests under it. Only a job with no such call gets its
			// own notice.
			if r, ok := t.lastUntiedCall(toolname.Shell, t.monitors); ok {
				t.monitors[p.ID] = r
				t.line(r).Tone = ToneWorking
				t.stream = nil
				return
			}
			if r, ok := t.find(t.appendItem(cleanLines(EventLines(ev))), isNotBlank); ok {
				t.monitors[p.ID] = r
			}
			return
		}
	case event.MonitorFired:
		// The outcome joins the "started" notice's item, like tool output
		// joins its call.
		var p event.MonitorFiredPayload
		if ev.Decode(&p) == nil {
			if p.Kind == "" {
				p.Kind = t.monKinds[p.ID]
			}
			tone := ToneNone
			if p.IsError {
				tone = ToneError
			}
			t.settleMonitor(p.ID, tone, cleanLines(monitorFiredLines(p)))
			return
		}
	case event.MonitorStopped:
		var p event.MonitorRefPayload
		if ev.Decode(&p) == nil {
			t.settleMonitor(p.ID, ToneError, cleanLines(monitorStoppedLines(t.monKinds[p.ID], p.Reason)))
			return
		}
	case event.ToolCallFinished:
		var p event.ToolFinishedPayload
		if ev.Decode(&p) == nil {
			r, ok := t.calls[p.CallID]
			t.finishCall(p)
			t.stream = nil
			if ok && t.valid(r) {
				// Output joins the call's item, right under the call, even
				// when other items (a question, say) were committed while the
				// call ran.
				t.insertIntoItem(r.item, cleanLines(EventLines(ev)))
				return
			}
		}
		t.stream = nil
	}
	t.appendItem(cleanLines(EventLines(ev)))
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

func isToolLine(l Line) bool   { return l.Kind == LineTool }
func isPromptLine(l Line) bool { return l.Glyph == GlyphPrompt }
func isNotBlank(l Line) bool   { return l.Kind != LineBlank }

// --- item storage ---

// valid reports whether r addresses a committed line.
func (t *Transcript) valid(r lineRef) bool {
	return r.item >= 0 && r.item < len(t.items) && r.off >= 0 && r.off < len(t.items[r.item])
}

// line returns the committed line at r for modification (nil if r is not
// valid); the item is marked changed.
func (t *Transcript) line(r lineRef) *Line {
	if !t.valid(r) {
		return nil
	}
	t.touch(r.item)
	return &t.items[r.item][r.off]
}

// touch records that item changed: the flattened view is rebuilt on the
// next read and the item is rendered afresh.
func (t *Transcript) touch(item int) {
	t.version++
	t.revs[item] = t.version
	t.flat = nil
}

// find returns the first of refs whose line satisfies ok.
func (t *Transcript) find(refs []lineRef, ok func(Line) bool) (lineRef, bool) {
	for _, r := range refs {
		if t.valid(r) && ok(t.items[r.item][r.off]) {
			return r, true
		}
	}
	return lineRef{}, false
}

// appendItem commits lines as new items and returns a ref for each line.
// Thinking is always its own item: each contiguous run of LineThink lines,
// and the run of other lines after it, become successive items.
func (t *Transcript) appendItem(lines []Line) []lineRef {
	if len(lines) == 0 {
		return nil
	}
	refs := make([]lineRef, 0, len(lines))
	for _, run := range splitThinking(lines) {
		i := len(t.items)
		run = slices.Clone(run) // each item owns its array: later inserts must not overwrite a neighbour
		for j := range run {
			run[j].Item = i
			refs = append(refs, lineRef{i, j})
		}
		t.items = append(t.items, run)
		t.version++
		t.revs = append(t.revs, t.version)
		if t.flat != nil {
			t.start = append(t.start, len(t.flat))
			t.flat = append(t.flat, run...)
		}
	}
	return refs
}

// insertIntoItem appends lines to the end of an existing item, so the item
// stays contiguous, and returns their refs.
func (t *Transcript) insertIntoItem(item int, lines []Line) []lineRef {
	if len(lines) == 0 || item < 0 || item >= len(t.items) {
		return nil
	}
	refs := make([]lineRef, 0, len(lines))
	for j := range lines {
		lines[j].Item = item
		refs = append(refs, lineRef{item, len(t.items[item]) + j})
	}
	t.items[item] = append(t.items[item], lines...)
	t.touch(item)
	return refs
}

// replaceItem swaps every line of item for lines. Refs into the item are
// no longer meaningful; a compaction rule, the only item replaced, has none.
func (t *Transcript) replaceItem(item int, lines []Line) {
	if item < 0 || item >= len(t.items) || len(lines) == 0 {
		return
	}
	lines = slices.Clone(lines)
	for j := range lines {
		lines[j].Item = item
	}
	t.items[item] = lines
	t.touch(item)
}

// committed is every committed line in item order.
func (t *Transcript) committed() []Line {
	if t.flat == nil {
		n := 0
		for _, it := range t.items {
			n += len(it)
		}
		// A fresh array: slices handed out earlier stay as they were.
		t.flat, t.start = make([]Line, 0, n), make([]int, 0, len(t.items))
		for _, it := range t.items {
			t.start = append(t.start, len(t.flat))
			t.flat = append(t.flat, it...)
		}
	}
	return t.flat
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

// --- tracked lines ---

// settlePrompt ends the "in progress" tone on a prompt's "?" line: as-is
// when answered, red when denied by default or withdrawn.
func (t *Transcript) settlePrompt(id string, terminated bool) {
	r, ok := t.promptLine[id]
	delete(t.promptLine, id)
	if !ok {
		return
	}
	if l := t.line(r); l != nil {
		l.Tone = ToneNone
		if terminated {
			l.Tone = ToneError
		}
	}
}

// settleMonitor gives a job's line its final tone and nests the outcome
// under it (indented when the line is the shell call the job grew from); a
// job the transcript never saw start gets the outcome as its own item.
func (t *Transcript) settleMonitor(id string, tone Tone, lines []Line) {
	r, ok := t.monitors[id]
	delete(t.monitors, id)
	l := t.line(r)
	if !ok || l == nil {
		t.appendItem(lines)
		return
	}
	l.Tone = tone
	if l.Kind == LineTool {
		lines = nested(lines)
	}
	t.insertIntoItem(r.item, lines)
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

// lastUntiedCall returns the most recent call line of tool that no entry
// of tied already claims.
func (t *Transcript) lastUntiedCall(tool string, tied map[string]lineRef) (lineRef, bool) {
	taken := make(map[lineRef]bool, len(tied))
	for _, r := range tied {
		taken[r] = true
	}
	for i := len(t.items) - 1; i >= 0; i-- {
		for j := len(t.items[i]) - 1; j >= 0; j-- {
			if l := t.items[i][j]; l.Kind == LineTool && l.tool == tool && !taken[lineRef{i, j}] {
				return lineRef{i, j}, true
			}
		}
	}
	return lineRef{}, false
}

// openCallItem returns the item of the most recently started, still-open
// call of tool name.
func (t *Transcript) openCallItem(name string) (int, bool) {
	var best lineRef
	found := false
	for _, r := range t.calls {
		if t.valid(r) && strings.EqualFold(t.items[r.item][r.off].tool, name) && (!found || r.after(best)) {
			best, found = r, true
		}
	}
	return best.item, found
}

// Compacting reports whether a compaction rule is waiting for its result.
func (t *Transcript) Compacting() bool { return t.compactItem >= 0 }

// ChildSpawned ties a just-spawned child to the agent_create call that
// made it: the call line reads as in progress (yellow) until the child
// settles, the way a shell call tracks its job.
func (t *Transcript) ChildSpawned(childID string) {
	if r, ok := t.lastUntiedCall(toolname.AgentCreate, t.children); ok {
		t.line(r).Tone = ToneWorking
		t.children[childID] = r
	}
}

// ChildState colours the agent_create line of a child by its current
// state: working (yellow) while it runs or is blocked, red once killed,
// grey when it idles. Children no longer finish; they answer and wait.
func (t *Transcript) ChildState(childID string, state protocol.AgentState) {
	r, ok := t.children[childID]
	l := t.line(r)
	if !ok || l == nil {
		return
	}
	switch {
	case state.Busy():
		l.Tone = ToneWorking
	case state == protocol.AgentKilled:
		l.Tone = ToneError
		delete(t.children, childID)
	default:
		l.Tone = ToneNone
	}
}

func (t *Transcript) finishCall(p event.ToolFinishedPayload) {
	r, ok := t.calls[p.CallID]
	l := t.line(r)
	if !ok || l == nil {
		return
	}
	l.Running = false
	l.Err = p.IsError
	switch {
	case p.Cancelled:
		l.Suffix = "(cancelled)"
	case p.Denied:
		l.Suffix = "(denied)"
	}
	// A delivered agent_message waits for that agent's answer: yellow until
	// its agent_response lands (see answered), like a shell call and its job.
	if target, ok := t.askTarget[p.CallID]; ok {
		delete(t.askTarget, p.CallID)
		if !p.IsError && !p.Cancelled && !p.Denied {
			l.Tone = ToneWorking
			t.asks[target] = append(t.asks[target], r)
		}
	}
	delete(t.calls, p.CallID)
}

// answered settles every outstanding agent_message to the agent named in a
// response's From ("label (shortid)" or a bare id): one answer covers all
// the questions asked of it so far.
func (t *Transcript) answered(from string) {
	for target, refs := range t.asks {
		if len(refs) == 0 || (from != target && !strings.Contains(from, "("+shortID(target)+")")) {
			continue
		}
		t.setTone(refs, ToneNone)
		delete(t.asks, target)
	}
}

// AskerGone marks every outstanding agent_message to a killed agent red:
// no answer is coming.
func (t *Transcript) AskerGone(id string) {
	t.setTone(t.asks[id], ToneError)
	delete(t.asks, id)
}

func (t *Transcript) setTone(refs []lineRef, tone Tone) {
	for _, r := range refs {
		if l := t.line(r); l != nil {
			l.Tone = tone
		}
	}
}

// stopRunning clears every spinner when a turn ends.
func (t *Transcript) stopRunning() {
	for i := range t.items {
		for j := range t.items[i] {
			if t.items[i][j].Running {
				t.items[i][j].Running = false
				t.touch(i)
			}
		}
	}
	t.calls = map[string]lineRef{}
}

// ApplyStream folds a transient stream notification into the live buffer.
// ToolName+Text is partial tool output; ToolName alone is a tool_use block
// starting in the model's response.
func (t *Transcript) ApplyStream(n protocol.StreamNotification) {
	n.Text, n.Thinking, n.ToolName = textsafe.Clean(n.Text), textsafe.Clean(n.Thinking), toolname.Canonical(n.ToolName)
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
	t.appendItem(ls)
}

// All returns the committed lines followed by the live streaming buffer.
// The slice must not be modified.
func (t *Transcript) All() []Line {
	lines, tail := t.committed(), t.tail()
	if len(tail) == 0 {
		return lines
	}
	return append(slices.Clip(lines), tail...)
}

// tail renders the live streaming buffer as lines. They belong to the
// in-progress item: the running tool call when the buffer continues one,
// otherwise new items after the committed ones.
func (t *Transcript) tail() []Line {
	if len(t.stream) == 0 {
		return nil
	}
	next := len(t.items)
	item := next
	if next > 0 {
		if last := t.items[next-1]; len(last) > 0 {
			if l := last[len(last)-1]; l.Kind == LineTool && l.Running {
				item = next - 1
			}
		}
	}
	var out []Line
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
	for i := range out {
		if i > 0 && (out[i].Kind == LineThink) != (out[i-1].Kind == LineThink) && !(cur == item && item != next) {
			cur++
		}
		out[i].Item = cur
	}
	return out
}

// Items is the number of items All() spans (committed plus the in-progress
// one when the streaming buffer starts a new item).
func (t *Transcript) Items() int {
	if tail := t.tail(); len(tail) > 0 {
		return max(len(t.items), tail[len(tail)-1].Item+1)
	}
	return len(t.items)
}

// ItemRange returns the first and last index into All() of item i, or
// (-1, -1) when there is no such item. Items are contiguous: tool output is
// nested under its call even when other events landed in between.
func (t *Transcript) ItemRange(i int) (first, last int) {
	if len(t.stream) > 0 && i >= len(t.items)-1 {
		return itemRange(t.All(), i) // the buffer may extend or follow the last item
	}
	if i < 0 || i >= len(t.items) || len(t.items[i]) == 0 {
		return -1, -1
	}
	t.committed()
	return t.start[i], t.start[i] + len(t.items[i]) - 1
}

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
func (t *Transcript) Empty() bool { return len(t.items) == 0 && len(t.stream) == 0 }

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
		case event.MsgPrompt, "":
			if p.From != "" {
				return block(BlockUser, "from "+p.From, p.Text)
			}
			return block(BlockUser, "", p.Text)
		case event.MsgSteer: // shown exactly like a prompt: blue ›, no title
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
		case event.MsgMonitorFired:
			return blockWith(BlockChild, "job result", p.Text, monitorGlyph("command"))
		default:
			return block(BlockUser, string(p.Kind), p.Text)
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
		p.Name = toolname.Canonical(p.Name) // logs from before a rename read as the current tool
		if p.Name == toolname.AgentResponse {
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
		if toolname.Canonical(p.Name) == toolname.AgentResponse && !p.IsError {
			return nil // the message already sits under the call; "response delivered" adds nothing
		}
		return outputLines(strings.TrimRight(p.Output, "\n"))

	case event.TurnEnded:
		var p event.TurnEndedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		switch p.Reason {
		case event.ReasonCancelled:
			return []Line{{Kind: LineDim, Glyph: GlyphTurn, Tone: ToneError, Text: "turn cancelled"}, {Kind: LineBlank}}
		case event.ReasonError:
			msg := p.Error
			if msg == "" {
				msg = "turn error"
			}
			return errorBlock(msg)
		case event.ReasonMaxTokens:
			return []Line{{Kind: LineDim, Glyph: GlyphTurn, Tone: ToneError, Text: "turn stopped: max_tokens"}, {Kind: LineBlank}}
		case event.ReasonEndTurn:
			// the reply speaks for itself
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

	case event.SessionYoloChanged: // legacy logs
		var p event.YoloPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		mode := protocol.ModeAsk
		if p.On {
			mode = protocol.ModeYolo
		}
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "mode → " + mode + " · " + modeDesc(mode)}}

	case event.SessionModeChanged:
		var p event.ModePayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "mode → " + p.Mode + " · " + modeDesc(p.Mode)}}

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

	case event.MCPStarted:
		var p event.MCPStartedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineDim, Glyph: glyphToolMCP, Text: fmt.Sprintf("mcp: %s connected · %d tools", p.Server, len(p.Tools))}}

	case event.MCPFailed:
		var p event.MCPFailedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineError, Glyph: glyphToolMCP, Tone: ToneError, Text: fmt.Sprintf("mcp: %s failed: %s", p.Server, p.Error)}}

	case event.AgentDirAdded:
		var p event.DirAddedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineDim, Glyph: glyphToolFiles, Text: fmt.Sprintf("dirs: + %s (%s)", shortHome(p.Dir), p.Source)}}

	case event.MCPStopped:
		var p event.MCPRefPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineDim, Glyph: glyphToolMCP, Text: "mcp: " + p.Server + " stopped"}}

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

	case event.CompactionStarted:
		return []Line{{Kind: LineBlank}, {Kind: LineRule, Text: GlyphCompacting}, {Kind: LineBlank}}

	case event.CompactionFailed:
		var p event.CompactionPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineBlank}, {Kind: LineDim, Text: "compaction failed: " + p.Error}, {Kind: LineBlank}}

	case event.Compacted:
		var p event.CompactedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		rule := GlyphCompacted
		if p.Before > 0 && p.After > 0 {
			rule = fmt.Sprintf("┄┄ compacted %s → %s tokens ┄┄", fmtTokens(p.Before), fmtTokens(p.After))
		}
		lines := []Line{{Kind: LineBlank}, {Kind: LineRule, Text: rule}}
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
	name = toolname.Canonical(name) // a direct caller (a prompt, a test) may hold an old name
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
	case toolname.Shell:
		return str("command")
	case toolname.ShellKill:
		return str("id")
	case toolname.WebFetch:
		return str("url")
	case toolname.WebSearch:
		return str("query")
	case toolname.Read:
		return str("path")
	case toolname.ApplyPatch:
		if patch, ok := in["patch"].(string); ok {
			return patchFiles(patch)
		}
		return ""
	case toolname.AgentCreate:
		label, arch := str("label"), str("archetype")
		switch {
		case label != "" && arch != "":
			return label + " (" + arch + ")"
		case label != "":
			return label
		default:
			return arch
		}
	case toolname.AgentMessage, toolname.AgentCancel, toolname.AgentStatus:
		return str("id")
	case toolname.Skill:
		return str("name")
	case toolname.AskUser:
		var a struct {
			Questions []struct{ Question string }
		}
		if json.Unmarshal(raw, &a) == nil {
			var qs []string
			for _, q := range a.Questions {
				qs = append(qs, q.Question)
			}
			return strings.Join(qs, " · ")
		}
		return ""
	case toolname.TodoAdd:
		return str("text")
	case toolname.TodoUpdate:
		out := str("id")
		if st := str("status"); st != "" {
			out += " → " + st
		}
		if tx := str("text"); tx != "" {
			out += "  " + tx
		}
		return out
	case toolname.AgentResponse:
		return "→ " + str("to") // outgoing: who it answers
	}
	return compactArgs(raw)
}

// toolTitle is the display name of a tool on its chat line: titleCase of
// the name; agent_response reads as what it did.
func toolTitle(name string) string {
	if name == toolname.AgentResponse {
		return "Agent response delivered"
	}
	if strings.HasPrefix(name, toolname.MCPPrefix) {
		// mcp__server__tool reads "server · tool"
		if parts := strings.SplitN(strings.TrimPrefix(name, toolname.MCPPrefix), "__", 2); len(parts) == 2 {
			return parts[0] + " · " + parts[1]
		}
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
