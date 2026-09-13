package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchAddUpdateDeleteMove(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Dir: dir}
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {\n\told()\n\tmore()\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "gone.txt"), []byte("x\n"), 0o644)
	patch := `*** Begin Patch
*** Add File: docs/new.md
+# New
+
+hello
*** Update File: main.go
@@ func main() {
-	old()
+	new()
+	extra()
*** Delete File: gone.txt
*** End Patch`
	in, _ := json.Marshal(map[string]string{"patch": patch})
	tool := patchTool{}
	if got := tool.PolicyArgs(in); len(got) != 3 || got[0] != "docs/new.md" || got[1] != "main.go" || got[2] != "gone.txt" {
		t.Fatalf("policy args %v", got)
	}
	r := tool.Run(context.Background(), in, env)
	if r.IsError {
		t.Fatal(r.Output)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "docs/new.md")); string(b) != "# New\n\nhello\n" {
		t.Fatalf("added: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "main.go")); string(b) != "package main\n\nfunc main() {\n\tnew()\n\textra()\n\tmore()\n}\n" {
		t.Fatalf("updated: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.txt")); err == nil {
		t.Fatal("not deleted")
	}
	// move
	patch = "*** Begin Patch\n*** Update File: main.go\n*** Move to: cmd/main.go\n@@\n-\tmore()\n+\tless()\n*** End Patch"
	in, _ = json.Marshal(map[string]string{"patch": patch})
	if r := tool.Run(context.Background(), in, env); r.IsError {
		t.Fatal(r.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err == nil {
		t.Fatal("old path still exists after move")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "cmd/main.go")); !strings.Contains(string(b), "less()") || strings.Contains(string(b), "more()") {
		t.Fatalf("moved: %q", b)
	}
}

func TestApplyPatchAtomicAndErrors(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Dir: dir}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	tool := patchTool{}
	// second section fails → first must not be applied
	patch := "*** Begin Patch\n*** Update File: a.txt\n-one\n+uno\n*** Update File: missing.txt\n-x\n+y\n*** End Patch"
	in, _ := json.Marshal(map[string]string{"patch": patch})
	r := tool.Run(context.Background(), in, env)
	if !r.IsError || !strings.Contains(r.Output, "missing.txt") {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one\ntwo\nthree\n" {
		t.Fatalf("partial apply: %q", b)
	}
	// context not found
	patch = "*** Begin Patch\n*** Update File: a.txt\n-nope\n+x\n*** End Patch"
	in, _ = json.Marshal(map[string]string{"patch": patch})
	if r := tool.Run(context.Background(), in, env); !r.IsError || !strings.Contains(r.Output, "context not found") {
		t.Fatalf("%+v", r)
	}
	// whitespace-lenient match, and adding an existing file is refused
	patch = "*** Begin Patch\n*** Update File: a.txt\n two   \n-three\n+3\n*** End Patch"
	in, _ = json.Marshal(map[string]string{"patch": patch})
	if r := tool.Run(context.Background(), in, env); r.IsError {
		t.Fatal(r.Output)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one\ntwo\n3\n" {
		t.Fatalf("%q", b)
	}
	patch = "*** Begin Patch\n*** Add File: a.txt\n+dup\n*** End Patch"
	in, _ = json.Marshal(map[string]string{"patch": patch})
	if r := tool.Run(context.Background(), in, env); !r.IsError || !strings.Contains(r.Output, "already exists") {
		t.Fatalf("%+v", r)
	}
	// missing markers
	in, _ = json.Marshal(map[string]string{"patch": "just text"})
	if r := tool.Run(context.Background(), in, env); !r.IsError {
		t.Fatal("accepted a non-patch")
	}
}

func TestApplyPatchInsertionAndEOF(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Dir: dir}
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\nb\nc\n"), 0o644)
	tool := patchTool{}
	// pure insertion after an anchor, and an EOF-anchored hunk
	patch := "*** Begin Patch\n*** Update File: f.txt\n@@ a\n+a2\n@@\n c\n+d\n*** End of File\n*** End Patch"
	in, _ := json.Marshal(map[string]string{"patch": patch})
	if r := tool.Run(context.Background(), in, env); r.IsError {
		t.Fatal(r.Output)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f.txt")); string(b) != "a\na2\nb\nc\nd\n" {
		t.Fatalf("%q", b)
	}
}
