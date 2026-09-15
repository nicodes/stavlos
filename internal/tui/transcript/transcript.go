// Package transcript folds an agent's events into chat lines grouped in
// items (a prompt, a tool call with its output, an answer), tracks the
// lines later events update, and exposes items with revisions so a
// renderer can cache them.
package transcript

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
	"github.com/nicodes/stavlos/internal/tui/format"
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
	Tool    string // raw tool name on a LineTool line
	Note    bool   // the agent's own text, which reaches no one: drawn dimmed
	Agent   string // in the session chat: the agent this line links to
	Indent  int    // nesting depth, two columns each (a reply under its post in the session chat)
	Spacer  bool   // a blank row kept inside an item (the session chat's gap before a reply)
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
	MaxOutputCollapsed = 3
	MaxOutputExpanded  = 40
	maxSummaryLine     = 3
	maxThinkChars      = 100
)

// streamSeg is one piece of the live buffer for the current turn.
type streamSeg struct {
	kind LineKind // LineStream (text), LineDim (thinking), LineTool (tool name), LineToolOut (tool output)
	text string
	Tool string // raw tool name for LineTool
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

	calls      map[string]lineRef   // tool call id → its LineTool line
	prompts    map[string]int       // prompt id → item of the tool call it gates
	promptLine map[string]lineRef   // prompt id → its "?" line (tone flips when settled)
	monitors   map[string]lineRef   // monitor id → its "started" line, or the shell call it grew from
	children   map[string]lineRef   // child agent id → the agent_create line that spawned it
	asks       map[string][]lineRef // agent name → message lines still waiting for its answer
	monKinds   map[string]string    // monitor id → kind, for the glyph on later events

	streamTurn  int
	stream      []streamSeg
	turn        bool      // a turn is in progress (TurnStarted seen, not yet ended)
	turnStart   time.Time // when the current turn began
	turnTokens  int       // input + output tokens used so far this turn
	turnVerb    string    // the indicator's verb for this turn ("Galloping")
	compactItem int       // item of the running compaction's rule (replaced by the result), -1 when none

	chat  bool              // the session chat (chat.go), not one agent's transcript
	names map[string]string // in the chat: agent id → name
	posts map[string]int    // in the chat: post id → its item, which its replies join
	open  map[string]int    // in the chat: agent name → the thread still waiting on its reply
}

// turnVerbs are the horse-flavoured labels the turn indicator cycles
// through, one per turn (stable within a turn so the line does not
// flicker).
var TurnVerbs = []string{
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
		asks: map[string][]lineRef{}, compactItem: -1,
	}
}

// Apply folds ev into the transcript. An assistant.message (or the end of
// a turn) replaces the in-progress streaming buffer; tool.call.finished
// updates the matching tool line and nests the output under it.
func (t *Transcript) Apply(ev event.Event) {
	if t.chat {
		t.applyChat(ev)
		return
	}
	if t.applyCompaction(ev) {
		return
	}
	switch ev.Type {
	case event.ToolCallStarted, event.ToolCallFinished:
		if t.applyToolCall(ev) {
			return
		}
	case event.PromptRequested, event.PromptClaimed, event.PromptAnswered, event.PromptWithdrawn, event.PromptDefaulted:
		if t.applyPrompt(ev) {
			return
		}
	case event.MonitorStarted, event.MonitorFired, event.MonitorStopped:
		if t.applyMonitor(ev) {
			return
		}
	}
	t.appendItem(CleanLines(EventLines(ev)))
	t.afterAppend(ev)
}

// applyCompaction keeps a compaction as one chat item: the rule with the
// sweeping bar while it runs, replaced in place by the result (or by a note
// when it failed, or when the turn ended without one — the daemon died).
// It reports whether ev was fully handled.
func (t *Transcript) applyCompaction(ev event.Event) bool {
	switch ev.Type {
	case event.CompactionStarted:
		if refs := t.appendItem(CleanLines(EventLines(ev))); len(refs) > 0 {
			t.compactItem = refs[0].item
		}
		return true
	case event.Compacted, event.CompactionFailed:
		if t.compactItem >= 0 {
			t.replaceItem(t.compactItem, CleanLines(EventLines(ev)))
			t.compactItem = -1
			return true
		}
	case event.TurnEnded, event.TurnAborted:
		if t.compactItem >= 0 {
			t.replaceItem(t.compactItem, []Line{{Kind: LineBlank}, {Kind: LineDim, Text: "compaction interrupted"}, {Kind: LineBlank}})
			t.compactItem = -1
		}
	}
	return false
}

