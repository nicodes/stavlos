package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// searchTree is a small tree for the search tools: two Go files, a hidden
// one, a binary one.
func searchTree(t *testing.T) string {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"a/x.go":     "package a\nfunc Hello() {}\n",
		"a/b/y.go":   "hello world\n",
		"a/notes.md": "Hello in markdown\n",
		".hidden/z":  "Hello\n",
		"bin.dat":    "Hello\x00\x01",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestSearchTools runs grep and glob on the tree walk and, when it is
// installed, on ripgrep: the two agree on what the model sees.
func TestSearchTools(t *testing.T) {
	backends := map[string]string{"walk": ""}
	if rgBinary != "" {
		backends["rg"] = rgBinary
	}
	saved := rgBinary
	t.Cleanup(func() { rgBinary = saved })
	for name, bin := range backends {
		t.Run(name, func(t *testing.T) {
			rgBinary = bin
			dir := searchTree(t)
			env := &Env{Dir: dir}
			run := func(tool, in string) Result {
				t.Helper()
				return Builtin()[tool].Run(context.Background(), json.RawMessage(in), env)
			}
			r := run("grep", `{"pattern":"Hello"}`)
			if r.IsError || !strings.Contains(r.Output, "a/x.go:2:func Hello() {}") || !strings.Contains(r.Output, "a/notes.md:1:") ||
				strings.Contains(r.Output, ".hidden") || strings.Contains(r.Output, "bin.dat") || strings.Contains(r.Output, "y.go") {
				t.Fatalf("grep: %+v", r)
			}
			if r := run("grep", `{"pattern":"hello","ignore_case":true,"glob":"*.go"}`); !strings.Contains(r.Output, "a/b/y.go:1:hello world") || strings.Contains(r.Output, "notes.md") {
				t.Fatalf("grep -i --glob: %+v", r)
			}
			if r := run("grep", `{"pattern":"Hello","path":"a/b"}`); r.Output != "no matches" {
				t.Fatalf("grep in a path: %+v", r)
			}
			if r := run("grep", `{"pattern":"Hello","limit":1}`); !strings.Contains(r.Output, "stopped at 1 matching lines") {
				t.Fatalf("grep limit: %+v", r)
			}
			// Nothing the model writes becomes a flag.
			if r := run("grep", `{"pattern":"--pre=sh"}`); r.IsError || r.Output != "no matches" {
				t.Fatalf("a flag-shaped pattern: %+v", r)
			}
			if r := run("grep", `{"pattern":"("}`); !r.IsError {
				t.Fatalf("a bad pattern: %+v", r)
			}
			if r := run("glob", `{"pattern":"*.go"}`); r.Output != "a/b/y.go\na/x.go" {
				t.Fatalf("glob by name: %q", r.Output)
			}
			if r := run("glob", `{"pattern":"a/*.go"}`); r.Output != "a/x.go" {
				t.Fatalf("glob by path: %q", r.Output)
			}
			if r := run("glob", `{"pattern":"**/*.md","path":"a"}`); !strings.HasSuffix(r.Output, "notes.md") || strings.Contains(r.Output, "\n") {
				t.Fatalf("glob in a path: %q", r.Output)
			}
		})
	}
	for _, tool := range []string{"grep", "glob"} {
		sub := Builtin()[tool].Subject(json.RawMessage(`{"pattern":"x"}`))
		if sub.Primary() != "." || Builtin()[tool].Subject(json.RawMessage(`{"pattern":"x","path":"/etc"}`)).Primary() != "/etc" {
			t.Errorf("%s subject %+v", tool, sub)
		}
	}
}
