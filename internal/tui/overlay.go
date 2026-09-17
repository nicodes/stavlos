package tui

// overlay is a modal box drawn over the transcript: in list mode a title
// (with "esc: close" at the right), a search field, a blank line and a
// filtered, scrollable list; in login mode the device-code sign-in
// instructions. While one is open it owns
// every key; the main input is blurred. It is generic; what a selection
// means is decided by Model via overlayKind.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/tui/dialog"
	"github.com/nicodes/stavlos/internal/tui/format"
	"github.com/nicodes/stavlos/internal/tui/theme"
)

const (
	overlayMaxRows = 10
)

type overlayMode int

const (
	overlayList  overlayMode = iota // search field + list
	overlayLogin                    // device-code sign-in (no field, no list)
	overlayInput                    // a text field only (naming a channel): enter submits, space types
)

// overlayKind tells Model what a submit means.
type overlayKind int

const (
	ovProviders     overlayKind = iota // pick a provider
	ovMethods                          // pick a login method (providers with more than one)
	ovModels                           // pick a model
	ovRoles                            // pick a role (preset) for the selected agent
	ovVariants                         // pick a model variant (reasoning effort) for the selected agent
	ovChannels                         // pick a channel of this directory to resume
	ovMode                             // pick the channel's permission mode (ask | auto | yolo)
	ovNewChannel                       // name a new channel of this directory
	ovNewChannelDir                    // choose its default directory
	ovDiscord                          // daemon-wide Discord status and controls
)

// loginState is what the login mode shows. Before url is set the login is
// still being started; err replaces the waiting line once set.
type loginState struct {
	url, code, instructions string
	browser                 bool // browser method: no code, the callback lands on this machine
	err                     string
}

type overlayItem struct {
	id, label, hint string
	sub             string // dim text after the label (model id)
	dim             bool   // whole row dim
	good            bool   // hint in green (connected)
}

type overlay struct {
	kind  overlayKind
	mode  overlayMode
	title string
	empty string // shown in place of the list while it has no items ("no channels here yet")
	bad   bool   // empty is an error (red)

	input   textinput.Model
	newName string // new-channel directory step
	items   []overlayItem
	query   string        // last filter applied
	shown   []overlayItem // items matching query
	cursor  int           // index into shown
	offset  int           // first visible row

	login loginState // login mode only

	hints []dialog.Hint // the footer's key hints, set by the model before each render
}

const (
	loginWaitingText = "waiting for you to finish signing in…"
	loginStartText   = "starting sign-in…"
	loginKeysWaiting = "o: open in browser"
	loginKeysError   = "enter: retry"
)

func newOverlay(kind overlayKind, mode overlayMode, title string) *overlay {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.Focus()
	if mode == overlayList {
		ti.Placeholder = "search"
	}
	return &overlay{kind: kind, mode: mode, title: title, input: ti}
}

// setEmpty sets what an empty list shows instead of "(nothing to list)";
// bad renders it red.
func (o *overlay) setEmpty(text string, bad bool) { o.empty, o.bad = text, bad }

// setItems replaces the list and re-applies the current query.
func (o *overlay) setItems(items []overlayItem) {
	o.items = items
	o.shown = filterItems(items, o.input.Value())
	o.query = o.input.Value()
	o.cursor, o.offset = 0, 0
}

// switchLogin turns the overlay into the "Sign in to <name>" screen in its
// starting state (no URL yet).
func (o *overlay) switchLogin(name string) {
	o.kind, o.mode, o.title = ovProviders, overlayLogin, "Sign in to "+name
	o.setEmpty("", false)
	o.items, o.shown = nil, nil
	o.cursor, o.offset = 0, 0
	o.input.Reset()
	o.login = loginState{}
}

// setLogin fills in the device-code details and clears any error.
func (o *overlay) setLogin(url, code, instructions string) {
	o.login = loginState{url: url, code: code, instructions: instructions, browser: code == ""}
}

// setLoginError replaces the waiting line with err.
func (o *overlay) setLoginError(err string) { o.login.err = textsafe.Clean(err) }

// filterItems keeps items whose id, label or metadata contains query (case-insensitive),
// preserving the given order. An empty query keeps everything.
func filterItems(items []overlayItem, query string) []overlayItem {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return items
	}
	var out []overlayItem
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.id), q) || strings.Contains(strings.ToLower(it.label), q) || strings.Contains(strings.ToLower(it.hint), q) {
			out = append(out, it)
		}
	}
	return out
}

func (o *overlay) selected() *overlayItem {
	if o.cursor < 0 || o.cursor >= len(o.shown) {
		return nil
	}
	return &o.shown[o.cursor]
}

func (o *overlay) move(delta int) {
	n := len(o.shown)
	if n == 0 {
		o.cursor = 0
		return
	}
	o.cursor += delta
	if o.cursor < 0 {
		o.cursor = 0
	}
	if o.cursor >= n {
		o.cursor = n - 1
	}
	o.clampOffset()
}

func (o *overlay) clampOffset() {
	o.offset, _ = listWindow(o.cursor, o.offset, len(o.shown), overlayMaxRows)
}

