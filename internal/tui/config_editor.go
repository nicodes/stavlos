package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/charmbracelet/bubbles/key"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/pkg/client"
)

var configSections = []string{"Settings", "Agents", "Commands", "Skills", "Files"}

func (m *Model) settingsCommand(arg string) tea.Cmd {
	if arg != "" && arg != "system" && arg != "project" {
		return m.setStatus("usage: /settings [project|system]", true)
	}
	return m.openConfigEditor(arg == "system")
}

type configEditor struct {
	epoch                            uint64
	scope                            protocol.ConfigScope
	tree                             protocol.ConfigTree
	doc                              *protocol.ConfigDocument
	section, fileSel, fieldSel, pane int
	mode                             string // browse | raw | field | body | json | new | rename | delete | discard
	previous, baseline, status       string
	busy                             bool
	area                             textarea.Model
	input                            textinput.Model
	collapsed                        map[string]bool
}

type configEditorMsg struct {
	epoch  uint64
	op     string
	tree   protocol.ConfigTree
	doc    *protocol.ConfigDocument
	notice string
	err    error
}

func (m *Model) openConfigEditor(system bool) tea.Cmd {
	if m.switching {
		return m.setStatus("wait for the channel to open", false)
	}
	m.configEpoch++
	e := &configEditor{epoch: m.configEpoch, scope: protocol.ConfigScope{Scope: "project", Channel: m.channelID}, mode: "browse", busy: true, collapsed: map[string]bool{}}
	if system {
		e.scope = protocol.ConfigScope{Scope: "system"}
	}
	e.area = textarea.New()
	e.area.CharLimit = 0
	e.area.ShowLineNumbers = true
	e.input = textinput.New()
	e.input.Prompt = "› "
	e.input.CharLimit = 0
	m.cfgEditor, m.ov = e, nil
	m.input.Blur()
	m.promptInput.Blur()
	m.dirInput.Blur()
	return configTreeCmd(m.ctx, m.c, e)
}

func configTreeCmd(ctx context.Context, c *client.Client, e *configEditor) tea.Cmd {
	epoch, scope := e.epoch, e.scope
	return rpcCmd(ctx, func(ctx context.Context) tea.Msg {
		tree, err := client.Do(ctx, c, protocol.ConfigList, scope)
		return configEditorMsg{epoch: epoch, op: "list", tree: tree, err: err}
	})
}

func (m *Model) readConfigFile(path string) tea.Cmd {
	e := m.cfgEditor
	if e == nil {
		return nil
	}
	e.busy = true
	epoch := e.epoch
	p := protocol.ConfigFileParams{ConfigScope: e.scope, Root: e.tree.Root, Path: path}
	return rpcCmd(m.ctx, func(ctx context.Context) tea.Msg {
		doc, err := client.Do(ctx, m.c, protocol.ConfigRead, p)
		return configEditorMsg{epoch: epoch, op: "read", doc: &doc, err: err}
	})
}

func (m *Model) editConfigFile(action, path, destination, content string, field []string, value json.RawMessage) tea.Cmd {
	e := m.cfgEditor
	if e == nil || e.busy {
		return nil
	}
	e.busy = true
	e.previous = e.mode
	epoch := e.epoch
	p := protocol.ConfigEditParams{ConfigFileParams: protocol.ConfigFileParams{ConfigScope: e.scope, Root: e.tree.Root, Path: path}, Action: action, Destination: destination, Content: content, FieldPath: field, FieldValue: value, Revision: e.tree.Revision}
	return rpcCmd(m.ctx, func(ctx context.Context) tea.Msg {
		r, err := client.Do(ctx, m.c, protocol.ConfigEdit, p)
		return configEditorMsg{epoch: epoch, op: "edit", tree: r.Tree, doc: r.Document, notice: r.Notice, err: err}
	})
}

