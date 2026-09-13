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

	styleDim      = lipgloss.NewStyle().Foreground(colMuted)
	styleKey      = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
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
	styleRunning  = lipgloss.NewStyle().Foreground(colSuccess)

	styleLogoMuted  = lipgloss.NewStyle().Foreground(colMuted)
	styleLogoBright = lipgloss.NewStyle().Bold(true)

	styleStatusOK      = lipgloss.NewStyle().Foreground(colSuccess)
	styleStatusErr     = lipgloss.NewStyle().Foreground(colError).Bold(true)
	styleSelected      = lipgloss.NewStyle().Bold(true)
	styleSep           = lipgloss.NewStyle().Foreground(colBorder)
	styleBox           = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colMuted).Padding(0, 1)
	styleBoxTitle      = lipgloss.NewStyle().Foreground(colMuted).Bold(true)
	styleBoxFocus      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(0, 1)
	styleBoxTitleFocus = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styleGutter        = lipgloss.NewStyle().Foreground(colAccent)

	styleBorderUser     = lipgloss.NewStyle().Foreground(colAccent)
	styleBorderMuted    = lipgloss.NewStyle().Foreground(colMuted)
	styleBorderSteer    = lipgloss.NewStyle().Foreground(colWarning)
	styleBorderChild    = lipgloss.NewStyle().Foreground(colBorder)
	styleBorderError    = lipgloss.NewStyle().Foreground(colError)
	styleBorderFinished = lipgloss.NewStyle().Foreground(colSuccess)
)

// agentOutcome collapses an agent's fields into the state the sidebar
// colours: "working", "error", "complete", or "idle".
func agentOutcome(a protocol.AgentInfo) string {
	switch {
	case a.State == "running" || a.State == "blocked":
		return "working"
	case a.LastError != "" || (a.State == "finished" && a.Status == "failure"):
		return "error"
	case a.State == "finished" || a.State == "killed":
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
	case "complete":
		return lipgloss.NewStyle().Foreground(colMuted).Render("●")
	}
	return lipgloss.NewStyle().Foreground(colMuted).Render("○")
}

func blockBorder(k BlockKind) string {
	switch k {
	case BlockUser:
		return styleBorderUser.Render("│")
	case BlockSteer:
		return styleBorderSteer.Render("│")
	case BlockChild:
		return styleBorderChild.Render("│")
	case BlockError:
		return styleBorderError.Render("│")
	case BlockFinished:
		return styleBorderFinished.Render("│")
	}
	return ""
}

// --- transcript rendering ---

// RenderOpts controls Render. Spinner is the glyph drawn in front of
// running tool calls (falls back to ↳ when empty). Expanded overrides the
// global Details toggle per item (the chat cursor's enter). With Focused
// set, the lines of item Cursor carry the accent gutter marker.
type RenderOpts struct {
	Width    int
	Details  bool
	Spinner  string
	Expanded map[int]bool
	Cursor   int
	Focused  bool
	NoFold   bool // render every item in full (exports, line-level tests)
}

