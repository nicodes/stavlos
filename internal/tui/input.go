package tui

import (
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/tui/theme"
)

// inputCap is how tall the input may grow on this window: half its height,
// never under inputMaxLines nor over inputHardMax.
func (m Model) inputCap() int {
	c := m.height / 2
	if c < inputMaxLines {
		c = inputMaxLines
	}
	if c > inputHardMax {
		c = inputHardMax
	}
	return c
}

// newInputArea builds the message input: a textarea that starts one line
// tall and grows with the text (see inputRows), no line numbers, no cursor
// line highlight, enter sends and ctrl+j (or alt+enter) breaks a line.
func newInputArea() textarea.Model {
	ta := textarea.New()
	ta.Prompt = "› "
	// One chevron: the first line carries ›, continuation lines are
	// indented under it.
	ta.SetPromptFunc(2, func(lineIdx int) string {
		if lineIdx == 0 {
			return "› "
		}
		return "  "
	})
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// The textarea itself is always inputMaxLines tall so it never scrolls
	// until a message really is that long; the view shows only the rows
	// the text needs (see inputRows).
	ta.MaxHeight = inputHardMax
	ta.SetHeight(inputMaxLines) // layout raises it to inputCap for the window
	ta.EndOfBufferCharacter = ' '
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "alt+enter"))
	// A subtle background makes the input stand out from the chat above
	// and the meta row below; every part of the block shares it.
	bg := lipgloss.NewStyle().Background(theme.ColInputBg)
	ta.FocusedStyle.Base, ta.BlurredStyle.Base = bg, bg
	ta.FocusedStyle.CursorLine, ta.BlurredStyle.CursorLine = bg, bg
	ta.FocusedStyle.EndOfBuffer, ta.BlurredStyle.EndOfBuffer = bg, bg
	ta.FocusedStyle.Text, ta.BlurredStyle.Text = bg, bg
	ta.FocusedStyle.Placeholder, ta.BlurredStyle.Placeholder = theme.StyleDim.Background(theme.ColInputBg), theme.StyleDim.Background(theme.ColInputBg)
	// The prompt chevron carries the focus colour (there is no box border).
	ta.FocusedStyle.Prompt, ta.BlurredStyle.Prompt = theme.StyleBorderUser.Background(theme.ColInputBg), theme.StyleBorderMuted.Background(theme.ColInputBg)
	return ta
}

// textareaWrap mirrors the soft-wrap the bubbles textarea uses to draw a
// logical line (word wrap that keeps trailing spaces on the row and spills a
// row that exactly fills the width), so inputRows counts the rows the
// textarea will actually draw. A generic word wrap counts fewer rows and
// leaves the textarea scrolling inside a too-short box.
func textareaWrap(runes []rune, width int) [][]rune {
	var (
		lines  = [][]rune{{}}
		word   = []rune{}
		row    int
		spaces int
	)
	rw := func(rs []rune) int { return ansi.StringWidth(string(rs)) }
	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}
		if spaces > 0 {
			if rw(lines[row])+rw(word)+spaces > width {
				row++
				lines = append(lines, []rune{})
			}
			lines[row] = append(lines[row], word...)
			lines[row] = append(lines[row], []rune(strings.Repeat(" ", spaces))...)
			spaces = 0
			word = nil
		} else {
			last := ansi.StringWidth(string(word[len(word)-1]))
			if rw(word)+last > width {
				if len(lines[row]) > 0 {
					row++
					lines = append(lines, []rune{})
				}
				lines[row] = append(lines[row], word...)
				word = nil
			}
		}
	}
	if rw(lines[row])+rw(word)+spaces >= width {
		lines = append(lines, []rune{})
		lines[row+1] = append(lines[row+1], word...)
		spaces++
		lines[row+1] = append(lines[row+1], []rune(strings.Repeat(" ", spaces))...)
	} else {
		lines[row] = append(lines[row], word...)
		spaces++
		lines[row] = append(lines[row], []rune(strings.Repeat(" ", spaces))...)
	}
	return lines
}

