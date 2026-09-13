package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

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
	LineDim                      // secondary information ("∴ thinking…", "· turn cancelled")
	LineTool                     // tool call: "Bash  git status" (glyph added by Render)
	LineToolOut                  // tool output (indented, dim)
	LineNotice                   // local notice such as /help output
	LineLabel                    // dim label inside a block ("steer", "child", "task")
	LineFinished                 // "finished · success" (green, bold)
	LineRule                     // centered rule ("── compacted ──")
	LineError                    // error text
	LineStream                   // in-progress streaming text (rendered like LineText)
	LineModel                    // "· <model>" trailer after the final assistant text
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
	Err     bool   // tool call failed (✗ glyph)
	Suffix  string // dim trailer, e.g. "(cancelled)"
	Item    int    // index of the item (event group) this line belongs to
	callID  string
}

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
}

// Transcript accumulates rendered lines for one agent. Lines are grouped
// into items, one per rendered event group (a user block, an assistant
// message, a tool call with its output, …); the chat cursor walks items.
type Transcript struct {
	Lines []Line

	calls      map[string]int // tool call id → index of its LineTool
	items      int            // committed items so far
	streamTurn int
	stream     []streamSeg
}

// NewTranscript returns an empty transcript.
func NewTranscript() *Transcript { return &Transcript{calls: map[string]int{}} }

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
	case event.AssistantMessage:
		t.stream = nil
	case event.TurnEnded, event.TurnAborted, event.AgentFinished, event.AgentKilled:
		t.stream = nil
		t.stopRunning()
	}
}

// appendItem commits lines under item; a fresh item index bumps the count.
func (t *Transcript) appendItem(item int, lines []Line) {
	if len(lines) == 0 {
		return
	}
	for i := range lines {
		lines[i].Item = item
	}
	t.Lines = append(t.Lines, lines...)
	if item == t.items {
		t.items++
	}
}

// insertIntoItem places lines immediately after the last line of item so
// the item stays contiguous. Tracked call indices past the splice shift.
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
	delete(t.calls, p.CallID)
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
			t.stream = append(t.stream, streamSeg{LineToolOut, n.Text})
		}
	case n.Text != "":
		if k > 0 && t.stream[k-1].kind == LineStream {
			t.stream[k-1].text += n.Text
		} else {
			t.stream = append(t.stream, streamSeg{LineStream, n.Text})
		}
	case n.Thinking != "":
		if k == 0 || t.stream[k-1].kind != LineDim {
			t.stream = append(t.stream, streamSeg{LineDim, "∴ thinking…"})
		}
	case n.ToolName != "":
		t.stream = append(t.stream, streamSeg{LineTool, titleCase(n.ToolName)})
	}
}

