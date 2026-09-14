// Package render draws transcript lines into the chat viewport's rows:
// styles, indentation, wrapping, folding under the details toggle and the
// cursor, spacing between items, and the turn indicator. A Cache keeps each
// committed item's rows while the item and the options that shape it are
// unchanged.
package render

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/internal/tui/transcript"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// CompactTick is how often a running compaction's bar advances.
const CompactTick = 120 * time.Millisecond

// RenderOpts controls Render. Spinner is the glyph drawn in front of
// running tool calls (falls back to ◆ when empty). Expanded overrides the
// global Details toggle per item (the chat cursor's enter). With Focused
// set, the lines of item Cursor carry the accent gutter marker.
type Options struct {
	Width    int
	Details  bool
	Spinner  string
	Working  bool   // a turn is in progress: append the ephemeral indicator line
	Waiting  bool   // …and it is blocked on a permission: "! permission requested" instead
	Verb     string // the indicator's label ("Galloping"); "working" when empty
	Active   string // the in-progress todo item, shown after the verb, or ""
	Stats    string // "(12s · 1.2k tokens)" shown after the indicator, or ""
	Expanded map[int]bool
	Cursor   int
	Focused  bool
	NoFold   bool // render every item in full (exports, line-level tests)
	// CompactFrame animates a running compaction's rule (the sweeping bar).
	CompactFrame int
}

// gutterMark is the chat cursor marker drawn in the one-column gutter.
// gutterMark is the cursor marker tests swap in for highlightRow (the real
// cursor is a background colour, invisible without a colour profile).
const GutterMark = "▍"

// rowRange is the first and last rendered row of an item (inclusive).
type RowRange struct{ First, Last int }

// renderAll is Render plus, for every item, the rendered rows it occupies
// (so the model can scroll the cursor item into view). Items are
// contiguous runs of lines.
func Lines(lines []transcript.Line, o Options) (string, map[int]RowRange) {
	var parts []itemRows
	for start := 0; start < len(lines); {
		end := start + 1
		for end < len(lines) && lines[end].Item == lines[start].Item {
			end++
		}
		parts = append(parts, renderChatItem(lines[start:end], o))
		start = end
	}
	return assemble(parts, o)
}

// itemRows is one item rendered: its rows, and whether it gets a blank row
// of spacing above and below.
type itemRows struct {
	item   int
	rows   []string
	spaced bool
}

// renderChatItem renders the lines of one item: the details toggle, folding
// and the cursor highlight apply; blank lines are dropped (assemble spaces
// items uniformly).
func renderChatItem(lines []transcript.Line, o Options) itemRows {
	r := itemRows{item: lines[0].Item, spaced: isSpaced(lines)}
	f, folded := o.folds(lines)[r.item]
	cur := o.Focused && r.item == o.Cursor
	for i, l := range lines {
		if !o.showLine(l) || l.Kind == transcript.LineBlank || folded && !f.show[i] {
			continue
		}
		if folded && f.hidden > 0 && i == f.last {
			l.Suffix = strings.TrimSpace(l.Suffix + fmt.Sprintf(" +%d", f.hidden))
		}
		for _, part := range strings.Split(renderLine(l, o, cur), "\n") {
			if cur {
				part = highlight(part, o.Width)
			}
			r.rows = append(r.rows, part)
		}
	}
	return r
}

