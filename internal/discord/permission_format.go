package discord

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"

	"github.com/nicodes/stavlos/internal/protocol"
	"github.com/nicodes/stavlos/internal/textsafe"
	"github.com/nicodes/stavlos/internal/toolname"
)

func permissionHeading(p protocol.PromptInfo) string {
	title := permissionToolLabel(p.Tool)
	if name, ok := strings.CutPrefix(p.Tool, toolname.MCPPrefix); ok {
		if server, tool, ok := strings.Cut(name, "__"); ok {
			title = permissionToolLabel(server) + " · " + permissionToolLabel(tool)
		}
	}
	head := "❗ **" + escapeMarkdown(title) + "**"
	if p.Dir != "" {
		head += " " + escapeMarkdown(p.Dir)
	}
	return head
}

func permissionToolLabel(name string) string {
	label := []rune(strings.Join(strings.Fields(strings.ReplaceAll(name, "_", " ")), " "))
	if len(label) == 0 {
		return "Tool"
	}
	label[0] = unicode.ToUpper(label[0])
	return string(label)
}

func escapeMarkdown(text string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "`", "\\`").Replace(text)
}

// permissionCode shows text a model wrote as a code block, which is where
// the human reads what they are about to approve. The text cannot end the
// block: a fence inside it would close it, and whatever followed would be
// drawn as the bot's own words (a second, harmless-looking command; a
// mention; a link). A zero-width space between the backticks keeps them
// backticks and stops them being a fence. Control characters and direction
// overrides are removed for the same reason they are in the terminal: what
// is approved must be what is seen.
func permissionCode(language, text string) string {
	text = strings.ReplaceAll(textsafe.Clean(text), "```", "`\u200b`\u200b`")
	return "```" + language + "\n" + strings.TrimRight(text, "\n") + "\n```"
}

type permissionField struct{ key, label string }

// Known subjects are decoded into readable text; unrecognised arguments stay
// visible as formatted JSON so a tool-specific view never hides extra inputs.
func permissionSubject(p protocol.PromptInfo) string {
	switch p.Tool {
	case toolname.Shell:
		var input struct {
			Command *string `json:"command"`
		}
		if json.Unmarshal(p.Input, &input) == nil && input.Command != nil {
			return permissionCode("sh", *input.Command)
		}
	case toolname.ApplyPatch:
		return permissionFields(p.Input, "diff", []permissionField{{"patch", ""}})
	case toolname.WebFetch:
		return permissionFields(p.Input, "text", []permissionField{{"url", ""}, {"start", "Start"}})
	case toolname.WebSearch:
		return permissionFields(p.Input, "text", []permissionField{{"query", ""}, {"n", "Results"}})
	case toolname.Read:
		return permissionFields(p.Input, "text", []permissionField{{"path", ""}, {"offset", "First line"}, {"limit", "Line limit"}})
	case toolname.Grep:
		return permissionFields(p.Input, "text", []permissionField{{"pattern", "Pattern"}, {"path", "Path"}, {"glob", "File glob"}, {"ignore_case", "Ignore case"}, {"limit", "Match limit"}})
	case toolname.Glob:
		return permissionFields(p.Input, "text", []permissionField{{"pattern", "Pattern"}, {"path", "Path"}, {"limit", "Path limit"}})
	case toolname.Skill:
		return permissionFields(p.Input, "text", []permissionField{{"name", "Skill"}})
	case toolname.AgentCancel, toolname.ShellKill:
		return permissionFields(p.Input, "text", []permissionField{{"id", "Target"}})
	}
	return permissionJSON(p.Input)
}

func permissionFields(raw json.RawMessage, language string, fields []permissionField) string {
	var input map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || len(input) == 0 {
		return permissionJSON(raw)
	}
	var primary *string
	if json.Unmarshal(input[fields[0].key], &primary) != nil || primary == nil {
		return permissionJSON(raw)
	}
	var lines []string
	for _, field := range fields {
		value, ok := input[field.key]
		if !ok {
			continue
		}
		text := string(value)
		var decoded *string
		if json.Unmarshal(value, &decoded) == nil && decoded != nil {
			text = *decoded
		}
		if field.label != "" {
			text = field.label + ": " + text
		}
		lines = append(lines, text)
		delete(input, field.key)
	}
	text := permissionCode(language, strings.Join(lines, "\n"))
	if len(input) > 0 {
		extra, _ := json.Marshal(input)
		text += "\nAdditional arguments:\n" + permissionJSON(extra)
	}
	return text
}

func permissionJSON(raw json.RawMessage) string {
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") == nil {
		return permissionCode("json", pretty.String())
	}
	return permissionCode("text", string(raw))
}

func trustText(p protocol.PromptInfo) string {
	var data struct {
		Dir   string   `json:"dir"`
		Files []string `json:"files"`
	}
	if json.Unmarshal(p.Input, &data) != nil {
		return "❗ **Project configuration trust**\n" + permissionJSON(p.Input)
	}
	head := "❗ **Project configuration trust**"
	if data.Dir != "" {
		head += " · " + escapeMarkdown(data.Dir)
	}
	text := head + "\nProject configuration can define MCP servers, policy, presets, skills and AGENTS.md."
	if len(data.Files) > 0 {
		text += "\nConfiguration files:\n" + permissionCode("text", strings.Join(data.Files, "\n"))
	}
	return text
}