// gutterMark is the chat cursor marker drawn in the one-column gutter.
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
		if folded && i != f.show {
			continue
		}
		cur := o.Focused && l.Item == o.Cursor
		if l.Kind == LineBlank {
			// Blank lines inside items are dropped; spacing is applied
			// per item below so it is uniform whether folded or not.
			continue
		}
		if folded && f.hidden > 0 {
			l.Suffix = strings.TrimSpace(l.Suffix + fmt.Sprintf(" +%d", f.hidden))
		}
		text := renderLine(l, o, cur)
		for _, part := range strings.Split(text, "\n") {
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
		if r.blank && o.Focused && r.item == o.Cursor {
			b.WriteString(styleGutter.Render(gutterMark))
		} else {
			b.WriteString(r.text)
		}
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
	return b.String(), rows
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

// fold describes a collapsed item: the one line index to show and how many
// other non-blank lines are hidden behind it.
type fold struct{ show, hidden int }

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
		if o.Focused && item == o.Cursor {
			continue
		}
		if v, ok := o.Expanded[item]; ok && v {
			continue
		}
		out[item] = fold{show: in.show, hidden: in.nonblank - 1}
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
	gutter := " "
	if cursor {
		gutter = styleGutter.Render(gutterMark)
	}
	leader := "  "
	if l.Block != BlockNone {
		leader = blockBorder(l.Block) + "  "
	}
	glyph := ""
	text := l.Text
	var style func(...string) string

	switch l.Kind {
	case LineText, LineStream:
		style = func(s ...string) string { return inlineMarkdown(strings.Join(s, ""), lipgloss.NewStyle()) }
	case LineHeading:
		style = func(s ...string) string { return inlineMarkdown(strings.Join(s, ""), styleBold) }
	case LineCode:
		leader = "    "
		style = styleDim.Render
	case LineDim, LineLabel, LineThink:
		style = styleDim.Render
	case LineModel:
		glyph = styleDim.Render("· ")
		style = styleDim.Render
	case LineNotice:
		style = styleNotice.Render
	case LineTool:
		switch {
		case l.Running && o.Spinner != "":
			glyph = o.Spinner + " "
		case l.Err:
			glyph = styleError.Render("✗") + " "
		default:
			glyph = styleTool.Render("↳") + " "
		}
		style = renderToolText
		if l.Suffix != "" {
			text += " " + l.Suffix
		}
	case LineToolOut:
		leader = "      "
		style = styleToolOut.Render
	case LineToolNote:
		leader = "      "
		style = styleDim.Render
	case LineFinished:
		style = styleFinished.Render
	case LineRule:
		return gutter + centerText(styleRule.Render(l.Text), o.Width-1)
	case LineError:
		style = styleError.Render
	default:
		style = func(s ...string) string { return strings.Join(s, "") }
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
	promptBoxMin  = 75
	sidebarWidth  = 32
	sidebarMinW   = 100
	tipsMaxWidth  = 75
	inputBoxLines = 2
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

// inputBox draws the left-bordered box around the input and its meta line;
// the border is accent while the input has focus, muted otherwise.
func inputBox(input, meta string, focused bool) string {
	border := styleBorderMuted.Render("│")
	if focused {
		border = styleBorderUser.Render("│")
	}
	return border + "  " + input + "\n" + border + "  " + meta
}

// metaLine is "Coder  ·  claude-opus-5 anthropic" (or the no-model nudge).
func metaLine(label, model string, queued int) string {
	s := titleCase(label) + "  ·  "
	if model == "" {
		return s + styleWarn.Render("no model — /models")
	}
	short, prov := splitModel(model)
	s += short
	if prov != "" {
		s += " " + styleDim.Render(prov)
	}
	if queued > 0 {
		s += styleDim.Render(fmt.Sprintf("  ·  %d queued", queued))
	}
	return s
}

var tipLines = []string{
	"/provider   sign in with ChatGPT or Grok",
	"/models     pick a model",
	"/spawn      delegate to a child agent",
	"/help       all commands",
}

// --- footer ---

type footerInfo struct {
	home      bool
	connected bool
	label     string // selected agent label
	model     string // selected agent model (provider/id)
	tokens    int
	cost      float64
	pending   int // pending prompts
}

// footerRight builds the right side of the footer.
func footerRight(f footerInfo) string {
	var parts []string
	if f.pending > 0 {
		parts = append(parts, styleWarn.Render(fmt.Sprintf("△ %d Permissions", f.pending)))
	}
	switch {
	case !f.connected:
		parts = append(parts, styleBold.Render("Get started")+" "+styleDim.Render("/provider"))
	case f.home:
		parts = append(parts, styleAccent.Render("●")+" "+f.label+" · "+f.model, styleDim.Render("/help"))
	default:
		parts = append(parts, fmt.Sprintf("%s tokens · $%s", fmtTokens(f.tokens), fmtCost(f.cost)), styleDim.Render("/help"))
	}
	return strings.Join(parts, "  ")
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
	mainH := m.height - 1 - kb
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
	}
	return main + "\n" + keybar + "\n" + m.footerView()
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
		w -= sidebarWidth + 1
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
	return m.contentWidth()
}

// inputBoxView is the input box for the selected agent.
func (m Model) inputBoxView() string {
	label, model, queued := "agent", m.session.Model, 0
	if a := m.selectedAgent(); a != nil {
		label, queued = a.Label, a.Queued
		if a.Model != "" {
			model = a.Model
		}
	}
	return inputBox(m.input.View(), metaLine(label, model, queued), m.focus == focusInput)
}

// homeView centers the logo, the prompt box and the tips vertically.
func (m Model) homeView(width, height int) string {
	boxW := promptBoxWidth(width)
	var lines []string
	add := func(block string, w int) {
		x := (width - w) / 2
		if x < 0 {
			x = 0
		}
		pad := strings.Repeat(" ", x)
		for _, l := range strings.Split(block, "\n") {
			lines = append(lines, pad+l)
		}
	}
	logo := logoLines(width)
	add(strings.Join(logo, "\n"), lipgloss.Width(logo[0]))
	lines = append(lines, "")
	if mv := m.monitorsView(boxW); mv != "" {
		add(mv, boxW)
		lines = append(lines, "")
	}
	if pb := m.promptView(boxW); pb != "" {
		add(pb, boxW)
	}
	add(m.inputBoxView(), boxW)

	if m.showTips {
		tipsW := tipsMaxWidth
		if tipsW > boxW {
			tipsW = boxW
		}
		tips := make([]string, len(tipLines))
		for i, t := range tipLines {
			tips[i] = styleDim.Render(truncRunes(t, tipsW))
		}
		if len(lines)+1+len(tips) <= height {
			lines = append(lines, "")
			add(strings.Join(tips, "\n"), tipsW)
		}
	}

	top := (height - len(lines)) / 2
	if top < 0 {
		top = 0
	}
	out := make([]string, 0, height)
	for i := 0; i < top; i++ {
		out = append(out, "")
	}
	out = append(out, lines...)
	for len(out) < height {
		out = append(out, "")
	}
	return strings.Join(out[:height], "\n")
}

// sessionView is the transcript over the input box, plus the sidebar.
func (m Model) sessionView(width, height int) string {
	cw := m.contentWidth()
	parts := []string{m.vp.View(), ""}
	if mv := m.monitorsView(cw); mv != "" {
		parts = append(parts, mv)
	}
	if pb := m.promptView(cw); pb != "" {
		parts = append(parts, pb)
	}
	parts = append(parts, m.inputBoxView())
	left := padLines(strings.Join(parts, "\n"), cw)
	if !m.sidebarVisible() {
		return left
	}
	sep := styleSep.Render(strings.TrimSuffix(strings.Repeat("│\n", height), "\n"))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, sep, m.sidebarView(height))
}