// assemble joins rendered items: a blank row above and below spaced items
// (user inputs, thinking, assistant responses), never doubled, none at the
// very top; then the ephemeral turn indicator.
func assemble(parts []itemRows, o Options) (string, map[int]RowRange) {
	lastPart := -1
	for i, p := range parts {
		if len(p.rows) > 0 {
			lastPart = i
		}
	}
	var b strings.Builder
	rows := map[int]RowRange{}
	n := 0
	lastBlank := true // suppress a leading blank
	emit := func(item int, text string, blank bool) {
		if blank && lastBlank {
			return
		}
		if n > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
		// The cursor's highlight spans the item's own rows only, never the
		// blank spacing rows above and below it.
		if !blank {
			if rr, ok := rows[item]; ok {
				rr.Last = n
				rows[item] = rr
			} else {
				rows[item] = RowRange{n, n}
			}
		}
		n++
		lastBlank = blank
	}
	for i, p := range parts {
		if len(p.rows) == 0 {
			continue
		}
		if p.spaced {
			emit(p.item, "", true)
		}
		for _, row := range p.rows {
			emit(p.item, row, false)
		}
		if p.spaced && i != lastPart {
			emit(p.item, "", true)
		}
	}
	// The ephemeral turn indicator: not an item (no cursor, no fold), gone
	// as soon as the turn ends.
	if o.Working {
		if n > 0 {
			b.WriteString("\n\n")
		}
		// Gutter + leader, like every chat line.
		if o.Waiting {
			b.WriteString(theme.StyleWarn.Render("!") + " " + theme.StyleWarn.Render("permission requested"))
		} else {
			verb := o.Verb
			if verb == "" {
				verb = "working"
			}
			b.WriteString(o.Spinner + " " + theme.StyleDim.Render(verb+"…"))
			if o.Active != "" {
				b.WriteString(theme.StyleDim.Render(" · " + o.Active))
			}
		}
		if o.Stats != "" {
			b.WriteString(" " + theme.StyleDim.Render(o.Stats))
		}
	}
	return b.String(), rows
}

// renderCache keeps each committed item's rows between renders of one
// transcript. An entry is reused while the item's revision and the options
// that shape it are unchanged, so a streamed token or a spinner frame
// renders only the live tail, not the whole chat.
type Cache struct {
	entries []cachedItem
	misses  int // items rendered rather than reused (tests)
}

type cachedItem struct {
	ok       bool
	animated bool // holds the running compaction's rule: the frame matters
	key      renderKey
	rows     itemRows
}

// renderEpoch invalidates every render cache when a package-level
// rendering hook is swapped (tests replace highlightRow).
var epoch uint64

type renderKey struct {
	epoch                   uint64
	rev                     uint64
	width                   int
	details, noFold, cursor bool
	expanded                int8 // per-item override: 0 none, 1 collapsed, 2 expanded
	frame                   int
}

// renderTranscript renders t like renderAll(t.All(), o), reusing from c the
// rows of committed items that have not changed.
func Transcript(t *transcript.Transcript, c *Cache, o Options) (string, map[int]RowRange) {
	if len(c.entries) > t.Committed() {
		c.entries = c.entries[:t.Committed()]
	}
	for len(c.entries) < t.Committed() {
		c.entries = append(c.entries, cachedItem{})
	}
	tail := t.Tail()
	extended := -1 // the committed item the live buffer continues (a running call)
	if len(tail) > 0 && tail[0].Item < t.Committed() {
		extended = tail[0].Item
	}
	parts := make([]itemRows, 0, t.Committed()+2)
	for i := range t.Committed() {
		lines := t.Item(i)
		if len(lines) == 0 {
			continue
		}
		if i == extended {
			k := 0
			for k < len(tail) && tail[k].Item == i {
				k++
			}
			parts = append(parts, renderChatItem(append(slices.Clip(lines), tail[:k]...), o))
			tail = tail[k:]
			continue
		}
		key := renderKey{epoch: epoch, rev: t.Rev(i), width: o.Width, details: o.Details, noFold: o.NoFold, cursor: o.Focused && o.Cursor == i}
		if v, ok := o.Expanded[i]; ok {
			key.expanded = 1
			if v {
				key.expanded = 2
			}
		}
		e := &c.entries[i]
		if e.ok && e.animated {
			key.frame = o.CompactFrame
		}
		if !e.ok || e.key != key {
			e.animated = slices.ContainsFunc(lines, func(l transcript.Line) bool {
				return l.Kind == transcript.LineRule && l.Text == transcript.GlyphCompacting
			})
			key.frame = 0
			if e.animated {
				key.frame = o.CompactFrame
			}
			e.ok, e.key, e.rows = true, key, renderChatItem(lines, o)
			c.misses++
		}
		parts = append(parts, e.rows)
	}
	for start := 0; start < len(tail); {
		end := start + 1
		for end < len(tail) && tail[end].Item == tail[start].Item {
			end++
		}
		parts = append(parts, renderChatItem(tail[start:end], o))
		start = end
	}
	return assemble(parts, o)
}

// highlightRow paints one row of the item under the chat cursor: a
// background across the full width, keeping the row's own colours (the
// background is re-asserted after every reset inside the row). It is a
// variable so tests can swap in a visible marker.
var highlight = func(s string, width int) string {
	pad := width - ansi.StringWidth(s)
	if pad < 0 {
		pad = 0
	}
	bg := sgrPrefix(theme.StyleCursorRow)
	if bg == "" { // no colour profile
		return s + strings.Repeat(" ", pad)
	}
	return bg + strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+bg) + strings.Repeat(" ", pad) + "\x1b[0m"
}

