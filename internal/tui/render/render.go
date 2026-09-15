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
	// TurnGaps is an agent's chat: no spacing inside a turn, one blank row
	// before each item that starts a new turn (or follows one).
	TurnGaps bool
	// WhoStyle colours the glyph and leading @name of a line that names
	// someone (Line.Who); WhoKey changes whenever its colours do.
	WhoStyle func(name string) lipgloss.Style
	WhoKey   string
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
	turn   bool // it starts a new turn's stretch (Line.TurnStart)
}

// renderChatItem renders the lines of one item: the details toggle, folding
// and the cursor highlight apply; blank lines are dropped (assemble spaces
// items uniformly).
func renderChatItem(lines []transcript.Line, o Options) itemRows {
	r := itemRows{item: lines[0].Item, spaced: isSpaced(lines), turn: lines[0].TurnStart}
	f, folded := o.folds(lines)[r.item]
	cur := o.Focused && r.item == o.Cursor
	lit := cur || o.Expanded[r.item] // the item being read: its text reads lighter
	for i, l := range lines {
		if !o.showLine(l) || l.Kind == transcript.LineBlank || folded && !f.show[i] {
			continue
		}
		// the +N count shows only in the preview under the chat cursor (or the
		// pointer); a fully folded row stays clean
		if folded && cur && f.hidden > 0 && i == f.last {
			l.Suffix = strings.TrimSpace(l.Suffix + fmt.Sprintf(" +%d", f.hidden))
		}
		if folded && len(f.show) == 1 {
			l = oneRow(l, o)
		}
		for _, part := range strings.Split(renderLine(l, o, lit), "\n") {
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
		if o.TurnGaps && p.turn || !o.TurnGaps && p.spaced {
			emit(p.item, "", true)
		}
		for _, row := range p.rows {
			emit(p.item, row, false)
		}
		if !o.TurnGaps && p.spaced && i != lastPart {
			emit(p.item, "", true)
		}
	}
	// The ephemeral turn indicator: not an item (no cursor, no fold), gone
	// as soon as the turn ends.
	if o.Working {
		if n > 0 {
			b.WriteString("\n\n") // one blank row above the indicator, in every chat
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

// oneRow cuts a folded item's one shown line to a single row, so its +N
// marker ends that row instead of landing inside a wrapped second one.
func oneRow(l transcript.Line, o Options) transcript.Line {
	leader, glyph, _ := kindStyle(l)
	if l.Glyph != "" {
		glyph = l.Glyph + " "
	}
	avail := o.Width - 1 - ansi.StringWidth(leader) - ansi.StringWidth(glyph) - 2*l.Indent
	if l.Suffix != "" {
		avail -= ansi.StringWidth(l.Suffix) + 1
	}
	if avail < 10 || ansi.StringWidth(l.Text) <= avail {
		return l
	}
	l.Text = ansi.Truncate(l.Text, avail, "…")
	return l
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
	expanded                int8   // per-item override: 0 none, 1 collapsed, 2 expanded
	who                     string // Options.WhoKey
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
		key := renderKey{epoch: epoch, rev: t.Rev(i), width: o.Width, details: o.Details, noFold: o.NoFold, cursor: o.Focused && o.Cursor == i, who: o.WhoKey}
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

// folds decides which items collapse to a single line. The human's input
// always shows in full, and so does text still streaming and the channel
// chat's messages (which fold by their own lines); everything else (the
// agent's notes, tool calls with their output and permission notices,
// thinking, prompts and responses from other agents, spawns, errors,
// notices) folds unless the chat cursor is on it, it was expanded with
// space, or /details is on.
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
		case l.Kind == transcript.LineStream:
			in.full = true
		case l.Kind == transcript.LineText || l.Kind == transcript.LineHeading || l.Kind == transcript.LineCode:
			if l.Block == transcript.BlockNone && l.Agent != "" {
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
// optional glyph, and the wrapped, styled text; lit (the item under the
// cursor, or expanded) draws the text lighter (litStyle). The cursor
// highlight is applied by renderChatItem.
func renderLine(l transcript.Line, o Options, lit bool) string {
	if l.Kind == transcript.LineRule {
		if l.Text == transcript.GlyphCompacting {
			return centerText(theme.StyleRule.Render("┄┄ compacting ")+CompactSweep(o.CompactFrame)+theme.StyleRule.Render(" ┄┄"), o.Width)
		}
		return centerText(theme.StyleRule.Render(l.Text), o.Width)
	}
	leader, glyph, style := kindStyle(l)
	leader = strings.Repeat("  ", l.Indent) + leader
	text := l.Text
	if l.Suffix != "" {
		text += " " + l.Suffix
	}
	// A line's own glyph, in its kind's colour. Glyphs never change colour
	// with a line's state (running, waiting, failed): colour on a glyph says
	// who a line is about (see whoColours).
	if l.Glyph != "" && l.Kind != transcript.LineTool { // a call's glyph comes from CallGlyph
		glyph = glyphStyle(l).Render(l.Glyph) + " "
	}
	if l.Block != transcript.BlockNone && (l.Kind == transcript.LineText || l.Kind == transcript.LineLabel) {
		bs := blockStyle(l.Block)
		style = markdownStyle(bs)
		// User prompts and steers read like a shell: "› text" on the first
		// line, later lines indented to align under it. Only the glyph (and
		// the @user it leads with, see whoColours) takes the colour; the text
		// reads as plain text.
		if l.Kind == transcript.LineText && (l.Block == transcript.BlockUser || l.Block == transcript.BlockSteer) {
			style = markdownStyle(lipgloss.NewStyle())
			if l.Lead {
				glyph = bs.Render("›") + " "
			} else {
				leader += "  "
			}
		}
	}

	if lit {
		style = litStyle(l, style)
	}
	glyph, text, name, nameStyle := whoColours(l, o, glyph, text)

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
		switch {
		case len(l.Names) > 0 && o.WhoStyle != nil:
			b.WriteString(paintNames(p, l.Names, style, o.WhoStyle))
		case i == 0 && name != "" && strings.HasPrefix(p, name):
			b.WriteString(nameStyle.Render(name) + style(p[len(name):]))
		default:
			b.WriteString(style(p))
		}
	}
	return b.String()
}

// paintNames draws a post's leading @names, each bold in that agent's
// colour, and the rest of the text in style: the names at the front are the
// recipients, and an @ after them belongs to the message.
func paintNames(text string, names []string, style func(...string) string, who func(string) lipgloss.Style) string {
	var b strings.Builder
	i := 0
	for i < len(text) && text[i] == '@' {
		j := i + 1
		for j < len(text) && nameByte(text[j]) {
			j++
		}
		name := ""
		for _, n := range names {
			if strings.EqualFold(text[i+1:j], n) {
				name = n
				break
			}
		}
		if name == "" {
			break
		}
		k := j
		for k < len(text) && text[k] == ' ' {
			k++
		}
		b.WriteString(who(name).Bold(true).Render(text[i:j]) + text[j:k])
		i = k
	}
	if i < len(text) {
		b.WriteString(style(text[i:]))
	}
	return b.String()
}

// nameByte reports whether c can be part of an agent's name.
func nameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// whoColours gives a line that names someone (Line.Who) that one's colour
// on its glyph and its leading @name: the role's colour for an agent, blue
// for the human. It returns the glyph, the text with the name's bold
// markers dropped, the name to draw ("" when the text does not lead with
// it) and its style.
func whoColours(l transcript.Line, o Options, glyph, text string) (string, string, string, lipgloss.Style) {
	if l.Who == "" || o.WhoStyle == nil {
		return glyph, text, "", lipgloss.Style{}
	}
	ws := o.WhoStyle(l.Who)
	switch {
	case l.Kind == transcript.LineTool:
		g, gap := transcript.CallGlyph(l)
		glyph = ws.Render(g) + gap
	case l.Glyph != "":
		glyph = ws.Render(l.Glyph) + " "
	case l.Lead:
		glyph = ws.Render("›") + " "
	}
	name := "@" + l.Who
	text = strings.Replace(text, "**"+name+"**", name, 1)
	if !strings.HasPrefix(text, name) {
		name = ""
	}
	return glyph, text, name, ws.Bold(true)
}

// kindStyle is how a line of l's kind is drawn: its leader (an indent), a
// glyph of its kind, and the style of its text.
func kindStyle(l transcript.Line) (leader, glyph string, style func(...string) string) {
	switch l.Kind {
	case transcript.LineText, transcript.LineStream:
		if l.Note {
			return "", "", markdownStyle(theme.StyleDim)
		}
		return "", "", markdownStyle(lipgloss.NewStyle())
	case transcript.LineHeading:
		if l.Note {
			return "", "", markdownStyle(theme.StyleDim.Bold(true))
		}
		return "", "", markdownStyle(theme.StyleBold)
	case transcript.LineCode:
		return " ", "", theme.StyleDim.Render
	case transcript.LineDim:
		return "", "", markdownStyle(theme.StyleDim) // status lines lead with a **bold** title
	case transcript.LineLabel, transcript.LineThink:
		return "", "", theme.StyleDim.Render
	case transcript.LineModel:
		return "", theme.StyleDim.Render("· "), theme.StyleDim.Render
	case transcript.LineNotice:
		return "", "", markdownStyle(theme.StyleNotice)
	case transcript.LineTool:
		if transcript.IsPromptCall(l) {
			return "", toolLineGlyph(l), renderMessageText
		}
		return "", toolLineGlyph(l), renderToolText
	case transcript.LineToolOut:
		switch l.Diff { // a patch's diff: added green, removed red
		case '+':
			return "  ", "", theme.StyleStatusOK.Render
		case '-':
			return "  ", "", theme.StyleError.Render
		case '@':
			return "  ", "", theme.StyleDim.Render
		case 'f':
			return "  ", "", theme.StyleBold.Render
		}
		return "  ", "", theme.StyleToolOut.Render // under the tool name (after "◆ ")
	case transcript.LineToolNote:
		return "  ", "", markdownStyle(theme.StyleDim)
	case transcript.LineFinished:
		if l.Tone == transcript.ToneError {
			return "", "", markdownStyle(theme.StyleError)
		}
		return "", "", markdownStyle(theme.StyleFinished)
	case transcript.LineError:
		return "", "", markdownStyle(theme.StyleError)
	}
	return "", "", func(s ...string) string { return strings.Join(s, "") }
}

// litStyle is a line's text style in the item being read (under the cursor,
// or expanded): the text colour the human's posts have, so grey text (an
// agent's reply or aside, tool output) lightens and the part being read
// stands out. Lines whose colour is their
// meaning (an error, a finish, a diff's added and removed lines) keep it;
// glyphs and @names are coloured apart and keep theirs.
func litStyle(l transcript.Line, style func(...string) string) func(...string) string {
	switch l.Kind {
	case transcript.LineHeading:
		return markdownStyle(theme.StyleLit.Bold(true))
	case transcript.LineNotice:
		return markdownStyle(theme.StyleLit.Italic(true))
	case transcript.LineCode, transcript.LineLabel, transcript.LineThink, transcript.LineModel:
		return theme.StyleLit.Render
	case transcript.LineTool:
		if transcript.IsPromptCall(l) {
			return litMessageText
		}
		return litToolText
	case transcript.LineToolOut:
		switch l.Diff {
		case '+', '-':
			return style
		case 'f':
			return theme.StyleLit.Bold(true).Render
		}
		return theme.StyleLit.Render
	case transcript.LineFinished, transcript.LineError:
		return style
	default:
		return markdownStyle(theme.StyleLit)
	}
}

// litToolText is renderToolText in the item being read: the bold name, then
// the argument, all light.
func litToolText(strs ...string) string {
	s := strings.Join(strs, "")
	if name, rest, ok := strings.Cut(s, "  "); ok {
		return theme.StyleLit.Bold(true).Render(name) + theme.StyleLit.Render("  "+rest)
	}
	return theme.StyleLit.Bold(true).Render(s)
}

// litMessageText is renderMessageText in the item being read.
func litMessageText(strs ...string) string {
	s := strings.Join(strs, "")
	if name, rest, ok := strings.Cut(s, " "); ok && strings.HasPrefix(s, "@") {
		return theme.StyleLit.Bold(true).Render(name) + " " + theme.StyleLit.Render(rest)
	}
	return theme.StyleLit.Render(s)
}

// markdownStyle renders **bold** spans over base.
func markdownStyle(base lipgloss.Style) func(...string) string {
	return func(s ...string) string { return inlineMarkdown(strings.Join(s, ""), base) }
}

// toolLineGlyph is a tool call's glyph in the colour of its state, and the
// gap after it.
func toolLineGlyph(l transcript.Line) string {
	g, gap := transcript.CallGlyph(l)
	return theme.StyleTool.Render(g) + gap // one colour whatever the call's state
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
	if j := strings.Index(name, " ("); j >= 0 && strings.HasSuffix(name, ")") {
		out = theme.StyleToolName.Render(name[:j]) + theme.StyleDim.Render(name[j:]) // "Shell (denied)"
	}
	if rest == "" {
		return out
	}
	if i := strings.LastIndex(rest, " ("); i >= 0 && strings.HasSuffix(rest, ")") {
		return out + theme.StyleTool.Render(rest[:i]) + theme.StyleDim.Render(rest[i:])
	}
	return out + theme.StyleTool.Render(rest)
}

// renderMessageText styles a message call "@scout look at …": the bold
// recipient, then the text as written (a wrapped row carries no name).
func renderMessageText(strs ...string) string {
	s := strings.Join(strs, "")
	if !strings.HasPrefix(s, "@") {
		return theme.StyleTool.Render(s)
	}
	name, rest, _ := strings.Cut(s, " ")
	if rest == "" {
		return theme.StyleToolName.Render(name)
	}
	return theme.StyleToolName.Render(name) + " " + theme.StyleTool.Render(rest)
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
