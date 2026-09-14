package tui

import (
	"encoding/json"
	"fmt"
	"github.com/nicodes/stavlos/internal/protocol"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// --- palette (the single place colors live) ---

var (
	colAccent  = lipgloss.AdaptiveColor{Light: "#3B6FD9", Dark: "#5B8DEF"}
	colMuted   = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#8A8F98"}
	colSuccess = lipgloss.AdaptiveColor{Light: "#1F8F4E", Dark: "#3DD68C"}
	colWarning = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#F5A524"}
	colError   = lipgloss.AdaptiveColor{Light: "#C0392B", Dark: "#F26D6D"}
	colBlocked = lipgloss.AdaptiveColor{Light: "#9333EA", Dark: "#C084FC"}
	colBorder  = colMuted
	colSelBg   = lipgloss.AdaptiveColor{Light: "#E5E7EB", Dark: "#2A2F3A"} // chat cursor row background
	colInputBg = lipgloss.AdaptiveColor{Light: "#F3F4F6", Dark: "#1C2129"} // the message input's background

	styleDim      = lipgloss.NewStyle().Foreground(colMuted)
	styleKey      = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styleWorking  = lipgloss.NewStyle().Foreground(colWarning)
	styleBold     = lipgloss.NewStyle().Bold(true)
	styleAccent   = lipgloss.NewStyle().Foreground(colAccent)
	styleNotice   = lipgloss.NewStyle().Foreground(colMuted).Italic(true)
	styleTool     = lipgloss.NewStyle().Foreground(colMuted)
	styleToolName = lipgloss.NewStyle().Foreground(colMuted).Bold(true)
	styleToolOut  = lipgloss.NewStyle().Foreground(colMuted)
	styleFinished = lipgloss.NewStyle().Foreground(colSuccess).Bold(true)
	styleRule     = lipgloss.NewStyle().Foreground(colBorder)
	styleError    = lipgloss.NewStyle().Foreground(colError)
	styleWarn     = lipgloss.NewStyle().Foreground(colWarning)
	styleRunning  = lipgloss.NewStyle().Foreground(colWarning) // spinner colour outside the chat

	styleLogoMuted  = lipgloss.NewStyle().Foreground(colMuted)
	styleLogoBright = lipgloss.NewStyle().Bold(true)

	styleStatusOK      = lipgloss.NewStyle().Foreground(colSuccess)
	styleStatusErr     = lipgloss.NewStyle().Foreground(colError).Bold(true)
	styleSelected      = lipgloss.NewStyle().Bold(true)
	styleSep           = lipgloss.NewStyle().Foreground(colBorder)
	styleBoxTitleFocus = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styleCursorRow     = lipgloss.NewStyle().Background(colSelBg) // chat cursor: the item's rows get this background
	styleSelection     = lipgloss.NewStyle().Reverse(true)        // mouse text selection

	styleBorderMuted = lipgloss.NewStyle().Foreground(colMuted)
	styleBorderUser  = lipgloss.NewStyle().Foreground(colAccent) // the input prompt while it has focus
)

// agentOutcome collapses an agent's fields into the state the sidebar
// colours: "working", "error", "complete", or "idle".
func agentOutcome(a protocol.AgentInfo) string {
	switch {
	case a.State == "running" || a.State == "blocked":
		return "working"
	case a.LastError != "":
		return "error"
	case a.State == "waiting":
		return "waiting"
	case a.State == "killed":
		return "complete"
	}
	return "idle"
}

// agentDot is the coloured marker for an agent row.
func agentDot(a protocol.AgentInfo) string {
	switch agentOutcome(a) {
	case "working":
		return lipgloss.NewStyle().Foreground(colWarning).Render("●")
	case "error":
		return lipgloss.NewStyle().Foreground(colError).Render("●")
	case "waiting":
		return lipgloss.NewStyle().Foreground(colWarning).Render("◐")
	case "complete":
		return lipgloss.NewStyle().Foreground(colMuted).Render("●")
	}
	return lipgloss.NewStyle().Foreground(colMuted).Render("○")
}

// blockStyle colours a message block's text; blocks carry no border so the
// only vertical bar on screen is the chat cursor.
func blockStyle(k BlockKind) lipgloss.Style {
	switch k {
	case BlockUser:
		return lipgloss.NewStyle().Foreground(colAccent)
	case BlockSteer:
		return lipgloss.NewStyle().Foreground(colWarning)
	case BlockChild:
		return lipgloss.NewStyle().Foreground(colMuted)
	case BlockError:
		return lipgloss.NewStyle().Foreground(colError)
	case BlockFinished:
		return lipgloss.NewStyle().Foreground(colSuccess)
	}
	return lipgloss.NewStyle()
}

// --- transcript rendering ---

// RenderOpts controls Render. Spinner is the glyph drawn in front of
// running tool calls (falls back to ◆ when empty). Expanded overrides the
// global Details toggle per item (the chat cursor's enter). With Focused
// set, the lines of item Cursor carry the accent gutter marker.
type RenderOpts struct {
	Width    int
	Details  bool
	Spinner  string
	Working  bool   // a turn is in progress: append the ephemeral indicator line
	Waiting  bool   // …and it is blocked on a permission: "! permission requested" instead
	Verb     string // the indicator's label ("Galloping"); "working" when empty
	Stats    string // "(12s · 1.2k tokens)" shown after the indicator, or ""
	Expanded map[int]bool
	Cursor   int
	Focused  bool
	NoFold   bool // render every item in full (exports, line-level tests)
}

// gutterMark is the chat cursor marker drawn in the one-column gutter.
// gutterMark is the cursor marker tests swap in for highlightRow (the real
// cursor is a background colour, invisible without a colour profile).
const gutterMark = "▍"

// rowRange is the first and last rendered row of an item (inclusive).
type rowRange struct{ first, last int }

// Render styles, indents and wraps transcript lines into viewport content.
// Lines hidden by the details toggle are skipped.
func Render(lines []Line, o RenderOpts) string {
	s, _ := renderAll(lines, o)
	return s
}

// renderAll is Render plus, for every item, the rendered rows it occupies
// (so the model can scroll the cursor item into view).
func renderAll(lines []Line, o RenderOpts) (string, map[int]rowRange) {
	folds := o.folds(lines)
	spaced := spacedItems(lines)

	// Pass 1: render each visible line into rows, tagged with its item.
	type row struct {
		item  int
		text  string
		blank bool
	}
	var out []row
	for i, l := range lines {
		if !o.showLine(l) {
			continue
		}
		f, folded := folds[l.Item]
		if folded && !f.show[i] {
			continue
		}
		cur := o.Focused && l.Item == o.Cursor
		if l.Kind == LineBlank {
			// Blank lines inside items are dropped; spacing is applied
			// per item below so it is uniform whether folded or not.
			continue
		}
		if folded && f.hidden > 0 && i == f.last {
			l.Suffix = strings.TrimSpace(l.Suffix + fmt.Sprintf(" +%d", f.hidden))
		}
		text := renderLine(l, o, cur)
		for _, part := range strings.Split(text, "\n") {
			if cur {
				part = highlightRow(part, o.Width)
			}
			out = append(out, row{item: l.Item, text: part})
		}
	}

	// Pass 2: a blank row above and below spaced items (user inputs,
	// thinking, assistant responses), never doubled, none at the very top.
	var b strings.Builder
	rows := map[int]rowRange{}
	n := 0
	lastBlank := true // suppress a leading blank
	emit := func(r row) {
		if r.blank && lastBlank {
			return
		}
		if n > 0 {
			b.WriteByte('\n')
		}
		// The cursor's gutter mark spans the item's own rows only, never
		// the blank spacing rows above and below it.
		b.WriteString(r.text)
		if !r.blank {
			if rr, ok := rows[r.item]; ok {
				rr.last = n
				rows[r.item] = rr
			} else {
				rows[r.item] = rowRange{n, n}
			}
		}
		n++
		lastBlank = r.blank
	}
	for i, r := range out {
		startOfItem := i == 0 || out[i-1].item != r.item
		endOfItem := i == len(out)-1 || out[i+1].item != r.item
		if startOfItem && spaced[r.item] {
			emit(row{item: r.item, blank: true})
		}
		emit(r)
		if endOfItem && spaced[r.item] && i != len(out)-1 {
			emit(row{item: r.item, blank: true})
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
			b.WriteString(styleWarn.Render("!") + " " + styleWarn.Render("permission requested"))
		} else {
			verb := o.Verb
			if verb == "" {
				verb = "working"
			}
			b.WriteString(o.Spinner + " " + styleDim.Render(verb+"…"))
		}
		if o.Stats != "" {
			b.WriteString(" " + styleDim.Render(o.Stats))
		}
	}
	return b.String(), rows
}

// highlightRow paints one row of the item under the chat cursor: a
// background across the full width, keeping the row's own colours (the
// background is re-asserted after every reset inside the row). It is a
// variable so tests can swap in a visible marker.
var highlightRow = func(s string, width int) string {
	pad := width - ansi.StringWidth(s)
	if pad < 0 {
		pad = 0
	}
	bg := sgrPrefix(styleCursorRow)
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

// spacedItems marks the items that get breathing room: user inputs,
// thinking, and assistant responses.
func spacedItems(lines []Line) map[int]bool {
	out := map[int]bool{}
	for _, l := range lines {
		switch {
		case l.Block == BlockUser || l.Block == BlockSteer:
			out[l.Item] = true
		case l.Kind == LineThink:
			out[l.Item] = true
		case (l.Kind == LineText || l.Kind == LineHeading || l.Kind == LineCode || l.Kind == LineStream) && l.Block == BlockNone:
			out[l.Item] = true
		}
	}
	return out
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
const previewLines = 3

// folds decides which items collapse to a single line. User inputs and
// assistant responses always show in full; everything else (tool calls with
// their output and permission notices, thinking, child results, spawns,
// errors, finish blocks, notices) folds unless the chat cursor is on it, it
// was expanded with enter, or /details is on.
func (o RenderOpts) folds(lines []Line) map[int]fold {
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
		case l.Block == BlockUser || l.Block == BlockSteer:
			in.full = true
		case l.Kind == LineText || l.Kind == LineHeading || l.Kind == LineCode || l.Kind == LineStream || l.Kind == LineModel:
			if l.Block == BlockNone {
				in.full = true
			}
		}
		if l.Kind == LineBlank || !o.showLine(l) {
			continue
		}
		in.nonblank++
		in.visible = append(in.visible, i)
		// prefer the first content line over a block label ("child", "task")
		if in.show < 0 || (lines[in.show].Kind == LineLabel && l.Kind != LineLabel && !in.seen) {
			in.show = i
			in.seen = l.Kind != LineLabel
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
			for k := start; k < len(in.visible) && n < previewLines; k++ {
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
func (o RenderOpts) showLine(l Line) bool {
	details := o.Details
	if v, ok := o.Expanded[l.Item]; ok {
		details = v
	}
	return !((l.Vis == VisCollapsed && details) || (l.Vis == VisExpanded && !details))
}

// renderLine draws one logical line: the gutter (cursor marker or space),
// a leader (block border or indent), an optional glyph, and the wrapped,
// styled text.
func renderLine(l Line, o RenderOpts, cursor bool) string {
	// No gutter column and no margin: chat rows start at the same column as
	// the strip and the input below; the cursor is a background highlight
	// applied by renderAll.
	gutter := ""
	leader := ""
	glyph := ""
	text := l.Text
	var style func(...string) string

	switch l.Kind {
	case LineText, LineStream:
		style = func(s ...string) string { return inlineMarkdown(strings.Join(s, ""), lipgloss.NewStyle()) }
	case LineHeading:
		style = func(s ...string) string { return inlineMarkdown(strings.Join(s, ""), styleBold) }
	case LineCode:
		leader = " "
		style = styleDim.Render
	case LineDim, LineLabel, LineThink:
		style = styleDim.Render
	case LineModel:
		glyph = styleDim.Render("· ")
		style = styleDim.Render
	case LineNotice:
		style = styleNotice.Render
	case LineTool:
		g, gap := toolGlyph(l.tool)
		switch {
		case l.Running || l.Tone == ToneWorking:
			glyph = styleWorking.Render(g) + gap // in progress: the glyph, yellow
		case l.Err || l.Tone == ToneError:
			glyph = styleError.Render(g) + gap // same glyph, red, on failure
		default:
			glyph = styleTool.Render(g) + gap
		}
		style = renderToolText
	case LineToolOut:
		leader = "  " // under the tool name (after "◆ ")
		style = styleToolOut.Render
	case LineToolNote:
		leader = "  "
		style = styleDim.Render
	case LineFinished:
		style = styleFinished.Render
		if l.Tone == ToneError {
			style = styleError.Render
		}
	case LineRule:
		return gutter + centerText(styleRule.Render(l.Text), o.Width)
	case LineError:
		style = styleError.Render
	default:
		style = func(s ...string) string { return strings.Join(s, "") }
	}
	if l.Suffix != "" {
		text += " " + l.Suffix
	}
	// A line's own glyph, coloured by lifecycle: yellow in progress, red on
	// error or termination, otherwise the glyph's natural colour.
	if l.Glyph != "" {
		gs := glyphStyle(l)
		gap := " "
		if l.Running && l.Kind != LineTool {
			glyph = styleWorking.Render(l.Glyph) + gap
		} else {
			glyph = gs.Render(l.Glyph) + gap
		}
	}
	if l.Block != BlockNone && (l.Kind == LineText || l.Kind == LineLabel) {
		bs := blockStyle(l.Block)
		style = func(s ...string) string { return inlineMarkdown(strings.Join(s, ""), bs) }
		// User prompts and steers read like a shell: "› text" on the first
		// line, later lines indented to align under it.
		if l.Kind == LineText && (l.Block == BlockUser || l.Block == BlockSteer) {
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
	cont := gutter + leader + strings.Repeat(" ", glyphW)
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('\n')
			b.WriteString(cont)
		} else {
			b.WriteString(gutter)
			b.WriteString(leader)
			b.WriteString(glyph)
		}
		b.WriteString(style(p))
	}
	return b.String()
}

// renderToolText styles "Bash  git status (cancelled)": bold name, muted
// argument, dim parenthesised suffix.
func renderToolText(strs ...string) string {
	s := strings.Join(strs, "")
	name, rest := s, ""
	if i := strings.Index(s, "  "); i >= 0 {
		name, rest = s[:i], s[i:]
	}
	out := styleToolName.Render(name)
	if rest == "" {
		return out
	}
	if i := strings.LastIndex(rest, " ("); i >= 0 && strings.HasSuffix(rest, ")") {
		return out + styleTool.Render(rest[:i]) + styleDim.Render(rest[i:])
	}
	return out + styleTool.Render(rest)
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

// padLines pads (or truncates) every line of s to exactly width cells.
func padLines(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if w := ansi.StringWidth(l); w < width {
			lines[i] = l + strings.Repeat(" ", width-w)
		} else if w > width {
			lines[i] = ansi.Truncate(l, width, "")
		}
	}
	return strings.Join(lines, "\n")
}

// --- logo ---

// logoGlyphs are 4-row block letters, 4 cells wide, for the logo.
var logoGlyphs = map[rune][4]string{
	's': {"▄▀▀▀", "▀▀▀▄", "▄  █", "▀▀▀▀"},
	't': {"▀▀█▀", "  █ ", "  █ ", "  ▀ "},
	'a': {"▄▀▀▄", "█▄▄█", "█  █", "▀  ▀"},
	'v': {"█  █", "█  █", "▀▄▄▀", " ▀▀ "},
	'l': {"█   ", "█   ", "█   ", "▀▀▀▀"},
	'o': {"▄▀▀▄", "█  █", "█  █", "▀▀▀▀"},
}

const logoMinWidth = 50

// buildLogo lays out word as 4 rows of block glyphs separated by one space.
// Unknown letters render as blank cells so every row has the same width.
func buildLogo(word string) [4]string {
	var rows [4]string
	for i, r := range word {
		g, ok := logoGlyphs[r]
		if !ok {
			g = [4]string{"    ", "    ", "    ", "    "}
		}
		for k := range rows {
			if i > 0 {
				rows[k] += " "
			}
			rows[k] += g[k]
		}
	}
	return rows
}

// logoLines renders the two-tone "stav" (muted) + "los" (bright) logo, or a
// plain fallback when the terminal is narrower than logoMinWidth.
func logoLines(width int) []string {
	if width < logoMinWidth {
		return []string{styleLogoMuted.Render("stav") + styleLogoBright.Render("los")}
	}
	a, b := buildLogo("stav"), buildLogo("los")
	out := make([]string, 4)
	for i := range out {
		out[i] = styleLogoMuted.Render(a[i]) + "  " + styleLogoBright.Render(b[i])
	}
	return out
}

// --- placeholder cycling ---

var placeholders = []string{
	`Ask anything… "Fix a TODO in the codebase"`,
	`"What is the tech stack of this project?"`,
	`"Fix broken tests"`,
}

const placeholderPeriod = 8 * time.Second

// placeholderIndex picks the suggestion to show at t; it advances every
// placeholderPeriod so consecutive ticks show different text.
func placeholderIndex(t time.Time) int {
	return int((t.Unix() / int64(placeholderPeriod/time.Second)) % int64(len(placeholders)))
}

// --- prompt (input) box ---

const (
	promptBoxMin = 75
	sidebarWidth = 32
	sidebarMinW  = 100
)

// promptBoxWidth is the home-state box width: max(75, 70%) capped at width-4.
func promptBoxWidth(width int) int {
	w := width * 7 / 10
	if w < promptBoxMin {
		w = promptBoxMin
	}
	if w > width-4 {
		w = width - 4
	}
	if w < 20 {
		w = 20
	}
	return w
}

// inputBox is the input line over its meta line. Focus shows on the prompt
// chevron (set in layout), so there is no border.

// metaLine is "main (coder) · claude-opus-5 · high" (or the
// no-model nudge): the agent as "label (role)" like the tab rows, the
// model and its variant ("default" when none is set), led by a
// warning-coloured YOLO tag while the session auto-approves.
// sel is the part highlighted while the row has keyboard focus (metaNone
// otherwise).
func metaLine(label, role, model, variant string, queued int, yolo bool, sel metaPart) string {
	pick := func(part metaPart, text string, st lipgloss.Style) string {
		if part == sel {
			return styleBoxTitleFocus.Render(text)
		}
		return st.Render(text)
	}
	s := ""
	if yolo {
		s = pick(metaYolo, "YOLO", styleWarn) + " · "
	}
	name := label
	if role != "" {
		name = fmt.Sprintf("%s (%s)", label, role)
	}
	s += pick(metaRole, name, lipgloss.NewStyle()) + " · "
	if model == "" {
		return s + pick(metaModel, "no model — /models", styleWarn)
	}
	short, _ := splitModel(model) // just the model id; the provider is in /models
	s += pick(metaModel, short, lipgloss.NewStyle())
	if variant == "" {
		variant = "default"
	}
	s += " · " + pick(metaVariant, variant, lipgloss.NewStyle())
	if queued > 0 {
		s += styleDim.Render(fmt.Sprintf(" · %d queued", queued))
	}
	return s
}

// --- footer ---

type footerInfo struct {
	home      bool
	connected bool
	label     string // selected agent label
	model     string // selected agent model (provider/id)
	tokens    int
	cost      float64
}

// footerRight builds the right side of the meta row: a sign-in nudge,
// nothing on the home view (the left side already names the role and
// model), or the tokens and cost separated by a dot. The
// repo sits on the tab strip; waiting permissions and /help are not
// repeated here either (the strip shows the former, the "/" palette lists
// every command).
func footerRight(f footerInfo) string {
	switch {
	case !f.connected:
		return styleBold.Render("Get started") + " " + styleDim.Render("/providers")
	case f.home:
		return ""
	}
	return fmtTokens(f.tokens) + " tokens · $" + fmtCost(f.cost)
}

// turnStats formats the indicator's suffix: "(12s · 1.2k tokens)".
func turnStats(elapsed time.Duration, tokens int) string {
	return fmt.Sprintf("(%s · %s tokens)", fmtElapsed(elapsed), fmtTokens(tokens))
}

// fmtCost prints a dollar amount with 2–4 decimals.
func fmtCost(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	dot := strings.IndexByte(s, '.')
	for len(s)-dot-1 > 2 && strings.HasSuffix(s, "0") {
		s = s[:len(s)-1]
	}
	return s
}

// --- view ---

// View composes the main area (home or session) and the footer.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting…"
	}
	keybar, kb := m.keyBarView()
	mainH := m.height - kb
	if mainH < 1 {
		mainH = 1
	}
	var main string
	if m.isHome() {
		main = m.homeView(m.width, mainH)
	} else {
		main = m.sessionView(m.width, mainH)
	}
	if m.ov != nil {
		main = composite(main, m.width, mainH, m.ov.view(m.width, m.sp.View()))
	} else if isTab(m.focus) {
		main = composite(main, m.width, mainH, m.tabDialog(m.width))
	}
	frame := main
	if kb > 0 {
		frame = main + "\n" + keybar
	}
	return m.highlightSelection(frame)
}

// isHome reports whether the selected agent has nothing to show yet.
func (m Model) isHome() bool {
	t := m.transcripts[m.selectedID()]
	return t == nil || t.Empty()
}

// sidebarVisible is the tree toggle gated by the window width.
func (m Model) sidebarVisible() bool { return m.showTree && m.width >= sidebarMinW }

// contentWidth is the main column width (minus the sidebar when shown).
func (m Model) contentWidth() int {
	w := m.width
	if m.sidebarVisible() {
		w -= sidebarWidth + 2 // the separator and the space after it
	}
	if w < 10 {
		w = 10
	}
	return w
}

// boxWidth is the prompt/input box width for the current state.
func (m Model) boxWidth() int {
	if m.isHome() {
		return promptBoxWidth(m.width)
	}
	return m.width // the bottom block spans the window, sidebar or not
}

// inputBoxView is the input line over the meta row for the selected agent.
func (m Model) inputBoxView(width int) string {
	return m.inputView()
}

// metaRow is the line under the divider: role and model on the left, tokens
// and cost (or a transient status) on the right, dot separators within
// each side. The left side is truncated first when they collide.
func (m Model) metaRow(width int) string {
	label, role, model, variant, queued := "agent", "", m.session.Model, "", 0
	if a := m.selectedAgent(); a != nil {
		label, role, variant, queued = a.Label, a.Archetype, a.Variant, a.Queued
		if a.Model != "" {
			model = a.Model
		}
	}
	sel := metaNone
	if m.focus == focusMeta {
		sel = m.metaSel
	}
	left := metaLine(label, role, model, variant, queued, m.session.Yolo, sel)
	right := m.footerRightView()
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 4 {
		avail := width - lipgloss.Width(right) - 4
		if avail < 0 {
			avail = 0
		}
		left = ansi.Truncate(left, avail, "…")
		gap = width - lipgloss.Width(left) - lipgloss.Width(right)
		if gap < 1 {
			gap = 1
		}
	}
	return ansi.Truncate(left+strings.Repeat(" ", gap)+right, width, "")
}

// tagline sits under the logo on the home screen.
const tagline = "Saddle up."

// homeLayout is the logo screen's stack of lines and where things sit in
// it, shared by the renderer and the mouse.
type homeLayout struct {
	lines []string
	top   int // rows of padding above the stack (vertical centring)
}

// homeLines builds the logo screen: logo, tagline, the strip when it has
// something, the palette, and the input.
func (m Model) homeLines(width, height int) homeLayout {
	boxW := promptBoxWidth(width)
	lay := homeLayout{}
	add := func(block string, w int) {
		x := (width - w) / 2
		if x < 0 {
			x = 0
		}
		pad := strings.Repeat(" ", x)
		for _, l := range strings.Split(block, "\n") {
			lay.lines = append(lay.lines, pad+l)
		}
	}
	logo := logoLines(width)
	add(strings.Join(logo, "\n"), lipgloss.Width(logo[0]))
	lay.lines = append(lay.lines, "")
	add(styleDim.Render(tagline), lipgloss.Width(tagline))
	add(m.statusLine(boxW), boxW) // status messages sit above the meta row, as in a session
	add(m.metaRow(boxW), boxW)
	if m.stripShown() {
		if sv := m.sectionsView(boxW); sv != "" {
			add(sv, boxW)
		}
	}
	lay.lines = append(lay.lines, "")
	if pv := m.paletteViewFor(boxW); pv != "" {
		add(pv, boxW)
	}
	add(m.inputBoxView(boxW), boxW)
	lay.top = (height - len(lay.lines)) / 2
	if lay.top < 0 {
		lay.top = 0
	}
	return lay
}

// homeView centers the logo, the tagline and the prompt box vertically.
func (m Model) homeView(width, height int) string {
	lay := m.homeLines(width, height)
	out := make([]string, 0, height)
	for i := 0; i < lay.top; i++ {
		out = append(out, "")
	}
	out = append(out, lay.lines...)
	for len(out) < height {
		out = append(out, "")
	}
	return strings.Join(out[:height], "\n")
}

// sessionView is the transcript over the input box, plus the sidebar.
func (m Model) sessionView(width, height int) string {
	cw := m.contentWidth()
	// The chat (and the status line above the rule) share the top with the
	// sidebar; everything from the rule down spans the whole window, so the
	// footer cuts the sidebar off, not the other way round.
	top := padLines(m.vp.View()+"\n"+m.statusLine(cw), cw)
	if m.sidebarVisible() {
		h := m.vp.Height + 1
		sep := styleSep.Render(strings.TrimSuffix(strings.Repeat("│ \n", h), "\n")) // a space keeps the chat off the line
		top = lipgloss.JoinHorizontal(lipgloss.Top, m.sidebarView(h), sep, top)     // the sidebar sits on the left
	}
	// Under the rule: the meta row (YOLO, role, model, variant, usage), then
	// the tab strip, a blank line, the palette, and the input.
	parts := []string{top, styleRule.Render(strings.Repeat("─", width)), m.metaRow(width)}
	if sv := m.sectionsView(width); sv != "" {
		parts = append(parts, sv, "") // a blank line below the strip, before the input
	}
	if pv := m.paletteViewFor(width); pv != "" {
		parts = append(parts, pv)
	}
	parts = append(parts, m.inputBoxView(width))
	return padLines(strings.Join(parts, "\n"), width)
}

// sidebarView is the left panel: session summary, agent tree, prompts.
func (m Model) sidebarView(height int) string {
	id := m.sessionID
	if len(id) > 8 {
		id = id[:8]
	}
	model := m.session.Model
	if model == "" {
		model = "—"
	}
	inner := sidebarWidth - 1 // rows start at the left edge; one column of right padding
	rows := []string{
		styleDim.Render("session") + "  " + id,
		styleDim.Render("model") + "    " + truncRunes(model, inner-9),
		styleDim.Render("cost") + "     $" + fmtCost(m.totalCost()),
		"",
		styleBold.Render("agents") + m.sidebarFocusHint(),
	}
	rows = append(rows, m.treeRows(inner)...)
	if len(rows) > height {
		rows = rows[:height]
	}
	return lipgloss.NewStyle().Width(sidebarWidth).Height(height).MaxHeight(height).Render(strings.Join(rows, "\n"))
}

func (m Model) treeRows(width int) []string {
	rows := make([]string, 0, len(m.agents))
	focused := m.focus == focusSidebar && m.sidebarVisible()
	for i, a := range m.agents {
		indent := strings.Repeat("  ", a.Depth)
		marker := "  "
		switch {
		case focused && i == m.sbCursor:
			marker = styleAccent.Render("▶") + " "
		case i == m.selected:
			marker = styleAccent.Render("▸") + " "
		}
		dot := agentDot(a)
		// The prefix indent + marker + dot + " " is four columns plus the
		// indent, and truncRunes adds its ellipsis on top of the budget; the
		// text gets the rest so a truncated row reaches the panel's edge
		// (the caller leaves one column before the separator).
		avail := width - len([]rune(indent)) - 5
		if avail < 4 {
			avail = 4
		}
		label := a.State
		if agentOutcome(a) == "error" {
			label = "error"
		}
		text := truncRunes(fmt.Sprintf("%s (%s) · %s", a.Label, a.Archetype, label), avail)
		switch {
		case focused && i == m.sbCursor:
			text = styleBold.Render(text)
		case i == m.selected:
			text = styleSelected.Render(text)
		default:
			text = styleDim.Render(text)
		}
		row := indent + marker + dot + " " + text
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		rows = append(rows, styleDim.Render("  (no agents)"))
	}
	return rows
}

// sectionsView is the one-line strip between the chat and the input: the
// permission, agents and async tabs with their counts (always shown, "(0)"
// when empty) and the repo at the right edge. The body of whichever tab has
// focus is a dialog (tabDialog), not an inline block.
func (m Model) sectionsView(width int) string {
	return m.sectionTabs(m.liveChildren(), m.runningJobs(), m.currentPrompt(), width)
}

// tabDialogHeader is how many lines precede the rows in a tab dialog: the
// title, the hint line and the rule.
const tabDialogHeader = 3

// tabDialog is the dialog of the open tab, drawn over the chat like every
// other dialog: its title and count, a hint line, a rule, then its rows (the
// pending prompt, the live children, the running jobs) or a note that it is
// empty. Each tab has its own; nothing switches between them from inside.
func (m Model) tabDialog(bodyWidth int) string {
	w := dialogWidth(bodyWidth)
	inner := w - 4 // border + padding
	title, info := m.tabDialogHead()
	lines := []string{
		styleOvTitle.Render(truncRunes(title, inner)),
		styleDim.Render(ansi.Truncate(info, inner, "…")),
		styleRule.Render(strings.Repeat("─", inner)),
	}
	for _, l := range m.tabBodyLines(inner) {
		lines = append(lines, ansi.Truncate(l, inner, "…"))
	}
	return styleOvBox.Width(inner + 2).Render(strings.Join(lines, "\n"))
}

// tabDialogHead is the open tab's title (with its count) and key hints.
func (m Model) tabDialogHead() (title, info string) {
	switch m.focus {
	case focusAgents:
		return fmt.Sprintf("Agents (%d)", len(m.liveChildren())), "enter: select agent · esc: close"
	case focusAsync:
		return fmt.Sprintf("Async (%d)", len(m.runningJobs())), "esc: close"
	}
	n := len(m.prompts)
	p := m.currentPrompt()
	switch {
	case p == nil:
		return fmt.Sprintf("Permission (%d)", n), "esc: close"
	case p.Kind == "question":
		return fmt.Sprintf("Question (%d)", n), "enter: answer · 1-9: pick an option · esc: close"
	case p.Kind == "trust":
		return fmt.Sprintf("Trust (%d)", n), "y: trust project config · n: skip · esc: close"
	}
	return fmt.Sprintf("Permission (%d)", n), "y: allow once · a: allow for session · n: deny · esc: close"
}

// dialogWidth is the box width every dialog uses: overlayWidth, narrowed to
// fit the body with a margin, never under 24.
func dialogWidth(bodyWidth int) int {
	w := overlayWidth
	if w > bodyWidth-4 {
		w = bodyWidth - 4
	}
	if w < 24 {
		w = 24
	}
	return w
}

// tabBodyLines is the focused tab's rows, laid out for width columns.
func (m Model) tabBodyLines(width int) []string {
	switch m.focus {
	case focusAgents:
		kids := m.liveChildren()
		if len(kids) == 0 {
			return []string{styleDim.Render("  no subagents running")}
		}
		return m.cursorRows(agentRows(m.agents, m.selectedID(), m.spawned, m.lastLines(), time.Now(), width-2))
	case focusAsync:
		jobs := m.runningJobs()
		if len(jobs) == 0 {
			return []string{styleDim.Render("  no async jobs running")}
		}
		owner, role := "", ""
		if a := m.selectedAgent(); a != nil {
			owner, role = a.Label, a.Archetype
		}
		return m.cursorRows(monitorRows(jobs, owner, role, time.Now(), width-2))
	case focusPermission:
		p := m.currentPrompt()
		if p == nil {
			return []string{styleDim.Render("  no prompts waiting")}
		}
		return strings.Split(m.promptBox(p, width), "\n")
	}
	return nil
}

// sectionTabs is the one-line strip: the three tabs with their counts, the
// open one in accent, the rest dim (except a permission tab with prompts
// waiting, which is warning orange), and the repo at the right edge. Key
// hints live in the key bar.
func (m Model) sectionTabs(kids []protocol.AgentInfo, jobs []protocol.MonitorInfo, p *protocol.PromptInfo, width int) string {
	line := m.tabLabels(kids, jobs, p)
	// The repo (cwd) sits at the right edge of the strip.
	repo := styleDim.Render(shortHome(m.session.Dir))
	if gap := width - lipgloss.Width(line) - lipgloss.Width(repo); gap >= 4 {
		line += strings.Repeat(" ", gap) + repo
	}
	return ansi.Truncate(line, width, "…")
}

// tabLabels is "permission (n) · agents (n) · async (n)": the highlighted
// tab (while the strip has focus) or the open one (while its dialog is up)
// in accent, the rest dim.
func (m Model) tabLabels(kids []protocol.AgentInfo, jobs []protocol.MonitorInfo, p *protocol.PromptInfo) string {
	on := func(f focus) bool {
		if m.focus == focusTabs {
			return tabFocuses[m.tabSel] == f
		}
		return m.focus == f
	}
	tab := func(label string, on bool) string {
		if on {
			return styleBoxTitleFocus.Render(label)
		}
		return styleDim.Render(label)
	}
	label := fmt.Sprintf("permission (%d)", len(m.prompts))
	if p != nil && p.Kind != "permission" {
		label = fmt.Sprintf("%s (%d)", p.Kind, len(m.prompts))
	}
	// An unfocused permission tab with prompts waiting is warning-coloured
	// so it stands out until someone tabs to it.
	permTab := tab(label, on(focusPermission))
	if p != nil && !on(focusPermission) {
		permTab = styleWarn.Render(label)
	}
	tabs := []string{
		permTab,
		tab(fmt.Sprintf("agents (%d)", len(kids)), on(focusAgents)),
		tab(fmt.Sprintf("async (%d)", len(jobs)), on(focusAsync)),
	}
	return strings.Join(tabs, styleDim.Render(" · "))
}

// cursorRows puts the ▸ marker (as in every dialog) on the row under
// agCursor and indents the rest to match.
func (m Model) cursorRows(rows []string) []string {
	for i := range rows {
		marker := "  "
		if i == m.agCursor%len(rows) {
			marker = styleOvMarker.Render("▸") + " "
		}
		rows[i] = marker + strings.TrimPrefix(rows[i], "  ")
	}
	return rows
}

// promptBox renders the head of the prompt queue under the strip, without a
// border: a permission is a row naming the tool (like the chat's tool line)
// over its whole argument (the command, path or files); a question shows its
// text, options and answer field; trust shows the directory and files. Key
// hints live in the key bar, so only a status line (claimed, answering) is
// added.
func (m Model) promptBox(p *protocol.PromptInfo, width int) string {
	var lines []string
	agent := ""
	if p.Agent != "" {
		agent = styleDim.Render(" · " + m.agentLabel(p.Agent))
	}
	switch p.Kind {
	case "question":
		lines = append(lines, strings.Split(strings.TrimRight(p.Question, "\n"), "\n")...)
		for i, o := range p.Options {
			lines = append(lines, fmt.Sprintf("  %d) %s", i+1, o))
		}
		lines = append(lines, m.promptInput.View())
	case "trust":
		var t struct {
			Dir   string   `json:"dir"`
			Hash  string   `json:"hash"`
			Files []string `json:"files"`
		}
		_ = json.Unmarshal(p.Input, &t)
		lines = append(lines, "trust project configuration in "+t.Dir+"?")
		if len(t.Files) > 0 {
			lines = append(lines, "files:")
			for i, f := range t.Files {
				if i == 10 {
					lines = append(lines, fmt.Sprintf("  (+%d more)", len(t.Files)-10))
					break
				}
				lines = append(lines, "  "+f)
			}
		}
	default:
		g, gap := toolGlyph(p.Tool)
		lines = append(lines, styleWorking.Render(g)+gap+styleBold.Render(toolTitle(p.Tool))+agent)
		// The argument is shown whole: a shell command is what the user is
		// approving, so it is never cut. Long lines wrap under the tool name.
		if arg := fullToolArg(p.Tool, p.Input); arg != "" {
			const indent = "     "
			wrapW := width - 2 - len(indent)
			if wrapW < 20 {
				wrapW = 20
			}
			for _, l := range strings.Split(ansi.Hardwrap(arg, wrapW, true), "\n") {
				lines = append(lines, indent+l)
			}
		}
	}
	switch {
	case p.ClaimedBy != "" && !m.claimedByUs[p.ID]:
		lines = append(lines, styleStatusErr.Render("claimed by another client"))
	case m.promptBusy == p.ID:
		lines = append(lines, styleDim.Render("answering…"))
	}
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// fullToolArg is toolArg without the one-line flattening for the tools
// whose argument is text the user must read in full before approving.
func fullToolArg(tool string, raw json.RawMessage) string {
	switch tool {
	case "bash", "bash_async":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(raw, &in) == nil {
			return strings.TrimRight(in.Command, "\n")
		}
	}
	return toolArg(tool, raw)
}

// statusLine is the line between the chat and the divider: the transient
// message (copied, resumed, an error…) left-aligned, or blank.
func (m Model) statusLine(width int) string {
	var s string
	switch {
	case m.status != "" && m.statusErr:
		s = styleStatusErr.Render(m.status)
	case m.status != "":
		s = styleStatusOK.Render(m.status)
	case m.loading:
		s = styleDim.Render("replaying events…")
	default:
		return ""
	}
	return ansi.Truncate(s, width, "…")
}

func (m Model) footerRightView() string {
	f := footerInfo{home: m.isHome(), connected: m.connected(), model: m.session.Model}
	if a := m.selectedAgent(); a != nil {
		f.label, f.tokens, f.cost = a.Label, a.Tokens, a.CostUSD
		if a.Model != "" {
			f.model = a.Model
		}
	}
	return footerRight(f)
}

// connected reports whether a provider and a model are usable.
func (m Model) connected() bool {
	if !m.reconciled {
		return true // unknown yet; avoid flashing the nudge
	}
	if m.session.Model == "" || (len(m.agents) > 0 && m.agents[0].Model == "") {
		return false
	}
	if len(m.providers) > 0 {
		for _, p := range m.providers {
			if p.Connected {
				return true
			}
		}
		return false
	}
	return true
}

// agentGlyph is the fork, coloured by the agent's lifecycle like the chat:
// yellow working, red errored, grey complete, dim otherwise.
func agentGlyph(a protocol.AgentInfo) string {
	switch agentOutcome(a) {
	case "working":
		return styleWorking.Render(glyphToolAgents)
	case "error":
		return styleError.Render(glyphToolAgents)
	case "waiting":
		return styleWorking.Render(glyphToolAgents) // idle, but a question or job is outstanding
	case "complete":
		return styleDim.Render(glyphToolAgents)
	}
	return styleTool.Render(glyphToolAgents)
}

// jobGlyph is the clock, yellow while the job runs, red if it errored or
// was lost, dim otherwise.
func jobGlyph(j protocol.MonitorInfo) string {
	switch j.State {
	case "running", "":
		return styleWorking.Render(glyphToolMonitors)
	case "lost", "stopped":
		return styleError.Render(glyphToolMonitors)
	}
	return styleDim.Render(glyphToolMonitors)
}

// agentRows is the pure part of agentsView.
// last maps an agent id to a snippet of the latest line in its chat; it
// sits between the name and the meta, like the job on an async row.
func agentRows(agents []protocol.AgentInfo, parent string, spawned map[string]time.Time, last map[string]string, now time.Time, width int) []string {
	var rows []string
	for _, a := range agents {
		if a.Parent != parent || a.State == "killed" {
			continue
		}
		lead := agentGlyph(a)
		var meta []string // the state shows in the glyph's colour (orange working, red error, plain idle)
		if a.CostUSD > 0 {
			meta = append(meta, "$"+fmtCost(a.CostUSD))
		}
		if t, ok := spawned[a.ID]; ok && !t.IsZero() {
			meta = append(meta, fmtElapsed(now.Sub(t)))
		}
		text := fmt.Sprintf("%s (%s)", a.Label, a.Archetype)
		row := "  " + lead + " " + styleBold.Render(text)
		if s := last[a.ID]; s != "" {
			row += "  " + truncRunes(s, snippetChars)
		}
		if len(meta) > 0 {
			row += "  " + styleDim.Render(strings.Join(meta, " · "))
		}
		rows = append(rows, ansi.Truncate(row, width, "…"))
	}
	return rows
}

// snippetChars caps the chat snippet on an agent row.
const snippetChars = 40

// lastLines is a snippet of the latest chat line of every agent that has
// one: the last non-blank line, flattened, for the agents tab rows.
func (m Model) lastLines() map[string]string {
	out := map[string]string{}
	for id, t := range m.transcripts {
		if s := lastSnippet(t); s != "" {
			out[id] = s
		}
	}
	return out
}

// lastSnippet is the latest chat item (message, tool call, notice) read
// from its beginning: its lines flattened into one, for the caller to cut
// at the end.
func lastSnippet(t *Transcript) string {
	if t == nil {
		return ""
	}
	lines := t.All()
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i].Text) != "" && lines[i].Kind != LineLabel && lines[i].Kind != LineRule {
			last = i
			break
		}
	}
	if last < 0 {
		return ""
	}
	item := lines[last].Item
	var parts []string
	for _, l := range lines {
		if l.Item != item {
			continue
		}
		switch l.Kind {
		case LineBlank, LineLabel, LineRule:
			continue
		}
		s := strings.TrimSpace(strings.ReplaceAll(l.Text, "\n", " "))
		if s == "" {
			continue
		}
		if l.Kind == LineTool && l.Suffix != "" {
			s += " " + l.Suffix
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// monitorRows is the pure part of monitorsView: one row per running
// monitor with its kind glyph, bold label, a spinner for commands still
// running, and dim meta (progress, elapsed).
// Each row reads like an agent row, then the job: "$ coder (coder)  go
// test  42 lines · 1m15s" — the owning agent in bold with its role, the
// job's label, and dim progress/elapsed.
func monitorRows(monitors []protocol.MonitorInfo, owner, ownerRole string, now time.Time, width int) []string {
	var rows []string
	for _, mo := range monitors {
		switch mo.State {
		case "fired", "stopped", "lost":
			continue
		}
		lead := jobGlyph(mo) + monitorGlyphGap(mo.Kind)
		who := owner
		if ownerRole != "" {
			who = fmt.Sprintf("%s (%s)", owner, ownerRole)
		}
		label := mo.Label
		if who != "" {
			label = styleBold.Render(who) + "  " + mo.Label
		}
		var meta []string // the kind is always "command" now, so it is not shown
		if mo.Progress != "" {
			meta = append(meta, mo.Progress)
		}
		if t, err := time.Parse(time.RFC3339, mo.Started); err == nil && !t.IsZero() {
			meta = append(meta, fmtElapsed(now.Sub(t)))
		}
		row := "  " + lead + label
		if len(meta) > 0 {
			row += "  " + styleDim.Render(strings.Join(meta, " · "))
		}
		rows = append(rows, ansi.Truncate(row, width, "…"))
	}
	return rows
}

// monitorGlyph is the single-width marker for a monitor kind: $ command,
// (the gear is used for every kind; ⏱ draws two cells wide in many terminals).
// Tool-call glyphs by group: the gear for files, shell and finish; the
// clock for monitors; the fork for agent tools.
const (
	glyphToolFiles    = "◆" // file tools (read, apply_patch, skill)
	glyphToolShell    = "$" // bash, bash_async, bash_async_kill: the shell prompt
	glyphToolMonitors = "$" // async jobs are shell commands
	glyphToolAgents   = "⑂"
)

// toolGlyph returns the glyph for a tool name and the gap after it.
func toolGlyph(tool string) (string, string) {
	switch {
	case strings.HasPrefix(tool, "agent_"):
		return glyphToolAgents, " "
	case tool == "bash" || tool == "bash_async" || tool == "bash_async_kill":
		return glyphToolShell, " "
	}
	return glyphToolFiles, " "
}

// monitorGlyph is the shell prompt for every monitor kind.
func monitorGlyph(kind string) string { return glyphToolMonitors }

// monitorGlyphGap is the spacing after a kind glyph.
func monitorGlyphGap(kind string) string {
	return " "
}

// fmtElapsed renders a duration as 12s, 1m05s, 1h02m.
func fmtElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// sidebarFocusHint marks the agent list as focused.
func (m Model) sidebarFocusHint() string {
	if m.focus == focusSidebar && m.sidebarVisible() {
		return styleDim.Render("  ↑/↓ enter")
	}
	return ""
}

// glyphStyle picks a glyph's colour from its tone, then its kind.
func glyphStyle(l Line) lipgloss.Style {
	switch l.Tone {
	case ToneWorking:
		return styleWorking
	case ToneError:
		return styleError
	}
	switch {
	case l.Kind == LineFinished:
		return styleFinished
	case l.Kind == LineError:
		return styleError
	case l.Block != BlockNone:
		return blockStyle(l.Block)
	case l.Kind == LineNotice:
		return styleNotice
	}
	return styleDim
}

// paletteViewFor is the "/" command dropdown when the input is typing a
// command name and has focus.
func (m Model) paletteViewFor(width int) string {
	if m.focus != focusInput {
		return ""
	}
	pm := paletteMatches(m.input.Value())
	if len(pm) == 0 {
		return ""
	}
	return paletteView(pm, m.palIdx, width)
}