// sidebarView is the right panel: session summary, agent tree, prompts.
func (m Model) sidebarView(height int) string {
	id := m.sessionID
	if len(id) > 8 {
		id = id[:8]
	}
	model := m.session.Model
	if model == "" {
		model = "—"
	}
	inner := sidebarWidth - 2
	rows := []string{
		" " + styleDim.Render("session") + "  " + id,
		" " + styleDim.Render("model") + "    " + truncRunes(model, inner-9),
		" " + styleDim.Render("cost") + "     $" + fmtCost(m.totalCost()),
		"",
		" " + styleBold.Render("agents") + m.sidebarFocusHint(),
	}
	rows = append(rows, m.treeRows(inner)...)
	if n := len(m.prompts); n > 0 {
		rows = append(rows, "", " "+styleWarn.Render(fmt.Sprintf("△ %d pending", n)))
	}
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
		avail := width - len([]rune(indent)) - 7
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
		row := " " + indent + marker + dot + " " + text
		if a.State == "running" {
			row += " " + m.sp.View()
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		rows = append(rows, styleDim.Render("   (no agents)"))
	}
	return rows
}

// promptView renders the permission/question/trust box for the head of the
// queue at the given width, or "". The box is accent while the permission
// section has focus (its hotkeys apply only then), muted otherwise.
func (m Model) promptView(width int) string {
	p := m.currentPrompt()
	if p == nil {
		return ""
	}
	focused := m.focus == focusPermission
	inner := width - 4
	if inner < 20 {
		inner = 20
	}
	title := fmt.Sprintf("%s · %s", p.Kind, m.agentLabel(p.Agent))
	if p.Agent == "" {
		title = p.Kind
	}
	if n := len(m.prompts); n > 1 {
		title += fmt.Sprintf("  [1 of %d]", n)
	}
	box, titleStyle := styleBox, styleBoxTitle
	if focused {
		box, titleStyle = styleBoxFocus, styleBoxTitleFocus
	}
	lines := []string{titleStyle.Render(title)}
	var hint string

	switch p.Kind {
	case "question":
		lines = append(lines, strings.Split(strings.TrimRight(p.Question, "\n"), "\n")...)
		for i, o := range p.Options {
			lines = append(lines, fmt.Sprintf("  %d) %s", i+1, o))
		}
		lines = append(lines, m.promptInput.View())
		hint = "type an answer (or an option number) · enter answers"
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
		hint = "y trust · n do not trust"
	default:
		lines = append(lines, "tool: "+p.Tool)
		in := strings.Split(strings.TrimRight(prettyJSON(p.Input), "\n"), "\n")
		const maxIn = 8
		for i, l := range in {
			if i == maxIn {
				lines = append(lines, styleDim.Render(fmt.Sprintf("  (+%d lines)", len(in)-maxIn)))
				break
			}
			lines = append(lines, "  "+truncRunes(l, inner-4))
		}
		hint = "y allow · n deny · a allow always"
	}

	switch {
	case p.ClaimedBy != "" && !m.claimedByUs[p.ID]:
		lines = append(lines, styleStatusErr.Render("claimed by another client"))
	case m.promptBusy == p.ID:
		lines = append(lines, styleDim.Render("answering…"))
	case !focused:
		lines = append(lines, styleDim.Render("tab to focus, then "+hint))
	default:
		lines = append(lines, styleDim.Render(hint))
	}
	return box.Width(inner).Render(strings.Join(lines, "\n"))
}