// applyToolCall tracks a call's line and nests its output under it. It
// reports whether ev was fully handled.
func (t *Transcript) applyToolCall(ev event.Event) bool {
	if ev.Type == event.ToolCallStarted {
		var p event.ToolStartedPayload
		if ev.Decode(&p) != nil || p.CallID == "" {
			return false
		}
		if r, ok := t.find(t.appendItem(CleanLines(EventLines(ev))), isToolLine); ok {
			t.calls[p.CallID] = r
		}
		return true
	}
	var p event.ToolFinishedPayload
	if ev.Decode(&p) != nil {
		t.stream = nil
		return false
	}
	r, ok := t.calls[p.CallID]
	t.finishCall(p)
	t.stream = nil
	if ok && t.valid(r) {
		// Output joins the call's item, right under the call, even when
		// other items (a question, say) were committed while the call ran.
		t.insertIntoItem(r.item, CleanLines(EventLines(ev)))
		return true
	}
	return false
}

// applyPrompt nests a permission prompt, and what became of it, under the
// call it gates, and settles the prompt's "?" line. It reports whether ev
// was fully handled.
func (t *Transcript) applyPrompt(ev event.Event) bool {
	if ev.Type == event.PromptRequested {
		// A permission prompt belongs to the call it gates: the open call
		// with the same tool name (the latest one if several).
		var p event.PromptRequestedPayload
		if ev.Decode(&p) != nil {
			return false
		}
		lines := CleanLines(EventLines(ev))
		var refs []lineRef
		if item, gated := t.openCallItem(p.Tool); p.Kind == "permission" && gated {
			t.prompts[p.ID] = item
			refs = t.insertIntoItem(item, nested(lines))
		} else {
			refs = t.appendItem(lines)
		}
		if r, ok := t.find(refs, isPromptLine); ok {
			t.promptLine[p.ID] = r
		}
		return true
	}
	var p event.PromptRefPayload
	if ev.Decode(&p) != nil {
		return false
	}
	if ev.Type != event.PromptClaimed {
		t.settlePrompt(p.ID, ev.Type != event.PromptAnswered)
	}
	item, ok := t.prompts[p.ID]
	if !ok {
		return false
	}
	if ev.Type != event.PromptClaimed {
		delete(t.prompts, p.ID)
	}
	t.insertIntoItem(item, nested(CleanLines(EventLines(ev))))
	return true
}

// applyMonitor ties a background job to the shell call it grew from, or to
// a notice of its own, and nests its outcome there. It reports whether ev
// was fully handled.
func (t *Transcript) applyMonitor(ev event.Event) bool {
	switch ev.Type {
	case event.MonitorStarted:
		var p event.MonitorStartedPayload
		if ev.Decode(&p) != nil || p.ID == "" {
			return false
		}
		t.monKinds[p.ID] = p.Kind
		// A job that grew out of a shell call is represented by that call's
		// own line: it stays yellow while the job runs and the outcome nests
		// under it. Only a job with no such call gets its own notice.
		if r, ok := t.lastUntiedCall(toolname.Shell, t.monitors); ok {
			t.monitors[p.ID] = r
			t.line(r).Tone = ToneWorking
			t.stream = nil
			return true
		}
		if r, ok := t.find(t.appendItem(CleanLines(EventLines(ev))), isNotBlank); ok {
			t.monitors[p.ID] = r
		}
		return true
	case event.MonitorFired:
		// The outcome joins the "started" notice's item, like tool output
		// joins its call.
		var p event.MonitorFiredPayload
		if ev.Decode(&p) != nil {
			return false
		}
		if p.Kind == "" {
			p.Kind = t.monKinds[p.ID]
		}
		tone := ToneNone
		if p.IsError {
			tone = ToneError
		}
		t.settleMonitor(p.ID, tone, CleanLines(monitorFiredLines(p)))
		return true
	case event.MonitorStopped:
		var p event.MonitorRefPayload
		if ev.Decode(&p) != nil {
			return false
		}
		t.settleMonitor(p.ID, ToneError, CleanLines(monitorStoppedLines(t.monKinds[p.ID], p.Reason)))
		return true
	}
	return false
}

