package tools

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/tool_defs.json from the current tool definitions")

// TestToolDefsSnapshot pins every built-in tool's model-facing definition
// (name, description, JSON schema). A change here is a change to what
// every model sees: review the diff, then run with -update.
func TestToolDefsSnapshot(t *testing.T) {
	set := Builtin()
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	var defs []json.RawMessage
	for _, n := range names {
		d := set[n].Def()
		var schema any
		if err := json.Unmarshal(d.Schema, &schema); err != nil {
			t.Fatalf("%s: schema is not JSON: %v", n, err)
		}
		b, _ := json.Marshal(map[string]any{"name": d.Name, "description": d.Description, "schema": schema})
		defs = append(defs, b)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(defs)
	path := filepath.Join("testdata", "tool_defs.json")
	if *update {
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(want, out.Bytes()) {
		t.Fatalf("tool definitions changed; review and run `go test ./internal/tools -run TestToolDefsSnapshot -update`\n%s", diffLines(string(want), out.String()))
	}
}

// diffLines is a crude line diff: enough to see what moved.
func diffLines(a, b string) string {
	al, bl := bytes.Split([]byte(a), []byte("\n")), bytes.Split([]byte(b), []byte("\n"))
	seen := map[string]bool{}
	for _, l := range al {
		seen[string(l)] = true
	}
	var out bytes.Buffer
	for _, l := range bl {
		if !seen[string(l)] {
			out.WriteString("+ " + string(l) + "\n")
		}
	}
	seen = map[string]bool{}
	for _, l := range bl {
		seen[string(l)] = true
	}
	for _, l := range al {
		if !seen[string(l)] {
			out.WriteString("- " + string(l) + "\n")
		}
	}
	return out.String()
}