// sgrPrefix extracts the escape sequence a style opens with ("" when the
// renderer has no colour profile).
func sgrPrefix(st lipgloss.Style) string {
	r := strings.TrimSuffix(st.Render(" "), "\x1b[0m")
	return strings.TrimSuffix(r, " ")
}

// isSpaced reports whether an item gets breathing room: user inputs,
// thinking, and assistant responses.
func isSpaced(lines []transcript.Line) bool {
	for _, l := range lines {
		switch {
		case l.Block == transcript.BlockUser || l.Block == transcript.BlockSteer:
			return true
		case l.Kind == transcript.LineThink:
			return true
		case (l.Kind == transcript.LineText || l.Kind == transcript.LineHeading || l.Kind == transcript.LineCode || l.Kind == transcript.LineStream) && l.Block == transcript.BlockNone:
			return true
		}
	}
	return false
}

// fold describes a collapsed item: which line indices to show (one when
// folded, up to previewLines under the chat cursor) and how many other
// non-blank lines are hidden; the "+N" marker goes on the last shown line.
type fold struct {
	show   map[int]bool
	last   int
	hidden int
}

// previewLines is how many lines an item shows while the chat cursor is on
// it; enter expands it fully.
const PreviewLines = 3

// folds decides which items collapse to a single line. User inputs and
// assistant responses always show in full; everything else (tool calls with
// their output and permission notices, thinking, child results, spawns,
// errors, finish blocks, notices) folds unless the chat cursor is on it, it
// was expanded with enter, or /details is on.
func (o Options) folds(lines []transcript.Line) map[int]fold {
	out := map[int]fold{}
	if o.Details || o.NoFold {
		return out
	}
	type info struct {
		full     bool
		show     int
		nonblank int
		seen     bool
		visible  []int // non-blank, shown line indices in order
	}
	byItem := map[int]*info{}
	order := []int{}
	for i, l := range lines {
		in := byItem[l.Item]
		if in == nil {
			in = &info{show: -1}
			byItem[l.Item] = in
			order = append(order, l.Item)
		}
		switch {
		case l.Block == transcript.BlockUser || l.Block == transcript.BlockSteer:
			in.full = true
		case l.Kind == transcript.LineText || l.Kind == transcript.LineHeading || l.Kind == transcript.LineCode || l.Kind == transcript.LineStream || l.Kind == transcript.LineModel:
			if l.Block == transcript.BlockNone {
				in.full = true
			}
		}
		if l.Kind == transcript.LineBlank || !o.showLine(l) {
			continue
		}
		in.nonblank++
		in.visible = append(in.visible, i)
		// prefer the first content line over a block label ("child", "task")
		if in.show < 0 || (lines[in.show].Kind == transcript.LineLabel && l.Kind != transcript.LineLabel && !in.seen) {
			in.show = i
			in.seen = l.Kind != transcript.LineLabel
		}
	}
	for _, item := range order {
		in := byItem[item]
		if in.full || in.show < 0 {
			continue
		}
		if v, ok := o.Expanded[item]; ok && v {
			continue // enter: fully expanded
		}
		f := fold{show: map[int]bool{}}
		if o.Focused && item == o.Cursor {
			// preview: the first few lines, starting from the chosen lead line
			start := 0
			for k, idx := range in.visible {
				if idx == in.show {
					start = k
				}
			}
			n := 0
			for k := start; k < len(in.visible) && n < PreviewLines; k++ {
				f.show[in.visible[k]] = true
				f.last = in.visible[k]
				n++
			}
			f.hidden = in.nonblank - n
		} else {
			f.show[in.show] = true
			f.last = in.show
			f.hidden = in.nonblank - 1
		}
		out[item] = f
	}
	return out
}

// showLine applies the details toggle, honouring a per-item override.
func (o Options) showLine(l transcript.Line) bool {
	details := o.Details
	if v, ok := o.Expanded[l.Item]; ok {
		details = v
	}
	return !((l.Vis == transcript.VisCollapsed && details) || (l.Vis == transcript.VisExpanded && !details))
}

