package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/toolname"
	"github.com/nicodes/stavlos/internal/tui/dialog"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/render"
	"github.com/nicodes/stavlos/internal/tui/theme"
	"github.com/nicodes/stavlos/internal/tui/transcript"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// agentOutcome collapses an agent's fields into the state the sidebar
// colours: "working", "error", "complete", or "idle".
func agentOutcome(a protocol.AgentInfo) string {
	switch {
	case a.State.Busy():
		return "working"
	case a.LastError != "":
		return "error"
	case a.State == protocol.AgentWaiting:
		return "waiting"
	case a.State == protocol.AgentKilled:
		return "complete"
	}
	return "idle"
}

// agentDot is the coloured marker for an agent row.
func agentDot(a protocol.AgentInfo) string {
	switch agentOutcome(a) {
	case "error":
		return lipgloss.NewStyle().Foreground(theme.ColError).Render("●")
	case "complete":
		return lipgloss.NewStyle().Foreground(theme.ColMuted).Render("●")
	}
	return stateDot(agentOutcome(a))
}

// promptMark is what takes a channel's state dot while the human is waited
// on: an orange "!" for a permission or trust prompt (first, since it holds
// up a tool call), an orange "?" for a question, "" for neither.
func promptMark(permissions, questions int) string {
	switch {
	case permissions > 0:
		return theme.StyleWarn.Render("!")
	case questions > 0:
		return theme.StyleWarn.Render("?")
	}
	return ""
}

// stateDot is the marker for a working / waiting / idle state, shared by
// agent rows and the channels section: a full orange circle while
// working, a half one while waiting, an empty dim one when idle.
func stateDot(state string) string {
	switch state {
	case "working":
		return lipgloss.NewStyle().Foreground(theme.ColWarning).Render("●")
	case "waiting":
		return lipgloss.NewStyle().Foreground(theme.ColWarning).Render("◐")
	}
	return lipgloss.NewStyle().Foreground(theme.ColMuted).Render("○")
}

// --- transcript rendering ---

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

// logoGlyphs are 5-row full-block letters for the logo; widths vary per
// letter (7 or 8 cells) and buildLogo separates them with one space.
var logoGlyphs = map[rune][5]string{
	's': {"███████", "██     ", "███████", "     ██", "███████"},
	't': {"████████", "   ██   ", "   ██   ", "   ██   ", "   ██   "},
	'a': {" █████ ", "██   ██", "███████", "██   ██", "██   ██"},
	'v': {"██    ██", "██    ██", "██    ██", " ██  ██ ", "  ████  "},
	'l': {"██     ", "██     ", "██     ", "██     ", "███████"},
	'o': {" ██████ ", "██    ██", "██    ██", "██    ██", " ██████ "},
}

const logoMinWidth = 62