// update handles a key that is not a navigation/submit key: it edits the
// field and, in list mode, re-filters.
func (o *overlay) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	o.input, cmd = o.input.Update(msg)
	if o.mode == overlayList && o.input.Value() != o.query {
		o.query = o.input.Value()
		o.shown = filterItems(o.items, o.query)
		o.cursor, o.offset = 0, 0
	}
	return cmd
}

// handleNav applies cursor keys; reports whether the key was consumed.
func (o *overlay) handleNav(msg tea.KeyMsg) bool {
	if o.mode != overlayList {
		return false
	}
	switch {
	case key.Matches(msg, keys.OvUp):
		o.move(-1)
	case key.Matches(msg, keys.OvDown):
		o.move(1)
	case key.Matches(msg, keys.PageUp):
		o.move(-overlayMaxRows)
	case key.Matches(msg, keys.PageDown):
		o.move(overlayMaxRows)
	default:
		return false
	}
	return true
}

// --- rendering ---

// view renders the box dialog.Width(bodyWidth) wide. spinner is the
// current spinner glyph (login mode).
func (o *overlay) view(bodyWidth int, spinner string) string {
	w := dialog.Width(bodyWidth)
	inner := w - 4 // border + padding

	lines := []string{dialog.Title(o.title, inner)}
	switch o.mode {
	case overlayLogin:
		lines = append(lines, o.loginLines(inner, spinner)...)
	case overlayInput:
		o.input.Width = inner - len([]rune(o.input.Prompt)) - 1
		lines = append(lines, o.input.View())
	default:
		o.input.Width = inner - len([]rune(o.input.Prompt)) - 1
		lines = append(lines, o.input.View(), "")
		lines = append(lines, o.listLines(inner)...)
	}
	if f := dialog.HintLines(o.hints, inner); len(f) > 0 {
		lines = append(lines, "")
		lines = append(lines, f...)
	}
	// Width covers padding but not the border: inner content + 2 padding + 2 border = w.
	return dialog.Box(inner, lines)
}

// loginLines renders the device-code instructions: URL, spaced code, the
// daemon's instructions, then the waiting spinner or the error.
func (o *overlay) loginLines(inner int, spinner string) []string {
	l := o.login
	if spinner == "" {
		spinner = "…"
	}
	var out []string
	if l.url == "" && l.err == "" {
		return append(out,
			theme.StyleRunning.Render(spinner)+" "+loginStartText,
			theme.StyleDim.Render("esc cancel"),
		)
	}
	if l.url != "" {
		if l.browser {
			out = append(out, "Your browser should open to sign in. If it does not, open:")
		} else {
			out = append(out, "Open this URL on any device:")
		}
		for _, u := range strings.Split(ansi.Hardwrap(l.url, inner-2, true), "\n") {
			out = append(out, "  "+theme.StyleOvURL.Render(u))
		}
		if !l.browser {
			out = append(out, "and enter the code:")
			out = append(out, "  "+theme.StyleOvCode.Render(format.Trunc(spacedCode(l.code), inner-2)))
		}
		if ins := strings.TrimSpace(l.instructions); ins != "" {
			for _, s := range strings.Split(ansi.Wrap(ins, inner, ""), "\n") {
				out = append(out, theme.StyleDim.Render(s))
			}
		}
	}
	if l.err != "" {
		for _, s := range strings.Split(ansi.Wrap(l.err, inner, ""), "\n") {
			out = append(out, theme.StyleStatusErr.Render(s))
		}
		return append(out, theme.StyleDim.Render(loginKeysError))
	}
	return append(out,
		theme.StyleRunning.Render(spinner)+" "+loginWaitingText,
		theme.StyleDim.Render(loginKeysWaiting),
	)
}

// spacedCode renders "ABCD-EFGH" as "A B C D - E F G H" so it reads large.
func spacedCode(code string) string {
	r := []rune(strings.TrimSpace(code))
	parts := make([]string, len(r))
	for i, c := range r {
		parts[i] = string(c)
	}
	return strings.Join(parts, " ")
}

func (o *overlay) listLines(inner int) []string {
	if len(o.shown) == 0 {
		if len(o.items) == 0 {
			if o.empty != "" {
				if o.bad {
					return []string{theme.StyleStatusErr.Render("  " + ansi.Truncate(o.empty, inner-2, "…"))}
				}
				return []string{theme.StyleDim.Render("  " + ansi.Truncate(o.empty, inner-2, "…"))}
			}
			return []string{theme.StyleDim.Render("  (nothing to list)")}
		}
		return []string{theme.StyleDim.Render("  no match")}
	}
	o.clampOffset()
	_, end := listWindow(o.cursor, o.offset, len(o.shown), overlayMaxRows)
	var out []string
	if o.offset > 0 {
		out = append(out, theme.StyleDim.Render(fmt.Sprintf("  ↑ %d more", o.offset)))
	}
	for i := o.offset; i < end; i++ {
		out = append(out, renderItem(o.shown[i], i == o.cursor, inner))
	}
	if rest := len(o.shown) - end; rest > 0 {
		out = append(out, theme.StyleDim.Render(fmt.Sprintf("  ↓ %d more", rest)))
	}
	return out
}