func (m *Model) onConfigEditor(msg configEditorMsg) tea.Cmd {
	e := m.cfgEditor
	if e == nil || e.epoch != msg.epoch {
		return nil
	}
	e.busy = false
	if msg.err != nil {
		e.status = msg.err.Error()
		return nil
	}
	if msg.op == "list" {
		e.tree, e.status = msg.tree, "Files are saved directly in this directory."
		path := "stavlos.json"
		if e.doc != nil {
			path = e.doc.Path
		}
		return m.readConfigFile(path)
	}
	if msg.op == "edit" {
		e.tree, e.status = msg.tree, msg.notice
	}
	selectedField := ""
	if e.doc != nil && e.fieldSel >= 0 && e.fieldSel < len(e.doc.Fields) {
		selectedField = strings.Join(e.doc.Fields[e.fieldSel].Path, "/")
	}
	e.doc = msg.doc
	e.fieldSel = 0
	e.mode = "browse"
	if msg.doc != nil {
		if msg.op == "read" {
			selectedField = ""
		}
		for i, f := range msg.doc.Fields {
			if strings.Join(f.Path, "/") == selectedField {
				e.fieldSel = i
				break
			}
		}
		e.area.SetValue(msg.doc.Content)
		e.baseline = msg.doc.Content
		if len(msg.doc.Fields) == 0 || msg.op == "edit" && e.previous == "raw" {
			e.mode = "raw"
		}
		if msg.op == "read" {
			for i, section := range configSections[1:4] {
				if strings.HasPrefix(msg.doc.Path, strings.ToLower(section)+"/") && e.section != 4 {
					e.section = i + 1
				}
			}
		}
		e.pane = 1
		if e.mode == "raw" {
			e.area.Focus()
		}
		for i, file := range e.entries() {
			if file.Path == msg.doc.Path {
				e.fileSel = i
				break
			}
		}
	}
	if msg.op == "edit" {
		return tea.Batch(customCommandsCmd(m.ctx, m.c, m.requestScope()), rolesCmd(m.ctx, m.c, m.requestScope(), true))
	}
	return nil
}

func (e *configEditor) entries() []protocol.ConfigEntry {
	if e.section == 0 {
		out := []protocol.ConfigEntry{{Path: "stavlos.json"}}
		if e.scope.Scope == "project" {
			out = append(out, protocol.ConfigEntry{Path: "stavlos.local.json"})
		}
		return out
	}
	prefix := ""
	if e.section < 4 {
		prefix = strings.ToLower(configSections[e.section]) + "/"
	}
	var out []protocol.ConfigEntry
	for _, file := range e.tree.Files {
		if !strings.HasPrefix(file.Path, prefix) {
			continue
		}
		hidden := false
		for dir, collapsed := range e.collapsed {
			if collapsed && strings.HasPrefix(file.Path, dir+"/") {
				hidden = true
				break
			}
		}
		if !hidden {
			out = append(out, file)
		}
	}
	if e.doc != nil && !e.doc.Exists && strings.HasPrefix(e.doc.Path, prefix) {
		out = append(out, protocol.ConfigEntry{Path: e.doc.Path})
	}
	return out
}

func (e *configEditor) selectedFile() (protocol.ConfigEntry, bool) {
	files := e.entries()
	if e.fileSel < 0 || e.fileSel >= len(files) {
		return protocol.ConfigEntry{}, false
	}
	return files[e.fileSel], true
}

func (e *configEditor) dirty() bool {
	switch e.mode {
	case "raw", "body", "json":
		return e.area.Value() != e.baseline
	case "field":
		return e.input.Value() != e.baseline
	}
	return false
}

func (e *configEditor) multiline() bool {
	return e.mode == "raw" || e.mode == "body" || e.mode == "json"
}