// buildLogo lays out word as 5 rows of block glyphs separated by one space.
// Unknown letters render as blank cells so every row has the same width.
func buildLogo(word string) [5]string {
	var rows [5]string
	for i, r := range word {
		g, ok := logoGlyphs[r]
		if !ok {
			g = [5]string{"       ", "       ", "       ", "       ", "       "}
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
		return []string{theme.StyleLogoMuted.Render("stav") + theme.StyleLogoBright.Render("los")}
	}
	a, b := buildLogo("stav"), buildLogo("los")
	out := make([]string, 5)
	for i := range out {
		out[i] = theme.StyleLogoMuted.Render(a[i]) + " " + theme.StyleLogoBright.Render(b[i])
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

// modeTagStyle colours the mode tag: dim ASK (every permission asks), accent
// AUTO (allowed inside the directories, denied outside), warning YOLO
// (nothing asks).
func modeTagStyle(tag string) lipgloss.Style {
	switch tag {
	case "AUTO":
		return theme.StyleAccent
	case "YOLO":
		return theme.StyleWarn
	}
	return theme.StyleDim
}

// metaLineSpans is the meta row, "main (coder) · claude-opus-5 · high" (or
// the no-model nudge), with where each clickable part was drawn: the agent
// as "label (role)" like the tab rows, the model and its variant ("default"
// when none is set), led by the mode tag ("ASK", "AUTO" or "YOLO"; "" for
// none). Every part is grey but sel, in accent: the part whose dialog is
// open, or the one the row's keyboard focus is on (metaNone for neither);
// hover, the part under the pointer, is in the lighter text colour.
func metaLineSpans(label, role, model, variant string, queued int, modeTag string, sel, hover metaPart) (string, []span[metaPart]) {
	var b strings.Builder
	var spans []span[metaPart]
	x := 0
	part := func(p metaPart, text string, st lipgloss.Style) {
		switch p {
		case sel:
			st = theme.StyleBoxTitleFocus
		case hover:
			st = theme.StyleLit
		}
		w := ansi.StringWidth(text)
		spans = append(spans, span[metaPart]{x, x + w, p})
		b.WriteString(st.Render(text))
		x += w
	}
	sep := func() { // the divider's own rule separates its parts, as it does the tabs
		b.WriteString(theme.StyleRule.Render(" ─ "))
		x += 3
	}
	if modeTag != "" {
		part(metaMode, modeTag, modeTagStyle(modeTag))
		sep()
	}
	name := label
	if role != "" {
		name = fmt.Sprintf("%s (%s)", label, role)
	}
	part(metaRole, name, theme.StyleDim)
	sep()
	if model == "" {
		part(metaModel, "no model — /models", theme.StyleWarn)
		return b.String(), spans
	}
	short, _ := transcript.SplitModel(model) // just the model id; the provider is in /models
	part(metaModel, short, theme.StyleDim)
	if variant == "" {
		variant = "default"
	}
	sep()
	part(metaVariant, variant, theme.StyleDim)
	if queued > 0 {
		b.WriteString(theme.StyleRule.Render(" ─ ") + theme.StyleDim.Render(fmt.Sprintf("%d queued", queued)))
	}
	return b.String(), spans
}

// span is the columns [from, to) of one clickable thing drawn on a row.
type span[T any] struct {
	from, to int
	id       T
}

// hitSpan returns what was drawn at column x.
func hitSpan[T any](spans []span[T], x int) (T, bool) {
	for _, s := range spans {
		if x >= s.from && x < s.to {
			return s.id, true
		}
	}
	var zero T
	return zero, false
}

// --- footer ---

type footerInfo struct {
	home      bool
	connected bool
	label     string // selected agent label
	model     string // selected agent model (provider/id)
	context   int    // estimated tokens the next model call carries
	window    int    // the model's context window; 0 hides the bar
}

// footerRight is the right end of the divider over the input: the sign-in
// nudge while nothing is connected, nothing on the home view, else how full
// the agent's context is ("31% · 62k/200k tokens"; nothing while the
// window is unknown, and in the channel chat, which has no one agent's
// context). Tokens and cost are the nav's (its usage rows), not repeated.
func footerRight(f footerInfo) string {
	switch {
	case !f.connected:
		return theme.StyleBold.Render("Get started") + " " + theme.StyleDim.Render("/providers")
	case f.home:
		return ""
	}
	return contextBar(f.context, f.window)
}

// contextBar reads how full the model's context is — "31% · 62k/200k tokens" — which
// is what auto-compaction watches (it summarises at 80%). Dim until 70%,
// warning-coloured from there. "" when the window is unknown.
func contextBar(context, window int) string {
	pct, st, ok := contextFill(context, window)
	if !ok {
		return ""
	}
	return st.Render(fmt.Sprintf("%d%% · %s/%s tokens", pct, format.Tokens(context), format.Tokens(window)))
}

// contextFill is how full the context is, in percent (at most 100), and its
// colour: dim until 70%, warning from there. ok is false while the window is
// unknown.
func contextFill(context, window int) (int, lipgloss.Style, bool) {
	if window <= 0 || context < 0 {
		return 0, lipgloss.Style{}, false
	}
	pct := min(context*100/window, 100)
	if pct >= 70 {
		return pct, theme.StyleWarn, true
	}
	return pct, theme.StyleDim, true
}

// --- view ---

// View composes the main area (home or channel) and the footer.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting…"
	}
	if m.viewDirty { // only when View runs without Update, which redraws once at its end
		m.refreshViewport()
	}
	f := m.computeFrame()
	keybar, kb := f.keybar, f.keybarRows
	mainH := m.height - kb
	if mainH < 1 {
		mainH = 1
	}
	var main string
	if m.isHome() {
		main = m.homeView(m.width, mainH)
	} else {
		main = m.channelView(f, m.width)
	}
	if m.cfgEditor != nil {
		main = dialog.Composite(main, m.width, mainH, m.cfgEditor.view(m.width, mainH))
	} else if m.ov != nil {
		m.ov.hints = m.keyHints() // the dialog's own keys, shown whatever the key bar setting
		main = dialog.Composite(main, m.width, mainH, m.ov.view(m.width, m.sp.View()))
	} else if isTab(m.focus) {
		main = dialog.Composite(main, m.width, mainH, m.tabDialog(m.width))
	}
	frame := main
	if kb > 0 {
		frame = main + "\n" + keybar
	}
	// Everything from the daemon was cleaned on the way in; the frame is
	// filtered once more so a field that was missed can restyle a few cells
	// at most, never move the cursor or retitle the window.
	return textsafe.Frame(m.highlightSelection(frame))
}

// isHome reports whether the selected agent has nothing to show yet.
func (m Model) isHome() bool {
	if m.opened {
		return false
	}
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

// metaLeft is the meta row's left side for the selected agent and where
// its parts were drawn (the mouse hit-tests the same spans).
func (m Model) metaLeft() (string, []span[metaPart]) {
	label, role, model, variant, queued := "agent", "", m.channel.Model, "", 0
	if a := m.selectedAgent(); a != nil {
		label, role, variant, queued = a.Name, a.Role, a.Variant, a.Queued
		if a.Model != "" {
			model = a.Model
		}
	}
	sel := metaNone
	switch {
	case m.ov != nil && m.ov.kind == ovRoles:
		sel = metaRole
	case m.ov != nil && m.ov.kind == ovModels:
		sel = metaModel
	case m.ov != nil && m.ov.kind == ovVariants:
		sel = metaVariant
	case m.ov == nil && m.focus == focusMeta:
		sel = m.metaSel
	}
	if m.superChat { // the channel chat: role, model and variant are an agent's, and the mode leads the input
		if m.channel.Dir == "" {
			return "", nil
		}
		sep := theme.StyleRule.Render(" ─ ")
		label := theme.StyleDim.Render(channelLabel(m.channel)) + sep + theme.StyleDim.Render(format.ShortHome(m.channel.Dir))
		if m.channel.DirError != "" {
			label += sep + theme.StyleDim.Render("directory unavailable")
		}
		return label, nil
	}
	return metaLineSpans(label, role, model, variant, queued, "", sel, m.hover.meta) // the mode tag leads the input instead
}

// metaRow is the home screen's line under the input: role and model on the
// left, the sign-in nudge and the agent's tabs on the right, dot separators
// within each side. The left side is truncated first when they collide. A
// chat carries all of it on the divider instead (divider).
func (m Model) metaRow(width int) string {
	left, _ := m.metaLeft()
	var parts []string // the usage sits on the divider over the input (ruleLine)
	if !m.connected() {
		parts = append(parts, m.footerRightView()) // the sign-in nudge
	}
	if tabs := m.agentTabs(); tabs != "" {
		parts = append(parts, tabs) // async · due · todo · mcp at the right end
	}
	right := strings.Join(parts, "   ")
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
const tagline = "Giddy up!"

// styleTagline: a terminal cannot draw the tagline larger, so it is bold in
// the logo's bright tone to read as a heading rather than a caption.
var styleTagline = lipgloss.NewStyle().Bold(true).Foreground(theme.ColAccent)

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
	add(styleTagline.Render(tagline), lipgloss.Width(tagline))
	lay.lines = append(lay.lines, "") // air between the tagline and the input
	add(m.statusLine(boxW), boxW)     // status messages sit above the input, as in a channel
	if pv := m.paletteViewFor(boxW); pv != "" {
		add(pv, boxW)
	}
	add(m.inputBoxView(boxW), boxW)
	lay.lines = append(lay.lines, "")
	if m.stripShown() {
		if sv := m.sectionsView(boxW); sv != "" {
			add(sv, boxW)
		}
	}
	// The directory the channel will work in, dim, above the meta row.
	add(theme.StyleDim.Render(format.ShortHome(m.channel.Dir)), boxW)
	add(m.metaRow(boxW), boxW)
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

// channelView is the transcript over the input box, plus the sidebar.
func (m Model) channelView(f frame, width int) string {
	cw := m.contentWidth()
	// The chat shares the top with the sidebar; everything from the rule down
	// (the status and usage sit on the rule) spans the whole window, so the
	// footer cuts the sidebar off, not the other way round.
	top := padLines(m.vp.View(), cw)
	if status := m.statusText(); status != "" { // the transient message, at the right of the chat's last row
		top = withStatus(top, status, cw)
	}
	if m.sidebarVisible() {
		h := m.vp.Height
		sep := theme.StyleSep.Render(strings.TrimSuffix(strings.Repeat("│ \n", h), "\n")) // a space keeps the chat off the line
		top = lipgloss.JoinHorizontal(lipgloss.Top, m.sidebarView(h), sep, top)           // the sidebar sits on the left
	}
	// Under the divider: the palette (while open) and the input, then a blank
	// line and the tab strip when there is one.
	parts := []string{top, m.ruleLine(width), ""} // air between the divider and the input (dividerGap)
	if f.palette != "" {
		parts = append(parts, f.palette)
	}
	parts = append(parts, m.inputBoxView(width))
	if f.sections != "" {
		parts = append(parts, "", f.sections) // air between the input and the strip
	}
	return padLines(strings.Join(parts, "\n"), width)
}

// withStatus draws the transient status at the right end of the last row of
// top (width columns wide), over whatever the chat has there.
func withStatus(top, status string, width int) string {
	lines := strings.Split(top, "\n")
	if width < 8 {
		return top
	}
	status = ansi.Truncate(status, width-4, "…")
	keep := width - lipgloss.Width(status) - 1
	last := ansi.Truncate(lines[len(lines)-1], keep, "")
	lines[len(lines)-1] = last + strings.Repeat(" ", max(0, width-lipgloss.Width(last)-lipgloss.Width(status))) + status
	return strings.Join(lines, "\n")
}

// sidebarView is the left panel — the swarm nav: the header (app name,
// channel directory, cost and age, swarm state) then the agent tree with a
// badge and cost per row. What the tree selects, the rest of the screen
// shows and the footer controls.
func (m Model) sidebarView(height int) string {
	rows, _ := m.sidebarLines(height)
	return lipgloss.NewStyle().Width(sidebarWidth).Height(height).MaxHeight(height).Render(strings.Join(rows, "\n"))
}

// sidebarLines is the sidebar as drawn height rows tall: the header, then
// the window of the body that keeps the cursor's row in view (the body
// scrolls once it is taller than the room under the header). items maps
// each drawn row to its cursor index, -1 for rows the cursor skips; a
// click reads it, so it always matches what is drawn.
func (m Model) sidebarLines(height int) (rows []string, items []int) {
	inner := sidebarWidth - 1 // rows start at the left edge; one column of right padding
	header := m.sidebarHeader(inner)
	body, bodyItems := m.sidebarBody(inner)
	room := max(0, height-len(header))
	start, end := 0, min(len(body), room)
	if len(body) > room && room > 0 {
		// the window is wherever the nav is scrolled to; the cursor moving is
		// what scrolls it (followSidebarCursor), not drawing it
		start = min(max(m.sbTop, 0), len(body)-room)
		end = start + room
	}
	rows = append(header, body[start:end]...)
	items = make([]int, len(header), len(rows))
	for i := range items {
		items[i] = -1
	}
	items = append(items, bodyItems[start:end]...)
	if len(rows) > height {
		rows, items = rows[:height], items[:height]
	}
	return rows, items
}

// sidebarHeader is what precedes the tree: the app name, a blank, the
// system's tokens and cost (every channel), the selected chat's (the
// channel's in its chat, the agent's in an agent's chat), a blank, the
// prompt-cache share of the last hour's calls, Discord status, and a blank; the "channels" title is the body's first row. The tree's
// first row follows, which is how a click on the sidebar finds its agent.
func (m Model) sidebarHeader(width int) []string {
	rows := []string{
		theme.StyleAccent.Bold(true).Render("Stavlos") + strings.Repeat(" ", max(1, width-len("Stavlos")-2)) + theme.StyleDim.Render(channelGear+" "),
		"",
	}
	rows = append(rows, m.planUsageRows(width, time.Now())...)
	rows = append(rows,
		m.navUsageRow(m.sidebarSystemRow(), width),
		m.navUsageRow(m.sidebarSelectedRow(), width),
		"",
	)
	if cache := m.cacheRow(width); cache != "" {
		rows = append(rows, cache)
	}
	return append(rows, m.discordIndicator(width), "")
}

// usageRow is "label        12k · $0.25", grey: the label at the left, the
// tokens and cost flush with the right edge (like the tree's costs); a
// label too long for width is cut, never the figures. The figures are
// buttons, like the divider's: open (1 tokens, 2 cost, 0 neither) is the
// one whose chart is open, in accent; hover the one under the pointer, in
// the lighter text colour.
func usageRow(label string, tokens int, cost float64, width, open, hover int) string {
	t, c := format.Tokens(tokens), "$"+format.Cost(cost)
	fw := ansi.StringWidth(t) + 3 + ansi.StringWidth(c)
	label = ansi.Truncate(label, max(1, width-fw-1), "…")
	gap := max(1, width-ansi.StringWidth(label)-fw)
	figure := func(n int, text string) string {
		switch n {
		case open:
			return theme.StyleBoxTitleFocus.Render(text)
		case hover:
			return theme.StyleLit.Render(text)
		}
		return theme.StyleDim.Render(text)
	}
	return theme.StyleDim.Render(label+strings.Repeat(" ", gap)) + figure(1, t) + theme.StyleDim.Render(" · ") + figure(2, c)
}

// usageFigureAt is the figure of a usage row width wide at column x: the
// tokens or the cost.
func usageFigureAt(tokens int, cost float64, width, x int) (usageKind, bool) {
	tw, cw := ansi.StringWidth(format.Tokens(tokens)), ansi.StringWidth("$"+format.Cost(cost))
	start := width - tw - 3 - cw
	return hitSpan([]span[usageKind]{{start, start + tw, usageTokens}, {start + tw + 3, width, usageCost}}, x)
}

// usageRowFigures is what the nav's usage row at header row y shows: whose
// usage (system) and its tokens and cost; ok is false for other rows.
func (m Model) usageRowFigures(y int) (label string, system bool, tokens int, cost float64, ok bool) {
	switch y {
	case m.sidebarSystemRow():
		return "System", true, m.systemTokens(), m.systemCost(), true
	case m.sidebarSelectedRow():
		if a := m.selectedAgent(); a != nil && !m.superChat {
			return "@" + a.Name, false, a.Tokens, a.CostUSD, true
		}
		return channelLabel(m.channel), false, m.totalTokens(), m.totalCost(), true
	}
	return "", false, 0, 0, false
}

// navUsageRow draws the nav's usage row at header row y, its figure in
// accent while its chart is open and lighter under the pointer.
func (m Model) navUsageRow(y, width int) string {
	label, system, tokens, cost, _ := m.usageRowFigures(y)
	open := 0
	if m.focus == focusUsage && (system && m.usage.channel == "" || !system && m.usageOnSelectedChat()) {
		open = int(m.usage.kind) + 1
	}
	hover := 0
	if m.hover.navRow == y {
		hover = m.hover.navUsage
	}
	return usageRow(label, tokens, cost, width, open, hover)
}

// newChannelMark sits at the right of the channels title, in the gears'
// column: a click on it, or space or → on the title, names a new channel.
const newChannelMark = "✚"

// channelGear ends every channel row: → on the row or a click on it opens the
// channel's project configuration.
const channelGear = "⚙"

// sidebarSystemRow and sidebarSelectedRow are the header's usage rows,
// under the plan usage block: a click on the tokens figure opens the tokens
// dialog, on the cost the cost dialog.
func (m Model) sidebarSystemRow() int   { return 2 + len(m.planUsageRows(sidebarWidth-1, time.Now())) }
func (m Model) sidebarSelectedRow() int { return m.sidebarSystemRow() + 1 }

// sidebarDiscordRow opens the Discord status/control panel when clicked; a
// cache monitor row sits above it once there are calls to measure.
func (m Model) sidebarDiscordRow() int {
	row := m.sidebarSystemRow() + 3
	if m.cacheRow(sidebarWidth-1) != "" {
		row++
	}
	return row
}

// stripRows is how many tab rows the footer strip draws: the ! ? dirs row
// while the sidebar is hidden, none while it shows (! and ? sit in the
// sidebar then). An agent's own tabs sit at the right end of the meta row.
func (m Model) stripRows() int {
	if m.sidebarVisible() {
		return 0
	}
	return 1
}

// agentTabs is the agent's tab row, async · todo · mcp, as the divider draws
// it before the usage; "" in the channel chat.
func (m Model) agentTabs() string {
	if m.superChat {
		return ""
	}
	labels, _ := m.tabLabels(m.currentPrompt())
	lines := strings.Split(labels, "\n")
	return lines[len(lines)-1]
}

// metaTabAt maps a column of the divider, drawn width wide, to the agent tab
// drawn there.
func (m Model) metaTabAt(x, width int) (focus, bool) {
	d := m.divider(width)
	if d.tabsX < 0 {
		return 0, false
	}
	return hitSpan(d.tabSpans, x-d.tabsX)
}

// sidebarBody is everything under the header: the "channels" title with its
// + for a new channel, then the
// directory's channels in alphabetical order (they never move on their
// own), each "● #name ⚙" (its state dot, its name, its gear for the
// channel's dirs), this channel's row (its chat) with its agent tree right
// under it. items maps each row to its
// cursor index (the title 0, then top to bottom; see sidebarRows), -1 for rows
// the cursor skips.
func (m Model) sidebarBody(width int) (rows []string, items []int) {
	focused := m.focus == focusSidebar && m.sidebarVisible()
	line := func(text string, idx int) {
		if focused && m.sbCursor == idx {
			text = render.Highlight(text, width)
		}
		rows, items = append(rows, text), append(items, idx)
	}
	// "● #name · ~/dir    ⚙ ", flush with the "channels" title: the
	// channel's state dot, its name, its directory (cut from the left, so
	// the project's own folder stays; "! ~/dir" in warning colour when it
	// is unavailable), and its gear a space in from the right edge (→ on
	// the row or a click on it: the channel's dirs)
	channel := func(dot, name, dir string, unavailable bool, style lipgloss.Style, idx int) {
		room := width - 6
		name = ansi.Truncate(name, room, "…")
		text, w := style.Render(name), ansi.StringWidth(name)
		if dir != "" {
			path, pathStyle := format.ShortHome(dir), theme.StyleDim
			if unavailable {
				path, pathStyle = "! "+path, theme.StyleWarn
			}
			if rest := room - w - 3; rest >= 4 {
				if pw := ansi.StringWidth(path); pw > rest {
					path = ansi.TruncateLeft(path, pw-rest+1, "")
					if i := strings.Index(path, "/"); i > 0 {
						path = path[i:] // start at a folder boundary
					}
					path = "…" + path
				}
				text += theme.StyleDim.Render(" · ") + pathStyle.Render(path)
				w += 3 + ansi.StringWidth(path)
			}
		}
		line(dot+" "+text+strings.Repeat(" ", max(1, width-4-w))+theme.StyleDim.Render(channelGear)+" ", idx)
	}
	other := func(s protocol.ChannelInfo, idx int) {
		dot := stateDot(string(s.State))
		if mark := promptMark(m.promptCountsIn(promptScope{channel: s.ID})); mark != "" {
			dot = mark
		}
		channel(dot, channelLabel(s), s.Dir, s.DirError != "", theme.StyleDim, idx)
	}
	tree := m.treeRows(width)
	// another channel's kept tree, rendered once and indexed by its row
	others := map[string][]string{}
	for _, s := range m.navChannels {
		if m.otherTreeShown(s.ID) {
			others[s.ID] = m.agentRows(m.trees[s.ID], "", width)
		}
	}
	for i, r := range m.sidebarRows() {
		switch r.kind {
		case sbNewChannel:
			// the "channels" title, with its + (a new channel) a space in from
			// the right edge, in the gears' column
			line(theme.StyleAccent.Bold(true).Render("Channels")+strings.Repeat(" ", max(1, width-10))+theme.StyleDim.Render(newChannelMark)+" ", i)
		case sbOther:
			other(m.navChannels[r.k], i)
		case sbHere:
			states := make([]protocol.AgentState, 0, len(m.agents))
			for _, a := range m.agents {
				states = append(states, a.State)
			}
			style := theme.StyleDim
			if m.superChat {
				style = theme.StyleSelected
			}
			dot := stateDot(string(protocol.RollUp(states)))
			if mark := promptMark(m.promptCountsIn(promptScope{channel: m.channelID})); mark != "" {
				dot = mark
			}
			channel(dot, channelLabel(m.channel), m.channel.Dir, m.channel.DirError != "", style, i)
			if len(m.agents) == 0 && !m.treeClosed[m.channelID] {
				rows, items = append(rows, tree[0]), append(items, -1) // the "(no agents)" row
			}
		case sbAgent:
			line(tree[r.k], i)
		case sbHereAll:
			_, quiet := m.shownAgents(m.channelID, m.agents, m.openAgentID())
			line(showAllLabel(m.treeAll[m.channelID], quiet, width), i)
		case sbOtherAll:
			id := m.navChannels[r.k].ID
			_, quiet := m.shownAgents(id, m.trees[id], "")
			line(showAllLabel(m.treeAll[id], quiet, width), i)
		case sbOtherAgent:
			if rs := others[m.navChannels[r.k].ID]; r.j < len(rs) {
				line(rs[r.j], i)
			}
		}
	}
	// a blank row at the end, so the nav scrolled to its bottom never ends
	// on the frame's edge
	return append(rows, ""), append(items, -1)
}

// showAllLabel is the row under a channel's tree that shows its quiet
// agents ("  ▸ show all · 3 idle") or hides them again ("  ▾ hide idle").
func showAllLabel(all bool, quiet, width int) string {
	text := fmt.Sprintf("▸ show all · %d idle", quiet)
	if all {
		text = "▾ hide idle"
	}
	return theme.StyleDim.Render(ansi.Truncate("  "+text, width, "…"))
}

// channelLabel is a channel's label in the sidebar and the picker: "#name".
func channelLabel(s protocol.ChannelInfo) string {
	if s.Name == "" {
		return "#channel"
	}
	return "#" + s.Name
}

// needsHuman is the badge for an agent with a prompt of its own waiting:
// "!" for a permission or trust prompt, "?" for a question, "" otherwise.
func (m Model) needsHuman(agent string) string {
	badge := ""
	for _, p := range m.prompts {
		if p.Agent != agent {
			continue
		}
		if p.Kind == "question" {
			if badge == "" {
				badge = "?"
			}
			continue
		}
		return "!"
	}
	return badge
}

// treeRows renders the agent tree, one row per agent: indent, the state
// dot (an orange ! or ? in its place while a permission or question of the
// agent's own waits), "@label (role)", then the agent's cost at the right
// edge. Every row is exactly width wide. The selected agent
// reads bold; the sidebar cursor is a background across the row, as in
// the chat.
func (m Model) treeRows(width int) []string {
	return m.agentRows(m.agents, m.openAgentID(), width)
}

// agentRows renders one channel's agent tree. selected names the agent
// drawn bold, "" for none: another channel's tree has no selection.
func (m Model) agentRows(agents []protocol.AgentInfo, selected string, width int) []string {
	rows := make([]string, 0, len(agents))
	for _, a := range agents {
		indent := "  " + strings.Repeat("  ", a.Depth) // one level under this channel's "#name" row
		dot := agentDot(a)
		if b := m.needsHuman(a.ID); b != "" {
			dot = theme.StyleWarn.Render(b) // waiting on the human: the mark takes the dot's place
		}
		// The right column: the cost (dim), with a space before it.
		right, rightW := "", 0
		if a.CostUSD > 0 {
			c := "$" + format.Cost(a.CostUSD)
			right, rightW = theme.StyleDim.Render(c), len([]rune(c))
		}
		// indent + dot + " " is two columns plus the indent; the text gets
		// what is left before the right column and one space.
		avail := width - len([]rune(indent)) - 2 - rightW
		if rightW > 0 {
			avail--
		}
		if avail < 4 {
			avail = 4
		}
		text := fmt.Sprintf("@%s (%s)", a.Name, a.Role) // an agent reads @name, as it is addressed
		if agentOutcome(a) == "error" {
			text += " · error"
		}
		if len([]rune(text)) > avail {
			text = format.Trunc(text, avail-1) // the ellipsis takes the last column
		}
		textW := ansi.StringWidth(text)
		tint := ""
		if r := m.roleInfo(a.Role); r != nil {
			tint = r.Color
		}
		switch {
		case selected != "" && a.ID == selected:
			text = roleStyle(tint).Inherit(theme.StyleSelected).Render(text)
		case tint != "":
			text = roleStyle(tint).Render(text)
		default:
			text = theme.StyleDim.Render(text)
		}
		gap := width - len([]rune(indent)) - 2 - textW - rightW
		if gap < 1 && rightW > 0 {
			gap = 1
		}
		if gap < 0 {
			gap = 0
		}
		rows = append(rows, indent+dot+" "+text+strings.Repeat(" ", gap)+right)
	}
	if len(rows) == 0 {
		rows = append(rows, theme.StyleDim.Render("  (no agents)"))
	}
	return rows
}

// sectionsView is the two-line strip at the bottom of the footer: the tabs
// with their counts (always shown, "(0)" when empty). The body of whichever tab has
// focus is a dialog (tabDialog), not an inline block.
func (m Model) sectionsView(width int) string {
	return m.sectionTabs(m.currentPrompt(), width)
}

// tabDialog is the dialog of the open tab, drawn over the chat like every
// other dialog: its title and count (with "esc: close" at the right), a
// blank line, then its rows (the pending prompt, the live children, the
// running jobs) or a note that it is empty. Each tab has its own; nothing
// switches between them from inside.
func (m Model) tabDialog(bodyWidth int) string {
	box, _ := m.tabDialogBox(bodyWidth)
	return box
}

// tabDialogBox is tabDialog plus, for every line inside the border (the
// title is line 0), the selectable row drawn there or -1.
func (m Model) tabDialogBox(bodyWidth int) (string, []int) {
	w := dialog.Width(bodyWidth)
	inner := w - 4 // border + padding
	lines := []string{dialog.Title(m.tabDialogTitle(), inner), ""}
	rows := []int{-1, -1}
	body, bodyRows := m.tabBodyRows(inner)
	for i, l := range body {
		for k, part := range strings.Split(l, "\n") {
			lines = append(lines, ansi.Truncate(part, inner, "…"))
			if k == 0 {
				rows = append(rows, bodyRows[i])
			} else {
				rows = append(rows, -1)
			}
		}
	}
	if f := dialog.HintLines(m.keyHints(), inner); len(f) > 0 {
		lines = append(lines, "")
		lines = append(lines, f...)
		for range len(f) + 1 {
			rows = append(rows, -1)
		}
	}
	return dialog.Box(inner, lines), rows
}

// tabDialogTitle is the open tab's title with its count; a prompt dialog
// is named after the kind of prompt at the head of the queue.
func (m Model) tabDialogTitle() string {
	if m.focus == focusUsage {
		return m.usageTitle()
	}
	for _, t := range m.tabs() {
		if t.focus == m.focus {
			return t.name + " " + t.count
		}
	}
	return ""
}

// tab is one tab of the strip, as its label and its dialog's title show it.
type tab struct {
	focus focus
	label string // the word on the strip: "permission", "questions", "async" …
	name  string // the dialog's title: "Permission", "MCP" …
	glyph string // drawn on the strip in place of the label (the prompt tabs, to save room)
	count string
	warn  bool // something waits on the human: warning-coloured while the tab is not open
}

// text is the tab as the strip would spell it out: "async 4".
func (t tab) text() string { return t.label + " " + t.count }

// tabs is every tab of the strip in tabFocuses order. Counts: permission
// and questions read position/total while something waits ("1/2": the
// first of two; "2/3": the second question of three) and "0" otherwise;
// todo and mcp read done/total and connected/listed; the rest are plain
// counts. A prompt tab is named after the kind of prompt at the head of
// the queue ("trust").
func (m Model) tabs() []tab {
	perms, _ := m.promptCountsIn(m.scope) // an open, scoped dialog counts what it shows
	permKind := string(protocol.PromptPermission)
	if p := m.currentPrompt(); p != nil && p.Kind != "" && p.Kind != protocol.PromptPermission {
		permKind = string(p.Kind) // "trust"
	}
	permCount := "0"
	if perms > 0 {
		permCount = fmt.Sprintf("1/%d", perms)
	}
	waitingPerms, _ := m.promptCounts()
	byTab := map[focus]tab{
		focusPermission: {label: permKind, glyph: transcript.GlyphPermission, count: permCount, warn: waitingPerms > 0},
		focusDirs:       {label: "dirs", count: strconv.Itoa(len(m.channelDirs()))},
		focusAsync:      {label: "async", count: strconv.Itoa(m.asyncCount())},
		focusNudges:     {label: "nudges", count: strconv.Itoa(m.nudgeCount()), warn: m.nudgeCount() > 0},
		focusTodo:       {label: "todo", count: todoCount(m.selectedTodos())},
		focusMCP:        {label: "mcp", name: "MCP", count: mcpCount(m.selectedMCP())},
	}
	tabs := make([]tab, len(tabFocuses))
	for i, f := range tabFocuses {
		t := byTab[f]
		t.focus = f
		if t.name == "" && t.label != "" {
			t.name = strings.ToUpper(t.label[:1]) + t.label[1:]
		}
		tabs[i] = t
	}
	return tabs
}

// tabBodyRows is tabBodyLines plus, for every line, the selectable row it
// draws (-1 for none): what a click or hover on that line picks.
func (m Model) tabBodyRows(width int) ([]string, []int) {
	// rowsAt maps n rows drawn one per line from line start.
	rowsAt := func(lines []string, start, n int) ([]string, []int) {
		rows := make([]int, len(lines))
		for i := range rows {
			rows[i] = -1
			if start >= 0 && i >= start && i < start+n {
				rows[i] = i - start
			}
		}
		return lines, rows
	}
	note := func(text string) ([]string, []int) { return rowsAt([]string{theme.StyleDim.Render(text)}, -1, 0) }
	switch m.focus {
	case focusUsage:
		return rowsAt(m.usageBody(width), -1, 0)
	case focusAsync:
		if m.hasReplyDetails() {
			return m.replyBodyRows(width)
		}
		// what the selected agent waits on (the agents whose answer it
		// expects, then its running jobs), then who waits on its reply (you,
		// then agents), each under a dim label the cursor skips
		waiting, jobs := m.awaitedAgents(), m.runningJobs()
		human, owed := m.dueOf()
		owner, role := "", ""
		if a := m.selectedAgent(); a != nil {
			owner, role = a.Name, a.Role
		}
		now := time.Now()
		sel := agentRows(waiting, m.spawned, m.lastLines(), m.roleTints(), now, width-2)
		sel = append(sel, jobRows(jobs, owner, role, now, width-2)...)
		nWait := len(sel)
		if human {
			sel = append(sel, "  "+theme.StyleBold.Render("you")+"  "+theme.StyleDim.Render("the channel chat"))
		}
		sel = append(sel, agentRows(owed, m.spawned, m.lastLines(), m.roleTints(), now, width-2)...)
		if len(sel) == 0 {
			return note("  not waiting on anything, and no replies due")
		}
		marked := m.cursorRows(sel)
		var lines []string
		var rows []int
		add := func(line string, row int) { lines, rows = append(lines, line), append(rows, row) }
		if nWait > 0 {
			add(theme.StyleDim.Render("waiting on"), -1)
			for i := range nWait {
				add(marked[i], i)
			}
		}
		if len(sel) > nWait {
			if nWait > 0 {
				add("", -1)
			}
			add(theme.StyleDim.Render("owes a reply to"), -1)
			for i := nWait; i < len(sel); i++ {
				add(marked[i], i)
			}
		}
		return lines, rows
	case focusNudges:
		return m.nudgeRows(width - 2)
	case focusTodo:
		items := m.selectedTodos()
		if len(items) == 0 {
			return note("  no todo items")
		}
		rows := todoRows(items, width-2)
		return rowsAt(m.cursorRows(rows), 0, len(rows))
	case focusMCP:
		items := m.selectedMCP()
		if len(items) == 0 {
			return note("  no mcp servers")
		}
		rows, _ := mcpRows(items, m.mcpOpen, time.Now(), width-2)
		return rowsAt(m.cursorRows(rows), 0, len(rows))
	case focusDirs:
		items := m.channelDirs()
		var rows []string
		n := 0
		if len(items) == 0 {
			rows = []string{theme.StyleDim.Render("  no directories")}
		} else {
			rows = m.cursorRows(dirRows(items, width-2))
			n = len(rows)
		}
		if m.dirEdit != "" {
			label := "add a directory"
			if m.dirEdit != "add" {
				label = "replace " + format.ShortHome(m.dirEdit)
			}
			if m.dirEdit == "default" {
				label = "default directory (idle only; resets approvals)"
			}
			rows = append(rows, "", theme.StyleDim.Render(label), m.dirInput.View())
		}
		return rowsAt(rows, 0, n)
	case focusPermission:
		p := m.currentPrompt()
		if p == nil {
			return note("  no prompts waiting")
		}
		lines, start := m.promptBox(p, width)
		return rowsAt(lines, start, len(permOptions(p)))
	}
	return nil, nil
}

// questionLines renders the checklist and free-text field. The question,
// asking agent and position belong to the message line. The n selectable
// rows (options, custom answer and Submit) start at optStart.
func (m Model) questionLines(p *protocol.PromptInfo, width int) (lines []string, optStart, n int) {
	q := m.q
	if q.id != p.ID || len(p.Questions) == 0 {
		q = questionState{answers: make([]string, len(p.Questions))}
	}
	if q.idx >= len(p.Questions) {
		q.idx = len(p.Questions) - 1
	}
	if len(p.Questions) == 0 {
		return nil, -1, 0
	}
	cur := p.Questions[q.idx]
	// The checklist: every option, then a last row for a typed answer.
	optStart, n = len(lines), len(cur.Options)+1
	for i, o := range cur.Options {
		marker := cursorMarker(m.focus == focusQuestions && i == q.sel && !q.typing)
		mark := theme.StyleDim.Render(transcript.QuestionUnchecked)
		if q.marks[i] {
			mark = theme.StyleAccent.Render(transcript.QuestionChecked)
		}
		row := marker + mark + " " + o.Label
		if o.Description != "" {
			row += "  " + theme.StyleDim.Render(o.Description)
		}
		lines = append(lines, ansi.Truncate(row, width, "…"))
	}
	marker := cursorMarker(m.focus == focusQuestions && q.sel == len(cur.Options) && !q.typing)
	customMark := theme.StyleDim.Render(transcript.QuestionUnchecked)
	custom := q.custom
	if q.typing {
		custom = m.promptInput.Value()
	}
	if strings.TrimSpace(custom) != "" {
		customMark = theme.StyleAccent.Render(transcript.QuestionChecked)
	}
	customLead := marker + customMark + " "
	switch {
	case q.typing:
		field := m.promptInput
		field.Prompt = "" // the checkbox is the field's inline leader
		lines = append(lines, ansi.Truncate(customLead+field.View(), width, "…"))
	case strings.TrimSpace(q.custom) != "":
		lines = append(lines, ansi.Truncate(customLead+q.custom, width, "…"))
	default:
		lines = append(lines, ansi.Truncate(customLead+theme.StyleDim.Render("Reply with a custom answer…"), width, "…"))
	}
	lines = append(lines, ansi.Truncate(cursorMarker(m.focus == focusQuestions && q.sel == len(cur.Options)+1)+"[Submit answer]", width, "…"))
	n++
	if done := answered(q.answers); done > 0 && done < len(p.Questions) {
		lines = append(lines, "", theme.StyleDim.Render(fmt.Sprintf("%d of %d answered · ←/→ to review", done, len(p.Questions))))
	}
	return lines, optStart, n
}

func answered(answers []string) int {
	n := 0
	for _, a := range answers {
		if a != "" {
			n++
		}
	}
	return n
}

// sectionTabs is the one-line strip: the tabs with their counts, the
// highlighted one in accent, the rest dim (except a permission tab with
// prompts waiting, which is warning orange). Key hints live in the key bar.
func (m Model) sectionTabs(p *protocol.PromptInfo, width int) string {
	labels, _ := m.tabLabels(p)
	rows := strings.Split(labels, "\n")
	rows = rows[:m.stripRows()] // the ! ? dirs row sits in the sidebar while it shows; the agent's row on the meta row
	for i := range rows {
		rows[i] = ansi.Truncate(rows[i], width, "…") // the channel directory lives in the dirs tab
	}
	return strings.Join(rows, "\n")
}

// tabLabels is the strip, a line per row of tabRows: "! 1/2 · ? 0 · dirs n" (the
// channel's) over "async n · due n · todo … · mcp …" (the selected agent's). The highlighted tab (while the strip has focus) or the open one
// (while its dialog is up) is in accent, the rest dim; with where each label
// was drawn, per row.
func (m Model) tabLabels(p *protocol.PromptInfo) (string, [][]span[focus]) {
	layout := m.tabLayout()
	order := slices.Concat(layout...)
	on := func(f focus) bool {
		if m.focus == focusTabs {
			return m.tabSel < len(order) && order[m.tabSel] == f
		}
		return m.focus == f
	}
	byFocus := make(map[focus]tab, len(tabFocuses))
	for _, t := range m.tabs() {
		byFocus[t.focus] = t
	}
	tabs := make([]string, len(order))
	spans := make([][]span[focus], len(layout))
	row, x, first := 0, 0, 0 // first: the index of row's first tab
	for i, f := range order {
		if i-first == len(layout[row]) {
			first += len(layout[row])
			row, x = row+1, 0
		}
		t := byFocus[f]
		label := t.text()
		if t.glyph != "" { // "! 1/2" (permission or trust), "? 0"; the dialogs keep the word
			label = t.glyph + " " + t.count
		}
		w := ansi.StringWidth(label)
		spans[row] = append(spans[row], span[focus]{x, x + w, f})
		x += w + 3 // " · "
		switch {
		case on(f):
			tabs[i] = theme.StyleBoxTitleFocus.Render(label)
		case m.hover.tabOK && m.hover.tab == f: // under the pointer
			tabs[i] = theme.StyleLit.Render(label)
		// An unfocused tab with something waiting on the human is
		// warning-coloured so it stands out until someone opens it.
		case t.warn:
			tabs[i] = theme.StyleWarn.Render(label)
		default:
			tabs[i] = theme.StyleDim.Render(label)
		}
	}
	lines := make([]string, len(layout))
	first = 0
	for r, tr := range layout {
		sep := theme.StyleDim.Render(" · ")
		if slices.Equal(tr, tabRows[1]) {
			sep = theme.StyleRule.Render(" ─ ") // the agent's row sits on the divider: its own rule separates the tabs
		}
		lines[r] = strings.Join(tabs[first:first+len(tr)], sep)
		first += len(tr)
	}
	return strings.Join(lines, "\n"), spans
}

// cursorRows puts the ▸ marker (as in every dialog) on the row under
// agCursor and indents the rest to match.
func (m Model) cursorRows(rows []string) []string {
	for i := range rows {
		rows[i] = cursorMarker(i == m.agCursor%len(rows)) + strings.TrimPrefix(rows[i], "  ")
	}
	return rows
}

// promptBox renders the head of the prompt queue as the body of the
// permission dialog: the subject first — "$ command  name (role)" with the
// whole argument (the command, path or files) wrapped onto indented
// continuation lines, since it is what the user is approving and is never
// cut; a boundary prompt adds the line saying it reaches outside the
// channel's directories; trust shows the project directory and its files —
// then the single-select list of answers, and the reason or path row
// while one is open. Key hints live in the key bar. The options start at
// line optStart.
func (m Model) promptBox(p *protocol.PromptInfo, width int) (lines []string, optStart int) {
	who := ""
	if p.Agent != "" {
		who = m.promptWho(p)
	}
	switch p.Kind {
	case "trust":
		var t struct {
			Dir   string   `json:"dir"`
			Hash  string   `json:"hash"`
			Files []string `json:"files"`
		}
		_ = json.Unmarshal(p.Input, &t)
		// The directory and file names come from the repository: shown
		// with their controls visible, like a command.
		head := theme.StyleWorking.Render("◆") + " " + textsafe.Visible(format.ShortHome(t.Dir))
		if who != "" {
			head += "  " + theme.StyleDim.Render(who)
		}
		lines = append(lines, head)
		for i, f := range t.Files {
			if i == 10 {
				lines = append(lines, fmt.Sprintf("  (+%d more)", len(t.Files)-10))
				break
			}
			lines = append(lines, "  "+textsafe.Visible(f))
		}
	default:
		g, gap := transcript.ToolGlyph(p.Tool)
		head := theme.StyleWorking.Render(g) + gap
		// Controls in the subject are shown, not stripped: a command that
		// tried to erase part of itself from the screen reads as "^[".
		arg := textsafe.Visible(fullToolArg(p.Tool, p.Input))
		if arg == "" {
			arg = transcript.ToolTitle(p.Tool)
		}
		text := arg
		if who != "" {
			text += "  " + who
		}
		// Continuation lines are indented once from the dialog's left edge.
		// The first line shares its row with the head, so the text is
		// wrapped with the head's width reserved in front of it. The agent
		// label sits after the subject, dim, as in the questions dialog.
		const indent = 2
		wrapW := width - indent
		if wrapW < 20 {
			wrapW = 20
		}
		reserve := strings.Repeat(" ", max(lipgloss.Width(head)-indent, 0))
		rows := strings.Split(ansi.Hardwrap(reserve+text, wrapW, true), "\n")
		for i, l := range rows {
			if i == 0 {
				l = strings.TrimPrefix(l, reserve)
			}
			if i == len(rows)-1 && who != "" {
				if k := strings.LastIndex(l, "  "+who); k >= 0 {
					l = l[:k] + "  " + theme.StyleDim.Render(who)
				}
			}
			if i == 0 {
				lines = append(lines, head+l)
			} else {
				lines = append(lines, strings.Repeat(" ", indent)+l)
			}
		}
		if p.Dir != "" {
			lines = append(lines, theme.StyleWarn.Render("outside the channel's directories")+theme.StyleDim.Render(" · "+format.ShortHome(p.Dir)))
		}
	}
	lines = append(lines, "")
	optStart = len(lines)
	sel := m.permSelection(p)
	for i, o := range permOptions(p) {
		marker := cursorMarker(i == sel && m.permEdit == "")
		mark := theme.StyleDim.Render("○")
		if i == sel {
			mark = theme.StyleAccent.Render("●")
		}
		row := marker + mark + " " + o.label
		if o.desc != "" {
			row += "  " + theme.StyleDim.Render(o.desc)
		}
		lines = append(lines, ansi.Truncate(row, width, "…"))
	}
	switch m.permEdit {
	case "dir":
		lines = append(lines, "", theme.StyleDim.Render("directory to add"), m.dirInput.View())
	case "deny":
		lines = append(lines, "", theme.StyleDim.Render("deny · a reason the agent will read, or leave it empty"), m.dirInput.View())
	}
	switch {
	case p.ClaimedBy != "" && !m.claimedByUs[p.ID]:
		lines = append(lines, theme.StyleStatusErr.Render("claimed by another client"))
	case m.promptBusy == p.ID:
		lines = append(lines, theme.StyleDim.Render("answering…"))
	}
	return lines, optStart
}

// agentWhoLabel is "label (role)" for an agent, as the async rows name a
// job's owner; just the label when the role is unknown, "" for no agent.
func (m Model) agentWhoLabel(id string) string {
	if id == "" {
		return ""
	}
	if i := m.findAgent(id); i >= 0 {
		a := m.agents[i]
		if a.Role != "" {
			return fmt.Sprintf("%s (%s)", a.Name, a.Role)
		}
		return a.Name
	}
	return id
}

// promptWho names who a prompt waits for: "label (role)" for an agent of this
// channel, else "@name" from the prompt, with "#channel" after it when the
// prompt is another channel's.
func (m Model) promptWho(p *protocol.PromptInfo) string {
	var parts []string
	if who := m.agentWhoLabel(p.Agent); m.findAgent(p.Agent) >= 0 {
		parts = append(parts, who)
	} else if p.From != "" {
		if p.Role != "" {
			parts = append(parts, p.From+" ("+p.Role+")")
		} else {
			parts = append(parts, "@"+p.From)
		}
	} else if who != "" {
		parts = append(parts, who)
	}
	if p.Channel != "" && p.Channel != m.channelID && p.ChannelName != "" {
		parts = append(parts, "#"+p.ChannelName)
	}
	return strings.Join(parts, " · ")
}

// fullToolArg is transcript.ToolArg without the one-line flattening for the tools
// whose argument is text the user must read in full before approving.
func fullToolArg(tool string, raw json.RawMessage) string {
	switch tool {
	case toolname.WebFetch:
		var in struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(raw, &in)
		return strings.TrimSpace(in.URL)
	case toolname.WebSearch:
		var in struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(raw, &in)
		return strings.TrimSpace(in.Query)
	case toolname.Shell:
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(raw, &in) == nil {
			return strings.TrimRight(in.Command, "\n")
		}
	}
	return transcript.ToolArg(tool, raw)
}

// statusLine is the transient message (copied, resumed, an error…) cut to
// width, or blank: over the input on the home screen; a channel's view puts
// it on the divider instead (ruleLine).
func (m Model) statusLine(width int) string {
	return ansi.Truncate(m.statusText(), width, "…")
}

// statusText is the transient message, coloured: an error red, anything
// else green, "replaying events…" dim while history loads; "" for none.
func (m Model) statusText() string {
	switch {
	case m.status != "" && m.statusErr:
		return theme.StyleStatusErr.Render(m.status)
	case m.status != "":
		return theme.StyleStatusOK.Render(m.status)
	case m.loading:
		return theme.StyleDim.Render("replaying events…")
	}
	return ""
}

func (m Model) footerRightView() string {
	f := footerInfo{home: m.isHome(), connected: m.connected(), model: m.channel.Model}
	if m.superChat { // the channel chat: no one agent's context
		return footerRight(f)
	}
	if a := m.selectedAgent(); a != nil {
		f.label = a.Name
		f.context, f.window = a.Context, a.ContextWindow
		if a.Model != "" {
			f.model = a.Model
		}
	}
	return footerRight(f)
}

// divider is the line over the input and where its buttons were drawn:
// the selected agent's role, model and variant at its left end ("─ main
// (general) · gpt-5.4 · default ───"), then at its right the agent's tabs
// and the usage, or the sign-in nudge while nothing is connected ("async 0
// · todo 0 · mcp 0 · 31% · 62k/200k tokens · $0.02 ─"). The channel chat
// has no agent, so only its usage. When they do not fit, the tabs go first,
// then the usage, and the left side is cut.
type divider struct {
	line      string
	metaX     int // column where the role, model and variant start
	metaSpans []span[metaPart]
	tabsX     int // column where the agent's tabs start, -1 when not drawn
	tabSpans  []span[focus]
}

func (m Model) divider(width int) divider {
	dash := theme.StyleRule.Render
	d := divider{tabsX: -1}
	tabs, usage := m.agentTabs(), m.footerRightView()
	right, rightW := "", 0
	fit := func(parts ...string) bool {
		text := strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), dash(" ─ "))
		if text == "" || lipgloss.Width(text)+4 > width {
			return false
		}
		right, rightW = " "+text+" "+dash("─"), lipgloss.Width(text)+3
		return true
	}
	switch {
	case tabs != "" && fit(tabs, usage):
		d.tabsX = width - rightW + 1
		_, spans := m.tabLabels(m.currentPrompt())
		d.tabSpans = spans[len(spans)-1]
	case fit(usage):
	}
	left, leftW := "", 0
	if meta, spans := m.metaLeft(); meta != "" {
		if avail := width - rightW - 4; avail >= 4 {
			meta = ansi.Truncate(meta, avail, "…")
			left, leftW = dash("─")+" "+meta+" ", lipgloss.Width(meta)+3
			d.metaX, d.metaSpans = 2, spans
		}
	}
	d.line = left + dash(strings.Repeat("─", max(0, width-leftW-rightW))) + right
	return d
}

// ruleLine is the divider as drawn width wide.
func (m Model) ruleLine(width int) string { return m.divider(width).line }

// connected reports whether a provider and a model are usable.
func (m Model) connected() bool {
	if !m.reconciled {
		return true // unknown yet; avoid flashing the nudge
	}
	if m.channel.Model == "" || (len(m.agents) > 0 && m.agents[0].Model == "") {
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

// roleColors maps a role's colour name to the theme colour it tints with.
var roleColors = map[string]lipgloss.AdaptiveColor{
	"red": theme.ColError, "blue": theme.ColAccent, "green": theme.ColSuccess, "yellow": theme.ColYellow,
	"purple": theme.ColBlocked, "orange": theme.ColWarning, "pink": theme.ColPink, "cyan": theme.ColCyan,
}

// roleStyle is a foreground style for a role colour name; plain for "" or
// an unknown name.
func roleStyle(name string) lipgloss.Style {
	if c, ok := roleColors[name]; ok {
		return lipgloss.NewStyle().Foreground(c)
	}
	return lipgloss.NewStyle()
}

// agentRows renders the agents tab's rows, one per agent passed (the
// awaited set, see awaitedOf).
// last maps an agent id to a snippet of the latest line in its chat; it
// sits between the name and the meta, like the job on an async row.
// tint maps a role name to its colour name; a tinted role colours its
// "label (role)" text.
func agentRows(agents []protocol.AgentInfo, spawned map[string]time.Time, last map[string]string, tint map[string]string, now time.Time, width int) []string {
	var rows []string
	for _, a := range agents {
		var meta []string // the state first when it says something (working, waiting, error), then cost and age
		if o := agentOutcome(a); o != "idle" && o != "complete" {
			meta = append(meta, o)
		}
		if a.CostUSD > 0 {
			meta = append(meta, "$"+format.Cost(a.CostUSD))
		}
		if t, ok := spawned[a.ID]; ok && !t.IsZero() {
			meta = append(meta, format.Elapsed(now.Sub(t)))
		}
		text := fmt.Sprintf("%s (%s)", a.Name, a.Role)
		row := "  " + roleStyle(tint[a.Role]).Bold(true).Render(text)
		if s := last[a.ID]; s != "" {
			row += "  " + format.Trunc(s, snippetChars)
		}
		if len(meta) > 0 {
			row += "  " + theme.StyleDim.Render(strings.Join(meta, " · "))
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
func lastSnippet(t *transcript.Transcript) string {
	if t == nil {
		return ""
	}
	lines := t.All()
	last := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i].Text) != "" && lines[i].Kind != transcript.LineLabel && lines[i].Kind != transcript.LineRule {
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
		case transcript.LineBlank, transcript.LineLabel, transcript.LineRule:
			continue
		}
		// plain text: newlines flattened, **bold** markers and an aside's title dropped
		text := l.Text
		if l.Glyph == transcript.GlyphAside {
			text = strings.TrimPrefix(text, transcript.AsideTitle)
		}
		s := strings.TrimSpace(strings.NewReplacer("\n", " ", "**", "").Replace(text))
		if s == "" {
			continue
		}
		if l.Kind == transcript.LineTool && l.Suffix != "" {
			s += " " + l.Suffix
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// jobRows is the async dialog's running jobs, one row per job: the
// owning agent in bold with its role, the job's label, and
// dim meta (progress, elapsed): "coder (coder)  go test  42 lines · 1m15s".
func jobRows(jobs []protocol.JobInfo, owner, ownerRole string, now time.Time, width int) []string {
	var rows []string
	for _, mo := range jobs {
		who := owner
		if ownerRole != "" {
			who = fmt.Sprintf("%s (%s)", owner, ownerRole)
		}
		label := mo.Label
		if who != "" {
			label = theme.StyleBold.Render(who) + "  " + mo.Label
		}
		var meta []string
		if mo.Progress != "" {
			meta = append(meta, mo.Progress)
		}
		if t, err := time.Parse(time.RFC3339, mo.Started); err == nil && !t.IsZero() {
			meta = append(meta, format.Elapsed(now.Sub(t)))
		}
		row := "  " + label
		if len(meta) > 0 {
			row += "  " + theme.StyleDim.Render(strings.Join(meta, " · "))
		}
		rows = append(rows, ansi.Truncate(row, width, "…"))
	}
	return rows
}

// todoCount is "done/total" for a todo list, "0" when empty; done counts
// finished and cancelled items.
func todoCount(items []event.TodoItem) string {
	if len(items) == 0 {
		return "0"
	}
	done := 0
	for _, it := range items {
		if it.Status == event.TodoDone || it.Status == event.TodoCancelled {
			done++
		}
	}
	return fmt.Sprintf("%d/%d", done, len(items))
}

// todoRows renders a todo list, one row per item: a status glyph (○
// pending, ◐ in progress, ● done, × cancelled) and the text; the item in
// progress is bold, finished ones dim.
func todoRows(items []event.TodoItem, width int) []string {
	rows := make([]string, 0, len(items))
	for _, it := range items {
		var row string
		switch it.Status {
		case event.TodoInProgress:
			row = theme.StyleWarn.Render("◐") + " " + theme.StyleBold.Render(it.Text)
		case event.TodoDone:
			row = theme.StyleDim.Render("● " + it.Text)
		case event.TodoCancelled:
			row = theme.StyleDim.Render("× " + it.Text)
		default:
			row = theme.StyleDim.Render("○") + " " + it.Text
		}
		rows = append(rows, ansi.Truncate("  "+row, width, "…"))
	}
	return rows
}

// dirRows renders the channel's working directories: the path (home
// abbreviated) in bold, then where it came from (channel, human) in dim.
func dirRows(items []protocol.DirInfo, width int) []string {
	rows := make([]string, 0, len(items))
	for _, d := range items {
		source := d.Source
		if source == "channel" {
			source = "default"
		}
		row := "  " + theme.StyleBold.Render(format.ShortHome(d.Path)) + "  " + theme.StyleDim.Render(source)
		rows = append(rows, ansi.Truncate(row, width, "…"))
	}
	return rows
}

// mcpCount is "connected/listed" for an agent's MCP servers, "0" when its
// role lists none.
func mcpCount(items []protocol.MCPInfo) string {
	if len(items) == 0 {
		return "0"
	}
	up := 0
	for _, it := range items {
		if it.State == protocol.MCPConnected {
			up++
		}
	}
	return fmt.Sprintf("%d/%d", up, len(items))
}

// mcpRows renders an agent's MCP servers, one row each: a state glyph (●
// connected, ◐ starting, ○ pending, × failed or stopped), the name in bold,
// then the tool count and uptime, or the error. A server in open shows its
// tools as indented rows under it. owners names the server behind each row
// ("" for a tool row), so enter can toggle the right one.
func mcpRows(items []protocol.MCPInfo, open map[string]bool, now time.Time, width int) (rows, owners []string) {
	for _, it := range items {
		var glyph string
		switch it.State {
		case protocol.MCPConnected:
			glyph = theme.StyleOvGood.Render("●")
		case protocol.MCPStarting:
			glyph = theme.StyleWarn.Render("◐")
		case protocol.MCPFailed, protocol.MCPStopped:
			glyph = theme.StyleError.Render("×")
		default:
			glyph = theme.StyleDim.Render("○")
		}
		row := glyph + " " + theme.StyleBold.Render(it.Name)
		var meta []string
		switch it.State {
		case protocol.MCPConnected:
			meta = append(meta, fmt.Sprintf("%d tools", len(it.Tools)))
			if t, err := time.Parse(time.RFC3339, it.Started); err == nil && !t.IsZero() {
				meta = append(meta, format.Elapsed(now.Sub(t)))
			}
		case protocol.MCPFailed, protocol.MCPStopped:
			if it.Error != "" {
				meta = append(meta, it.Error)
			}
		case protocol.MCPPending:
			meta = append(meta, "starts at the next turn")
		default:
			meta = append(meta, string(it.State))
		}
		if len(meta) > 0 {
			row += "  " + theme.StyleDim.Render(strings.Join(meta, " · "))
		}
		rows = append(rows, ansi.Truncate("  "+row, width, "…"))
		owners = append(owners, it.Name)
		if open[it.Name] {
			for _, tool := range it.Tools {
				rows = append(rows, ansi.Truncate("      "+theme.StyleDim.Render(strings.TrimPrefix(tool, "mcp__"+it.Name+"__")), width, "…"))
				owners = append(owners, "")
			}
		}
	}
	return rows, owners
}

// paletteViewFor is the "/" command dropdown when the input is typing a
// command name and has focus.
func (m Model) paletteViewFor(width int) string {
	if m.focus != focusInput {
		return ""
	}
	if mm := m.mentionMatches(); len(mm) > 0 {
		return mentionView(mm, m.palIdx, width)
	}
	pm := m.paletteMatches(m.input.Value())
	if len(pm) == 0 {
		return ""
	}
	return paletteView(pm, m.palIdx, width)
}
