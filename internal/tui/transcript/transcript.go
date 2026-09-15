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
	Kind      LineKind
	Text      string
	Block     BlockKind
	Vis       Visibility
	Running   bool   // tool call still in progress (spinner glyph)
	Err       bool   // tool call failed (red gear)
	Suffix    string // dim trailer, e.g. "(cancelled)"
	Item      int    // index of the item (event group) this line belongs to
	Lead      bool   // first text line of a user/steer block: carries the "›" glyph
	Glyph     string // leader glyph for this line (Render styles it by Tone)
	Tone      Tone   // in progress / error; zero means "as is"
	callID    string
	Tool      string   // raw tool name on a LineTool line
	Note      bool     // drawn grey: the agent\'s aside, or an agent\'s reply in the channel chat (only the human\'s posts keep the text colour)
	Agent     string   // in the channel chat: the agent this line links to
	Who       string   // the @name this line leads with, whose colour its glyph and name take: an agent\'s name, or "user"
	Names     []string // @names coloured wherever this line mentions them (a channel chat post\'s recipients)
	Diff      byte     // a patch diff line: '+' added, '-' removed, '@' a hunk's anchor, 'f' a file header, 0 otherwise
	Indent    int      // extra indent, two columns each (a chat reply's later lines, past its glyph)
	TurnStart bool     // first line of the first item after a turn starts or ends: an agent\'s chat spaces turns apart there
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
	GlyphChild      = "⑂" // a child agent reported back (same fork as spawn)
	GlyphReply      = "‹" // a response: an agent's reply in the channel chat, a response to or from another agent
	GlyphAsk        = "›" // a prompt to or from another agent (the human's own prompts draw › in blue)
	GlyphSpawn      = "⋙" // a child agent was spawned: agent_create's mark, since its task is the prompt that made it
	GlyphInfo       = "»" // an info message another agent sent: it needs no reply (the double of a prompt\'s ›)
	GlyphInfoSent   = "«" // an info message this agent sends (the double of a message\'s ‹)
	GlyphTask       = "▹" // the task handed to a child
	GlyphFinished   = "✓" // an agent finished
	GlyphError      = "!" // a turn error
	GlyphKilled     = "⊘" // an agent was killed
	GlyphTurn       = "◦" // a turn notice (cancelled, stopped, aborted)
	GlyphNudge      = "↻" // the harness nudged the agent to reply
	GlyphAside      = "§" // the agent\'s own text, which reaches no one: "§ **Aside** …"
	GlyphModel      = "⇄" // model changed
	GlyphNotice     = "»" // a local notice (/help, lists)
	GlyphPrompt     = "?" // a question for the user
	GlyphAnswer     = "?" // the user's answer to a question (same mark as the question)
	GlyphPermission = "!" // a permission or trust prompt, and its answer
	GlyphFailed     = "✗" // a denied call (in place of its tool's glyph), a failed finish
	GlyphCompacted  = "┄┄ compacted ┄┄"
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

// Transcript accumulates rendered lines for one agent. Lines are grouped
// into items, one per rendered event group (a user block, an assistant
// message, a tool call with its output, …); the chat cursor walks items.
type Transcript struct {
	items   [][]Line // committed lines by item; items[i][j].Item == i
	revs    []uint64 // revs[i] changes whenever items[i] does (render cache key)
	version uint64   // source of revisions
	flat    []Line   // items flattened; nil when stale
	start   []int    // start[i]: index of items[i]'s first line in flat

	calls       map[string]lineRef   // tool call id → its LineTool line
	prompts     map[string]int       // prompt id → item of the tool call it gates
	promptLine  map[string]lineRef   // prompt id → its "?" line (tone flips when settled)
	jobs        map[string]lineRef   // job id → its "started" line, or the shell call it grew from
	toolLines   map[string][]lineRef // tool name → its call lines in transcript order: what lastUntiedCall walks instead of every line
	children    map[string]lineRef   // child agent id → the agent_create line that spawned it
	asks        map[string][]lineRef // agent name → message lines still waiting for its answer
	promptKinds map[string]string    // prompt id → kind, so its answer draws the prompt's glyph

	streamTurn  int
	stream      []streamSeg
	turn        bool        // a turn is in progress (TurnStarted seen, not yet ended)
	turnStart   time.Time   // when the current turn began
	turnTokens  int         // input + output tokens used so far this turn
	turnVerb    string      // the indicator's verb for this turn ("Galloping")
	compactItem int         // item of the running compaction's rule (replaced by the result), -1 when none
	turnGap     bool        // a turn started (or a nudge came): the next item appended starts a new stretch (TurnStart)
	nudged      bool        // a nudge was just drawn: the turn it starts continues right under it
	spawnTask   string      // the input id of a spawned agent's task, drawn with the spawn when taken
	spawnHeld   event.Event // a child's spawn waiting to see whether a task follows
	spawnParent string      // …and its parent ("" when none is held)
	inputs      map[string]event.Input
	callInputs  map[string]json.RawMessage // a tool call's arguments, until its tool.started
	spawnAs     string                     // …and what it was spawned as: "scout (general) · model"

	self string // this agent's name, from its spawn: the other side of every message it sends or receives

	chat  bool              // the channel chat (chat.go), not one agent's transcript
	names map[string]string // in the chat: agent id → name
	open  map[string]bool   // in the chat: agents a post is still waiting on
}

