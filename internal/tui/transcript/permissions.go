package transcript

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/nicodes/stavlos/internal/event"
	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/toolname"
)

type permissionItem struct {
	item     int
	p        protocol.PromptInfo
	resolved bool
}

func (t *Transcript) PermissionItem(id string) (int, bool) {
	p, ok := t.permissions[id]
	return p.item, ok
}

func (t *Transcript) EnsurePermission(p protocol.PromptInfo) {
	if t.permissions == nil {
		t.permissions = map[string]permissionItem{}
	}
	old, ok := t.permissions[p.ID]
	if ok {
		if len(p.Input) == 0 {
			p.Input = old.p.Input
		}
		if p.Dir == "" {
			p.Dir = old.p.Dir
		}
		if p.Prefix == "" {
			p.Prefix = old.p.Prefix
		}
		if p.From == "" {
			p.From = old.p.From
		}
	}
	if ok && (old.resolved || reflect.DeepEqual(old.p, p)) {
		return
	}
	if ok {
		old.p = p
		t.permissions[p.ID] = old
		t.replaceItem(old.item, t.permissionLines(p, nil))
		return
	}
	t.permissions[p.ID] = permissionItem{item: len(t.items), p: p}
	t.appendItem(stamped(t.permissionLines(p, nil), p.Created))
}

func PermissionHeader(p protocol.PromptInfo, chat bool) Line {
	p.Question, p.Questions = "Permission: "+ToolTitle(p.Tool), nil
	head := QuestionHeader(p, 0, chat)
	head.Glyph = GlyphPermission
	return head
}

func (t *Transcript) permissionLines(p protocol.PromptInfo, result *event.AskResolvedPayload) []Line {
	lines := []Line{PermissionHeader(p, t.chat)}
	subject := string(p.Input)
	if p.Tool == toolname.Shell {
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(p.Input, &in) == nil {
			subject = strings.TrimRight(in.Command, "\n")
		}
	}
	for i, text := range strings.Split(textsafe.Visible(subject), "\n") {
		if text != "" {
			line := Line{Kind: LineText, Text: text, Indent: 2}
			if i == 0 {
				line.Glyph, _ = ToolGlyph(p.Tool)
				line.Indent = 1
			}
			lines = append(lines, line)
		}
	}
	if p.Dir != "" {
		lines = append(lines, Line{Kind: LineText, Text: "Outside the channel's directories: " + p.Dir, Indent: 1})
	}
	text := "Waiting for permission"
	if result != nil {
		text = protocol.PermissionResult(p, *result)
	}
	lines = append(lines, Line{Kind: LineDim, Text: text, Indent: 1})
	if result != nil {
		for i := range lines {
			lines[i].Note = true
		}
	}
	lines = CleanLines(lines)
	if t.chat {
		linked(lines, p.Agent)
	}
	return lines
}

func (t *Transcript) applyPermission(ev event.Event) bool {
	switch ev.Type {
	case event.AskRequested:
		var p event.AskRequestedPayload
		if ev.Decode(&p) != nil || p.Kind != string(protocol.PromptPermission) {
			return false
		}
		from := p.From
		if from == "" {
			from = t.self
			if t.chat || from == "" {
				from = t.agentName(ev.Agent)
			}
		}
		t.EnsurePermission(protocol.PromptInfo{ID: p.ID, Channel: ev.Channel, Agent: ev.Agent, From: from, Role: p.Role, Kind: protocol.PromptPermission, Tool: p.Tool, Input: p.Input, Dir: p.Dir, Prefix: p.Prefix, Question: p.Question})
		return true
	case event.AskResolved:
		var r event.AskResolvedPayload
		if ev.Decode(&r) != nil {
			return false
		}
		p, ok := t.permissions[r.ID]
		if !ok {
			return false
		}
		p.resolved = true
		t.permissions[r.ID] = p
		t.replaceItem(p.item, t.permissionLines(p.p, &r))
		return true
	default:
		return false
	}
}

// stamped dates a prompt's lines by when the prompt was created (RFC 3339),
// for a card drawn before its event arrives; unparsable leaves them undated.
func stamped(lines []Line, created string) []Line {
	at, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return lines
	}
	for i := range lines {
		lines[i].At = at
	}
	return lines
}