// afterAppend updates the turn state an event carries once its lines are in.
func (t *Transcript) afterAppend(ev event.Event) {
	switch ev.Type {
	case event.TurnStarted:
		t.turn, t.turnStart, t.turnTokens = true, ev.Time, 0
		var p event.TurnPayload
		_ = ev.Decode(&p)
		t.turnVerb = TurnVerbs[((p.Turn-1)%len(TurnVerbs)+len(TurnVerbs))%len(TurnVerbs)]
	case event.Usage:
		var p event.UsagePayload
		if ev.Decode(&p) == nil {
			t.turnTokens += p.Usage.InputTokens + p.Usage.OutputTokens
		}
	case event.UserMessage:
		var p event.UserMessagePayload
		if ev.Decode(&p) == nil && p.Kind == event.MsgAgentResponse {
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

// lastUntiedCall returns the most recent call line of tool that no entry
// of tied already claims.
func (t *Transcript) lastUntiedCall(tool string, tied map[string]lineRef) (lineRef, bool) {
	taken := make(map[lineRef]bool, len(tied))
	for _, r := range tied {
		taken[r] = true
	}
	for i := len(t.items) - 1; i >= 0; i-- {
		for j := len(t.items[i]) - 1; j >= 0; j-- {
			if l := t.items[i][j]; l.Kind == LineTool && l.Tool == tool && !taken[lineRef{i, j}] {
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
		if t.valid(r) && strings.EqualFold(t.items[r.item][r.off].Tool, name) && (!found || r.after(best)) {
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
	// A new message to an agent waits for its answer: yellow until the
	// answer lands (see answered), like a shell call and its job.
	if name, ok := messagedAgent(p); ok {
		l.Tone = ToneWorking
		t.asks[name] = append(t.asks[name], r)
	}
	delete(t.calls, p.CallID)
}

// partyList reads reply parties for the human: "user" is "you".
func partyList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n
		if n == "user" {
			out[i] = "you"
		}
	}
	return strings.Join(out, ", ")
}

// messageDelivered starts the result of a message that the sender now
// waits on (an answer or a message to the user reads differently).
const messageDelivered = "message delivered to "

// messagedAgent is the name of the agent a finished message call is waiting
// on: set only for a new message delivered to an agent, not for an answer,
// a message to the user, or a call that failed.
func messagedAgent(p event.ToolFinishedPayload) (string, bool) {
	if toolname.Canonical(p.Name) != toolname.Message || p.IsError || p.Cancelled || p.Denied {
		return "", false
	}
	rest, ok := strings.CutPrefix(p.Output, messageDelivered)
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, ";")
	if name == "" || name == "the user" {
		return "", false
	}
	return name, true
}

// answered settles every outstanding message to the agent named in an
// answer's From: one answer covers all the questions asked of it so far.
func (t *Transcript) answered(from string) {
	t.setTone(t.asks[from], ToneNone)
	delete(t.asks, from)
}

// AskerGone marks every outstanding message to a killed agent (by name)
// red: no answer is coming.
func (t *Transcript) AskerGone(name string) {
	t.setTone(t.asks[name], ToneError)
	delete(t.asks, name)
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
		t.stream = append(t.stream, streamSeg{LineTool, ToolTitle(n.ToolName), n.ToolName})
	}
}

// All returns the committed lines followed by the live streaming buffer.
// The slice must not be modified.
func (t *Transcript) All() []Line {
	lines, tail := t.committed(), t.Tail()
	if len(tail) == 0 {
		return lines
	}
	return append(slices.Clip(lines), tail...)
}

// Tail renders the live streaming buffer as lines. They belong to the
// in-progress item: the running tool call when the buffer continues one,
// otherwise new items after the committed ones.
func (t *Transcript) Tail() []Line {
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
			out = append(out, OutputLines(strings.TrimRight(s.text, "\n"))...)
		case LineTool:
			out = append(out, Line{Kind: LineTool, Text: s.text, Running: true, Tool: s.Tool})
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
	if tail := t.Tail(); len(tail) > 0 {
		return max(len(t.items), tail[len(tail)-1].Item+1)
	}
	return len(t.items)
}

func ItemCount(lines []Line) int {
	n := 0
	for _, l := range lines {
		if l.Item+1 > n {
			n = l.Item + 1
		}
	}
	return n
}

func ItemRange(lines []Line, item int) (first, last int) {
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
func ItemIsTool(lines []Line, item int) bool {
	for _, l := range lines {
		if l.Item == item && l.Kind == LineTool {
			return true
		}
	}
	return false
}

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

// EventLines renders a single event. Types without a renderer (usage,
// turn.started, prompt.queued, …) yield no lines.
func EventLines(ev event.Event) []Line {
	if render, ok := eventRenderers[ev.Type]; ok {
		return render(ev)
	}
	return nil
}

// decoded adapts a renderer of payload P: a payload that does not decode
// renders as an error line.
func decoded[P any](render func(P) []Line) func(event.Event) []Line {
	return func(ev event.Event) []Line {
		var p P
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return render(p)
	}
}

// eventRenderers draws each chat-visible event type.
var eventRenderers = map[event.Type]func(event.Event) []Line{
	event.AgentSpawned: decoded(func(p event.AgentSpawnedPayload) []Line {
		if p.Parent == "" {
			return nil // the root's own spawn is not a message; keeps the home state empty
		}
		lines := []Line{{Kind: LineDim, Glyph: GlyphSpawn, Text: fmt.Sprintf("spawned %s (%s) · %s", p.Label, p.Archetype, p.Model)}}
		if p.Task != "" {
			lines = append(lines, blockWith(BlockChild, "task", p.Task, GlyphTask)...)
		}
		return lines
	}),

	event.UserMessage: decoded(func(p event.UserMessagePayload) []Line {
		switch p.Kind {
		case event.MsgPrompt, "", event.MsgSteer: // a steer reads exactly like a prompt
			if p.From != "" {
				return incoming("Message from "+p.From, p.Text)
			}
			return block(BlockUser, "", p.Text) // the human's own input: blue ›
		case event.MsgAgentResponse:
			if p.From != "" {
				return incoming("Answer from "+p.From, p.Text)
			}
			return incoming("Answer", p.Text)
		case "child_finished": // legacy: finished children from old logs
			return blockWith(BlockChild, "agent response", p.Text, GlyphChild)
		case event.MsgMonitorFired:
			return blockWith(BlockChild, "job result", p.Text, GlyphToolMonitors)
		case event.MsgReminder:
			return nil // the reminder.queued notice already said it
		default:
			return block(BlockUser, string(p.Kind), p.Text)
		}
	}),

	event.AssistantMessage: decoded(func(p event.AssistantMessagePayload) []Line {
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
				// The text a turn ends with reaches no one (replies go
				// through message): it reads as the agent's notes, dimmed.
				for _, l := range markdownLines(text) {
					l.Note = true
					lines = append(lines, l)
				}
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
	}),

	event.ReminderQueued: decoded(func(p event.RepliesPayload) []Line {
		return []Line{{Kind: LineNotice, Glyph: GlyphPrompt, Tone: ToneWorking, Text: "reminded: no reply yet to " + partyList(p.Names)}}
	}),

	event.ReplyMissing: decoded(func(p event.RepliesPayload) []Line {
		return []Line{{Kind: LineNotice, Glyph: GlyphPrompt, Tone: ToneError, Text: "ended without replying to " + partyList(p.Names)}}
	}),

	event.ToolCallStarted: decoded(func(p event.ToolStartedPayload) []Line {
		p.Name = toolname.Canonical(p.Name) // logs from before a rename read as the current tool
		if p.Name == toolname.Message {
			// The message itself is the interesting part: show it under the
			// call the way an incoming answer shows its text.
			var in struct{ Text string }
			_ = json.Unmarshal(p.Input, &in)
			lines := []Line{{Kind: LineTool, Text: toolLine(p.Name, p.Input), Running: true, Tool: p.Name}}
			return append(lines, OutputLines(strings.TrimRight(in.Text, "\n"))...)
		}
		return []Line{{Kind: LineTool, Text: toolLine(p.Name, p.Input), Running: true, callID: p.CallID, Tool: p.Name}}
	}),

	event.ToolCallFinished: decoded(func(p event.ToolFinishedPayload) []Line {
		if toolname.Canonical(p.Name) == toolname.Message && !p.IsError {
			return nil // the text already sits under the call; "delivered" adds nothing
		}
		return OutputLines(strings.TrimRight(p.Output, "\n"))
	}),

	event.TurnEnded: decoded(func(p event.TurnEndedPayload) []Line {
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
	}),

	event.TurnAborted: func(event.Event) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphTurn, Tone: ToneError, Text: "turn aborted (daemon restart)"}, {Kind: LineBlank}}
	},

	event.AgentFinished: decoded(func(p event.AgentFinishedPayload) []Line {
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
	}),

	event.AgentKilled: func(event.Event) []Line {
		return errorBlockWith("killed", GlyphKilled)
	},

	event.AgentRoleChanged: decoded(func(p event.RoleChangedPayload) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "role → " + p.Role}}
	}),

	event.AgentModelChanged: decoded(func(p event.ModelChangedPayload) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "model → " + p.Model}}
	}),

	event.SessionYoloChanged: decoded(func(p event.YoloPayload) []Line { // legacy logs
		mode := protocol.ModeAsk
		if p.On {
			mode = protocol.ModeYolo
		}
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "mode → " + mode + " · " + protocol.ModeSummary(mode)}}
	}),

	event.SessionModeChanged: decoded(func(p event.ModePayload) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "mode → " + p.Mode + " · " + protocol.ModeSummary(p.Mode)}}
	}),

	event.AgentVariantChanged: decoded(func(p event.VariantChangedPayload) []Line {
		v := p.Variant
		if v == "" {
			v = "default"
		}
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: "variant → " + v}}
	}),

	event.MonitorStarted: decoded(func(p event.MonitorStartedPayload) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphToolMonitors, Tone: ToneWorking, Text: "job: " + p.Label}}
	}),

	event.MCPStarted: decoded(func(p event.MCPStartedPayload) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphToolMCP, Text: fmt.Sprintf("mcp: %s connected · %d tools", p.Server, len(p.Tools))}}
	}),

	event.MCPFailed: decoded(func(p event.MCPFailedPayload) []Line {
		return []Line{{Kind: LineError, Glyph: GlyphToolMCP, Tone: ToneError, Text: fmt.Sprintf("mcp: %s failed: %s", p.Server, p.Error)}}
	}),

	event.AgentDirAdded: decoded(func(p event.DirAddedPayload) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphToolFiles, Text: fmt.Sprintf("dirs: + %s (%s)", format.ShortHome(p.Dir), p.Source)}}
	}),

	event.MCPStopped: decoded(func(p event.MCPRefPayload) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphToolMCP, Text: "mcp: " + p.Server + " stopped"}}
	}),

	event.MonitorFired: decoded(func(p event.MonitorFiredPayload) []Line {
		return monitorFiredLines(p)
	}),

	// Without the transcript's id → kind memory the kind is unknown here;
	// Transcript.Apply looks it up.
	event.MonitorStopped: decoded(func(p event.MonitorRefPayload) []Line {
		return monitorStoppedLines("", p.Reason)
	}),

	event.CompactionStarted: func(event.Event) []Line {
		return []Line{{Kind: LineBlank}, {Kind: LineRule, Text: GlyphCompacting}, {Kind: LineBlank}}
	},

	event.CompactionFailed: decoded(func(p event.CompactionPayload) []Line {
		return []Line{{Kind: LineBlank}, {Kind: LineDim, Text: "compaction failed: " + p.Error}, {Kind: LineBlank}}
	}),

	event.Compacted: decoded(func(p event.CompactedPayload) []Line {
		rule := GlyphCompacted
		if p.Before > 0 && p.After > 0 {
			rule = fmt.Sprintf("┄┄ compacted %s → %s tokens ┄┄", format.Tokens(p.Before), format.Tokens(p.After))
		}
		lines := []Line{{Kind: LineBlank}, {Kind: LineRule, Text: rule}}
		if s := strings.TrimSpace(p.Summary); s != "" {
			lines = append(lines, truncLines(s, maxSummaryLine, LineDim)...)
		}
		return append(lines, Line{Kind: LineBlank})
	}),

	event.PromptRequested: decoded(func(p event.PromptRequestedPayload) []Line {
		switch p.Kind {
		case "question":
			return []Line{{Kind: LineNotice, Glyph: GlyphPrompt, Tone: ToneWorking, Text: "question: " + format.FirstLine(p.Question)}}
		case "trust":
			return []Line{{Kind: LineNotice, Glyph: GlyphPrompt, Tone: ToneWorking, Text: "trust requested"}}
		default:
			return []Line{{Kind: LineNotice, Glyph: GlyphPrompt, Tone: ToneWorking, Text: "permission: " + p.Tool}}
		}
	}),

	event.PromptAnswered: decoded(func(p event.PromptAnsweredPayload) []Line {
		ln := Line{Kind: LineNotice, Glyph: GlyphAnswer, Text: "answered: " + format.FirstLine(p.Answer)}
		if strings.HasPrefix(strings.ToLower(p.Answer), "deny") {
			ln.Tone = ToneError
		}
		return []Line{ln}
	}),

	event.PromptDefaulted: decoded(func(p event.PromptAnsweredPayload) []Line {
		return []Line{{Kind: LineNotice, Glyph: GlyphAnswer, Tone: ToneError, Text: "defaulted: " + format.FirstLine(p.Answer)}}
	}),

	event.PromptWithdrawn: func(event.Event) []Line {
		return []Line{{Kind: LineNotice, Glyph: GlyphAnswer, Tone: ToneError, Text: "prompt withdrawn"}}
	},
}