// normalizePaste turns the carriage returns a terminal sends for pasted
// line endings (CR or CRLF) into the line feeds the textarea splits lines
// on; left alone they render into the line and overwrite it.
func normalizePaste(msg tea.KeyMsg) tea.KeyMsg {
	if msg.Type != tea.KeyRunes || !strings.ContainsRune(string(msg.Runes), '\r') {
		return msg
	}
	text := strings.ReplaceAll(string(msg.Runes), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	msg.Runes = []rune(text)
	return msg
}

// modeTagCols is the columns the mode tag takes before the input's ›: the
// widest tag ("AUTO", "YOLO") and a space.
const modeTagCols = 5

// inputRows is how many rows the message needs: the textarea's own wrap
// of every logical line, at least one, at most inputMaxLines (past that the
// textarea scrolls to keep the cursor in view). The textarea is always
// inputMaxLines tall; inputView shows this many of its rows.
func (m Model) inputRows() int {
	w := m.input.Width()
	if w < 1 {
		w = 1
	}
	rows := 0
	for _, line := range strings.Split(m.input.Value(), "\n") {
		rows += len(textareaWrap([]rune(line), w))
	}
	if rows < 1 {
		rows = 1
	}
	if c := m.inputCap(); rows > c {
		rows = c
	}
	return rows
}

// inputView is the textarea trimmed to the rows the message needs.
func (m Model) inputView() string {
	lines := strings.Split(m.input.View(), "\n")
	if n := m.inputRows(); len(lines) > n {
		lines = lines[:n]
	}
	// The background spans the whole input width, not just the text: pad
	// every row out to the box in the same colour. The mode tag leads the
	// first row, right-aligned before the ›; later rows keep its columns.
	bg := lipgloss.NewStyle().Background(theme.ColInputBg)
	width := m.boxWidth()
	tag := m.modeTag()
	for i, l := range lines {
		lead := bg.Render(strings.Repeat(" ", modeTagCols))
		if i == 0 {
			lead = bg.Render(strings.Repeat(" ", modeTagCols-1-len(tag))) + modeTagStyle(tag).Background(theme.ColInputBg).Render(tag) + bg.Render(" ")
		}
		l = lead + l
		if pad := width - ansi.StringWidth(l); pad > 0 {
			l += bg.Render(strings.Repeat(" ", pad))
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// seedHistory gives a fresh channel's ↑/↓ history the first prompts of
// this directory's earlier channels (newest first under ↑), so the start
// screen recalls what was asked last time. Only when nothing has been typed
// here yet; the current and untitled channels are skipped.
func (m *Model) seedHistory(channels []protocol.ChannelInfo) {
	if len(m.history) > 0 || !m.isHome() {
		return
	}
	seen := map[string]bool{}
	var titles []string
	for _, s := range channels { // newest first
		if s.Title == "" || s.ID == m.channelID || seen[s.Title] {
			continue
		}
		seen[s.Title] = true
		titles = append(titles, s.Title)
	}
	for i := len(titles) - 1; i >= 0; i-- { // history is oldest → newest
		m.history = append(m.history, titles[i])
	}
	m.histIdx = len(m.history)
}

// inputKey handles keys while the input has focus: paging the chat, ↑/↓
// through the "/" palette or the prompt history, esc to clear (or cancel a
// busy turn), tab to complete a command, enter to run or send; anything
// else edits the text.
func (m *Model) inputKey(msg tea.KeyMsg) tea.Cmd {
	if m.mentionKey(msg) {
		return nil
	}
	switch {
	case key.Matches(msg, keys.PageUp):
		m.vp.PageUp()
		m.follow = m.vp.AtBottom()
		return nil
	case key.Matches(msg, keys.PageDown):
		m.vp.PageDown()
		m.follow = m.vp.AtBottom()
		return nil
	case key.Matches(msg, keys.Top):
		m.vp.GotoTop()
		m.follow = m.vp.AtBottom()
		return nil
	case key.Matches(msg, keys.Bottom):
		m.vp.GotoBottom()
		m.follow = true
		return nil
	case key.Matches(msg, keys.SelUp):
		if pm := paletteMatches(m.input.Value()); len(pm) > 0 {
			m.palIdx = (m.palIdx - 1 + len(pm)) % len(pm)
			return nil
		}
		// ↑ moves the cursor up a row (a logical line, or a wrapped row of
		// one) while there is a row above; on the top row it walks history.
		if m.input.Line() > 0 || m.input.LineInfo().RowOffset > 0 {
			break
		}
		m.historyMove(-1)
		return nil
	case key.Matches(msg, keys.SelDown):
		if pm := paletteMatches(m.input.Value()); len(pm) > 0 {
			m.palIdx = (m.palIdx + 1) % len(pm)
			return nil
		}
		// ↓ moves down a row while there is one below; on the bottom row it
		// walks history forward.
		if li := m.input.LineInfo(); m.input.Line() < m.input.LineCount()-1 || li.RowOffset < li.Height-1 {
			break
		}
		m.historyMove(1)
		return nil
	case key.Matches(msg, keys.Clear):
		if m.input.Value() != "" {
			m.input.Reset()
			m.palIdx = 0
			m.cancelArmed = time.Time{}
			return nil
		}
		return m.escCancel()
	case msg.Type == tea.KeyTab:
		// only reached when the palette is open (tab otherwise cycles focus)
		if pm := paletteMatches(m.input.Value()); len(pm) > 0 {
			m.completeCommand(pm[m.clampPal(len(pm))])
		}
		return nil
	case key.Matches(msg, keys.Submit):
		if pm := paletteMatches(m.input.Value()); len(pm) > 0 {
			c := pm[m.clampPal(len(pm))]
			typed := strings.ToLower(m.input.Value())
			if c.Direct && (typed == c.Name || typed != c.Name && c.Args == "" || isAlias(c, typed)) {
				m.input.Reset()
				m.palIdx = 0
				m.pushHistory(c.Name)
				return m.command(c.Name)
			}
			if typed != c.Name && !isAlias(c, typed) {
				m.completeCommand(c) // enter on a partial name completes it first
				return nil
			}
		}
		return m.submit()
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(normalizePaste(msg))
	if !paletteActive(m.input.Value()) {
		m.palIdx = 0
	} else if pm := paletteMatches(m.input.Value()); m.palIdx >= len(pm) {
		m.palIdx = 0
	}
	return cmd
}

// mentionKey handles the @name dropdown while it is open in the channel
// chat: ↑/↓ pick a name, tab or enter completes it. It reports whether it
// used the key.
func (m *Model) mentionKey(msg tea.KeyMsg) bool {
	mm := m.mentionMatches()
	if len(mm) == 0 {
		return false
	}
	switch {
	case key.Matches(msg, keys.SelUp):
		m.palIdx = (m.palIdx - 1 + len(mm)) % len(mm)
	case key.Matches(msg, keys.SelDown):
		m.palIdx = (m.palIdx + 1) % len(mm)
	case msg.Type == tea.KeyTab, key.Matches(msg, keys.Submit):
		m.completeMention(mm[m.clampPal(len(mm))])
	default:
		return false
	}
	return true
}

func (m *Model) clampPal(n int) int {
	if n == 0 {
		return 0
	}
	if m.palIdx < 0 || m.palIdx >= n {
		m.palIdx = 0
	}
	return m.palIdx
}

// completeCommand fills the input with the command name (plus a space when
// it takes arguments) so the user can keep typing.
func (m *Model) completeCommand(c Command) {
	v := c.Name
	if c.Args != "" {
		v += " "
	}
	m.input.SetValue(v)
	m.input.CursorEnd()
	m.palIdx = 0
}

func isAlias(c Command, typed string) bool {
	for _, a := range c.Aliases {
		if a == typed {
			return true
		}
	}
	return false
}

// submit handles Enter in the input: a /command or a prompt envelope.
func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	m.input.Reset()
	if text == "" {
		return nil
	}
	m.pushHistory(text)
	if strings.HasPrefix(text, "/") {
		return m.command(text)
	}
	if m.superChat {
		return postCmd(m.ctx, m.c, m.channelID, text) // the daemon delivers it by @mention
	}
	agent := m.selectedID()
	if agent == "" {
		return m.setStatus("no agent selected", true)
	}
	// Plain text is a steer: it reaches a busy agent at its next model-call
	// boundary, and simply starts a turn when the agent is idle. /queue is
	// the way to wait for the current turn to end.
	return sendCmd(m.ctx, m.c, agent, protocol.KindSteer, text, "")
}

// pushHistory records a submitted line; consecutive duplicates collapse.
func (m *Model) pushHistory(text string) {
	if n := len(m.history); n == 0 || m.history[n-1] != text {
		m.history = append(m.history, text)
	}
	m.histIdx = len(m.history)
	m.histDraft = ""
}

// historyMove walks the history: -1 older, +1 newer. Moving past the newest
// entry restores whatever was being typed before browsing began.
func (m *Model) historyMove(delta int) {
	n := len(m.history)
	if n == 0 {
		return
	}
	if m.histIdx == n {
		m.histDraft = m.input.Value()
	}
	idx := m.histIdx + delta
	if idx < 0 {
		idx = 0
	}
	if idx > n {
		idx = n
	}
	if idx == m.histIdx {
		return
	}
	m.histIdx = idx
	if idx == n {
		m.input.SetValue(m.histDraft)
	} else {
		m.input.SetValue(m.history[idx])
	}
	m.input.CursorEnd()
}

// placeholder is the input's hint: how the channel chat addresses agents,
// or a cycling suggestion in an agent's own chat.
func (m *Model) placeholder() string {
	if m.superChat {
		return "Message the channel · @name addresses an agent, no mention goes to the root"
	}
	return placeholders[placeholderIndex(time.Now())]
}