// TurnVerbs are the horse-flavoured labels the turn indicator cycles
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
		jobs: map[string]lineRef{}, toolLines: map[string][]lineRef{}, children: map[string]lineRef{}, inputs: map[string]event.Input{}, callInputs: map[string]json.RawMessage{},
		asks: map[string][]lineRef{}, promptKinds: map[string]string{}, compactItem: -1,
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
	reminder := isReminder(ev)
	if reminder {
		t.turnGap = true // the nudge opens the turn it starts (see the end of Apply)
	}
	if t.applyCompaction(ev) || t.holdSpawn(ev) {
		return
	}
	switch ev.Type {
	case event.AssistantMessage:
		t.rememberCalls(ev)
	case event.AgentUpdated:
		var p event.AgentUpdatedPayload
		if ev.Decode(&p) == nil && p.Name != nil {
			t.self = *p.Name // renamed: what it sends and receives from here on names it so
		}
	case event.InputQueued:
		t.queueInput(ev)
	case event.InputTaken:
		t.applyTaken(ev)
		return
	case event.ToolStarted, event.ToolFinished:
		if t.applyToolCall(ev) {
			return
		}
	case event.AskRequested, event.AskResolved:
		if t.applyPrompt(ev) {
			return
		}
	case event.JobStarted, event.JobFinished, event.JobStopped:
		if t.applyJob(ev) {
			return
		}
	}
	lines := CleanLines(EventLines(ev))
	t.answerGlyph(ev, lines)
	t.appendItem(lines)
	t.afterAppend(ev)
	// A new stretch of the chat (one blank row above it) starts with a
	// turn: what happens between turns (a mode or model change, a job's
	// result) stays with the turn before, above the gap. A nudge is the
	// exception: it opens the turn it starts, so the gap goes above it.
	switch {
	case reminder:
		t.nudged = true
	case ev.Type == event.TurnStarted:
		t.turnGap = t.turnGap || !t.nudged
		t.nudged = false
	}
}

// isReminder reports whether ev queues the harness's reminder of replies owed.
func isReminder(ev event.Event) bool {
	var in event.Input
	return ev.Type == event.InputQueued && ev.Decode(&in) == nil && in.Kind == event.InputReminder
}

// queueInput remembers an input until the agent takes it: that is when it
// joins the conversation, and when the chat shows it.
func (t *Transcript) queueInput(ev event.Event) {
	var in event.Input
	if ev.Decode(&in) == nil && in.ID != "" && in.Kind != event.InputReminder {
		t.inputs[in.ID] = in
	}
}

// applyTaken draws each input a model call took as an item of its own; an
// answer settles the messages waiting on its sender.
func (t *Transcript) applyTaken(ev event.Event) {
	var p event.InputTakenPayload
	if ev.Decode(&p) != nil {
		return
	}
	for _, id := range p.IDs {
		in, ok := t.inputs[id]
		if !ok {
			continue
		}
		delete(t.inputs, id)
		lines := InputLines(in, t.self)
		if id == t.spawnTask {
			t.spawnTask, lines = "", t.spawnLines(in)
		}
		t.appendItem(CleanLines(lines))
		if in.Kind == event.InputResponse {
			t.answered(in.FromName)
		}
	}
}

// InputLines draws an input to agent to as the chat shows it, from its
// sender to it: the human's own words after "@user → @to", another agent's
// after "@scout → @to" (just the sender while to is unknown). A job's result
// nests under its call and a reminder showed when it was queued, so neither
// draws again.
func InputLines(in event.Input, to string) []Line {
	switch in.Kind {
	case event.InputPrompt, event.InputSteer:
		lines := block(BlockUser, "", fromTo(toolname.User, to)+" "+in.Text)
		for i := range lines {
			if lines[i].Lead {
				lines[i].Who, lines[i].Names = toolname.User, names(toolname.User, to)
				break
			}
		}
		return lines
	case event.InputRequest, event.InputResponse:
		return received(in.FromName, to, in.Text, GlyphAsk)
	case event.InputInfo:
		return received(in.FromName, to, in.Text, GlyphInfo) // needs no reply
	default:
	}
	return nil
}

// rememberCalls keeps each tool call's input from the assistant message
// until its tool.started draws the call.
func (t *Transcript) rememberCalls(ev event.Event) {
	var p event.AssistantMessagePayload
	if ev.Decode(&p) != nil {
		return
	}
	for _, b := range p.Blocks {
		if b.Type == model.BlockToolUse {
			t.callInputs[b.ID] = b.Input
		}
	}
}

// holdSpawn keeps a child's spawn off the chat until the next event shows
// whether it came with a task: a task from its parent is drawn with the
// spawn as one item when the child takes it; anything else draws the
// spawn on its own first.
func (t *Transcript) holdSpawn(ev event.Event) bool {
	if ev.Type == event.AgentSpawned {
		var p event.AgentSpawnedPayload
		if ev.Decode(&p) != nil {
			return false
		}
		t.self = p.Name // a transcript is only given its own agent's spawn
		if p.Parent == "" {
			return false
		}
		t.spawnHeld, t.spawnParent = ev, p.Parent
		t.spawnAs = fmt.Sprintf("%s (%s)", p.Name, p.Role)
		if p.Model != "" {
			t.spawnAs += " · " + p.Model
		}
		return true
	}
	if t.spawnParent == "" {
		return false
	}
	parent := t.spawnParent
	t.spawnParent = ""
	var in event.Input
	if ev.Type == event.InputQueued && ev.Decode(&in) == nil && in.Kind == event.InputRequest && in.From == parent {
		t.spawnTask, t.inputs[in.ID] = in.ID, in
		return true
	}
	t.appendItem(CleanLines(EventLines(t.spawnHeld)))
	return false
}

