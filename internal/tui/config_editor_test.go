package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/config"
	"github.com/nicodes/stavlos/internal/protocol"
)

func readyConfigEditor(t *testing.T) Model {
	t.Helper()
	m := channelModel()
	m.width, m.height = 120, 40
	m.openConfigEditor(false)
	e := m.cfgEditor
	e.tree = protocol.ConfigTree{Root: "/repo/.stavlos", Revision: "rev", Files: []protocol.ConfigEntry{{Path: "stavlos.json"}, {Path: "commands/test.md"}}}
	content := "{\n  \"reminders\": false\n}\n"
	doc := protocol.ConfigDocument{Path: "stavlos.json", Content: content, Exists: true, Fields: config.EditorFields("stavlos.json", content)}
	m.onConfigEditor(configEditorMsg{epoch: e.epoch, op: "read", doc: &doc})
	return m
}

func TestConfigEditorGearsAndFieldClick(t *testing.T) {
	m := readyConfigEditor(t)
	e := m.cfgEditor
	if view := stripANSI(m.View()); !strings.Contains(view, "Project configuration") || !strings.Contains(view, "/repo/.stavlos") || !strings.Contains(view, "Commands") {
		t.Fatalf("editor view: %s", view)
	}
	for i, f := range e.doc.Fields {
		if strings.Join(f.Path, ".") == "reminders" {
			e.fieldSel = i
			break
		}
	}
	g := e.geometry(m.width, m.bodyHeight())
	start, _ := listWindow(e.fieldSel, 0, len(e.doc.Fields), g.rows)
	if cmd := m.configEditorMouse(g.x+2+g.left+4, g.y+1+4+e.fieldSel-start); cmd == nil || !e.busy {
		t.Fatal("boolean click did not save immediately")
	}
	m.closeConfigEditor()
	m.sidebarClick(sidebarWidth-3, 0)
	if m.cfgEditor == nil || m.cfgEditor.scope.Scope != "system" {
		t.Fatal("title gear did not open system settings")
	}
}

func TestConfigEditorRetainsInvalidDraftAndRejectsLateResponses(t *testing.T) {
	m := readyConfigEditor(t)
	e := m.cfgEditor
	m.configEditorKey(tea.KeyMsg{Type: tea.KeyF4})
	e.area.SetValue("{ broken")
	if !e.dirty() {
		t.Fatal("raw edit not marked dirty")
	}
	m.configEditorKey(tea.KeyMsg{Type: tea.KeyCtrlS})
	m.onConfigEditor(configEditorMsg{epoch: e.epoch, op: "edit", err: errors.New("invalid JSON")})
	if e.area.Value() != "{ broken" || !e.dirty() || e.busy {
		t.Fatal("validation failure discarded the draft")
	}
	m.configEditorKey(tea.KeyMsg{Type: tea.KeyEsc})
	if e.mode != "discard" {
		t.Fatal("dirty editor closed without confirmation")
	}
	m.configEditorKey(tea.KeyMsg{Type: tea.KeyEsc})
	if e.mode != "raw" {
		t.Fatal("cancelled discard lost editing mode")
	}
	epoch := e.epoch
	m.closeConfigEditor()
	m.openConfigEditor(true)
	m.onConfigEditor(configEditorMsg{epoch: epoch, op: "read", doc: &protocol.ConfigDocument{Path: "old-file"}})
	if m.cfgEditor.doc != nil {
		t.Fatal("late response overwrote a new editor session")
	}
}