// renderLine draws one logical line: a leader (block border or indent), an
// optional glyph, and the wrapped, styled text. The cursor highlight is
// applied by renderChatItem.
func renderLine(l transcript.Line, o Options, cursor bool) string {
	if l.Kind == transcript.LineRule {
		if l.Text == transcript.GlyphCompacting {
			return centerText(theme.StyleRule.Render("┄┄ compacting ")+CompactSweep(o.CompactFrame)+theme.StyleRule.Render(" ┄┄"), o.Width)
		}
		return centerText(theme.StyleRule.Render(l.Text), o.Width)
	}
	leader, glyph, style := kindStyle(l)
	text := l.Text
	if l.Suffix != "" {
		text += " " + l.Suffix
	}
	// A line's own glyph, coloured by lifecycle: yellow in progress, red on
	// error or termination, otherwise the glyph's natural colour.
	if l.Glyph != "" {
		if l.Running && l.Kind != transcript.LineTool {
			glyph = theme.StyleWorking.Render(l.Glyph) + " "
		} else {
			glyph = glyphStyle(l).Render(l.Glyph) + " "
		}
	}
	if l.Block != transcript.BlockNone && (l.Kind == transcript.LineText || l.Kind == transcript.LineLabel) {
		bs := blockStyle(l.Block)
		style = markdownStyle(bs)
		// User prompts and steers read like a shell: "› text" on the first
		// line, later lines indented to align under it.
		if l.Kind == transcript.LineText && (l.Block == transcript.BlockUser || l.Block == transcript.BlockSteer) {
			if l.Lead {
				glyph = bs.Render("›") + " "
			} else {
				leader += "  "
			}
		}
	}

	glyphW := ansi.StringWidth(glyph)
	avail := o.Width - 1 - ansi.StringWidth(leader) - glyphW
	parts := []string{text}
	if o.Width > 0 && avail >= 10 {
		parts = strings.Split(ansi.Wrap(text, avail, ""), "\n")
	}
	cont := leader + strings.Repeat(" ", glyphW)
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('\n')
			b.WriteString(cont)
		} else {
			b.WriteString(leader)
			b.WriteString(glyph)
		}
		b.WriteString(style(p))
	}
	return b.String()
}

// kindStyle is how a line of l's kind is drawn: its leader (an indent), a
// glyph of its kind, and the style of its text.
func kindStyle(l transcript.Line) (leader, glyph string, style func(...string) string) {
	switch l.Kind {
	case transcript.LineText, transcript.LineStream:
		return "", "", markdownStyle(lipgloss.NewStyle())
	case transcript.LineHeading:
		return "", "", markdownStyle(theme.StyleBold)
	case transcript.LineCode:
		return " ", "", theme.StyleDim.Render
	case transcript.LineDim, transcript.LineLabel, transcript.LineThink:
		return "", "", theme.StyleDim.Render
	case transcript.LineModel:
		return "", theme.StyleDim.Render("· "), theme.StyleDim.Render
	case transcript.LineNotice:
		return "", "", theme.StyleNotice.Render
	case transcript.LineTool:
		return "", toolLineGlyph(l), renderToolText
	case transcript.LineToolOut:
		return "  ", "", theme.StyleToolOut.Render // under the tool name (after "◆ ")
	case transcript.LineToolNote:
		return "  ", "", theme.StyleDim.Render
	case transcript.LineFinished:
		if l.Tone == transcript.ToneError {
			return "", "", theme.StyleError.Render
		}
		return "", "", theme.StyleFinished.Render
	case transcript.LineError:
		return "", "", theme.StyleError.Render
	}
	return "", "", func(s ...string) string { return strings.Join(s, "") }
}

// markdownStyle renders **bold** spans over base.
func markdownStyle(base lipgloss.Style) func(...string) string {
	return func(s ...string) string { return inlineMarkdown(strings.Join(s, ""), base) }
}

// toolLineGlyph is a tool call's glyph in the colour of its state, and the
// gap after it.
func toolLineGlyph(l transcript.Line) string {
	g, gap := transcript.ToolGlyph(l.Tool)
	switch {
	case l.Running || l.Tone == transcript.ToneWorking:
		return theme.StyleWorking.Render(g) + gap // in progress: the glyph, yellow
	case l.Err || l.Tone == transcript.ToneError:
		return theme.StyleError.Render(g) + gap // same glyph, red, on failure
	}
	return theme.StyleTool.Render(g) + gap
}

