package protocol

import "encoding/json"

const (
	MConfigList = "config.list"
	MConfigRead = "config.read"
	MConfigEdit = "config.edit"
)

var (
	ConfigList = Method[ConfigScope, ConfigTree]{MConfigList}
	ConfigRead = Method[ConfigFileParams, ConfigDocument]{MConfigRead}
	ConfigEdit = Method[ConfigEditParams, ConfigEditResult]{MConfigEdit}
)

type ConfigScope struct {
	Scope   string `json:"scope"` // project | system
	Channel string `json:"channel,omitempty"`
}
type ConfigEntry struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory,omitempty"`
}
type ConfigTree struct {
	Root     string        `json:"root"`
	Revision string        `json:"revision"`
	Files    []ConfigEntry `json:"files"`
}
type ConfigField struct {
	Path    []string `json:"path"`
	Kind    string   `json:"kind"`  // string | boolean | number | json | body
	Value   string   `json:"value"` // JSON value, except the Markdown body
	Set     bool     `json:"set"`
	Choices []string `json:"choices,omitempty"`
}
type ConfigFileParams struct {
	ConfigScope
	Root string `json:"root"`
	Path string `json:"path"`
}
type ConfigDocument struct {
	Path    string        `json:"path"`
	Content string        `json:"content"`
	Exists  bool          `json:"exists"`
	Fields  []ConfigField `json:"fields,omitempty"`
}
type ConfigEditParams struct {
	ConfigFileParams
	Revision    string          `json:"revision"`
	Action      string          `json:"action"` // write | rename | delete
	Content     string          `json:"content,omitempty"`
	Destination string          `json:"destination,omitempty"`
	FieldPath   []string        `json:"field_path,omitempty"`
	FieldValue  json.RawMessage `json:"field_value,omitempty"`
}
type ConfigEditResult struct {
	Tree     ConfigTree      `json:"tree"`
	Document *ConfigDocument `json:"document,omitempty"`
	Notice   string          `json:"notice"`
}