// --- helpers ---

func decodeErr(ev event.Event, err error) []Line {
	return []Line{{Kind: LineError, Text: fmt.Sprintf("(bad %s payload: %v)", ev.Type, err)}}
}

// monitorFiredLines renders "<glyph> <summary>" (red on error) followed by
// the output collapsed like tool output.
func monitorFiredLines(p event.MonitorFiredPayload) []Line {
	head := Line{Kind: LineDim, Glyph: GlyphToolMonitors}
	if p.IsError {
		head.Tone = ToneError
	}
	summary := strings.TrimSpace(p.Summary)
	if summary == "" {
		summary = "monitor fired"
	}
	head.Text = summary
	lines := []Line{head}
	return append(lines, OutputLines(strings.TrimRight(p.Output, "\n"))...)
}

// monitorStoppedLines renders "<glyph> monitor stopped (<reason>)".
func monitorStoppedLines(kind, reason string) []Line {
	text := "job stopped"
	if reason = strings.TrimSpace(reason); reason != "" {
		text += " (" + reason + ")"
	}
	return []Line{{Kind: LineDim, Glyph: GlyphToolMonitors, Tone: ToneError, Text: text}}
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

// incoming is what another agent sent this one, a message or an answer: a
// tool-like head ("⑂ Message from scout") over its text. Only the human's
// own input is drawn blue, and the agent's own message calls read
// "Message  → name", so incoming and outgoing never look alike.
func incoming(head, text string) []Line {
	lines := []Line{{Kind: LineBlank}, {Kind: LineText, Text: "**" + head + "**", Block: BlockChild, Glyph: GlyphChild}}
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		lines = append(lines, Line{Kind: LineText, Text: l, Block: BlockChild})
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
	return Line{Kind: LineThink, Text: "◌ " + format.Trunc(first, maxThinkChars)}
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
	title := ToolTitle(name)
	arg := ToolArg(name, input)
	if arg == "" {
		return title
	}
	return title + "  " + format.Trunc(arg, maxArgChars)
}

// toolArg picks the argument worth showing for a tool call.
func ToolArg(name string, raw json.RawMessage) string {
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
	case toolname.AgentCancel, toolname.AgentStatus:
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
	case toolname.Message:
		to := str("to")
		if to == "" {
			to = str("id") // a log from before message replaced agent_message
		}
		return "→ " + to
	}
	return compactArgs(raw)
}

