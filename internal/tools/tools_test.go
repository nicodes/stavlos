package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileToolsAndBash(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Dir: dir}
	ts := Builtin()
	ctx := context.Background()
	r := ts["write"].Run(ctx, json.RawMessage(`{"path":"a/b.txt","content":"hello\nworld\n"}`), env)
	if r.IsError {
		t.Fatal(r.Output)
	}
	r = ts["edit"].Run(ctx, json.RawMessage(`{"path":"a/b.txt","old_string":"world","new_string":"there"}`), env)
	if r.IsError {
		t.Fatal(r.Output)
	}
	r = ts["read"].Run(ctx, json.RawMessage(`{"path":"a/b.txt"}`), env)
	if !strings.Contains(r.Output, "2\tthere") {
		t.Fatal(r.Output)
	}
	if _, ok := ts["grep"]; ok {
		t.Fatal("grep should be gone: bash covers it")
	}
	if _, ok := ts["glob"]; ok {
		t.Fatal("glob should be gone: bash covers it")
	}
	var partial strings.Builder
	env.Partial = func(s string) { partial.WriteString(s) }
	r = ts["bash"].Run(ctx, json.RawMessage(`{"command":"echo hi; exit 3"}`), env)
	if !r.IsError || !strings.Contains(r.Output, "hi") || !strings.Contains(r.Output, "exit status 3") || partial.String() != "hi\n" {
		t.Fatalf("%+v partial=%q", r, partial.String())
	}
	// cancellation kills the process group and returns partial output
	cctx, cancel := context.WithCancel(ctx)
	start := time.Now()
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	r = ts["bash"].Run(cctx, json.RawMessage(`{"command":"echo start; sleep 30; echo never"}`), env)
	if time.Since(start) > 5*time.Second || !strings.Contains(r.Output, "start") || strings.Contains(r.Output, "never") {
		t.Fatalf("cancel: %+v after %v", r, time.Since(start))
	}
	if ts["bash"].PolicyArg(json.RawMessage(`{"command":"git push"}`)) != "git push" {
		t.Fatal("policy arg")
	}
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
}