func (m *Model) configEditorKey(msg tea.KeyMsg) tea.Cmd {
	e := m.cfgEditor
	if e.busy {
		return nil
	}
	if e.mode == "discard" {
		if key.Matches(msg, keys.EdConfirm) {
			return m.closeConfigEditor()
		}
		if key.Matches(msg, keys.Clear) {
			e.mode = e.previous
		}
		return nil
	}
	if key.Matches(msg, keys.EdClose) {
		return m.configEditorEscape()
	}
	if key.Matches(msg, keys.EdSave) {
		return m.saveConfigEditor()
	}
	if e.mode == "new" || e.mode == "rename" || e.mode == "delete" {
		return m.configFileOperationKey(msg)
	}
	if e.mode == "field" {
		if key.Matches(msg, keys.EdConfirm) {
			return m.saveConfigEditor()
		}
		var cmd tea.Cmd
		e.input, cmd = e.input.Update(msg)
		return cmd
	}
	if key.Matches(msg, keys.EdRaw) && e.doc != nil {
		if e.dirty() {
			e.status = "Save with Ctrl+S before switching editors."
			return nil
		}
		if e.mode == "raw" && len(e.doc.Fields) > 0 {
			e.mode = "browse"
			e.area.Blur()
		} else {
			e.mode, e.baseline = "raw", e.doc.Content
			e.area.SetValue(e.doc.Content)
			e.pane = 1
			return e.area.Focus()
		}
		return nil
	}
	if key.Matches(msg, keys.EdPane) {
		e.pane = 1 - e.pane
		e.area.Blur()
		if e.pane == 1 && e.multiline() {
			return e.area.Focus()
		}
		return nil
	}
	if e.pane == 0 {
		return m.configNavigationKey(msg)
	}
	if e.multiline() {
		var cmd tea.Cmd
		e.area, cmd = e.area.Update(msg)
		return cmd
	}
	if e.doc == nil {
		return nil
	}
	switch {
	case key.Matches(msg, keys.EdUp):
		e.fieldSel = max(0, e.fieldSel-1)
	case key.Matches(msg, keys.EdDown):
		e.fieldSel = min(len(e.doc.Fields)-1, e.fieldSel+1)
	case key.Matches(msg, keys.EdOpen):
		return m.openConfigField()
	}
	return nil
}

func (m *Model) configEditorEscape() tea.Cmd {
	e := m.cfgEditor
	if e.dirty() {
		e.previous, e.mode, e.status = e.mode, "discard", "Discard unsaved changes and close? Enter discards; Esc keeps editing."
		return nil
	}
	switch e.mode {
	case "field", "body", "json":
		e.mode = "browse"
	case "new", "rename", "delete":
		e.mode = e.previous
	default:
		return m.closeConfigEditor()
	}
	e.input.Blur()
	e.area.Blur()
	return nil
}

func (m *Model) configNavigationKey(msg tea.KeyMsg) tea.Cmd {
	e := m.cfgEditor
	if e.dirty() {
		e.status = "Save with Ctrl+S or discard with Esc before choosing another file."
		return nil
	}
	files := e.entries()
	switch {
	case key.Matches(msg, keys.EdPrev, keys.EdNext):
		delta := 1
		if key.Matches(msg, keys.EdPrev) {
			delta = -1
		}
		e.section = (e.section + delta + len(configSections)) % len(configSections)
		e.fileSel = 0
	case key.Matches(msg, keys.EdUp):
		e.fileSel = max(0, e.fileSel-1)
	case key.Matches(msg, keys.EdDown):
		e.fileSel = min(max(0, len(files)-1), e.fileSel+1)
	case key.Matches(msg, keys.EdNew):
		e.previous = e.mode
		e.mode = "new"
		e.input.SetValue(e.newFilePath())
		e.input.Placeholder = "relative file path"
		return e.input.Focus()
	case key.Matches(msg, keys.EdRename, keys.EdDelete):
		e.previous = e.mode
		file, ok := e.selectedFile()
		if !ok {
			return nil
		}
		if key.Matches(msg, keys.EdDelete) {
			e.mode, e.status = "delete", "Delete "+file.Path+" (including contents if a directory)? Enter confirms."
			return nil
		}
		e.mode = "rename"
		e.input.SetValue(file.Path)
		return e.input.Focus()
	case key.Matches(msg, keys.EdOpen):
		file, ok := e.selectedFile()
		if !ok {
			return nil
		}
		if file.Directory {
			e.collapsed[file.Path] = !e.collapsed[file.Path]
			return nil
		}
		return m.readConfigFile(file.Path)
	case key.Matches(msg, keys.EdReload):
		e.busy = true
		return configTreeCmd(m.ctx, m.c, e)
	}
	return nil
}

func (e *configEditor) newFilePath() string {
	switch e.section {
	case 0:
		return "stavlos.local.json"
	case 1:
		return "agents/new-agent.md"
	case 2:
		return "commands/new-command.md"
	case 3:
		return "skills/new-skill/SKILL.md"
	}
	return "new-file.md"
}

