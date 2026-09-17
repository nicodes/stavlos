package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/nicodes/stavlos/internal/protocol"
	"gopkg.in/yaml.v3"
)

func EditorFields(path, content string) []protocol.ConfigField {
	if path == "stavlos.json" || path == "stavlos.local.json" {
		var object map[string]json.RawMessage
		if json.Unmarshal(StripJSONC([]byte(content)), &object) != nil {
			return nil
		}
		return jsonFields(reflect.TypeOf(File{}), object, nil)
	}
	var specs []protocol.ConfigField
	switch {
	case strings.HasPrefix(path, "agents/") && strings.HasSuffix(path, ".md"):
		for _, key := range []string{"description", "type", "models", "loop", "tools", "skills", "mcp", "spawn", "max_turns", "color"} {
			kind := "json"
			switch key {
			case "description", "type", "loop", "color":
				kind = "string"
			case "max_turns":
				kind = "number"
			}
			f := protocol.ConfigField{Path: []string{key}, Kind: kind}
			if key == "type" {
				f.Choices = []string{TypeAll, TypePrimary, TypeSubagent}
			}
			if key == "color" {
				f.Choices = append([]string{""}, RoleColors...)
			}
			specs = append(specs, f)
		}
	case strings.HasPrefix(path, "commands/") && strings.HasSuffix(path, ".md"):
		specs = []protocol.ConfigField{{Path: []string{"description"}, Kind: "string"}}
	case strings.HasPrefix(path, "skills/") && filepath.Base(path) == "SKILL.md":
		specs = []protocol.ConfigField{{Path: []string{"name"}, Kind: "string"}, {Path: []string{"description"}, Kind: "string"}}
	default:
		return nil
	}
	var meta map[string]any
	body, err := frontmatter(content, &meta)
	if err != nil {
		return nil
	}
	for i := range specs {
		v, ok := meta[specs[i].Path[0]]
		if ok {
			b, err := json.Marshal(v)
			if err == nil {
				specs[i].Value, specs[i].Set = string(b), true
			}
		}
	}
	return append(specs, protocol.ConfigField{Path: []string{"$body"}, Kind: "body", Value: body, Set: true})
}

func jsonFields(t reflect.Type, object map[string]json.RawMessage, prefix []string) []protocol.ConfigField {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []protocol.ConfigField
	known := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		known[name] = true
		path := append(append([]string(nil), prefix...), name)
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		value, set := object[name]
		if ft.Kind() == reflect.Struct {
			var child map[string]json.RawMessage
			_ = json.Unmarshal(value, &child)
			out = append(out, jsonFields(ft, child, path)...)
			continue
		}
		kind := "json"
		switch ft.Kind() {
		case reflect.String:
			kind = "string"
		case reflect.Bool:
			kind = "boolean"
		case reflect.Int, reflect.Int64, reflect.Float64:
			kind = "number"
		default:
			// Arrays, maps and future schema kinds use the JSON value editor.
		}
		field := protocol.ConfigField{Path: path, Kind: kind, Value: string(value), Set: set}
		switch strings.Join(path, ".") {
		case "mode":
			field.Choices = []string{"ask", "auto", "yolo"}
		case "escalation.default":
			field.Choices = []string{"deny", "allow"}
		}
		out = append(out, field)
	}
	var extra []string
	for name := range object {
		if !known[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		out = append(out, protocol.ConfigField{Path: append(append([]string(nil), prefix...), name), Kind: "json", Value: string(object[name]), Set: true})
	}
	return out
}

// EditJSONField changes only the target value; unrelated JSONC comments and
// formatting remain byte-for-byte intact. Missing objects are created lazily.
func EditJSONField(content []byte, path []string, value json.RawMessage) ([]byte, error) {
	if len(path) == 0 || !json.Valid(value) {
		return nil, fmt.Errorf("invalid field path or JSON value")
	}
	if bytes.Equal(bytes.TrimSpace(content), []byte("null")) || len(bytes.TrimSpace(content)) == 0 {
		content = []byte("{}")
	}
	clean, err := maskJSONC(content)
	if err != nil {
		return nil, err
	}
	a, z, err := propertySpan(clean, path[0])
	if err != nil {
		return nil, err
	}
	if a >= 0 {
		if len(path) > 1 {
			value, err = EditJSONField(content[a:z], path[1:], value)
			if err != nil {
				return nil, err
			}
		}
		return []byte(string(content[:a]) + string(value) + string(content[z:])), nil
	}
	if len(path) > 1 {
		value, err = EditJSONField([]byte("{}"), path[1:], value)
		if err != nil {
			return nil, err
		}
	}
	start := bytes.IndexByte(clean, '{')
	key, _ := json.Marshal(path[0])
	comma := ""
	if len(bytes.TrimSpace(clean[start+1:bytes.LastIndexByte(clean, '}')])) > 0 {
		comma = ","
	}
	eol := "\n"
	if bytes.Contains(content, []byte("\r\n")) {
		eol = "\r\n"
	}
	insert := eol + "  " + string(key) + ": " + string(value) + comma
	return []byte(string(content[:start+1]) + insert + string(content[start+1:])), nil
}

func EditConfigField(path, content string, field []string, value json.RawMessage) (string, error) {
	if path == "stavlos.json" || path == "stavlos.local.json" {
		b, err := EditJSONField([]byte(content), field, value)
		return string(b), err
	}
	if len(field) != 1 {
		return "", fmt.Errorf("select a frontmatter field or the prompt body")
	}
	meta, body, err := markdownParts(content)
	if err != nil {
		return "", err
	}
	if field[0] == "$body" {
		if err := json.Unmarshal(value, &body); err != nil {
			return "", err
		}
	} else {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(meta), &doc); err != nil {
			return "", err
		}
		if len(doc.Content) == 0 {
			doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
		}
		mapping := doc.Content[0]
		if mapping.Kind != yaml.MappingNode {
			return "", fmt.Errorf("frontmatter must be a mapping")
		}
		var replacement yaml.Node
		if err := yaml.Unmarshal(value, &replacement); err != nil {
			return "", err
		}
		if len(replacement.Content) == 0 {
			return "", fmt.Errorf("field value is required")
		}
		v := replacement.Content[0]
		found := false
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == field[0] {
				old := mapping.Content[i+1]
				v.HeadComment, v.LineComment, v.FootComment = old.HeadComment, old.LineComment, old.FootComment
				mapping.Content[i+1], found = v, true
				break
			}
		}
		if !found {
			mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: field[0]}, v)
		}
		var b bytes.Buffer
		encoder := yaml.NewEncoder(&b)
		encoder.SetIndent(2)
		if err := encoder.Encode(mapping); err != nil {
			return "", err
		}
		meta = b.String()
	}
	return "---\n" + strings.TrimRight(meta, "\r\n") + "\n---\n" + body, nil
}

func markdownParts(content string) (string, string, error) {
	normal := strings.ReplaceAll(strings.TrimPrefix(content, "\uFEFF"), "\r\n", "\n")
	if !strings.HasPrefix(normal, "---\n") {
		return "", content, nil
	}
	end := strings.Index(normal[4:], "\n---")
	if end < 0 {
		return "", "", fmt.Errorf("unterminated frontmatter")
	}
	end += 4
	body := normal[end+4:]
	body = strings.TrimPrefix(body, "\n")
	return normal[4:end], body, nil
}
