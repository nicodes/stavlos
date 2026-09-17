package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/tui/dialog"
	"github.com/nicodes/stavlos/internal/tui/theme"
)

type configGeometry struct{ width, inner, left, right, rows, x, y int }

func (e *configEditor) geometry(width, height int) configGeometry {
	w := max(20, min(120, width-4))
	inner := w - 4
	left := min(30, max(8, inner/3))
	rows := max(3, height-15)
	h := rows + 12
	return configGeometry{w, inner, left, max(5, inner-left-3), rows, max(0, (width-w)/2), max(0, (height-h)/2)}
}

func (e *configEditor) view(width, height int) string {
	g := e.geometry(width, height)
	e.area.SetWidth(g.right)
	e.area.SetHeight(g.rows)
	e.input.Width = max(1, g.right-3)
	lines := []string{dialog.Title(e.title(), g.inner), theme.StyleDim.Render(ansi.Truncate(e.tree.Root, g.inner, "…"))}
	var sections []string
	for i, name := range configSections {
		if i == e.section {
			sections = append(sections, theme.StyleAccent.Bold(true).Render(name))
		} else {
			sections = append(sections, theme.StyleDim.Render(name))
		}
	}
	lines = append(lines, ansi.Truncate(strings.Join(sections, " · "), g.inner, "…"), "")
	left := make([]string, g.rows)
	files := e.entries()
	start, end := listWindow(e.fileSel, 0, len(files), g.rows)
	for i := start; i < end; i++ {
		f := files[i]
		label := f.Path
		if f.Directory {
			label += "/"
		}
		st := theme.StyleDim
		if i == e.fileSel {
			st = theme.StyleAccent
		}
		left[i-start] = st.Render(cursorMarker(e.pane == 0 && i == e.fileSel) + textsafe.Clean(label))
	}
	right := e.rightLines(g)
	for i := 0; i < g.rows; i++ {
		l := ansi.Truncate(left[i], g.left, "…")
		l += strings.Repeat(" ", max(0, g.left-ansi.StringWidth(l)))
		r := ""
		if i < len(right) {
			r = ansi.Truncate(right[i], g.right, "…")
		}
		lines = append(lines, l+theme.StyleDim.Render(" │ ")+r)
	}
	status := e.status
	if e.busy {
		status = "Loading / saving configuration…"
	}
	lines = append(lines, "")
	statusLines := strings.Split(ansi.Wrap(textsafe.Clean(status), g.inner, ""), "\n")
	for i := 0; i < 3; i++ {
		line := ""
		if i < len(statusLines) {
			line = statusLines[i]
		}
		lines = append(lines, theme.StyleDim.Render(line))
	}
	lines = append(lines, theme.StyleDim.Render(ansi.Truncate("Tab panes · ←/→ section · Enter edit/toggle · F4 raw/fields · Ctrl+S save", g.inner, "…")))
	lines = append(lines, theme.StyleDim.Render(ansi.Truncate("Files: Ctrl+N new · F2 rename · Ctrl+D delete · Ctrl+L reload · Esc close", g.inner, "…")))
	return dialog.Box(g.inner, lines)
}

func (e *configEditor) rightLines(g configGeometry) []string {
	if e.mode == "discard" || e.mode == "delete" {
		return []string{theme.StyleWarn.Render(e.status)}
	}
	if e.mode == "new" || e.mode == "rename" {
		return []string{theme.StyleBold.Render(e.mode + " file or directory"), e.input.View(), "Enter confirms · Esc cancels"}
	}
	if e.doc == nil {
		return []string{"Choose a file to edit.", "Use Ctrl+N to create one."}
	}
	if e.mode == "raw" || e.mode == "body" || e.mode == "json" {
		return strings.Split(e.area.View(), "\n")
	}
	if e.mode == "field" {
		f := e.doc.Fields[e.fieldSel]
		return []string{theme.StyleBold.Render(strings.Join(f.Path, ".")), e.input.View(), "Enter saves · Esc cancels"}
	}
	var out []string
	start, end := listWindow(e.fieldSel, 0, len(e.doc.Fields), g.rows)
	for i := start; i < end; i++ {
		f := e.doc.Fields[i]
		label := strings.Join(f.Path, ".")
		if f.Kind == "body" {
			label = "Prompt body"
		}
		value := configFieldText(f)
		if f.Kind == "boolean" {
			if value == "true" {
				value = "☑ true"
			} else if value == "false" {
				value = "☐ false"
			}
		}
		row := fmt.Sprintf("%s%s  %s", cursorMarker(e.pane == 1 && i == e.fieldSel), label, strings.Join(strings.Fields(value), " "))
		if i == e.fieldSel && e.pane == 1 {
			out = append(out, theme.StyleAccent.Render(row))
		} else {
			out = append(out, row)
		}
	}
	return out
}

// The modal owns mouse input; background chat/sidebar selections cannot move.
func (m *Model) configEditorMouse(x, y int) tea.Cmd {
	e := m.cfgEditor
	if e == nil || e.busy || e.mode == "field" || e.mode == "new" || e.mode == "rename" || e.mode == "delete" || e.mode == "discard" {
		return nil
	}
	g := e.geometry(m.width, m.bodyHeight())
	x, y = x-g.x-2, y-g.y-1
	if x < 0 || x >= g.inner {
		return nil
	}
	if y == 2 && !e.dirty() {
		at := 0
		for i, name := range configSections {
			if x >= at && x < at+len(name) {
				e.section, e.fileSel, e.pane = i, 0, 0
				break
			}
			at += len(name) + 3
		}
		return nil
	}
	if y < 4 || y >= 4+g.rows {
		return nil
	}
	row := y - 4
	if x < g.left {
		if e.dirty() {
			e.status = "Save with Ctrl+S before changing files."
			return nil
		}
		start, _ := listWindow(e.fileSel, 0, len(e.entries()), g.rows)
		if i := start + row; i < len(e.entries()) {
			e.fileSel, e.pane = i, 0
			e.area.Blur()
			return m.configNavigationKey(tea.KeyMsg{Type: tea.KeyEnter})
		}
		return nil
	}
	e.pane = 1
	if e.mode == "raw" || e.mode == "body" || e.mode == "json" {
		return e.area.Focus()
	}
	if e.doc != nil {
		start, _ := listWindow(e.fieldSel, 0, len(e.doc.Fields), g.rows)
		if i := start + row; i < len(e.doc.Fields) {
			e.fieldSel = i
			return m.openConfigField()
		}
	}
	return nil
}