func (m *Model) configFileOperationKey(msg tea.KeyMsg) tea.Cmd {
	e := m.cfgEditor
	if !key.Matches(msg, keys.EdConfirm) {
		var cmd tea.Cmd
		e.input, cmd = e.input.Update(msg)
		return cmd
	}
	file, ok := e.selectedFile()
	switch e.mode {
	case "delete":
		if ok {
			return m.editConfigFile("delete", file.Path, "", "", nil, nil)
		}
	case "rename":
		if ok {
			return m.editConfigFile("rename", file.Path, strings.TrimSpace(e.input.Value()), "", nil, nil)
		}
	case "new":
		path := filepath.ToSlash(strings.TrimSpace(e.input.Value()))
		for _, f := range e.tree.Files {
			if f.Path == path {
				e.status = "That path already exists."
				return nil
			}
		}
		return m.readConfigFile(path) // template is shown before the first save
	}
	return nil
}

func (m *Model) openConfigField() tea.Cmd {
	e := m.cfgEditor
	if e.doc == nil || e.fieldSel < 0 || e.fieldSel >= len(e.doc.Fields) {
		return nil
	}
	f := e.doc.Fields[e.fieldSel]
	if f.Kind == "boolean" {
		value := "true"
		if f.Value == "true" {
			value = "false"
		}
		return m.editConfigFile("write", e.doc.Path, "", "", f.Path, json.RawMessage(value))
	}
	if len(f.Choices) > 0 {
		var current string
		_ = json.Unmarshal([]byte(f.Value), &current)
		next := f.Choices[0]
		for i, v := range f.Choices {
			if v == current {
				next = f.Choices[(i+1)%len(f.Choices)]
				break
			}
		}
		value, _ := json.Marshal(next)
		return m.editConfigFile("write", e.doc.Path, "", "", f.Path, value)
	}
	value := f.Value
	if !f.Set {
		value = "null"
	}
	switch f.Kind {
	case "body":
		e.mode = "body"
	case "json":
		e.mode = "json"
	default:
		if f.Kind == "string" {
			value = ""
			_ = json.Unmarshal([]byte(f.Value), &value)
		}
		e.mode, e.baseline = "field", value
		e.input.SetValue(value)
		return e.input.Focus()
	}
	e.baseline = value
	e.area.SetValue(value)
	return e.area.Focus()
}

func (m *Model) saveConfigEditor() tea.Cmd {
	e := m.cfgEditor
	if e.doc == nil {
		return nil
	}
	if e.mode == "browse" && !e.doc.Exists {
		return m.editConfigFile("write", e.doc.Path, "", e.doc.Content, nil, nil)
	}
	if e.mode == "raw" {
		return m.editConfigFile("write", e.doc.Path, "", e.area.Value(), nil, nil)
	}
	if e.mode != "field" && e.mode != "body" && e.mode != "json" {
		return nil
	}
	f := e.doc.Fields[e.fieldSel]
	value := e.input.Value()
	if e.mode != "field" {
		value = e.area.Value()
	}
	var raw json.RawMessage
	if f.Kind == "string" || f.Kind == "body" {
		raw, _ = json.Marshal(value)
	} else {
		raw = json.RawMessage(value)
		if !json.Valid(raw) {
			e.status = "Enter a valid JSON value (or null for an unset value)."
			return nil
		}
	}
	return m.editConfigFile("write", e.doc.Path, "", "", f.Path, raw)
}

func (m *Model) closeConfigEditor() tea.Cmd {
	m.cfgEditor = nil
	m.configEpoch++
	return tea.Batch(m.setFocus(focusInput), m.input.Focus())
}

func (e *configEditor) title() string {
	name := "Project configuration"
	if e.scope.Scope == "system" {
		name = "System configuration"
	}
	if e.doc != nil {
		name += " · " + e.doc.Path
	}
	if e.doc != nil && !e.doc.Exists {
		name += " (new)"
	}
	if e.dirty() {
		name += " *"
	}
	return name
}

func configFieldText(f protocol.ConfigField) string {
	if !f.Set {
		return "(unset)"
	}
	if f.Kind == "body" {
		return "Markdown prompt"
	}
	var text string
	if json.Unmarshal([]byte(f.Value), &text) == nil {
		return text
	}
	return fmt.Sprint(f.Value)
}
