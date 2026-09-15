package tui

import "strings"

// frame is where the chat and the footer sit on screen, and the footer parts
// that decide it. It is one computation over the model: layout sizes the
// viewport from it, View draws it and the mouse maps rows through it, so
// the three cannot disagree about where the divider, the input or the tab
// strip is.
type frame struct {
	keybar     string // the divider and the key legend, "" when hidden
	keybarRows int
	palette    string // the command palette over the input, "" when closed
	sections   string // the tab strip, "" when there is none
	bodyH      int    // the chat viewport's height
	rule       int    // screen row of the divider (the agent, its tabs and the usage sit on it)
	input      int    // screen row of the input's first line
	strip      int    // screen row of the tab strip's first line
	stripRows  int    // lines the tab strip takes
}

// computeFrame lays the screen out from top to bottom: the chat, the
// divider (the agent, its tabs and the usage sit on it), the palette, the
// input, a blank line and the tab strip when there is one, then the key bar.
func (m Model) computeFrame() frame {
	f := frame{palette: m.paletteViewFor(m.width), sections: m.sectionsView(m.width)}
	f.keybar, f.keybarRows = m.keyBarView()
	f.stripRows = lineCount(f.sections)
	under := 0
	if f.stripRows > 0 {
		under = 1
	}
	f.bodyH = max(m.height-f.keybarRows-1-lineCount(f.palette)-m.inputRows()-under-f.stripRows, 1)
	f.rule = f.bodyH
	f.input = f.bodyH + 1 + lineCount(f.palette)
	f.strip = f.input + m.inputRows() + under
	return f
}

// lineCount is how many screen lines s takes (0 for none).
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