// spawnLines draws a child's first task with its spawn: "» @main as scout
// (general) · model" over the task.
func (t *Transcript) spawnLines(in event.Input) []Line {
	head := Line{Kind: LineText, Text: titled("@"+in.FromName, "as "+t.spawnAs), Block: BlockChild, Glyph: GlyphSpawn, Who: in.FromName}
	if t.self != "" { // "» @main → @scout (general) · model"
		head.Text = fromTo(in.FromName, t.self) + " " + strings.TrimPrefix(t.spawnAs, t.self+" ")
		head.Names = names(in.FromName, t.self)
	}
	lines := []Line{{Kind: LineBlank}, head}
	for _, l := range strings.Split(strings.TrimRight(in.Text, "\n"), "\n") {
		lines = append(lines, Line{Kind: LineText, Text: l, Block: BlockChild, Indent: 1})
	}
	return append(lines, Line{Kind: LineBlank})
}

// answerGlyph marks an answer, a default or a withdrawal with its prompt's
// glyph: ? for a question, ! for a permission or trust prompt (and for one
// whose kind was never seen).
func (t *Transcript) answerGlyph(ev event.Event, lines []Line) {
	var p event.AskResolvedPayload
	if ev.Type != event.AskResolved || ev.Decode(&p) != nil || t.promptKinds[p.ID] == string(protocol.PromptQuestion) {
		return
	}
	for i := range lines {
		if lines[i].Glyph == GlyphAnswer {
			lines[i].Glyph = GlyphPermission
		}
	}
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
	case event.CompactionDone, event.CompactionFailed:
		if t.compactItem >= 0 {
			t.replaceItem(t.compactItem, CleanLines(EventLines(ev)))
			t.compactItem = -1
			return true
		}
	case event.TurnEnded, event.TurnAborted:
		if t.compactItem >= 0 {
			t.replaceItem(t.compactItem, []Line{{Kind: LineBlank}, {Kind: LineDim, Text: titled("Compaction interrupted", "")}, {Kind: LineBlank}})
			t.compactItem = -1
		}
	}
	return false
}