// footerView is the bottom line: dim cwd on the left, summary or a
// transient status on the right.
func (m Model) footerView() string {
	left := styleDim.Render(shortHome(m.session.Dir))
	right := m.footerRightView()
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		avail := m.width - lipgloss.Width(right) - 1
		if avail < 0 {
			avail = 0
		}
		left = styleDim.Render(ansi.Truncate(shortHome(m.session.Dir), avail, "…"))
		gap = m.width - lipgloss.Width(left) - lipgloss.Width(right)
		if gap < 1 {
			gap = 1
		}
	}
	return ansi.Truncate(left+strings.Repeat(" ", gap)+right, m.width, "")
}

func (m Model) footerRightView() string {
	switch {
	case m.confirmKill:
		return styleStatusErr.Render(fmt.Sprintf("kill %s and its subtree? y/n", m.agentLabel(m.selectedID())))
	case m.status != "" && m.statusErr:
		return styleStatusErr.Render(m.status)
	case m.status != "":
		return styleStatusOK.Render(m.status)
	case m.loading:
		return styleDim.Render("replaying events…")
	}
	f := footerInfo{home: m.isHome(), connected: m.connected(), pending: len(m.prompts), model: m.session.Model}
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

// monitorsView lists the selected agent's live children. "wakes parent"
// marks the ones it armed with monitor; the rest report silently to its
// mailbox. A child that finishes leaves this block and its result shows up
// in the transcript when the parent next takes a turn.
func (m Model) monitorsView(width int) string {
	sel := m.selectedID()
	if sel == "" {
		return ""
	}
	rows := monitorRows(m.agents, sel, m.spawned, time.Now(), m.sp.View(), width)
	if len(rows) == 0 {
		return ""
	}
	head := styleDim.Render("monitors") + " " + styleDim.Render(fmt.Sprintf("(%d)", len(rows)))
	return strings.Join(append([]string{head}, rows...), "\n")
}

// monitorRows is the pure part of monitorsView.
func monitorRows(agents []protocol.AgentInfo, parent string, spawned map[string]time.Time, now time.Time, spinner string, width int) []string {
	var rows []string
	for _, a := range agents {
		if a.Parent != parent || a.State == "finished" || a.State == "killed" {
			continue
		}
		lead := agentDot(a)
		if a.State == "running" || a.State == "blocked" {
			lead = lipgloss.NewStyle().Foreground(colWarning).Render(spinner)
		}
		meta := []string{a.State}
		if agentOutcome(a) == "error" {
			meta = []string{styleStatusErr.Render("error")}
		}
		if a.Monitored {
			meta = append(meta, styleAccent.Render("wakes parent"))
		}
		if a.Turn > 0 {
			meta = append(meta, fmt.Sprintf("turn %d", a.Turn))
		}
		if a.CostUSD > 0 {
			meta = append(meta, "$"+fmtCost(a.CostUSD))
		}
		if t, ok := spawned[a.ID]; ok && !t.IsZero() {
			meta = append(meta, fmtElapsed(now.Sub(t)))
		}
		text := fmt.Sprintf("%s (%s)", a.Label, a.Archetype)
		row := "  " + lead + " " + styleBold.Render(text) + "  " + styleDim.Render(strings.Join(meta, " · "))
		rows = append(rows, ansi.Truncate(row, width, "…"))
	}
	return rows
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
