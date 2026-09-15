package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicodes/stavlos/internal/policy"
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
	if sub := tool.Subject(in); sub.Kind != policy.KindPath || len(sub.Values) != 3 || sub.Values[0] != "docs/new.md" || sub.Values[1] != "main.go" || sub.Values[2] != "gone.txt" {
		t.Fatalf("subject %+v", sub)
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

// TestApplyPatchWritesAtomically: a move keeps the file's permissions and
// refuses an existing target; a write that fails midway is rolled back and
// nothing is deleted.
func TestApplyPatchWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Dir: dir}
	tool := patchTool{}
	run := func(patch string) Result {
		in, _ := json.Marshal(map[string]string{"patch": patch})
		return tool.Run(context.Background(), in, env)
	}
	os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "taken.txt"), []byte("x\n"), 0o644)
	if r := run("*** Begin Patch\n*** Update File: run.sh\n*** Move to: taken.txt\n-echo hi\n+echo yo\n*** End Patch"); !r.IsError || !strings.Contains(r.Output, "already exists") {
		t.Fatalf("move onto an existing file: %+v", r)
	}
	if r := run("*** Begin Patch\n*** Update File: run.sh\n*** Move to: bin/run.sh\n-echo hi\n+echo yo\n*** End Patch"); r.IsError {
		t.Fatal(r.Output)
	}
	fi, err := os.Stat(filepath.Join(dir, "bin", "run.sh"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("moved file: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "run.sh")); err == nil {
		t.Fatal("the old path should be gone")
	}
	// A second file that cannot be written rolls the first back and deletes nothing.
	os.MkdirAll(filepath.Join(dir, "ro"), 0o555)
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "ro"), 0o755) })
	r := run("*** Begin Patch\n*** Update File: taken.txt\n-x\n+y\n*** Delete File: bin/run.sh\n*** Add File: ro/new.txt\n+n\n*** End Patch")
	if !r.IsError {
		t.Fatalf("write into a read-only directory should fail: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "taken.txt")); string(b) != "x\n" {
		t.Fatalf("rolled back content %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "run.sh")); err != nil {
		t.Fatal("a delete ran although a write failed")
	}
	if _, err := os.Stat(filepath.Join(dir, "taken.txt.stavlos-tmp")); err == nil {
		t.Fatal("temporary file left behind")
	}
}

// TestApplyPatchSectionsChain: each section sees the ones before it — two
// updates to one file, a delete and add of one path, an update after a
// move — and an insertion whose anchor is missing is an error, not an
// append at the end of the file.
func TestApplyPatchSectionsChain(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Dir: dir}
	run := func(patch string) Result {
		in, _ := json.Marshal(map[string]string{"patch": patch})
		return patchTool{}.Run(context.Background(), in, env)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return string(b)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	if r := run("*** Begin Patch\n*** Update File: a.txt\n-one\n+uno\n*** Update File: a.txt\n-three\n+tres\n*** End Patch"); r.IsError || read("a.txt") != "uno\ntwo\ntres\n" {
		t.Fatalf("two updates: %+v %q", r, read("a.txt"))
	}
	if r := run("*** Begin Patch\n*** Delete File: a.txt\n*** Add File: a.txt\n+fresh\n*** End Patch"); r.IsError || read("a.txt") != "fresh\n" {
		t.Fatalf("delete and add: %+v %q", r, read("a.txt"))
	}
	if r := run("*** Begin Patch\n*** Update File: a.txt\n*** Move to: b.txt\n-fresh\n+moved\n*** Update File: b.txt\n-moved\n+edited\n*** End Patch"); r.IsError || read("b.txt") != "edited\n" || read("a.txt") != "" {
		t.Fatalf("update after move: %+v %q", r, read("b.txt"))
	}
	if r := run("*** Begin Patch\n*** Update File: b.txt\n@@ not a line of the file\n+inserted\n*** End Patch"); !r.IsError || !strings.Contains(r.Output, "anchor not found") || read("b.txt") != "edited\n" {
		t.Fatalf("missing anchor: %+v %q", r, read("b.txt"))
	}
}

// TestApplyPatchKeepsCRLF: a CRLF file is patched with LF hunks and keeps
// its line endings.
func TestApplyPatchKeepsCRLF(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "w.txt"), []byte("one\r\ntwo\r\n"), 0o644)
	in, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Update File: w.txt\n one\n-two\n+2\n+3\n*** End Patch"})
	if r := (patchTool{}).Run(context.Background(), in, &Env{Dir: dir}); r.IsError {
		t.Fatal(r.Output)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "w.txt")); string(b) != "one\r\n2\r\n3\r\n" {
		t.Fatalf("%q", b)
	}
}

// TestApplyPatchIgnoresPlantedTempLinks: the temporary file is created
// fresh under a random name, so a link planted where the old fixed name
// was is never written through, and nothing is left behind.
func TestApplyPatchIgnoresPlantedTempLinks(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("safe\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\n"), 0o644)
	os.Symlink(victim, filepath.Join(dir, "f.txt.stavlos-tmp"))
	in, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Update File: f.txt\n-a\n+b\n*** End Patch"})
	if r := (patchTool{}).Run(context.Background(), in, &Env{Dir: dir}); r.IsError {
		t.Fatal(r.Output)
	}
	if b, _ := os.ReadFile(victim); string(b) != "safe\n" {
		t.Fatalf("wrote through a planted link: %q", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("left behind: %v", entries)
	}
}
