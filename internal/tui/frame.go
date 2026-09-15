package tui

import "strings"

// frame is where the chat and the footer sit on screen, and the footer parts
// that decide it. It is one computation over the model: layout sizes the
// viewport from it, View draws it and the mouse maps rows through it, so
// the three cannot disagree about where the input, the tab strip or the
// meta row is.
type frame struct {
	keybar     string // the divider and the key legend, "" when hidden
	keybarRows int
	palette    string // the command palette over the input, "" when closed
	sections   string // the tab strip, "" when there is none
	meta       bool   // the meta row shows
	bodyH      int    // the chat viewport's height
	input      int    // screen row of the input's first line
	strip      int    // screen row of the tab strip's first line
	stripRows  int    // lines the tab strip takes
	metaRow    int    // screen row of the meta row, -1 when hidden
}

// computeFrame lays the screen out from top to bottom: the chat, the rule
// (the status and usage sit on it), the palette, the input, a blank line
// when anything sits under the input, the tab strip, the meta row, then
// the key bar.
func (m Model) computeFrame() frame {
	f := frame{palette: m.paletteViewFor(m.width), sections: m.sectionsView(m.width), meta: m.metaShown()}
	f.keybar, f.keybarRows = m.keyBarView()
	f.stripRows = lineCount(f.sections)
	under := 0
	if f.stripRows > 0 || f.meta {
		under = 1
	}
	metaRows := 0
	if f.meta {
		metaRows = 1
	}
	f.bodyH = max(m.height-f.keybarRows-1-lineCount(f.palette)-m.inputRows()-under-f.stripRows-metaRows, 1)
	f.input = f.bodyH + 1 + lineCount(f.palette)
	f.strip = f.input + m.inputRows() + under
	f.metaRow = -1
	if f.meta {
		f.metaRow = f.strip + f.stripRows
	}
	return f
}

// lineCount is how many screen lines s takes (0 for none).
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