// Notice appends a local (non-event) notice, e.g. /help output, as one item.
func (t *Transcript) Notice(lines ...string) {
	ls := make([]Line, 0, len(lines))
	for _, l := range lines {
		ls = append(ls, Line{Kind: LineNotice, Text: l})
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
			out = append(out, Line{Kind: LineTool, Text: s.text, Running: true})
		default:
			out = append(out, Line{Kind: s.kind, Text: s.text})
		}
	}
	for i := start; i < len(out); i++ {
		out[i].Item = item
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
		lines := []Line{{Kind: LineDim, Text: fmt.Sprintf("spawned %s (%s) · %s", p.Label, p.Archetype, p.Model)}}
		if p.Task != "" {
			lines = append(lines, block(BlockChild, "task", p.Task)...)
		}
		return lines

	case event.UserMessage:
		var p event.UserMessagePayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		switch p.Kind {
		case "prompt", "":
			return block(BlockUser, "", p.Text)
		case "steer":
			return block(BlockSteer, "steer", p.Text)
		case "child_finished":
			return block(BlockChild, "child", p.Text)
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
				lines = append(lines, thinkingLine(b.Text))
			}
		}
		if hasText {
			if p.Model != "" && p.StopReason != string(model.StopToolUse) {
				short, _ := splitModel(p.Model)
				lines = append(lines, Line{Kind: LineModel, Text: short})
			}
			lines = append(lines, Line{Kind: LineBlank})
		}
		return lines

	case event.ToolCallStarted:
		var p event.ToolStartedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineTool, Text: toolLine(p.Name, p.Input), Running: true, callID: p.CallID}}

	case event.ToolCallFinished:
		var p event.ToolFinishedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return outputLines(strings.TrimRight(p.Output, "\n"))

	case event.TurnEnded:
		var p event.TurnEndedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		switch p.Reason {
		case "cancelled":
			return []Line{{Kind: LineDim, Text: "· turn cancelled"}, {Kind: LineBlank}}
		case "error":
			msg := p.Error
			if msg == "" {
				msg = "turn error"
			}
			return errorBlock(msg)
		case "max_tokens":
			return []Line{{Kind: LineDim, Text: "· turn stopped: max_tokens"}, {Kind: LineBlank}}
		}
		return nil

	case event.TurnAborted:
		return []Line{{Kind: LineDim, Text: "· turn aborted (daemon restart)"}, {Kind: LineBlank}}

	case event.AgentFinished:
		var p event.AgentFinishedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		lines := []Line{{Kind: LineBlank}, {Kind: LineFinished, Text: "finished · " + p.Status, Block: BlockFinished}}
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
		return errorBlock("killed")

	case event.AgentModelChanged:
		var p event.ModelChangedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineDim, Text: "· model → " + p.Model}}

	case event.Compacted:
		var p event.CompactedPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		lines := []Line{{Kind: LineBlank}, {Kind: LineRule, Text: "── compacted ──"}}
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
			return []Line{{Kind: LineNotice, Text: "? question: " + firstLine(p.Question)}}
		case "trust":
			return []Line{{Kind: LineNotice, Text: "? trust requested"}}
		default:
			return []Line{{Kind: LineNotice, Text: "? permission: " + p.Tool}}
		}

	case event.PromptAnswered:
		var p event.PromptAnsweredPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineNotice, Text: "→ answered: " + firstLine(p.Answer)}}

	case event.PromptDefaulted:
		var p event.PromptAnsweredPayload
		if err := ev.Decode(&p); err != nil {
			return decodeErr(ev, err)
		}
		return []Line{{Kind: LineNotice, Text: "→ defaulted: " + firstLine(p.Answer)}}

	case event.PromptWithdrawn:
		return []Line{{Kind: LineNotice, Text: "→ prompt withdrawn"}}
	}
	return nil
}

// --- helpers ---

func decodeErr(ev event.Event, err error) []Line {
	return []Line{{Kind: LineError, Text: fmt.Sprintf("(bad %s payload: %v)", ev.Type, err)}}
}

// block renders text as a left-bordered block (blank line before and after)
// with an optional dim label as its first line.
func block(kind BlockKind, label, text string) []Line {
	lines := []Line{{Kind: LineBlank}}
	if label != "" {
		lines = append(lines, Line{Kind: LineLabel, Text: label, Block: kind})
	}
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		lines = append(lines, Line{Kind: LineText, Text: l, Block: kind})
	}
	return append(lines, Line{Kind: LineBlank})
}

func errorBlock(text string) []Line {
	lines := []Line{{Kind: LineBlank}}
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		lines = append(lines, Line{Kind: LineError, Text: l, Block: BlockError})
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
		return Line{Kind: LineDim, Text: "∴ thinking…"}
	}
	first := summary
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	return Line{Kind: LineDim, Text: "∴ " + truncRunes(first, maxThinkChars)}
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
	title := titleCase(name)
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
	case "bash":
		return str("command")
	case "read", "write", "edit":
		return str("path")
	case "grep", "glob":
		return str("pattern")
	case "spawn":
		label, arch := str("label"), str("archetype")
		switch {
		case label != "" && arch != "":
			return label + " (" + arch + ")"
		case label != "":
			return label
		default:
			return arch
		}
	case "send", "steer", "cancel", "kill", "result", "status":
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
	case "finish":
		return str("status")
	}
	return compactArgs(raw)
}

// titleCase upper-cases the first letter: "bash" → "Bash".
func titleCase(s string) string {
	if s == "" {
		return s
	}
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