// ToolTitle is the display name of a tool on its chat line: titleCase of
// the name, with MCP tools read as "server · tool".
func ToolTitle(name string) string {
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
func SplitModel(id string) (short, provider string) {
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
	return format.Trunc(s, maxArgChars)
}

// outputLines renders tool output collapsed by default: the first
// MaxOutputCollapsed lines always, up to MaxOutputExpanded with /details,
// and a "… +N lines" trailer for whichever mode is hiding something.
func OutputLines(out string) []Line {
	if out == "" {
		return nil
	}
	parts := strings.Split(out, "\n")
	total := len(parts)
	var lines []Line
	for i, l := range parts {
		if i >= MaxOutputExpanded {
			break
		}
		vis := VisAlways
		if i >= MaxOutputCollapsed {
			vis = VisExpanded
		}
		lines = append(lines, Line{Kind: LineToolOut, Text: l, Vis: vis})
	}
	if total > MaxOutputCollapsed {
		lines = append(lines, Line{Kind: LineToolOut, Text: fmt.Sprintf("… +%d lines", total-MaxOutputCollapsed), Vis: VisCollapsed})
	}
	if total > MaxOutputExpanded {
		lines = append(lines, Line{Kind: LineToolOut, Text: fmt.Sprintf("… +%d lines", total-MaxOutputExpanded), Vis: VisExpanded})
	}
	return lines
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

func CleanLines(lines []Line) []Line {
	for i := range lines {
		lines[i].Text, lines[i].Suffix = textsafe.Clean(lines[i].Text), textsafe.Clean(lines[i].Suffix)
	}
	return lines
}

// Tool-call glyphs by group: the gear for files, shell and finish; the
// clock for monitors; the fork for agent tools.
const (
	GlyphToolFiles    = "◆" // file tools (read, apply_patch, skill)
	GlyphToolShell    = "$" // shell, shell_kill (and the old bash names): the shell prompt
	GlyphToolMonitors = "$" // async jobs are shell commands
	GlyphToolAgents   = "⑂"
	GlyphToolTodo     = "◇" // todo_add, todo_update
	GlyphToolMCP      = "≡" // mcp__<server>__<tool> and MCP server notices
	GlyphToolWeb      = "↗" // web_fetch, web_search
)

// toolGlyph returns the glyph for a tool name and the gap after it.
func ToolGlyph(tool string) (string, string) {
	tool = toolname.Canonical(tool)
	switch {
	case strings.HasPrefix(tool, "agent_") || tool == toolname.Message:
		return GlyphToolAgents, " "
	case tool == toolname.Shell || tool == toolname.ShellKill:
		return GlyphToolShell, " "
	case strings.HasPrefix(tool, "todo_"):
		return GlyphToolTodo, " "
	case strings.HasPrefix(tool, toolname.MCPPrefix):
		return GlyphToolMCP, " "
	case strings.HasPrefix(tool, "web_"):
		return GlyphToolWeb, " "
	}
	return GlyphToolFiles, " "
}

// Committed is the number of committed items; the live tail may add more.
func (t *Transcript) Committed() int { return len(t.items) }

// Item is the lines of committed item i. The slice must not be modified.
func (t *Transcript) Item(i int) []Line { return t.items[i] }

// Rev changes whenever committed item i does.
func (t *Transcript) Rev(i int) uint64 { return t.revs[i] }

// Build folds a full event sequence into lines.
func Build(evs []event.Event) []Line {
	t := NewTranscript()
	for _, ev := range evs {
		t.Apply(ev)
	}
	return t.All()
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