// applyToolCall tracks a call's line and nests its output under it. It
// reports whether ev was fully handled.
func (t *Transcript) applyToolCall(ev event.Event) bool {
	if ev.Type == event.ToolStarted {
		var p event.ToolStartedPayload
		if ev.Decode(&p) != nil || p.CallID == "" {
			return false
		}
		input := t.callInputs[p.CallID]
		delete(t.callInputs, p.CallID)
		lines := toolStartedLines(p.Name, p.CallID, input)
		if lines[0].Who != "" && t.self != "" { // a message or a task: "‹ @main → @scout …"
			lines[0].Text, lines[0].Names = "@"+t.self+" → "+lines[0].Text, names(t.self, lines[0].Who)
		}
		if r, ok := t.find(t.appendItem(CleanLines(lines)), isToolLine); ok {
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
	if ev.Type == event.AskRequested {
		var p event.AskRequestedPayload
		if ev.Decode(&p) != nil {
			return false
		}
		t.promptKinds[p.ID] = p.Kind
		lines := CleanLines(EventLines(ev))
		var refs []lineRef
		if item, gated := t.callItem(p.CallID, p.Tool); p.Kind == string(protocol.PromptPermission) && gated {
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
	var p event.AskResolvedPayload
	if ev.Decode(&p) != nil {
		return false
	}
	t.settlePrompt(p.ID, p.Outcome != event.AskAnswered)
	item, ok := t.prompts[p.ID]
	if !ok {
		return false
	}
	delete(t.prompts, p.ID)
	lines := CleanLines(EventLines(ev))
	t.answerGlyph(ev, lines)
	t.insertIntoItem(item, nested(lines))
	return true
}

// callItem is the item of the call a prompt gates: by its id, else the most
// recently started open call of the tool.
func (t *Transcript) callItem(id, tool string) (int, bool) {
	if r, ok := t.calls[id]; ok && t.valid(r) {
		return r.item, true
	}
	return t.openCallItem(tool)
}

// applyJob ties a background job to the shell call it grew from, or to
// a notice of its own, and nests its outcome there. It reports whether ev
// was fully handled.
func (t *Transcript) applyJob(ev event.Event) bool {
	switch ev.Type {
	case event.JobStarted:
		var p event.JobStartedPayload
		if ev.Decode(&p) != nil || p.ID == "" {
			return false
		}
		// A job that grew out of a shell call is represented by that call's
		// own line: it stays yellow while the job runs and the outcome nests
		// under it. Only a job with no such call gets its own notice.
		if r, ok := t.lastUntiedCall(toolname.Shell, t.jobs); ok {
			t.jobs[p.ID] = r
			t.line(r).Tone = ToneWorking
			t.stream = nil
			return true
		}
		if r, ok := t.find(t.appendItem(CleanLines(EventLines(ev))), isNotBlank); ok {
			t.jobs[p.ID] = r
		}
		return true
	case event.JobFinished:
		var p event.JobFinishedPayload
		if ev.Decode(&p) != nil {
			return false
		}
		tone := ToneNone
		if p.IsError {
			tone = ToneError
		}
		t.settleJob(p.ID, tone, CleanLines(jobFinishedLines(p)))
		return true
	case event.JobStopped:
		var p event.JobStoppedPayload
		if ev.Decode(&p) != nil {
			return false
		}
		t.settleJob(p.ID, ToneError, CleanLines(jobStoppedLines("command", p.Reason)))
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
	case event.AssistantMessage:
		t.stream = nil
		var p event.AssistantMessagePayload
		if ev.Decode(&p) == nil {
			t.turnTokens += p.Usage.InputTokens + p.Usage.OutputTokens
		}
	case event.TurnEnded, event.TurnAborted, event.AgentKilled:
		t.stream = nil
		t.turn = false
		t.stopRunning()
	}
}

func isToolLine(l Line) bool   { return l.Kind == LineTool }
func isPromptLine(l Line) bool { return l.Glyph == GlyphPrompt || l.Glyph == GlyphPermission }
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
		if t.turnGap {
			run[0].TurnStart, t.turnGap = true, false
		}
		for j := range run {
			run[j].Item = i
			refs = append(refs, lineRef{i, j})
			t.indexTool(lineRef{i, j}, run[j])
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
		t.indexTool(refs[j], lines[j])
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

// settleJob gives a job's line its final tone and nests the outcome
// under it (indented when the line is the shell call the job grew from); a
// job the transcript never saw start gets the outcome as its own item.
func (t *Transcript) settleJob(id string, tone Tone, lines []Line) {
	r, ok := t.jobs[id]
	delete(t.jobs, id)
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
	refs := t.toolLines[tool]
	for i := len(refs) - 1; i >= 0; i-- {
		r := refs[i]
		if !t.valid(r) || taken[r] {
			continue
		}
		if l := t.items[r.item][r.off]; l.Kind == LineTool && l.Tool == tool {
			return r, true
		}
	}
	return lineRef{}, false
}

// indexTool records a committed call line under its tool, keeping each
// tool's refs in transcript order (a line added to an older item goes
// before the later items' lines).
func (t *Transcript) indexTool(r lineRef, l Line) {
	if l.Kind != LineTool || l.Tool == "" {
		return
	}
	refs := t.toolLines[l.Tool]
	i, _ := slices.BinarySearchFunc(refs, r, func(a, b lineRef) int {
		if a.item != b.item {
			return a.item - b.item
		}
		return a.off - b.off
	})
	t.toolLines[l.Tool] = slices.Insert(refs, i, r)
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
		// "✗ Shell (not now)  rm -rf build": the ✗ takes the tool's glyph so a
		// denial is easy to spot, and its reason sits next to the tool's name
		l.Glyph = GlyphFailed
		mark := denialMark(p.Output)
		switch title, arg, ok := strings.Cut(l.Text, "  "); {
		case mark == "":
		case IsPromptCall(*l):
			l.Suffix = mark
		case ok:
			l.Text = title + " " + mark + "  " + arg
		default:
			l.Text += " " + mark
		}
	}
	// A created agent's name may differ from the label asked for (a taken
	// name gets a suffix): the line names the agent it made.
	if p.Name == toolname.AgentCreate && !p.IsError {
		if rest, ok := strings.CutPrefix(p.Output, "created "); ok {
			if name, _, ok := strings.Cut(rest, " ("); ok && name != l.Who {
				at := -1 // the recipient's @name: after the arrow, or leading the line
				if i := strings.Index(l.Text, "→ @"+l.Who); i >= 0 {
					at = i + len("→ ")
				} else if strings.HasPrefix(l.Text, "@"+l.Who) {
					at = 0
				}
				if at >= 0 {
					l.Text = l.Text[:at] + "@" + name + l.Text[at+1+len(l.Who):]
					for k, n := range l.Names {
						if n == l.Who {
							l.Names[k] = name
						}
					}
					l.Who = name
				}
			}
		}
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
const messageDelivered = "request delivered to "

// messagedAgent is the name of the agent a finished message call is waiting
// on: set only for a new message delivered to an agent, not for an answer,
// a message to the user, or a call that failed.
func messagedAgent(p event.ToolFinishedPayload) (string, bool) {
	if p.Name != toolname.Message || p.IsError || p.Cancelled || p.Denied {
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
	n.Text, n.Thinking = textsafe.Clean(n.Text), textsafe.Clean(n.Thinking)
	if n.Turn != t.streamTurn || n.Reset {
		t.streamTurn = n.Turn
		t.stream = nil
	}
	if n.Reset {
		return // the call is being sent again: its output starts over
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
			// drawn like the aside it becomes: "§ Aside …", later lines past the glyph
			for i, l := range strings.Split(strings.TrimRight(s.text, "\n"), "\n") {
				ln := Line{Kind: LineStream, Text: AsideTitle + " " + l, Glyph: GlyphAside}
				if i > 0 {
					ln.Glyph, ln.Indent = "", 1
				}
				out = append(out, ln)
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
		return []Line{{Kind: LineDim, Glyph: GlyphSpawn, Text: titled("Spawned", fmt.Sprintf("%s (%s) · %s", p.Name, p.Role, p.Model))}}
	}),

	event.InputQueued: decoded(func(in event.Input) []Line {
		if in.Kind != event.InputReminder {
			return nil // an input draws when the agent takes it (Transcript.applyTaken)
		}
		return []Line{{Kind: LineDim, Glyph: GlyphNudge, Text: titled("Nudge", "owes a reply to "+partyList(in.Names))}}
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
				// through message): it reads as the agent's aside, dimmed,
				// "§ Aside …" with later lines aligned under the text. Text
				// that opens with a heading, a list or code puts the title on
				// a line of its own.
				body := markdownLines(text)
				titled := slices.ContainsFunc(lines, func(x Line) bool { return x.Glyph == GlyphAside })
				if !titled && body[0].Kind != LineText {
					body = append([]Line{{Kind: LineText, Text: AsideTitle}}, body...)
				} else if !titled {
					body[0].Text = AsideTitle + " " + body[0].Text
				}
				for _, l := range body {
					l.Note = true
					if titled {
						l.Indent = 1
					} else {
						l.Glyph, titled = GlyphAside, true
					}
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

	event.ToolStarted: decoded(func(p event.ToolStartedPayload) []Line {
		return toolStartedLines(p.Name, p.CallID, nil) // the transcript draws calls with their input from the assistant message
	}),

	event.ToolFinished: decoded(func(p event.ToolFinishedPayload) []Line {
		if p.Denied {
			return nil // the denial reads on the call's own line (see finishCall)
		}
		if name := p.Name; (name == toolname.Message || name == toolname.AgentCreate || name == toolname.ApplyPatch) && !p.IsError {
			return nil // the text or diff already sits under the call; "delivered", "created" or "updated" adds nothing
		}
		return OutputLines(strings.TrimRight(p.Output, "\n"))
	}),

	event.TurnEnded: decoded(func(p event.TurnEndedPayload) []Line {
		switch p.Reason {
		case event.ReasonCancelled:
			return []Line{{Kind: LineDim, Glyph: GlyphTurn, Tone: ToneError, Text: titled("Turn cancelled", "")}, {Kind: LineBlank}}
		case event.ReasonError:
			msg := p.Error
			if msg == "" {
				msg = "turn error"
			}
			return errorBlock(msg)
		case event.ReasonMaxTokens:
			return []Line{{Kind: LineDim, Glyph: GlyphTurn, Tone: ToneError, Text: titled("Turn stopped", "max_tokens")}, {Kind: LineBlank}}
		case event.ReasonEndTurn:
			// the reply speaks for itself
		}
		return nil
	}),

	event.TurnAborted: func(event.Event) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphTurn, Tone: ToneError, Text: titled("Turn aborted", "daemon restart")}, {Kind: LineBlank}}
	},

	event.AgentKilled: func(event.Event) []Line {
		return errorBlockWith("killed", GlyphKilled)
	},

	event.AgentUpdated: decoded(func(p event.AgentUpdatedPayload) []Line {
		var lines []Line
		if p.Role != nil {
			lines = append(lines, Line{Kind: LineDim, Glyph: GlyphModel, Text: titled("Role", "→ "+*p.Role)})
		}
		if p.Model != nil {
			lines = append(lines, Line{Kind: LineDim, Glyph: GlyphModel, Text: titled("Model", "→ "+*p.Model)})
		}
		if p.Variant != nil {
			v := *p.Variant
			if v == "" {
				v = "default"
			}
			lines = append(lines, Line{Kind: LineDim, Glyph: GlyphModel, Text: titled("Variant", "→ "+v)})
		}
		return lines
	}),

	event.ChannelUpdated: decoded(func(p event.ChannelUpdatedPayload) []Line {
		if p.Mode == nil {
			return nil
		}
		return []Line{{Kind: LineDim, Glyph: GlyphModel, Text: titled("Mode", "→ "+*p.Mode+" · "+protocol.ModeSummary(*p.Mode))}}
	}),

	event.JobStarted: decoded(func(p event.JobStartedPayload) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphJob, Tone: ToneWorking, Text: titled("Job", p.Command)}}
	}),

	event.MCPStarted: decoded(func(p event.MCPStartedPayload) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphToolMCP, Text: titled("MCP", fmt.Sprintf("%s connected · %d tools", p.Server, len(p.Tools)))}}
	}),

	event.MCPFailed: decoded(func(p event.MCPFailedPayload) []Line {
		return []Line{{Kind: LineError, Glyph: GlyphToolMCP, Tone: ToneError, Text: titled("MCP", fmt.Sprintf("%s failed: %s", p.Server, p.Error))}}
	}),

	event.ChannelDirAdded: decoded(dirAddedLines),

	event.MCPStopped: decoded(func(p event.MCPRefPayload) []Line {
		return []Line{{Kind: LineDim, Glyph: GlyphToolMCP, Text: titled("MCP", p.Server+" stopped")}}
	}),

	event.JobFinished: decoded(jobFinishedLines),

	// Without the transcript's id → kind memory the kind is unknown here;
	// Transcript.Apply looks it up.
	event.JobStopped: decoded(func(p event.JobStoppedPayload) []Line {
		return jobStoppedLines("command", p.Reason)
	}),

	event.CompactionStarted: func(event.Event) []Line {
		return []Line{{Kind: LineBlank}, {Kind: LineRule, Text: GlyphCompacting}, {Kind: LineBlank}}
	},

	event.CompactionFailed: decoded(func(p event.CompactionPayload) []Line {
		return []Line{{Kind: LineBlank}, {Kind: LineDim, Text: titled("Compaction failed", p.Error)}, {Kind: LineBlank}}
	}),

	event.CompactionDone: decoded(func(p event.CompactionPayload) []Line {
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

	event.AskRequested: decoded(func(p event.AskRequestedPayload) []Line {
		switch protocol.PromptKind(p.Kind) {
		case protocol.PromptQuestion:
			return []Line{{Kind: LineNotice, Glyph: GlyphPrompt, Tone: ToneWorking, Text: titled("Question", format.FirstLine(p.Question))}}
		case protocol.PromptTrust:
			return []Line{{Kind: LineNotice, Glyph: GlyphPermission, Tone: ToneWorking, Text: titled("Trust requested", "")}}
		case protocol.PromptPermission:
		}
		return []Line{{Kind: LineNotice, Glyph: GlyphPermission, Tone: ToneWorking, Text: titled("Permission", p.Tool)}}
	}),

	event.AskResolved: decoded(func(p event.AskResolvedPayload) []Line {
		switch p.Outcome {
		case event.AskDefaulted:
			return []Line{{Kind: LineNotice, Glyph: GlyphAnswer, Tone: ToneError, Text: titled("Defaulted", format.FirstLine(p.Answer))}}
		case event.AskWithdrawn:
			return []Line{{Kind: LineNotice, Glyph: GlyphAnswer, Tone: ToneError, Text: titled("Prompt withdrawn", "")}}
		case event.AskAnswered:
		}
		ln := Line{Kind: LineNotice, Glyph: GlyphAnswer, Text: titled("Answered", format.FirstLine(p.Answer))}
		if strings.HasPrefix(strings.ToLower(p.Answer), "deny") {
			ln.Tone = ToneError
		}
		return []Line{ln}
	}),
}

// --- helpers ---

// toolStartedLines draws a call: its line, and for a message or a task the
// text under it, for a patch its diff. input is the call's arguments from
// the assistant message (nil when unknown).
func toolStartedLines(name, callID string, input json.RawMessage) []Line {
	if name == toolname.Message || name == toolname.AgentCreate {
		// "‹ @scout first line" (a message) or "» @scout first line" (the
		// task that creates it), then the rest of the text under it
		who, text := promptOf(name, input)
		lines := []Line{{Kind: LineTool, Text: toolLine(name, input), Running: true, Tool: name, Who: who}}
		var in struct{ Kind string }
		if _ = json.Unmarshal(input, &in); name == toolname.Message && in.Kind == "info" {
			lines[0].Glyph = GlyphInfoSent // needs no reply: « (a denial still swaps in ✗)
		}
		if _, rest, ok := strings.Cut(text, "\n"); ok {
			lines = append(lines, OutputLines(rest)...)
		}
		return lines
	}
	call := Line{Kind: LineTool, Text: toolLine(name, input), Running: true, callID: callID, Tool: name}
	if name == toolname.ApplyPatch {
		// the change itself is what matters: the call, then its diff
		var in struct{ Patch string }
		_ = json.Unmarshal(input, &in)
		return append([]Line{call}, DiffLines(in.Patch)...)
	}
	return []Line{call}
}

// denialMark is why a call was denied, for next to its name: "(<the
// human's reason>)", "(by policy)", "(no answer)" when nobody could answer
// the prompt, or "" when the human gave no reason.
func denialMark(output string) string {
	out := strings.TrimSpace(output)
	switch {
	case strings.HasPrefix(out, "Denied by policy:"):
		return "(by policy)"
	case strings.HasPrefix(out, "Permission denied: nobody answered"):
		return "(no answer)"
	case strings.HasPrefix(out, "Denied in auto mode:"):
		return "(outside dirs, auto mode)"
	}
	if reason, ok := strings.CutPrefix(out, "Permission denied by the user:"); ok {
		if reason = strings.TrimSuffix(strings.TrimSpace(reason), "."); reason != "" {
			return "(" + reason + ")"
		}
	}
	return ""
}

// AsideTitle leads the agent's own text: "§ Aside …".
const AsideTitle = "**Aside**"

// titled is a status line's text: a bold, capitalised title, then the
// detail ("**Mode** → yolo · …"), the way tool lines lead with their name.
func titled(title, detail string) string {
	if detail == "" {
		return "**" + title + "**"
	}
	return "**" + title + "** " + detail
}

func decodeErr(ev event.Event, err error) []Line {
	return []Line{{Kind: LineError, Text: fmt.Sprintf("(bad %s payload: %v)", ev.Type, err)}}
}

// jobFinishedLines renders "<glyph> <summary>" (red on error) followed by
// the output collapsed like tool output.
func jobFinishedLines(p event.JobFinishedPayload) []Line {
	head := Line{Kind: LineDim, Glyph: GlyphJob}
	if p.IsError {
		head.Tone = ToneError
	}
	summary := strings.TrimSpace(p.Summary)
	summary = strings.TrimPrefix(summary, "Job ") // the daemon's own "Job "go test" (m1): …"
	if summary == "" {
		summary = "finished"
	}
	head.Text = titled("Job", summary)
	lines := []Line{head}
	return append(lines, OutputLines(strings.TrimRight(p.Output, "\n"))...)
}

// jobStoppedLines renders "<glyph> Job stopped (<reason>)".
func jobStoppedLines(kind, reason string) []Line {
	text := titled("Job stopped", "")
	if reason = strings.TrimSpace(reason); reason != "" {
		text = titled("Job stopped", "("+reason+")")
	}
	return []Line{{Kind: LineDim, Glyph: GlyphJob, Tone: ToneError, Text: text}}
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

// received is what another agent sent agent to, a prompt or a response:
// "› @scout → @main …" with each name in its colour and later lines aligned
// under the text. What this agent sends reads "‹ @main → @scout …" (its
// message calls). An info message reads » instead.
func received(from, to, text, glyph string) []Line {
	body := strings.Split(strings.TrimRight(text, "\n"), "\n")
	head := Line{Kind: LineText, Text: body[0], Block: BlockChild, Glyph: glyph, Who: from}
	if from != "" {
		head.Text, head.Names = strings.TrimSpace(fromTo(from, to)+" "+body[0]), names(from, to)
	}
	lines := []Line{{Kind: LineBlank}, head}
	for _, l := range body[1:] {
		lines = append(lines, Line{Kind: LineText, Text: l, Block: BlockChild, Indent: 1})
	}
	return append(lines, Line{Kind: LineBlank})
}

// fromTo is how a message names its two sides, "@main → @scout"; just
// "@main" while the other side is unknown.
func fromTo(from, to string) string {
	if to == "" {
		return "@" + from
	}
	return "@" + from + " → @" + to
}

// names are the sides fromTo names, each painted in its own colour.
func names(from, to string) []string {
	if to == "" {
		return []string{from}
	}
	return []string{from, to}
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
	if name == toolname.Message || name == toolname.AgentCreate {
		// what this agent sends: "@scout look at the parser" (its first
		// line), for a message and for the task that creates an agent
		to, text := promptOf(name, input)
		first, _, _ := strings.Cut(text, "\n")
		return strings.TrimSpace("@" + to + " " + first)
	}
	title := ToolTitle(name)
	arg := ToolArg(name, input)
	if arg == "" {
		return title
	}
	return title + "  " + format.Trunc(arg, maxArgChars)
}

// ToolArg picks the argument worth showing for a tool call.
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
	case toolname.Grep, toolname.Glob:
		if p := str("path"); p != "" {
			return str("pattern") + "  in " + p
		}
		return str("pattern")
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
	case toolname.Todo:
		return todoArg(raw)
	case toolname.Message:
		to := str("to")
		to = strings.TrimPrefix(to, "@")
		if l := strings.ToLower(to); l == "user" || l == "human" {
			to = "user"
		}
		return "@" + to
	}
	return compactArgs(raw)
}

// todoArg is a todo call's summary, "t2 → done · Run the tests": the
// updates by id, then the steps added, each with its status when it does
// not start pending.
func todoArg(raw json.RawMessage) string {
	var a struct {
		Add    []struct{ Text, Status string }
		Update []struct{ ID, Status, Text string }
	}
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	parts := make([]string, 0, len(a.Update)+len(a.Add))
	for _, u := range a.Update {
		part := u.ID
		if u.Status != "" {
			part += " → " + u.Status
		}
		if u.Text != "" {
			part += "  " + u.Text
		}
		parts = append(parts, part)
	}
	for _, it := range a.Add {
		part := it.Text
		if it.Status != "" && it.Status != "pending" {
			part += " → " + it.Status
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " · ")
}

// ToolTitle is the display name of a tool on its chat line: titleCase of
// the name, bar the few that read shorter without it, with MCP tools read
// as "server · tool".
func ToolTitle(name string) string {
	if strings.HasPrefix(name, toolname.MCPPrefix) {
		// mcp__server__tool reads "server · tool"
		if parts := strings.SplitN(strings.TrimPrefix(name, toolname.MCPPrefix), "__", 2); len(parts) == 2 {
			return parts[0] + " · " + parts[1]
		}
	}
	switch name {
	case toolname.WebFetch:
		return "Fetch"
	case toolname.WebSearch:
		return "Search"
	case toolname.ApplyPatch:
		return "Patch"
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

// SplitModel splits "provider/model-id" into the short id and the provider.
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

// OutputLines renders tool output collapsed by default: the first
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

// MaxDiffExpanded is how many diff lines an expanded patch shows.
const MaxDiffExpanded = 200

// DiffLines is an apply_patch diff for under its call: a header per file
// ("a.go", "b.md (new)", "c.txt (deleted)", "→ d.go" for a move), each
// hunk's "@@" anchor, and the changed and context lines as written. Like a
// command's output, the first MaxOutputCollapsed lines always show and the
// rest, up to MaxDiffExpanded, when the item is expanded.
func DiffLines(patch string) []Line {
	var out []Line
	add := func(text string, diff byte) {
		out = append(out, Line{Kind: LineToolOut, Text: text, Diff: diff})
	}
	for _, l := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		switch {
		case l == "*** Begin Patch" || l == "*** End Patch" || strings.TrimSpace(l) == "*** End of File":
		case strings.HasPrefix(l, "*** Update File: "):
			add(strings.TrimPrefix(l, "*** Update File: "), 'f')
		case strings.HasPrefix(l, "*** Add File: "):
			add(strings.TrimPrefix(l, "*** Add File: ")+" (new)", 'f')
		case strings.HasPrefix(l, "*** Delete File: "):
			add(strings.TrimPrefix(l, "*** Delete File: ")+" (deleted)", 'f')
		case strings.HasPrefix(l, "*** Move to: "):
			add("→ "+strings.TrimPrefix(l, "*** Move to: "), 'f')
		case strings.HasPrefix(l, "@@"):
			add(l, '@')
		case strings.HasPrefix(l, "+"):
			add(l, '+')
		case strings.HasPrefix(l, "-"):
			add(l, '-')
		default:
			add(l, 0)
		}
	}
	total := len(out)
	if total > MaxDiffExpanded {
		out = out[:MaxDiffExpanded]
	}
	for i := range out {
		if i >= MaxOutputCollapsed {
			out[i].Vis = VisExpanded
		}
	}
	if total > MaxOutputCollapsed {
		out = append(out, Line{Kind: LineToolOut, Text: fmt.Sprintf("… +%d lines", total-MaxOutputCollapsed), Vis: VisCollapsed})
	}
	if total > MaxDiffExpanded {
		out = append(out, Line{Kind: LineToolOut, Text: fmt.Sprintf("… +%d lines", total-MaxDiffExpanded), Vis: VisExpanded})
	}
	return out
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

// Tool-call glyphs, one per group of tools; a background job draws the
// shell's, since every job is a shell command.
const (
	GlyphToolFiles  = "◆" // file tools (skill)
	GlyphToolRead   = "☰" // read: the lines of a file
	GlyphToolSearch = "⌕" // web_search: a magnifying glass
	GlyphToolPatch  = "±" // apply_patch: a diff
	GlyphToolShell  = "$" // shell, shell_kill (and the old bash names): the shell prompt
	GlyphJob        = "$" // async jobs are shell commands
	GlyphToolAgents = "⑂"
	GlyphToolCreate = "⋙" // agent_create: the triple of a prompt\'s ›, since it makes the agent it prompts
	GlyphToolTodo   = "□" // todo
	GlyphToolMCP    = "≡" // mcp__<server>__<tool> and MCP server notices
	GlyphToolWeb    = "↓" // web_fetch: pulling a page in
)

// CallGlyph is a tool line's glyph and the gap after it: ToolGlyph of its
// tool, except that a message this agent sends reads ‹ (what it receives
// reads ›).
func CallGlyph(l Line) (string, string) {
	if l.Glyph != "" {
		return l.Glyph, " " // a denied call's ✗
	}
	if IsMessage(l) {
		return GlyphReply, " "
	}
	return ToolGlyph(l.Tool)
}

// IsMessage reports whether l is a message call's line.
func IsMessage(l Line) bool {
	return l.Kind == LineTool && l.Tool == toolname.Message
}

// IsPromptCall reports whether l is a call that prompts an agent by name,
// "@scout …": a message, or the agent_create that makes it.
func IsPromptCall(l Line) bool {
	return IsMessage(l) || l.Kind == LineTool && l.Tool == toolname.AgentCreate
}

// promptOf is who a prompting call names and what it says: a message's
// recipient ("user" for the human) and text, or a new agent's label and
// task.
func promptOf(name string, input json.RawMessage) (who, text string) {
	var in struct{ To, ID, Text, Label, Task string }
	_ = json.Unmarshal(input, &in)
	if name == toolname.AgentCreate {
		return in.Label, strings.TrimSpace(in.Task)
	}
	return strings.TrimPrefix(ToolArg(name, input), "@"), strings.TrimSpace(in.Text)
}

// ToolGlyph returns the glyph for a tool name and the gap after it.
func ToolGlyph(tool string) (string, string) {
	switch {
	case tool == toolname.Read:
		return GlyphToolRead, " "
	case tool == toolname.ApplyPatch:
		return GlyphToolPatch, " "
	case tool == toolname.AgentCreate:
		return GlyphToolCreate, " "
	case strings.HasPrefix(tool, "agent_") || tool == toolname.Message:
		return GlyphToolAgents, " "
	case tool == toolname.Shell || tool == toolname.ShellKill:
		return GlyphToolShell, " "
	case tool == toolname.Todo:
		return GlyphToolTodo, " "
	case strings.HasPrefix(tool, toolname.MCPPrefix):
		return GlyphToolMCP, " "
	case tool == toolname.WebSearch:
		return GlyphToolSearch, " "
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

// dirAddedLines notes a directory joining the channel's working set, in the
// chat of the agent whose boundary prompt added it.
func dirAddedLines(p event.DirPayload) []Line {
	return []Line{{Kind: LineDim, Glyph: GlyphToolFiles, Text: titled("Dirs", fmt.Sprintf("+ %s (%s)", format.ShortHome(p.Dir), p.Source))}}
}