// itemAt maps a screen position to the list row drawn there, using the
// same geometry as view and composite: the box is centred in the body,
// its content starts inside the border after the title, the filter input
// and the blank line, then an "↑ more" marker when scrolled.
func (o *overlay) itemAt(x, y, bodyWidth, bodyHeight int, spinner string) (int, bool) {
	if o.mode != overlayList || len(o.shown) == 0 {
		return 0, false
	}
	box := o.view(bodyWidth, spinner)
	boxLines := strings.Split(box, "\n")
	bw := lipgloss.Width(box)
	x0 := (bodyWidth - bw) / 2
	if x0 < 0 {
		x0 = 0
	}
	y0 := (bodyHeight - len(boxLines)) / 2
	if y0 < 0 {
		y0 = 0
	}
	if x < x0 || x >= x0+bw {
		return 0, false
	}
	row := y - y0 - 1 // top border
	row -= 3          // title, filter input, blank line
	if o.offset > 0 {
		if row == 0 {
			return 0, false // the "↑ more" marker
		}
		row--
	}
	if row < 0 {
		return 0, false
	}
	_, end := listWindow(o.cursor, o.offset, len(o.shown), overlayMaxRows)
	idx := o.offset + row
	if idx >= end {
		return 0, false
	}
	return idx, true
}

// renderItem lays out "▸ label  sub" on the left and the hint on the right,
// truncating the left part when both do not fit.
func renderItem(it overlayItem, cur bool, width int) string {
	marker := cursorMarker(cur)
	avail := width - 2
	label, sub, hint := it.label, it.sub, it.hint
	if sub != "" {
		sub = "  " + sub
	}
	hintW := 0
	if hint != "" {
		hintW = len([]rune(hint)) + 2
		if hintW > avail/2 {
			hint = format.Trunc(hint, avail/2-2)
			hintW = len([]rune(hint)) + 2
		}
	}
	// format.Trunc appends an ellipsis, so budgets leave one cell for it.
	leftMax := avail - hintW
	if n := len([]rune(label)); n > leftMax {
		label, sub = format.Trunc(label, max(leftMax-1, 0)), ""
	} else if n+len([]rune(sub)) > leftMax {
		sub = format.Trunc(sub, max(leftMax-n-1, 0))
	}
	leftW := len([]rune(label)) + len([]rune(sub))
	pad := avail - leftW - hintW + 2
	if pad < 2 {
		pad = 2
	}

	var b strings.Builder
	b.WriteString(marker)
	switch {
	case it.dim:
		b.WriteString(theme.StyleDim.Render(label + sub))
	case cur:
		b.WriteString(theme.StyleOvCur.Render(label) + theme.StyleDim.Render(sub))
	default:
		b.WriteString(label + theme.StyleDim.Render(sub))
	}
	if hint != "" {
		b.WriteString(strings.Repeat(" ", pad))
		if it.good {
			b.WriteString(theme.StyleOvGood.Render(hint))
		} else {
			b.WriteString(theme.StyleDim.Render(hint))
		}
	}
	return b.String()
}

// --- item builders (pure; tested) ---

// providerItems turns the daemon's provider list into overlay rows: the
// name, and either the login method label or the connected account.
func providerItems(ps []protocol.ProviderInfo) []overlayItem {
	out := make([]overlayItem, 0, len(ps))
	for _, p := range ps {
		it := overlayItem{id: p.ID, label: p.Name, hint: "not signed in"}
		if p.Label != "" {
			it.hint = p.Label + " · not signed in"
		}
		if it.label == "" {
			it.label = p.ID
		}
		if p.Connected {
			it.hint, it.good = connectedHint(p), true
		}
		out = append(out, it)
	}
	return out
}

// connectedHint is "connected · <account>" (or just "connected").
func connectedHint(p protocol.ProviderInfo) string {
	if p.Account == "" {
		return "connected"
	}
	return "connected · " + p.Account
}

// modelItems turns the model catalog into overlay rows.
func modelItems(ms []protocol.ModelInfo) []overlayItem {
	out := make([]overlayItem, 0, len(ms))
	for _, m := range ms {
		label := m.Name
		if label == "" {
			label = m.ID
		}
		out = append(out, overlayItem{id: m.ID, label: label, sub: m.ID, hint: modelHint(m)})
	}
	return out
}

// modelHint builds "ctx 200k · $3/$15 per 1M", omitting zero parts.
func modelHint(m protocol.ModelInfo) string {
	var parts []string
	if m.Context > 0 {
		parts = append(parts, "ctx "+format.Tokens(m.Context))
	}
	if m.InputPrice > 0 || m.OutputPrice > 0 {
		parts = append(parts, "$"+fmtPrice(m.InputPrice)+"/$"+fmtPrice(m.OutputPrice)+" per 1M")
	}
	return strings.Join(parts, " · ")
}

func fmtPrice(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