// renderToolText styles "Bash  git status (cancelled)": bold name, muted
// argument, dim parenthesised suffix.
func renderToolText(strs ...string) string {
	s := strings.Join(strs, "")
	name, rest := s, ""
	if i := strings.Index(s, "  "); i >= 0 {
		name, rest = s[:i], s[i:]
	}
	out := theme.StyleToolName.Render(name)
	if rest == "" {
		return out
	}
	if i := strings.LastIndex(rest, " ("); i >= 0 && strings.HasSuffix(rest, ")") {
		return out + theme.StyleTool.Render(rest[:i]) + theme.StyleDim.Render(rest[i:])
	}
	return out + theme.StyleTool.Render(rest)
}

// inlineMarkdown renders **bold** spans; unbalanced markers are left as-is.
func inlineMarkdown(s string, base lipgloss.Style) string {
	if !strings.Contains(s, "**") {
		return base.Render(s)
	}
	parts := strings.Split(s, "**")
	if len(parts)%2 == 0 {
		return base.Render(s)
	}
	var b strings.Builder
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i%2 == 1 {
			b.WriteString(base.Bold(true).Render(p))
		} else {
			b.WriteString(base.Render(p))
		}
	}
	return b.String()
}

func centerText(s string, width int) string {
	w := ansi.StringWidth(s)
	if width <= w {
		return s
	}
	return strings.Repeat(" ", (width-w)/2) + s
}

// compactSweep is the bar inside a running compaction's rule: a segment
// sweeping across a ten-cell track (the summariser gives no progress, so
// the bar shows activity, not completion). frame advances one cell a tick.
func CompactSweep(frame int) string {
	const cells, seg = 10, 3
	pos := frame % (cells + seg)
	var b strings.Builder
	for i := 0; i < cells; i++ {
		if i >= pos-seg && i < pos {
			b.WriteString(theme.StyleWarn.Render("▰"))
		} else {
			b.WriteString(theme.StyleDim.Render("▱"))
		}
	}
	return b.String()
}

// compactFrame is the sweep position for now: one cell per tick period.
func CompactFrame(now time.Time) int {
	return int(now.UnixNano() / int64(CompactTick))
}

// glyphStyle picks a glyph's colour from its tone, then its kind.
func glyphStyle(l transcript.Line) lipgloss.Style {
	switch l.Tone {
	case transcript.ToneWorking:
		return theme.StyleWorking
	case transcript.ToneError:
		return theme.StyleError
	}
	switch {
	case l.Kind == transcript.LineFinished:
		return theme.StyleFinished
	case l.Kind == transcript.LineError:
		return theme.StyleError
	case l.Block != transcript.BlockNone:
		return blockStyle(l.Block)
	case l.Kind == transcript.LineNotice:
		return theme.StyleNotice
	}
	return theme.StyleDim
}

// blockStyle colours a message block's text; blocks carry no border so the
// only vertical bar on screen is the chat cursor.
func blockStyle(k transcript.BlockKind) lipgloss.Style {
	switch k {
	case transcript.BlockUser:
		return lipgloss.NewStyle().Foreground(theme.ColAccent)
	case transcript.BlockSteer:
		return lipgloss.NewStyle().Foreground(theme.ColWarning)
	case transcript.BlockChild:
		return lipgloss.NewStyle().Foreground(theme.ColMuted)
	case transcript.BlockError:
		return lipgloss.NewStyle().Foreground(theme.ColError)
	case transcript.BlockFinished:
		return lipgloss.NewStyle().Foreground(theme.ColSuccess)
	}
	return lipgloss.NewStyle()
}

// Highlight paints one row of the item under the cursor.
func Highlight(s string, width int) string { return highlight(s, width) }

// SwapHighlight replaces the row highlight (tests draw a visible mark
// instead of a background) and returns the previous one; every cache
// renders afresh.
func SwapHighlight(f func(s string, width int) string) func(s string, width int) string {
	prev := highlight
	highlight = f
	epoch++
	return prev
}

// TurnStats formats the indicator's suffix: "(12s · 1.2k tokens)".
func TurnStats(elapsed time.Duration, tokens int) string {
	return fmt.Sprintf("(%s · %s tokens)", format.Elapsed(elapsed), format.Tokens(tokens))
}
