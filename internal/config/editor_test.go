package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/protocol"
)

func TestEditorPreservesJSONCAndRejectsInvalidOrStaleWrites(t *testing.T) {
	root := t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", root)
	file := filepath.Join(root, "stavlos.json")
	original := "{\n  // keep this comment\n  \"limits\": {\"maxDepth\": 3},\n  \"model\": \"fake/model\",\n}\n"
	if err := os.WriteFile(file, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	tree, err := EditorTree(root)
	if err != nil {
		t.Fatal(err)
	}
	p := protocol.ConfigEditParams{ConfigFileParams: protocol.ConfigFileParams{Path: "stavlos.json"}, Action: "write", Revision: tree.Revision, FieldPath: []string{"limits", "maxDepth"}, FieldValue: json.RawMessage("4")}
	updated, err := EditorApply(root, true, p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	if string(b) != strings.Replace(original, "\"maxDepth\": 3", "\"maxDepth\": 4", 1) {
		t.Fatalf("unrelated formatting changed: %s", b)
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o640 {
		t.Fatal("file mode changed")
	}
	if _, err := EditorApply(root, true, p); err == nil {
		t.Fatal("stale revision overwrote disk changes")
	}
	p.Revision, p.FieldValue = updated.Revision, json.RawMessage("-1")
	if _, err := EditorApply(root, true, p); err == nil {
		t.Fatal("invalid setting was saved")
	}
	if after, _ := os.ReadFile(file); string(after) != string(b) {
		t.Fatal("validation failure changed the real file")
	}
	p.FieldPath, p.Content = nil, "{} garbage"
	if _, err := EditorApply(root, true, p); err == nil {
		t.Fatal("invalid trailing JSON was accepted")
	}
	for _, path := range []string{"../escape", "/absolute", "."} {
		if _, err := EditorRead(root, path); err == nil {
			t.Fatalf("escaped editor root: %s", path)
		}
	}
}

func TestEditorCreatesRenamesDeletesAndEditsMarkdown(t *testing.T) {
	root, global := t.TempDir(), t.TempDir()
	t.Setenv("STAVLOS_CONFIG_DIR", global)
	apply := func(p protocol.ConfigEditParams) {
		t.Helper()
		tree, err := EditorTree(root)
		if err != nil {
			t.Fatal(err)
		}
		p.Revision = tree.Revision
		if _, err := EditorApply(root, false, p); err != nil {
			t.Fatal(err)
		}
	}
	path := "commands/test.md"
	apply(protocol.ConfigEditParams{ConfigFileParams: protocol.ConfigFileParams{Path: path}, Action: "write", Content: "---\n# keep this\ndescription: Old description\n---\n\nOriginal prompt.\n"})
	apply(protocol.ConfigEditParams{ConfigFileParams: protocol.ConfigFileParams{Path: path}, Action: "write", FieldPath: []string{"description"}, FieldValue: json.RawMessage(`"New description"`)})
	doc, err := EditorRead(root, path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Content, "# keep this") || !strings.Contains(doc.Content, "Original prompt.") || len(doc.Fields) != 2 {
		t.Fatalf("Markdown field edit: %+v", doc)
	}
	apply(protocol.ConfigEditParams{ConfigFileParams: protocol.ConfigFileParams{Path: path}, Action: "write", FieldPath: []string{"$body"}, FieldValue: json.RawMessage(`"Changed prompt.\n"`)})
	c, err := ReadCommand(filepath.Join(root, path))
	if err != nil || c.Description != "New description" || c.Body != "Changed prompt." {
		t.Fatalf("command edit: %+v %v", c, err)
	}
	apply(protocol.ConfigEditParams{ConfigFileParams: protocol.ConfigFileParams{Path: path}, Action: "rename", Destination: "commands/renamed.md"})
	if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
		t.Fatal("rename left the old file")
	}
	apply(protocol.ConfigEditParams{ConfigFileParams: protocol.ConfigFileParams{Path: "commands"}, Action: "delete"})
	if _, err := os.Stat(filepath.Join(root, "commands")); !os.IsNotExist(err) {
		t.Fatal("directory deletion failed")
	}
}

func TestEditorStructuredSettingsCoverSchemaAndNestedInsert(t *testing.T) {
	fields := EditorFields("stavlos.json", "{}")
	want := map[string]bool{"model": false, "limits.maxAgents": false, "sandbox.network": false, "discord.token": false, "mcp": false, "policy": false}
	for _, f := range fields {
		if _, ok := want[strings.Join(f.Path, ".")]; ok {
			want[strings.Join(f.Path, ".")] = true
		}
	}
	for field, found := range want {
		if !found {
			t.Fatalf("setting missing: %s", field)
		}
	}
	b, err := EditJSONField([]byte("{ /* keep */ \"model\": \"x\", }"), []string{"sandbox", "network"}, json.RawMessage("false"))
	if err != nil || !strings.Contains(string(b), "/* keep */") {
		t.Fatalf("nested insert: %s %v", b, err)
	}
	var file File
	if err := json.Unmarshal(StripJSONC(b), &file); err != nil || file.Sandbox == nil || file.Sandbox.Network == nil || *file.Sandbox.Network {
		t.Fatalf("nested field missing: %s %v", b, err)
	}
}
