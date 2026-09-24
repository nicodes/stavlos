package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The policy judges a path with its links resolved, before a prompt the
// human may take a while over; the tool opens it afterwards. A link swapped
// in between (a background job may write anywhere in the working set)
// would be followed by a plain open to wherever it leads. Opened through
// the working directory as a root, at the path that was judged, it is
// refused instead.

func rootedEnv(t *testing.T) (*Env, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Env{Dir: dir, Roots: []string{dir}}, dir
}

func TestReadRefusesALinkSwappedInAfterTheJudgement(t *testing.T) {
	env, dir := rootedEnv(t)
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("key\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "x"), []byte("ok\n"), 0o644)
	env.Judged = map[string]string{"x": ResolvePath(dir, "x")} // judged: a file inside
	_ = os.Remove(filepath.Join(dir, "x"))
	_ = os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "x"))
	r := Builtin()["read"].Run(context.Background(), json.RawMessage(`{"path":"x"}`), env)
	if !r.IsError || strings.Contains(r.Output, "key") {
		t.Fatalf("a swapped link was followed: %+v", r)
	}
	// grep walks and opens the same way
	r = Builtin()["grep"].Run(context.Background(), json.RawMessage(`{"pattern":"key","path":"x"}`), env)
	if strings.Contains(r.Output, "key") {
		t.Fatalf("grep followed a swapped link: %+v", r)
	}
}

func TestPatchRefusesADirectorySwappedForALink(t *testing.T) {
	env, dir := rootedEnv(t)
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "f.txt"), []byte("theirs\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte("mine\n"), 0o644)
	env.Judged = map[string]string{"sub/f.txt": ResolvePath(dir, "sub/f.txt")}
	_ = os.RemoveAll(filepath.Join(dir, "sub"))
	_ = os.Symlink(outside, filepath.Join(dir, "sub"))
	patch := "*** Begin Patch\n*** Update File: sub/f.txt\n@@\n-mine\n+changed\n*** End Patch"
	b, _ := json.Marshal(map[string]string{"patch": patch})
	r := Builtin()["apply_patch"].Run(context.Background(), b, env)
	if !r.IsError {
		t.Fatalf("a patch wrote through a swapped directory link: %+v", r)
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "f.txt")); string(got) != "theirs\n" {
		t.Fatalf("the file outside changed: %q", got)
	}
	// and an add through it lands nowhere outside either
	patch = "*** Begin Patch\n*** Add File: sub/new.txt\n+x\n*** End Patch"
	b, _ = json.Marshal(map[string]string{"patch": patch})
	env.Judged = map[string]string{"sub/new.txt": filepath.Join(dir, "sub", "new.txt")}
	if r := Builtin()["apply_patch"].Run(context.Background(), b, env); !r.IsError {
		t.Fatalf("an add wrote through a swapped directory link: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("a file appeared outside the working set")
	}
}

// A path the human allowed outside every root is opened as it is.
func TestOutsideARootOpensPlainly(t *testing.T) {
	env, _ := rootedEnv(t)
	other := t.TempDir()
	p := filepath.Join(other, "note.txt")
	_ = os.WriteFile(p, []byte("hello\n"), 0o644)
	env.Judged = map[string]string{p: p}
	if r := Builtin()["read"].Run(context.Background(), json.RawMessage(`{"path":"`+p+`"}`), env); r.IsError || r.Output != "hello\n" {
		t.Fatalf("%+v", r)
	}
}
